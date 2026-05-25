# quackdriver

Pure-Go [`database/sql`](https://pkg.go.dev/database/sql) driver for [DuckDB](https://duckdb.org/)'s **Quack** remote protocol.

> **Status: alpha (v0.1.x).** The Quack protocol itself is pre-1.0 (target DuckDB v2.0, 2026-09). This driver is pinned to DuckDB v1.5.3 servers and breaking wire-format changes are expected through v2.0. Production users should treat the wire layer as load-bearing and lock to specific DuckDB releases.

## Contents

- [Highlights](#highlights)
- [Install](#install)
- [Quick start](#quick-start)
- [Bulk insert with `Conn.Append`](#bulk-insert-with-connappend)
- [bun ORM](#bun-orm)
- [DSN](#dsn)
- [Type mapping](#type-mapping)
- [Limitations & gotchas](#limitations--gotchas)
- [Development](#development)
- [References](#references)
- [License](#license)

## Highlights

- **No CGO.** No `libduckdb` embedded. The driver speaks HTTP/2 to a remote DuckDB server, so it cross-compiles like any other pure-Go binary.
- **Zero third-party dependencies** in the core driver. Only the Go standard library — `net/http`, `encoding/binary`, `database/sql/driver`.
- **`database/sql/driver` compatible.** Works with `sql.Open("quack", dsn)`, `*sql.DB` connection pooling, prepared statements (client-side), and transactions.
- **Bulk-insert path** — `Conn.Append` ships rows as a single column-major DataChunk in one round-trip. Measured **~1000× faster** than parameterized INSERT for batches.
- **bun ORM** support via `sqlitedialect`.
- **Golden-fixture-locked wire format.** 38 captured request/response pairs in `testdata/golden/` guard against silent protocol drift. Unit tests run without a server.

## Install

```bash
go get github.com/zzir/quackdriver
```

Requirements:

- Go 1.22+
- DuckDB CLI v1.5.3+ with the Quack extension (`INSTALL quack; LOAD quack;`) for the server side

## Quick start

Start a server (in one terminal):

```bash
bash scripts/start-quack-server.sh
# or:
duckdb -cmd "INSTALL quack; LOAD quack; CALL quack_serve('quack:127.0.0.1:9494', token=>'super_secret');"
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
}
```

> **Use `127.0.0.1`, not `localhost`.** Go's resolver prefers IPv6 (`::1`), but `quack_serve` binds IPv4 only — you will get `connection refused` if you write `quack://localhost:9494` in the DSN.

More example programs in [`examples/`](./examples) (each is `go run ./examples/<name>`):

| Example | What it shows |
|---|---|
| `basic` | Connection, DDL/DML, Tx, multi-chunk fetch |
| `types` | All 13 primitive types round-tripped |
| `nested` | LIST / STRUCT / ARRAY / MAP / DECIMAL / INTERVAL decoding |
| `timeutc` | TIME → `time.Duration`, INTERVAL helpers |
| `append` | High-level `Conn.Append` API |
| `append-rich` | APPEND with DECIMAL / HUGEINT / UUID / INTERVAL / LIST / ARRAY |
| `append-struct` | APPEND with STRUCT / MAP / 2D ARRAY |
| `bench-append` | INSERT vs APPEND throughput benchmark |
| `bun` | bun ORM integration (separate `go.mod`) |

## Bulk insert with `Conn.Append`

Quack supports a wire-level `APPEND_REQUEST` path that bypasses SQL parsing and binding entirely — rows go straight from the client into the server's table buffer as a column-major DataChunk. For batches this is **vastly** faster than `INSERT VALUES`:

```
INSERT 10000 rows: 1.86s   (5,384 rows/sec)
APPEND 10000 rows: 1.78ms  (5,612,198 rows/sec)
speedup:           1042×
```

The high-level API:

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
            {Name: "result",  Type: "UNION(num INTEGER, err VARCHAR)"},
        },
        [][]any{
            {int64(1), map[string]any{"name": "alice", "score": "98.5"}, []any{"go", "duckdb"}, map[string]any{"num": 42}},
            {int64(2), map[string]any{"name": "bob",   "score": "73.2"}, []any{"sql"},          map[string]any{"err": "timeout"}},
        },
    )
})
```

The `Type` field accepts the full DuckDB type grammar:

| Form | Notes |
|---|---|
| `BOOLEAN`, `INTEGER`, `BIGINT`, `VARCHAR`, ... | Simple names (case-insensitive). |
| `DECIMAL(18, 4)` | Width 1–38, scale 0..width. |
| `HUGEINT`, `UHUGEINT`, `UUID`, `INTERVAL` | 128-bit / UUID / temporal interval. |
| `DATE`, `TIMESTAMP`, `TIMESTAMP_MS`, `TIMESTAMP_NS` | Accept `time.Time`. |
| `INTEGER[]`, `LIST(INTEGER)` | LIST; accepts `[]any`. |
| `INTEGER[3]` | Fixed-size ARRAY; accepts `[]any` of exact length. |
| `STRUCT(a INTEGER, b VARCHAR)` | Accepts `map[string]any`. |
| `MAP(VARCHAR, INTEGER)` | Accepts `map[string]any` or `[]any` of `{key, value}` entries. |
| `UNION(num INTEGER, str VARCHAR)` | Accepts `map[string]any` with **exactly one** non-nil variant key. |

Pass `nil` for SQL NULL anywhere.

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

> **bun gotcha — `INTEGER` vs `BIGINT`.** bun's sqlitedialect maps `int64` Go fields to SQL `INTEGER`, which is 64-bit in SQLite but **32-bit in DuckDB**. Scans of `int32` values back into `int64` then fail. Annotate `int64` columns explicitly with `type:BIGINT`.

Full working example in [`examples/bun/`](./examples/bun).

## DSN

```
quack://host:port?token=<token>&tls=<bool>&timeout=<duration>
quack:host:port?token=<token>
quack:[::1]:9494?token=<token>     # IPv6
```

| Param      | Default | Notes                                                |
|------------|---------|------------------------------------------------------|
| `token`    | (none)  | Auth token, must be at least 4 characters.           |
| `tls`      | `false` | If `true`, the driver uses `https://`.               |
| `timeout`  | `30s`   | Per-request HTTP timeout. Accepts any `time.Duration` string. |

## Type mapping

### Read direction (DuckDB → Go)

| DuckDB type | Go type returned to `Rows.Scan` |
|---|---|
| `BOOLEAN` | `bool` |
| `TINYINT` / `SMALLINT` / `INTEGER` / `BIGINT` | `int8` / `int16` / `int32` / `int64` |
| `UTINYINT` / `USMALLINT` / `UINTEGER` / `UBIGINT` | `uint8` / `uint16` / `uint32` / `uint64` |
| `HUGEINT`, `UHUGEINT` | `*big.Int` |
| `FLOAT`, `DOUBLE` | `float32`, `float64` |
| `VARCHAR`, `CHAR`, `UUID` | `string` |
| `BLOB`, `BIT`, `GEOMETRY` | `[]byte` |
| `DATE` | `time.Time` (UTC midnight) |
| `TIMESTAMP`, `TIMESTAMP_S/MS/NS`, `TIMESTAMP WITH TIME ZONE` | `time.Time` (UTC) |
| `TIME`, `TIME_NS` | `time.Duration` (since midnight) |
| `TIME WITH TIME ZONE` | `message.TimeTzValue` (raw 64-bit packed bits) |
| `INTERVAL` | `message.IntervalValue` (months / days / micros) |
| `DECIMAL(w, s)` | `string` (e.g. `"12345.67"`) |
| `LIST<T>`, `ARRAY<T, N>` | `[]any` |
| `STRUCT<...>` | `map[string]any` |
| `MAP<K, V>` | `[]any` of `map[string]any{"key": ..., "value": ...}` |
| `UNION<...>` | `map[string]any` including a `""` tag field (uint8) |
| `NULL` (any column) | `nil` |

### Write direction (Go → DuckDB via `Conn.Append`)

| DuckDB type tag | Accepted Go values |
|---|---|
| `BOOLEAN` | `bool` |
| `TINYINT` … `BIGINT` | Any signed integer; widened, range-checked. |
| `UTINYINT` … `UBIGINT` | Any unsigned integer; widened, range-checked. |
| `HUGEINT`, `UHUGEINT` | `*big.Int`, `string` (base-10), or any integer. |
| `FLOAT`, `DOUBLE` | `float32`, `float64`, `int` |
| `VARCHAR` | `string`, `[]byte` |
| `BLOB` | `[]byte`, `string` |
| `UUID` | `string` (8-4-4-4-12 hex) |
| `DATE` | `time.Time` |
| `TIMESTAMP*` | `time.Time` |
| `INTERVAL` | `time.Duration` or `message.IntervalValue` |
| `DECIMAL(w, s)` | `string`, `*big.Int`, numeric. Scale is auto-applied. |
| `LIST(T)`, `T[]` | `[]any` |
| `ARRAY<T, N>`, `T[N]` | `[]any` of exact length |
| `STRUCT(name TYPE, ...)` | `map[string]any` keyed by field name |
| `MAP(K, V)` | `map[string]any` (string keys) or `[]any` of `{"key", "value"}` |
| `UNION(name TYPE, ...)` | `map[string]any` with **exactly one** non-nil variant |
| Any column | `nil` for SQL NULL |

## Limitations & gotchas

- **No server-side prepared statements.** Quack v1.5.3 doesn't expose a bind path. The driver substitutes parameters as SQL literals in `internal/sqlfmt` before sending. **Treat that package as the entire injection-defense surface** — any new types accepted by `FormatValue` must double-escape strings.
- **`localhost` resolves to IPv6 first**; the server binds IPv4 only. Use `127.0.0.1` in DSNs and tests.
- **bun + `int64` columns** must be tagged `type:BIGINT` (see [bun ORM](#bun-orm)).
- **FSST-compressed vectors** are rejected with a clear error. They appear when DuckDB enables them for `VARCHAR` columns; if you hit one, `CALL enable_logging('Quack')` server-side or cast the column to a different type.
- **`TIME WITH TIME ZONE`** decodes to raw 64-bit bits — the offset packing in v1.5.3 isn't documented well enough to expose accessors safely. Cast to VARCHAR server-side for parsed display today.
- **`LastInsertId` returns `ErrNoLastInsertID`**; `RowsAffected` returns `(0, nil)`. Quack does not transmit either today.
- **Typed `Scan` for nested types** is not implemented yet — LIST/STRUCT/MAP/ARRAY come back as `[]any` / `map[string]any`. Use the [`Conn.Append`](#bulk-insert-with-connappend) high-level API to round-trip them.

## Development

The whole development loop runs locally — there is no CI yet.

```bash
make test           # build + vet + go test + every example, auto-starts server
make test-unit      # just `go test ./...` (no server needed)
make test-examples  # just the example programs (auto-starts server)
make test-bun       # bun ORM example (separate go.mod)

make server         # start a Quack server in the foreground
make server-bg      # start in the background; reuse if already up
make stop           # kill whatever is listening on QUACK_PORT
make bench          # APPEND vs INSERT throughput benchmark
make dump-wire      # regenerate testdata/golden/*.bin
make help           # all targets + variable values
```

The auto-managed server is conservative: if a server is already up on `QUACK_PORT` (default `9494`), `make test` reuses it and **never kills it**. Only servers `make` itself started are torn down.

### Project layout

```
quackdriver/
├── quackdriver.go, conn.go, stmt.go, rows.go, tx.go, value.go   ← driver.* surface
├── append.go, appendtype.go                                     ← high-level APPEND API
├── internal/
│   ├── codec/       — BinarySerializer (ULEB/SLEB, object framing, hugeint)
│   ├── logicaltype/ — DuckDB type system (LogicalType + ExtraInfo)
│   ├── message/     — 10 Quack messages + DataChunk encode/decode
│   ├── transport/   — URI parser + HTTP/2 client
│   └── sqlfmt/      — SQL literal escaper (security-critical)
├── cmd/dump-wire/   — captures wire fixtures from a live server
├── testdata/golden/ — request/response byte fixtures (committed)
├── examples/        — runnable end-to-end programs
└── doc/PROTOCOL.md  — wire-format cheat sheet
```

The layering is strict and bottom-up: `codec` → `logicaltype` → `message` → `transport` → driver surface. New protocol knowledge belongs at the lowest layer that can host it.

### Adding wire fixtures

`testdata/golden/*.bin` are **immutable** request/response byte captures from a known-good server, used by `go test ./internal/message/` to detect protocol drift without a live server. Regenerate when intentionally bumping the server version:

```bash
make dump-wire
git diff testdata/golden     # any byte change is a protocol drift — investigate first
```

### Reporting bugs

Please include:

1. The DuckDB server version (`SELECT version();` or the `quack_serve(...)` startup banner).
2. The exact DSN and the SQL statement (or `Conn.Append` arguments) that triggered the issue.
3. A `make dump-wire` capture (`testdata/golden/`) if the regression is wire-related.

## References

The Quack protocol is alpha and the upstream docs are thin. These are the load-bearing references:

| Source | Use for |
|---|---|
| [`duckdb/duckdb-quack`](https://github.com/duckdb/duckdb-quack) — `src/include/quack_message.json` | Authoritative message schema (field IDs, types). |
| [`tobilg/quack-protocol`](https://github.com/tobilg/quack-protocol) (TypeScript) | Cleanest algorithm reference — codec, logical types, vector encoding. |
| [`gizmodata/quack-jdbc`](https://github.com/gizmodata/quack-jdbc) (Java) | Structural reference — the Java package layout mirrors this driver's `internal/` tree. |
| [`tobilg`'s wire-format gist](https://gist.github.com/tobilg/9ced9d08f6141b723f26cf205d5b9ece) | Narrative protocol overview. |
| [`Maxxen`'s BinarySerializer gist](https://gist.github.com/Maxxen/364d32444d3f1577e30705e72ee3b80e) | DuckDB `BinarySerializer` byte layout. |
| [DuckDB docs — Quack overview](https://duckdb.org/docs/current/quack/overview) | Official user-facing protocol docs. |
| Local: [`doc/PROTOCOL.md`](./doc/PROTOCOL.md) | Distilled wire-format cheat sheet (what this driver implements). |

This driver translates the TypeScript implementation's algorithms into Go, cross-checked against the Java structural conventions and verified byte-for-byte against the official server output (see `testdata/golden/`). It would not exist without those three upstream projects — credit and respect to their maintainers.

## Changelog

See [CHANGELOG.md](./CHANGELOG.md). The current release is `v0.1.0-alpha.1`.

## License

MIT. See [LICENSE](./LICENSE).
