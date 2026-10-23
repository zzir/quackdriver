# Changelog

All notable changes to this project will be documented in this file.

The format follows [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/); the project follows [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html). While the driver is pre-1.0 (`v0.x`), **minor versions may include breaking changes**, including a move to a newer Quack protocol version when a DuckDB release changes it; patch versions do not. The driver currently speaks Quack protocol v3 (DuckDB 2.0); the API is to stabilize with v1.0.0.

## [Unreleased]

## [0.2.0] — 2026-10-23

Rewrite for Quack protocol **v3** (DuckDB 2.0). This is a breaking upgrade: DuckDB 1.x servers (Quack v1) are no longer supported, and the handshake reports that DuckDB 2.0 or newer is required.

### Changed — breaking

- Go 1.26 or newer is required (was 1.22): the oldest Go release still maintained upstream. The minimum now follows Go's support policy and moves up when a new Go release ships. The driver's own tests therefore run with the runtime defaults users get, such as post-quantum TLS key exchange, instead of Go 1.22 compatibility settings.
- Wire protocol v3: `PREPARE` with inline rows, numbered `FETCH` batches with acknowledgements and read-ahead (`fetch_ahead`, default 1), heartbeats keeping the session lease, `CANCEL` on context cancellation, `DISCONNECT` on close. HTTP/1.1 with the `application/vnd.duckdb` media type.
- `APPEND_REQUEST` is gone; `Conn.Append` now streams rows with `SEND_DATA` into one `INSERT ... SELECT ... FROM scan_data_from_quack_client(...)` statement.
- `Conn.Append` and the `Appender` send every value as text (VARCHAR chunks) that the server casts to the column's `Type`, so DuckDB's own cast rules apply. `AppendColumn.Type` is resolved by the server (one extra statement when an `Appender` starts): any DuckDB type expression works, user-defined types included, and only what DuckDB accepts (the driver's `LIST(T)` spelling is gone; use `T[]`). Ranges and formats are checked by the server: an out-of-range integer, a malformed value, an unknown ENUM label or a duplicate MAP key now fails the statement with a `*Error` instead of a `*ValueError`, and a DECIMAL string with more fractional digits than the scale is rounded instead of rejected. `Conn.Append` starts the INSERT before converting the rows and aborts it at the first row it cannot convert; nothing is inserted either way. UNION columns can no longer be appended (DuckDB's cast from text picks the member itself).
- VARIANT values are no longer decoded: a result with a VARIANT column, also nested, fails with an error suggesting a cast such as `v::JSON`.
- DSN: `timeout` now defaults to none (was 30s); TLS defaults to on for non-local hosts; unknown parameters are rejected. New parameters `disable_ssl`, `ssl_fingerprint`, `connect_timeout`, `heartbeat_timeout`, `client_id`, `inline_rows`, `fetch_ahead`.
- `RowsAffected` reports DuckDB's changed-row counts (DML, `COPY`, `CREATE TABLE ... AS`, the row count of `RETURNING`); `LastInsertId` returns an error.
- `TIME WITH TIME ZONE` decodes to `TimeTzValue` with accessors (built with `NewTimeTz`; the zero value is `00:00:00+00`); `IntervalValue` and `TimeTzValue` are re-exported from the root package. `float64` arguments are sent as DOUBLE literals and `*big.Rat` as DECIMAL literals of at most 38 digits.
- `AppendColumn.Name` now selects the target column: when every column has a name, the rows are inserted with `INSERT INTO t ("name", ...)`, so values map to table columns **by name** (in v0.1 names only appeared in error messages and values mapped by position). Names that are not table columns fail with a Binder error; leave a `Name` empty to keep the positional mapping.
- Appending a float to a `DECIMAL` column rounds like DuckDB's `DOUBLE` → `DECIMAL` cast (multiply by 10^scale, round half away from zero) instead of formatting the binary value half-to-even; appending a `time.Time` to a timestamp column of lower precision rounds to the nearest unit, halves away from the epoch, like DuckDB's timestamp casts (it used to floor). Both now store what `INSERT ... VALUES (?)` stores.
- A nil `[]byte` is NULL, as a query argument and in `Append`/`Appender` (it was an empty BLOB or VARCHAR).
- The client no longer rejects tokens shorter than 4 characters: the server's authentication function decides.
- An empty `ssl_fingerprint=` is rejected like any other malformed value instead of silently disabling the pin.
- A connection that gets no answer from the server for a whole heartbeat lease is marked gone (the server has expired the session by then) instead of retrying heartbeats forever; `database/sql` then replaces it.

