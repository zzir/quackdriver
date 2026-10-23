# quackdriver

Pure-Go [`database/sql`](https://pkg.go.dev/database/sql) driver for [DuckDB](https://duckdb.org/)'s **Quack** remote protocol.

> **Status: alpha (v0.2.0).** The driver speaks Quack protocol **v3**, the protocol of **DuckDB 2.0**. It does not talk to DuckDB 1.x servers (Quack v1): the handshake fails with a message saying that DuckDB 2.0 or newer is required. DuckDB does not consider the Quack protocol stable yet, so pin DuckDB releases in production.

## Highlights

- **No CGO.** No `libduckdb` embedded. The driver speaks HTTP/1.1 (`POST /quack`) to a remote DuckDB server, so it cross-compiles like any other pure-Go binary.
- **Zero third-party dependencies** in the core driver: only the Go standard library.
- **`database/sql` compatible.** `sql.Open("quack", dsn)`, connection pooling, client-side prepared statements, transactions, `context` cancellation (sent to the server as `CANCEL`), typed column metadata.
- **Streaming results.** Large results arrive in FETCH batches that the driver reads ahead in the background (`fetch_ahead`), with heartbeats keeping the session alive between batches.
- **Bulk insert.** `Conn.Append` and the streaming `Appender` stream rows (`SEND_DATA`) into one atomic `INSERT` that casts them with DuckDB's own rules: millions of narrow rows per second against a local server, versus thousands for single-row `INSERT` statements (see [Bulk insert](#bulk-insert-connappend-and-appender)).
- **TLS with certificate pinning** (`ssl_fingerprint`), custom TLS configuration and extra HTTP headers for reverse proxies.
- **Golden-fixture-locked wire format.** 79 request/response exchanges captured from the official DuckDB v2.0.0 release CLI (quack extension `f96420e`; see [`testdata/golden/README.md`](./testdata/golden/README.md)) guard against protocol drift; unit tests run without a server.

## Install

```bash
go get github.com/zzir/quackdriver
```

Without a version, `go get` takes the newest tag, `v0.2.0` or later. Do not pin `v0.1.0-alpha.1`: that is the Quack **v1** driver for DuckDB 1.x, and against a DuckDB 2.0 server it fails at the handshake with `codec: expected end-of-object marker at offset 87`, not with a message about versions.

Requirements:

- Go 1.26+. The minimum follows Go's own support policy: it is the older of the two Go releases maintained upstream, and it moves up when a new Go release ships.
- For the server side: the DuckDB CLI **v2.0.0 or newer** with the Quack extension (`INSTALL quack; LOAD quack;`)

## Quick start

Start a server (in one terminal):

```bash
bash scripts/start-quack-server.sh
# or:
duckdb -cmd "INSTALL quack; LOAD quack; CALL quack_serve('quack:127.0.0.1:9494', token => 'super_secret');"
```

Use it from Go:

```go
package main

import (
    "context"
    "database/sql"
    "fmt"
    "log"

    _ "github.com/zzir/quackdriver"
)

func main() {
    db, err := sql.Open("quack", "quack://127.0.0.1:9494?token=super_secret")
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()

    rows, err := db.QueryContext(context.Background(), "SELECT 42 AS i, 'hello' AS s")
    if err != nil {
        log.Fatal(err)
    }
    defer rows.Close()
    for rows.Next() {
        var i int
        var s string
        if err := rows.Scan(&i, &s); err != nil {
            log.Fatal(err)
        }
        fmt.Println(i, s)
    }
    if err := rows.Err(); err != nil {
        log.Fatal(err)
    }
}
```

A server bound to `127.0.0.1` or `localhost` speaks plain HTTP by default; one bound to any other address speaks HTTPS with a self-signed certificate (see [`ssl_fingerprint`](#dsn-and-connector-options)).

## Examples

Each example is `go run ./examples/<name>`. They connect to `QUACK_HOST:QUACK_PORT` (default `127.0.0.1:9494`) with `QUACK_TOKEN` (default `super_secret`), or to a full `QUACK_DSN`. The integration suite (`make test-integration`) builds and runs them all against its own server.

| Example | What it shows |
|---|---|
| `basic` | Connection, DDL/DML in a transaction, `RowsAffected`, `$N` and `$name` placeholders, a typed `*quackdriver.Error`; a result spanning many FETCH batches, early `Close`, updates while iterating inside a transaction |
| `types` | Every scalar type, each Go value checked against the server's own `VARCHAR` rendering; LIST / ARRAY / STRUCT / TUPLE / MAP / UNION values; TIME, TIMETZ (`TimeTzValue`), INTERVAL (`IntervalValue`), TIMESTAMPTZ and BC timestamps; time values as query arguments |
| `append` | `Conn.Append` with rich and nested types and a user-defined ENUM, a rejected Go value and a value the server cannot cast; the streaming `Appender` with a skipped row, `Close` returning the inserted count, `ErrConnBusy`, `Abort` and a constraint violation |
| `tls-pin` | HTTPS with `ssl_fingerprint` pinning; wrong pins and unpinned HTTPS are refused (prints setup instructions when no certificate is given) |
| `bench-append` | Per-row INSERT vs 1000-row INSERT vs `Conn.Append` vs `Appender` (`make bench`) |
| `bun` | bun ORM integration (separate `go.mod`) |

## Bulk insert: `Conn.Append` and `Appender`

Quack v3 has a client data stream: the server runs

```sql
INSERT INTO t (cols) SELECT c0::T0, c1::T1, ... FROM scan_data_from_quack_client(...)
```

and the client sends each value as text in DataChunks of VARCHAR columns (`SEND_DATA`), so values are never rendered as SQL. The server casts them, so DuckDB's own rules decide what a value means and whether it fits. The whole load is one statement: if it fails or is aborted, no row is inserted.

Measured against a local DuckDB 2.0 server (200,000 rows of `(BIGINT, VARCHAR, DOUBLE)`, `make bench`):

```
INSERT per row               1000 rows    312ms         3204 rows/s
INSERT 1000 rows/stmt      200000 rows    2.35s        85186 rows/s
Conn.Append                200000 rows     46ms      4310159 rows/s
Appender                   200000 rows     62ms      3243454 rows/s
```

Those are narrow rows that fit in memory; the cost per row grows with the number of columns. `Conn.Append` and the `Appender` share one send path, so `Conn.Append` is never faster per row: it differs in needing every row in memory as `[][]any` before it starts, which for millions of wide rows is gigabytes of boxed values. Use `Conn.Append` for batches that fit comfortably in memory and the `Appender` for large or unbounded loads, which it streams about 32 MiB at a time.

`Conn.Append` streams rows already in memory:

```go
import "github.com/zzir/quackdriver"

conn, _ := db.Conn(ctx)
defer conn.Close()

err := conn.Raw(func(d any) error {
    return d.(*quackdriver.Conn).Append(ctx, "users",
        []quackdriver.AppendColumn{
            {Name: "id",      Type: "BIGINT"},
            {Name: "profile", Type: "STRUCT(name VARCHAR, score DECIMAL(9,2))"},
            {Name: "tags",    Type: "VARCHAR[]"},
            {Name: "mood",    Type: "mood"}, // a user-defined ENUM
        },
        [][]any{
            {int64(1), map[string]any{"name": "alice", "score": "98.5"}, []any{"go", "duckdb"}, "happy"},
            {int64(2), map[string]any{"name": "bob",   "score": 73.2},   []any{"sql"},          nil},
        },
    )
})
```

The `Appender` streams rows as they are produced, buffering about `WithBatchBytes` (default 32 MiB) per request, and `Close` returns the number of rows the server inserted:

```go
err := conn.Raw(func(d any) error {
    a, err := d.(*quackdriver.Conn).NewAppender(ctx, "", "events", cols)
    if err != nil {
        return err
    }
    defer a.Abort() // no-op after a successful Close
    for e := range events {
        if err := a.AppendRow(e.ID, e.Name, e.At); err != nil {
            var ve *quackdriver.ValueError
            if errors.As(err, &ve) {
                continue // the row was skipped; the Appender is still usable
            }
            return err // the stream has ended
        }
    }
    n, err := a.Close()
    log.Printf("inserted %d rows", n)
    return err
})
```

While an `Appender` is open it holds its connection: other statements on it fail with `ErrConnBusy`. That is the driver's `*quackdriver.Conn`: inside the `conn.Raw` callback, `database/sql` holds the connection's lock, so a statement run through the same `*sql.Conn` there (`conn.ExecContext`, say) does not get `ErrConnBusy` but blocks forever. Run other statements on another connection, or after the callback returns. A connection returned to the pool with an `Appender` still open (neither `Close` nor `Abort` was called) is discarded, which aborts its `INSERT`. `WithUnordered()` lets the server insert in parallel without keeping the row order. Cancel the context given to `NewAppender` to abort a blocked call.

`AppendColumn.Type` is any DuckDB type expression (`INTEGER`, `DECIMAL(18, 4)`, `INTEGER[3]`, `STRUCT(a INTEGER, "b c" VARCHAR)`, `MAP(VARCHAR, INTEGER)`, an alias such as `TEXT`, or a user-defined type). The server resolves the types in a statement of its own when the `Appender` starts; the values each type accepts are in the [write table](#write-direction-go--duckdb-via-connappend-and-appender) below.

A Go value a column does not accept (a `bool` for a VARCHAR column, say) is a `*quackdriver.ValueError` naming the row and column: the `Appender` skips the row, `Conn.Append` aborts the statement. A value DuckDB cannot cast (an out-of-range integer, a malformed UUID, an unknown ENUM label, a duplicate MAP key) fails the statement on the server with a `*quackdriver.Error`; nothing is inserted.

## bun ORM

Use [`uptrace/bun`](https://github.com/uptrace/bun) with `sqlitedialect`:

```go
import (
    "database/sql"

    "github.com/uptrace/bun"
    "github.com/uptrace/bun/dialect/sqlitedialect"
    _ "github.com/zzir/quackdriver"
)

type User struct {
    bun.BaseModel `bun:"table:users"`

    ID     int64  `bun:"id,pk,type:BIGINT"`   // see gotcha below
    Name   string `bun:"name,notnull"`
    Active bool   `bun:"active"`
}

sqldb, _ := sql.Open("quack", "quack://127.0.0.1:9494?token=super_secret")
db := bun.NewDB(sqldb, sqlitedialect.New())
```

> **bun gotcha — `INTEGER` vs `BIGINT`.** bun's sqlitedialect maps `int64` Go fields to SQL `INTEGER`, which is 64-bit in SQLite but **32-bit in DuckDB**. Annotate `int64` columns explicitly with `type:BIGINT`.

Full working example in [`examples/bun/`](./examples/bun).

## DSN and connector options

```
quack://host:port?token=<token>&...
quack:host:port?token=<token>
quack:host                          # port 9494
quack:[::1]:9494?token=<token>      # IPv6
```

Parameter names are case-insensitive, each may appear once, and unknown names are rejected. Values are URL-decoded, so a literal `+`, `&` or `%` must be percent-encoded.

| Param | Default | Notes |
|---|---|---|
| `token` | (none) | Authentication token. The server's authentication function decides whether it is accepted (by default it must equal the server's token, which is at least 4 characters). |
| `tls` | local host: `false`, else `true` | HTTPS on/off. Local hosts are `localhost`, `127.0.0.1` and `::1`. |
| `disable_ssl` | | The official client's inverse of `tls`. |
| `ssl_fingerprint` | (none) | SHA-256 of the server certificate (as `quack_generate_keys()` reports it; colons, case and a `sha256:` prefix are ignored). Implies HTTPS; the pin replaces CA and hostname verification. |
| `timeout` | none | Upper bound for each HTTP request. Without it, statements run as long as they need and the `context` decides. |
| `connect_timeout` | `10s` | TCP connect and TLS handshake. |
| `heartbeat_timeout` | `60s` | Session lease requested from the server (whole seconds, at least 1s). The driver sends heartbeats well within it. |
| `client_id` | `$QUACK_CLIENT_ID`, else a random per-process id | Sent in the handshake; empty or at least 4 characters. |
| `inline_rows` | server setting | Rows the server may return inline in the PREPARE response. |
| `fetch_ahead` | `1` | FETCH requests kept in flight while a result is read (read-ahead); `0` disables it. |
| `max_buffered_rows` | `1000000` | The most rows of an open `Rows` the driver buffers in memory when another statement starts on the same connection (see [Limitations](#limitations--gotchas)); a statement that would need more fails with an error naming this parameter, and the `Rows` stays readable. `0` removes the bound. |
| `timezone` | server setting | The session's `TimeZone`, set (`SET SESSION TimeZone = ...`) right after each connection's handshake. Use `timezone=UTC` when `time.Time` arguments meet `TIMESTAMPTZ` values (see [Query parameters](#query-parameters)). |

Durations use Go syntax (`30s`, `1m`); a bare integer means seconds.

`NewConnector(dsn, opts...)` with `sql.OpenDB` takes options: `WithTLSConfig` (custom root CAs, client certificates), `WithHeader` (for example authentication for a reverse proxy), `WithMaxIdleConnsPerHost` (the idle HTTP connection pool, default 32; size it at about (2 + `fetch_ahead`) × the `database/sql` pool size for many busy connections) and `WithHTTPClient` (a caller-owned `*http.Client`; it should not follow redirects). Like any Go HTTP client, the driver honours `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`. The driver never follows HTTP redirects (a Quack server does not send them).

Through `sql.Conn.Raw`, `(*quackdriver.Conn).ServerInfo()` reports what the server said in the handshake: its DuckDB version and platform, the Quack protocol version and the heartbeat lease it granted.

## Query parameters

Quack carries only SQL text, so the driver substitutes arguments as SQL literals before sending (`internal/sqlfmt`, the driver's injection-defense surface; it is covered by property tests against a real server). Placeholders:

| Form | Meaning |
|---|---|
| `?` | The next positional argument. |
| `?N`, `$N` | Positional argument N (1-based); may repeat. Cannot be mixed with `?`. |
| `$name` | The argument passed as `sql.Named("name", v)`. Without such an argument, `$name` is left for DuckDB (session variables). |

Placeholders inside strings (`'...'`, `E'...'`, `$tag$...$tag$`), quoted identifiers and comments are ignored. Every placeholder needs an argument and every argument must be used.

| Go value | SQL literal |
|---|---|
| `nil` | `NULL` |
| `bool` | `TRUE` / `FALSE` |
| integers, `*big.Int`, `big.Int` | integer literal (DuckDB picks INTEGER, BIGINT, HUGEINT, ...) |
| `float32`, `float64` | DOUBLE literal in exponent form (`1.5e+00`); NaN, ±Inf and -0 as `'nan'::DOUBLE` etc. |
| `*big.Rat` | DECIMAL literal of at most 38 digits (rounded half away from zero if needed); integral values as integer literals. A non-integer with 38 or more integer digits is an error. |
| `string` | `'...'` with single quotes doubled (backslashes have no special meaning in DuckDB's standard strings) |
| `[]byte` | BLOB literal; a nil `[]byte` (what a NULL scans into) is `NULL` |
| `time.Time` | `TIMESTAMP '...'` of its UTC wall clock, `TIMESTAMP_NS '...'` with sub-microsecond digits; `InfinityTime` / `NegInfinityTime` as `TIMESTAMP 'infinity'` / `'-infinity'`. See the `TIMESTAMPTZ` note below. |
| `time.Duration` | `INTERVAL` (microsecond precision) |
| `IntervalValue`, `TimeTzValue` | `INTERVAL '...'`, `TIMETZ '...'` |
| `driver.Valuer` | its value |

**`time.Time` and `TIMESTAMPTZ`.** A `time.Time` argument is a zone-less `TIMESTAMP` literal of its UTC wall clock. Where it meets a `TIMESTAMPTZ` (a column, a comparison, a cast), DuckDB converts it in the session's `TimeZone`, which defaults to the server host's zone: on a server outside UTC the stored or compared instant is off by the zone offset, while `TIMESTAMPTZ` values always scan into the correct UTC instant. Add `timezone=UTC` to the DSN (or run `SET TimeZone = 'UTC'` on the session) whenever `time.Time` arguments meet `TIMESTAMPTZ` values. `Conn.Append` and the `Appender` write `TIMESTAMPTZ` columns from the instant and are not affected.

A query string with several statements runs them all, but returns only one result: the first statement's that returns rows, otherwise the last statement's. Its `RowsAffected` describes that one result.

DuckDB replaces Unicode space characters in the query text with `' '` before parsing, using a simpler quoting model than its tokenizer. If that would change a substituted string (for example when a comment or an `E''` string before the placeholder contains a quote), or change where a string literal, comment or placeholder of the query begins or ends (a Unicode space in the query text, for example copied from a web page, can split a word into an `E'...'` or `$tag$` opener), the query is rejected instead of being silently altered. A substituted string that DuckDB would join onto a string literal on the previous line (DuckDB concatenates `'a'` + newline + `'b'`, keeping the first literal's `E`/`X` kind) is parenthesized, so it stays a separate value.

## Type mapping

### Read direction (DuckDB → Go)

| DuckDB type | Go type returned to `Rows.Scan` (`any`) |
|---|---|
| `BOOLEAN` | `bool` |
| `TINYINT` / `SMALLINT` / `INTEGER` / `BIGINT` | `int8` / `int16` / `int32` / `int64` |
| `UTINYINT` / `USMALLINT` / `UINTEGER` / `UBIGINT` | `uint8` / `uint16` / `uint32` / `uint64` |
| `HUGEINT`, `UHUGEINT`, `BIGNUM` | `*big.Int` |
| `FLOAT`, `DOUBLE` | `float32`, `float64` |
| `DECIMAL(w, s)` | `string`, exact (e.g. `"12345.67"`) |
| `VARCHAR`, `CHAR`, `JSON` | `string` |
| `UUID` | `string` (lowercase 8-4-4-4-12) |
| `BIT` | `string` of `0`/`1` |
| `ENUM` | `string` label |
| `BLOB` | `[]byte` |
| `GEOMETRY` | `[]byte` (WKB) |
| `DATE` | `time.Time` (UTC midnight) |
| `TIMESTAMP`, `TIMESTAMP_S/_MS/_NS`, `TIMESTAMPTZ`, `TIMESTAMPTZ_NS` | `time.Time` (UTC) |
| `TIME`, `TIME_NS` | `time.Duration` since midnight |
| `TIME WITH TIME ZONE` | `quackdriver.TimeTzValue` (`TimeOfDay`, `OffsetSeconds`, `Location`, ...; build one with `NewTimeTz`, the zero value is `00:00:00+00`) |
| `INTERVAL` | `quackdriver.IntervalValue` (months / days / micros) |
| `LIST`, `ARRAY` | `[]any` |
| `STRUCT` | `map[string]any` |
| `TUPLE` (unnamed STRUCT) | `[]any`, positional |
| `MAP` | `[]any` of `map[string]any{"key": k, "value": v}` (keeps the order) |
| `UNION` | `map[string]any` with the members (the inactive ones `nil`) and the tag under `""` (`uint8`) |
| `NULL` (any column) | `nil` |

`VARIANT` values are not decoded: a query whose result has a VARIANT column, also nested in another type, fails with an error suggesting a cast in the query, such as `SELECT v::JSON`.

DuckDB's `'infinity'` / `'-infinity'` dates and timestamps scan into `quackdriver.InfinityTime` / `NegInfinityTime` (5881580-07-11 and 5877642-06-24 BC UTC; compare with `Equal`). BC dates use astronomical year numbering, as DuckDB does: Go year 0 is 1 BC.

`Rows.ColumnTypes()` reports `DatabaseTypeName` (e.g. `DECIMAL`, `TIMESTAMP WITH TIME ZONE`), `ScanType` and `DecimalSize`.

### Write direction (Go → DuckDB via `Conn.Append` and `Appender`)

Values are sent as text that the server casts to the column's type, so ranges and formats are DuckDB's.

| DuckDB type | Accepted Go values |
|---|---|
| `BOOLEAN` | `bool` |
| `TINYINT` … `BIGINT`, `UTINYINT` … `UBIGINT` | any Go integer |
| `HUGEINT`, `UHUGEINT`, `BIGNUM` | any Go integer, `*big.Int`, `big.Int`, decimal string |
| `FLOAT`, `DOUBLE` | any Go integer or float |
| `DECIMAL(w, s)` | decimal string (DuckDB rounds extra digits), `*big.Int` / `big.Int` (unscaled), Go integer, float (multiplied by 10^s and rounded half away from zero exactly like DuckDB's `DOUBLE` → `DECIMAL` cast, so it stores what `INSERT ... VALUES (?)` stores) |
| `VARCHAR`, `CHAR`, `JSON` | `string`, `[]byte` (valid UTF-8; a nil `[]byte` is NULL) |
| `BLOB` | `[]byte` (a nil `[]byte` is NULL), `string` |
| `BIT` | `string` of `0`/`1` |
| `UUID` | `string` (8-4-4-4-12 hex) |
| `DATE` | `time.Time` (its UTC date) |
| `TIME`, `TIME_NS` | `time.Duration` in [0, 24h], or `time.Time` (its UTC time of day) |
| `TIME WITH TIME ZONE` | `TimeTzValue`, or `time.Time` (its local time of day and offset) |
| `TIMESTAMP*`, `TIMESTAMPTZ*` | `time.Time`; digits below the column's precision round to the nearest unit, halves away from the epoch, like DuckDB's own timestamp casts |
| `INTERVAL` | `IntervalValue`, `time.Duration` |
| `ENUM(...)` | `string` label or integer index |
| `T[]` | `[]any` |
| `T[N]` | `[]any` of exactly N elements |
| `STRUCT(...)` | `map[string]any` (missing fields are NULL; unknown keys are errors) |
| `TUPLE(...)` | `[]any`, one value per member |
| `MAP(K, V)` | `map[string]any` (string keys, sent sorted) or `[]any` of `map[string]any{"key": k, "value": v}` |
| any column | `nil` for NULL |

UNION columns cannot be appended (DuckDB's cast from text picks the member itself), nor can VARIANT and GEOMETRY columns.

## Errors and results

- Server errors are `*quackdriver.Error` with DuckDB's exception `Type` (`"Catalog"`, `"Constraint"`, `"Parser"`, ...), `Message` and `ExtraInfo`:

  ```go
  var qe *quackdriver.Error
  if errors.As(err, &qe) && qe.Type == "Constraint" { ... }
  ```

- A lost session (server restart, expired lease) yields `driver.ErrBadConn`, so `database/sql` retries on a fresh connection when the statement provably did not run.
- `RowsAffected` reports DuckDB's changed-row count for `INSERT`, `UPDATE`, `DELETE`, `MERGE`, `COPY` and `CREATE TABLE ... AS`, the number of returned rows for DML with a `RETURNING` clause, and 0 for other statements. For a query string with several statements it describes the one result the server returns (the first statement's that returns rows, otherwise the last one's), not a total. `LastInsertId` is not supported (use `INSERT ... RETURNING`).
- A transaction statement (`BEGIN`, `COMMIT`, `ROLLBACK`) that fails without a definite answer (its `context` ended, or the connection broke after it was sent) discards the connection, because the session may be inside a transaction the driver no longer knows about.
- When a `context` ends, the driver cancels the statement on the server and returns `ctx.Err()`; the next statement on the session waits until the cancellation has settled.
- Closing `Rows` early cancels a read-only statement; other statements (e.g. `INSERT ... RETURNING`) are read to the end so their writes complete.

## Limitations & gotchas

- **No server-side bind.** Parameters are substituted client-side (see [Query parameters](#query-parameters)). New types accepted by `sqlfmt.FormatValue` must render injection-safe literals of the intended type.
- **One statement at a time per session.** Starting a statement while a `Rows` of the same connection is still open first reads the rest of that result into memory (Quack aborts a statement when the next one is prepared), so iterating rows while running updates in a transaction works, at the cost of buffering. The buffer is bounded by the DSN's `max_buffered_rows` (default 1,000,000): beyond it the new statement fails with an error naming the parameter and the `Rows` keeps streaming; finish or close the `Rows` first, use another connection, or raise the bound.
- **`DECIMAL(w, w)`** values decode as `"0.5"` where DuckDB renders `.5`; floats render as Go does (`NaN` vs DuckDB's `nan`).
- **Server bind address.** `quack_serve('quack:127.0.0.1:9494')` binds IPv4 only while `localhost` may resolve to `::1` first; the examples use `127.0.0.1`.
- **bun + `int64` columns** must be tagged `type:BIGINT` (see [bun ORM](#bun-orm)).
- **FSST-compressed vectors** are rejected with a clear error (the server has not been seen to send them).
- **VARIANT** values are not decoded; cast them in the query (`v::JSON`).
- **Typed `Scan` for nested types** is not implemented: LIST/STRUCT/MAP/ARRAY come back as `[]any` / `map[string]any`.

## Development

```bash
make test              # unit tests (fake server in internal/quacktest; no DuckDB needed)
make test-integration  # the unit tests plus `go test -tags integration ./...`: the tests start their own
                       # DuckDB 2.0 servers, and integration/examples runs every example and checks its output
make lint              # go vet + golangci-lint with the versions CI uses (.tool-versions)

make server            # a Quack server in the foreground (scripts/start-quack-server.sh); Ctrl-C stops it
make bench             # INSERT vs Conn.Append vs Appender throughput
make dump-wire         # regenerate testdata/golden/*.bin
```

The test servers and `make server` run `DUCKDB_BIN` (default `duckdb` on `PATH`; the primary target is the official v2.0.0 release CLI) with `QUACK_TEST_INIT_SQL` in place of the default `INSTALL quack; LOAD quack;` before `quack_serve`. Without httpfs (for example when the extension repository is unreachable) `quack_serve` needs `force_mbedtls_unsafe` to generate its keys:

```bash
make test-integration DUCKDB_BIN=/path/to/duckdb QUACK_TEST_INIT_SQL="SET force_mbedtls_unsafe = 'true'; INSTALL quack; LOAD quack;"
```

`make bench` and `make dump-wire` use a server already answering on `QUACK_HOST:QUACK_PORT` (default `127.0.0.1:9494`) as is; otherwise `scripts/with-server.sh` starts one and stops it afterwards.

`.tool-versions` pins the Go and golangci-lint that lint the code, locally and in CI; `make lint` refuses other versions (`mise install` or `asdf install` installs these). The tests themselves run on the minimum Go (`go.mod`'s go line) and on the latest one. Dependabot keeps the GitHub Actions and the bun example's dependencies current; the weekly `canary` workflow lints with the newest Go and golangci-lint and turns red when a Go release calls for bumping `.tool-versions` or the minimum Go version.

### Project layout

```
quackdriver/
├── quackdriver.go, connector.go, conn.go, stmt.go, rows.go, tx.go   ← driver.* surface
├── fetch.go, heartbeat.go                                          ← FETCH read-ahead, session lease
├── append.go, appender.go                                          ← Conn.Append, Appender
├── errors.go, types.go, value.go, options.go
├── internal/
│   ├── codec/       — BinarySerializer (ULEB/SLEB, object framing, hugeint)
│   ├── logicaltype/ — DuckDB type system (LogicalType + ExtraInfo, SQL rendering)
│   ├── message/     — Quack v3 messages + DataChunk/vector encode/decode
│   ├── value/       — IntervalValue, TimeTzValue, date/timestamp conversion
│   ├── transport/   — DSN parser + HTTP/1.1 client (TLS, pinning)
│   ├── sqlfmt/      — placeholder scanner + SQL literal rendering (security-critical)
│   ├── quacktest/   — fake Quack server for unit tests
│   └── livetest/    — starts real servers for the integration tests
├── integration/     — integration tests against real DuckDB servers (-tags integration)
├── cmd/dump-wire/   — captures wire fixtures from a live server
├── cmd/probe-fetch/ — runs and reports a FETCH loop by hand
├── testdata/golden/ — request/response byte fixtures (committed)
├── examples/        — runnable end-to-end programs
└── doc/PROTOCOL.md  — wire-format cheat sheet
```

The layering is bottom-up: `codec` → `logicaltype` → `message` → `transport` → driver surface. New protocol knowledge belongs at the lowest layer that can host it.

### Adding wire fixtures

`testdata/golden/*.bin` are request/response byte captures from the official DuckDB v2.0.0 release (see [`testdata/golden/README.md`](./testdata/golden/README.md)), used by `go test ./internal/message/` to detect protocol drift without a live server. Regenerate when intentionally bumping the server version:

```bash
make dump-wire
git diff testdata/golden     # any byte change is a protocol drift — investigate first
```

### Reporting bugs

Please include:

1. The DuckDB server version (`SELECT version();`).
2. The exact DSN (without the token) and the SQL statement (or `Conn.Append` / `Appender` arguments) that triggered the issue.
3. A `make dump-wire` capture (`testdata/golden/`) if the regression is wire-related.

## References

There is no official Quack wire-protocol specification: DuckDB's documentation describes the protocol only at a high level, and its FAQ says the protocol is not yet stable. The de-facto schema is the duckdb-quack source.

| Source | Use for |
|---|---|
| [`duckdb/duckdb-quack`](https://github.com/duckdb/duckdb-quack) — `src/include/quack_message.json`, the generated `src/serialize_quack_message.cpp`, `src/quack_server.cpp` | Message schema and server behaviour. |
| [DuckDB](https://github.com/duckdb/duckdb) — `src/common/serializer/`, `src/common/types/vector.cpp` | `BinarySerializer` and Vector/type serialization (`StorageVersion::V2_0_0`). |
| [DuckDB docs — Quack overview](https://duckdb.org/docs/current/quack/overview) | High-level, user-facing description. |
| [`tobilg/quack-protocol`](https://github.com/tobilg/quack-protocol) (TypeScript), [`gizmodata/quack-jdbc`](https://github.com/gizmodata/quack-jdbc) (Java) | Other client implementations. |
| Local: [`doc/PROTOCOL.md`](./doc/PROTOCOL.md) | Distilled wire-format notes (what this driver implements). |

Decoding and encoding are checked byte for byte against fixtures captured from the official DuckDB v2.0.0 release (`testdata/golden/`). Credit and respect to the maintainers of the upstream projects.

## Changelog

See [CHANGELOG.md](./CHANGELOG.md).

## License

MIT. See [LICENSE](./LICENSE).