### Changed

- `Conn.Append` and the `Appender` encode each `SEND_DATA` request into a buffer sized from their running estimate of the buffered rows and write the string data straight into it, instead of growing the buffer (and copying it) as they go. About half as much memory is allocated per request: a 2,000,000-row load into 11 TIMESTAMP/DECIMAL/BIGINT columns allocated 3.2 GiB instead of 6.6 GiB and ran about 10% faster. The bytes sent are unchanged.

### Added

- DSN parameter `max_buffered_rows` (default 1,000,000): the most rows of an open `Rows` buffered in memory when another statement starts on its connection; beyond it the new statement fails and the `Rows` stays readable.
- `Conn.ServerInfo()` reports the server's DuckDB version, platform, Quack protocol version and granted heartbeat lease.

- Streaming `Appender` (`NewAppender`, `AppendRow`, `Flush`, `Close` returning the inserted count, `Abort`, `WithBatchBytes`, `WithUnordered`), `ErrConnBusy`, and `ValueError` for rows that cannot be converted.
- `*Error` with DuckDB's exception type and extra info.
- `NewConnector` with `WithTLSConfig`, `WithHeader`, `WithMaxIdleConnsPerHost` and `WithHTTPClient`; certificate pinning with `ssl_fingerprint`. HTTP redirects are never followed.
- DSN parameter `timezone` (`SET SESSION TimeZone` after each handshake), for `time.Time` arguments that meet `TIMESTAMPTZ` values.
- A result read to its end through `FETCH`, or of at least 2048 rows returned whole by `PREPARE`, is acknowledged (`ACKNOWLEDGEMENT`), so a server with `quack_enable_reconnects` frees its retained copy.
- New types: `TIMESTAMPTZ_NS`, `TUPLE`, `BIGNUM`, `GEOMETRY` (read, WKB), `JSON` and other aliases; DuckDB's ±infinity dates and timestamps as `InfinityTime` / `NegInfinityTime`, which are also written back as infinity.
- Placeholders `?N`, `$N` and `$name` (`sql.Named`).
- Integration test suite against real DuckDB 2.0 servers (`go test -tags integration ./...`); golden wire fixtures captured from the official DuckDB v2.0.0 release CLI (quack extension `f96420e`).

### Removed

- Local mode (`quack:local[/path]`, which started a DuckDB CLI for the driver) and `CloseLocalServers`: run a server with `quack_serve` and connect to it. A `quack:local` DSN is rejected with an error saying so.
- The examples are consolidated into `basic`, `types`, `append`, `tls-pin`, `bench-append` and `bun` (now on bun v1.3.0).
- Development tooling is trimmed: `make test` is the unit tests, `make test-integration` the whole suite (which builds and runs the examples), `make lint` what CI lints; `make server`, `make bench` and `make dump-wire` stay. Gone are `make test-unit`/`test-go-integration`/`test-examples`/`test-bun`/`server-bg`/`stop`/`clean`/`help`, `scripts/dump-wire.sh` (`cmd/dump-wire` now reads `QUACK_HOST`/`QUACK_PORT`/`QUACK_TOKEN` or `QUACK_DSN` like the examples) and `scripts/run-examples.sh`. `scripts/start-quack-server.sh` drops `QUACK_EXTENSION_REPO` and `QUACK_LOG`; its `QUACK_TEST_INIT_SQL` replaces the default `INSTALL quack; LOAD quack;`, as in the integration tests. `.tool-versions` pins the Go and golangci-lint used by `make lint` and CI's lint job (`make lint` refuses others); Dependabot updates the GitHub Actions and the bun example's dependencies, and a weekly `canary` workflow flags when a new Go or golangci-lint release calls for a bump.

## [0.1.0-alpha.1] — 2026-05-25

First public release. The driver is functional end-to-end against DuckDB v1.5.3 servers running the Quack extension, with the wire format byte-locked by 38 captured request/response fixtures under `testdata/golden/`.

### Added — driver

- `database/sql/driver` implementation under module `github.com/zzir/quackdriver`. `sql.Open("quack", dsn)` returns a working `*sql.DB`.
- HTTP/2 transport over `POST /quack` (`application/duckdb`). Default port `9494`; URI schemes `quack://host:port`, `quack:host:port`, IPv6 `quack:[::1]:port`.
- DSN query parameters: `token` (auth, ≥4 chars), `tls` (enable HTTPS), `timeout` (per-request HTTP timeout, any `time.Duration` literal).
- All ten Quack message types: `CONNECTION_REQUEST/RESPONSE`, `PREPARE_REQUEST/RESPONSE`, `FETCH_REQUEST/RESPONSE`, `APPEND_REQUEST`, `SUCCESS_RESPONSE`, `DISCONNECT_MESSAGE`, `ERROR_RESPONSE`.
- Standard interfaces: `driver.Conn`, `driver.ConnBeginTx`, `driver.ConnPrepareContext`, `driver.ExecerContext`, `driver.QueryerContext`, `driver.Pinger`, `driver.SessionResetter`, `driver.Validator`, `driver.NamedValueChecker`. `Rows` implements `RowsColumnTypeDatabaseTypeName`, `RowsColumnTypeNullable`, `RowsColumnTypeScanType`.
- Transactions via SQL: `BeginTx` sends `BEGIN`; `Tx.Commit`/`Rollback` send `COMMIT`/`ROLLBACK`.
- Multi-chunk result sets — verified with 200,000-row queries that require follow-up `FETCH_REQUEST` round-trips.
- Parameter substitution via `internal/sqlfmt` (Quack has no server-side bind). All string values are single-quote double-escaped; the package is the entire injection-defense surface.

### Added — read path (DuckDB → Go)

- Primitive types: `BOOLEAN`, `TINYINT`/`SMALLINT`/`INTEGER`/`BIGINT`, `UTINYINT`/`USMALLINT`/`UINTEGER`/`UBIGINT`, `FLOAT`, `DOUBLE`, `VARCHAR`, `BLOB`, `UUID` (display form), `HUGEINT` and `UHUGEINT` → `*big.Int`.
- Temporal types: `DATE` and `TIMESTAMP` family (`_S`/`_MS`/`_NS`/`TZ`) decode into `time.Time` (UTC); `TIME` and `TIME_NS` decode into `time.Duration` (since midnight); `INTERVAL` into `message.IntervalValue` with `ApproxDuration()` and `String()` helpers.
- `DECIMAL(w, s)` decodes into a formatted string ("12345.67"), preserving exact precision via underlying `int16`/`int32`/`int64`/`int128` storage.
- Nested types: `LIST<T>` and `ARRAY<T, N>` decode into `[]any`; `STRUCT<...>` into `map[string]any`; `MAP<K, V>` into a list of `{key, value}` entries; `UNION<...>` into a tagged `map[string]any`.
- Vector encodings: FLAT, CONSTANT, DICTIONARY, SEQUENCE. FSST raises a clear error.

### Added — write path: `Conn.Append`

- High-level bulk-insert API: `Conn.Append(ctx, table, columns, rows)`. Measured ~1000× faster than parameterized INSERT for batches.
- SQL type expression parser for the `AppendColumn.Type` field: accepts `BOOLEAN`, `INTEGER`, `BIGINT`, `HUGEINT`, `UHUGEINT`, `FLOAT`, `DOUBLE`, `VARCHAR`, `BLOB`, `UUID`, `DATE`, `TIMESTAMP*`, `INTERVAL`, `DECIMAL(w, s)`, `ENUM('a', 'b', ...)`, `T[]` / `LIST(T)`, `T[N]`, `STRUCT(field TYPE, ...)`, `MAP(K, V)`, `UNION(tag TYPE, ...)`. Tokenizer + recursive descent — full DuckDB grammar including single-quoted string literals for ENUM labels.
- Value converters with range checks, NULL handling, and per-physical-type packing for INT128 / UINT128 / INTERVAL. `UNION` values pass as `map[string]any` with exactly one non-nil variant; the driver picks the tag. `ENUM` values accept either the string label or a numeric dictionary index; unknown labels are rejected with row/column context.

### Added — ORM integration

- `uptrace/bun` via `sqlitedialect`. Example under `examples/bun/` with its own `go.mod` so the core driver stays third-party-dependency-free.

### Added — tooling

- `Makefile` with `make test` (build + vet + go test + every example, auto-managed server lifecycle), `make server`/`server-bg`/`stop`, `make dump-wire`, `make bench`, plus the standard `build`/`vet`/`fmt`/`clean`. Auto-server logic reuses any existing process on `QUACK_PORT` and never kills servers it didn't start.
- `scripts/with-server.sh` — generic "run command with a Quack server" wrapper used by the Makefile and any one-off tooling.
- `cmd/dump-wire` — Go program that drives a known set of round-trips and captures both bodies into `testdata/golden/*.bin`. Run via `make dump-wire`.
- `cmd/probe-fetch` — diagnostic that reports how many `PREPARE_REQUEST` + `FETCH_REQUEST` round-trips a given SELECT issues. Used to confirm the multi-chunk integration test actually exercises the fetch loop.
- `testdata/golden/` — 38 wire-format fixture files (handshake, all primitive types, nested types, multi-chunk + fetch, error path, disconnect). 14 byte-equality / structural tests in `internal/message/golden_test.go` run without a live server.

### Hardening

Internal review surfaced nine correctness issues before tagging; all fixed in this release:

- **HUGEINT**: `Conn.Append` no longer rejects the valid INT128 minimum (`-2^127`). The prior `bi.BitLen() > 127` check fired for `-2^127` because `BitLen` operates on the absolute value. Replaced with explicit `Cmp` against pre-computed signed/unsigned 128-bit bounds.
- **DECIMAL**: `Conn.Append` rejects unscaled values that don't fit the declared precision instead of silently wrapping (`int16(32768) == -32768` is well-defined modular arithmetic but data corruption from the caller's perspective). Affected `DECIMAL` columns with width 4 / 9 / 18.
- **`UTINYINT`/`USMALLINT`/`UINTEGER`/`UBIGINT`** columns accept `int8` and `int16` Go values; the `toUnsigned` helper previously had only `int` / `int32` / `int64` cases.
- **`Conn.closed`** is now `atomic.Bool`. The earlier pattern — `sync.Mutex` on writes but unlocked reads from `IsValid`/`ResetSession` — was a data race per Go's memory model and could trip the race detector under concurrent pool reuse. `Close` is now idempotent via `CompareAndSwap`.
- **Multi-chunk fetch termination** no longer depends on `FetchResponse.batchIndex`. Per the upstream TypeScript SDK, the server signals exhaustion with an empty `results` list; `batchIndex` is purely informational and may be absent on legitimate non-final batches. The previous logic could exit early and lose data. Regression covered by `examples/multichunk`, which APPENDs 200,000 rows and exercises ~10 `FETCH_REQUEST` round-trips.
- **`Conn.Exec`** drains result chunks server-side via follow-up `FETCH_REQUEST`s when `NeedsMoreFetch=true`. Previously the result UUID was leaked until `DISCONNECT`.
- **ENUM** encoding accepts the string label or the numeric dictionary index, mirroring what the decoder produces. Previously a string label round-tripped through encode would `panic`.
- **`Conn.Close`** returns non-`context.Canceled` errors from the disconnect round-trip instead of swallowing them. `database/sql` ignores the return, so caller behaviour is unchanged, but callers using `Conn` directly now see real errors.
- **`IntervalValue.String`** handles extreme micros values (notably `math.MinInt64`) without overflow during negation or the `time.Duration` cast. Arithmetic moved to raw `int64` with explicit saturation.
- **`convertValue` / `toSigned`**: `uint` values above `math.MaxInt64` no longer silently wrap to a negative `int64` when written as SQL literals. `convertValue` passes them through as `uint64` (so `sqlfmt` renders the actual magnitude); `toSigned` returns an explicit overflow error. The previous `if x > (1 << 63)` boundary in `toSigned` was also off by one and incorrectly accepted exactly `MaxInt64+1`.
- **DECIMAL**: `parseDecimalString` rejects extra fractional digits instead of silently truncating (`"1.239"` at `scale=2` is now an error, not a quiet `1.23`). `convertAppendValue` additionally enforces the declared precision (`|unscaled| < 10^width`) — earlier code only checked the underlying physical-integer width, letting `DECIMAL(3,2)` swallow a value like `12.34` that fits `INT16` but exceeds the column's 3-digit precision.
- **`Rows`**: multi-chunk `FETCH_REQUEST`s now use the `context.Context` that was passed to `QueryContext` instead of `context.Background()`. Caller-side timeouts and cancellations propagate correctly across the whole result iteration.
- **`BeginTx`**: non-default `Isolation` levels and `ReadOnly` are rejected with a clear error rather than silently dropped, matching the `database/sql` driver contract.
- **Decoder panic-safety**: the `VARCHAR` row-count mismatch, `LIST` entry `offset+length` overflow, `ARRAY` short child vector, and `STRUCT` short child vector cases now return errors instead of panicking on slice out-of-range. A malicious or buggy server response can no longer crash the client process.
- **Tokenizer**: `parseAppendType("'")` (a lone unterminated quote with no preceding tokens) now returns an "unterminated string literal" error rather than panicking on `out[len(out)-1]` against an empty token slice.

Each fix has a unit-level regression test; the data race is exercised by `go test -race`. Bounds-failure paths are covered by `internal/message/decode_safety_test.go`, which hand-builds malformed wire bytes.

### Added — documentation

- `README.md` with a quick-start, featured APPEND example, complete read/write type-mapping tables, limitations + gotchas, development workflow, project layout, and a References section crediting the upstream Quack implementations.
- `doc/PROTOCOL.md` — distilled wire-format cheat sheet for contributors.
- `CLAUDE.md` — guidance for AI-assisted development sessions.

### Known limitations

- **No server-side prepared statements.** Quack v1.5.3 has no parameter-bind path; the driver substitutes SQL literals client-side. Any new types accepted by `internal/sqlfmt.FormatValue` must double-escape strings.
- **`localhost` resolves to IPv6** but `quack_serve(...)` binds IPv4 only. Use `127.0.0.1` in DSNs.
- **`TIME WITH TIME ZONE`** decodes to raw 64-bit bits — the offset packing in v1.5.3 isn't documented well enough to expose typed accessors. Cast to `VARCHAR` server-side for parsed display.
- **FSST-compressed vectors** raise an error rather than decode. Disable column compression or cast away from `VARCHAR` if you encounter them.
- **`LastInsertId` returns `ErrNoLastInsertID`**; `RowsAffected` returns `(0, nil)`. Quack does not transmit either.
- **Typed `Scan` for nested types** is not yet implemented — `LIST`/`STRUCT`/`MAP`/`ARRAY` come back as `[]any` / `map[string]any`.
- **`GEOMETRY`, `ENUM`, `BIT`** are decoded as `[]byte` / integers and are not exercised by the test suite. Read at your own risk.

[Unreleased]: https://github.com/zzir/quackdriver/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/zzir/quackdriver/compare/v0.1.0-alpha.1...v0.2.0
[0.1.0-alpha.1]: https://github.com/zzir/quackdriver/releases/tag/v0.1.0-alpha.1
