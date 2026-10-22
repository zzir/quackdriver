# Quack v3 golden wire fixtures

Request and response bodies of real Quack round trips, captured by
`cmd/dump-wire`. Each exchange `<name>` is stored as `<name>.req.bin` (the
body the client POSTed to `/quack`) and `<name>.resp.bin` (the response
body).

`internal/message/golden_test.go` decodes every response, re-encodes every
request byte for byte, rebuilds the requests from plain values and checks the
decoded results against the server's own `VARCHAR` rendering of the same
values; the two responses with VARIANT columns (`select_variant`,
`select_variant_columns`) must fail to decode with `message.ErrVariant`, since
the driver does not decode VARIANT, and `cmd/dump-wire` captures them
undecoded. `integration/golden` re-captures the set from a fresh server and
requires the same bytes, masking only the connection id and the server's
version and platform strings (so a development build also passes) and
sorting the `extra_info` entries of error responses (see
[Determinism](#determinism)).

## Provenance

| | |
|---|---|
| Server | the official DuckDB **v2.0.0** release CLI (`v2.0.0 (Cyanoptera) d54cbbe3df`, GitHub release asset `duckdb-cli-linux-amd64.tar.gz`), platform `linux_amd64` |
| Quack extension | duckdb-quack `f96420ed46aee9c42064bdb99ddfd1230d6c0086`, the commit DuckDB v2.0.0 pins, built against the v2.0.0 tag and loaded unsigned (the signed build from extensions.duckdb.org was not reachable) |
| Protocol | `QUACK_VERSION = 3` |
| Server start | `SET force_mbedtls_unsafe = 'true'; INSTALL quack; LOAD quack; CALL quack_serve('quack:127.0.0.1:<port>', token => 'super_secret');` on a fresh in-memory database |
| Captured | 2026-10-22 with `cmd/dump-wire` through `scripts/with-server.sh` (today `QUACK_PORT=<port> make dump-wire`) |

`force_mbedtls_unsafe` replaces the unavailable httpfs for key generation; it
does not affect the wire format. An earlier capture from a development build
(core `8d68e6f`, same quack commit) differed only in the version string of
`handshake.resp.bin` (`v2.0.0-dev1`) and the connection id.

## Determinism

Every client request is fixed: the handshake sends `client_duckdb_version
v2.0.0`, `client_platform linux_amd64`, `client_id quackdriver-golden`, a 60 s
lease and the token `super_secret` (the unit tests expect it); statement *n*
uses the query UUID `51c0ffee-0000-4000-8000-<n as 12 hex digits>`;
`client_query_id` counts up from 1; the SEND_DATA stream id is
`golden-stream-1`. Only the 32-hex-digit `connection_id` the server assigns
changes between captures (in `handshake.resp.bin` and every later request
header); no other response carries a connection id or query id.

Two things depend on where the server runs:

- **Time zone.** DuckDB renders `TIMESTAMPTZ` (the `s` column of
  `select_timestamptz` and `select_timestamptz_ns`) in the session's
  `TimeZone`, which defaults to the server host's zone, and
  `internal/message/golden_test.go` expects UTC (`+00`). The server must
  therefore run in UTC: `make dump-wire` starts it with `TZ=UTC`, and
  `dump-wire` fails on a server that renders `TIMESTAMPTZ` with another
  offset, leaving the fixtures here untouched.
- **Error `extra_info` order.** The server keeps an error's `extra_info` in a
  `std::unordered_map` and writes it in hash order, which differs between C++
  standard libraries: these fixtures come from `linux_amd64` (libstdc++), a
  macOS server (libc++) orders the entries differently. `integration/golden`
  sorts them before comparing.

Any other difference after regenerating is protocol drift (a new server build
or an encoder change) and must be reviewed, not just committed.

## Regenerating

```sh
make dump-wire    # or: TZ=UTC scripts/with-server.sh go run ./cmd/dump-wire
```

`with-server.sh` starts a server (in UTC, given `TZ=UTC`) and stops it
afterwards only when none answers on `QUACK_PORT` (default 9494); a server
already running there is reused, so it has to run in UTC as well. If
`dump-wire` reports that the server renders `TIMESTAMPTZ` outside UTC, stop
that server or choose another `QUACK_PORT`, and run it again.

`dump-wire` captures into a scratch directory and replaces the `*.bin` files
here only when the whole capture succeeded. It creates and drops the table
`golden_send` and sets `quack_target_batch_bytes` for its own session only;
`-v` prints every exchange. When the set of exchanges changes, update
`goldenCatalog` in `internal/message/golden_test.go`.

## Catalog

All exchanges run on one connection, in this order.

| Exchange | What it pins down |
|---|---|
| `handshake_bad_version` | CONNECTION_REQUEST offering only v1 → ERROR_RESPONSE `Unsupported Quack version - server only supports version 3 of quack` (`Invalid Input`); no session is created |
| `handshake` | CONNECTION_REQUEST with every field set (min = max = 3, `client_id`, `heartbeat_timeout_seconds` 60) → CONNECTION_RESPONSE (server version, platform, quack_version 3, lease echoed); the response header carries the connection id |
| `heartbeat` | HEARTBEAT_REQUEST (empty body, unset client_query_id) → SUCCESS_RESPONSE |
| `select_<type>` | one PREPARE per type family: `SELECT v, v::VARCHAR AS s FROM (VALUES ...) ORDER BY k` with edge values and a trailing NULL row. Types: BOOLEAN, TINYINT … UBIGINT, HUGEINT, UHUGEINT, FLOAT and DOUBLE (incl. inf/nan), DECIMAL(4,1)/(9,2)/(18,4)/(38,10) (INT16/32/64/128 storage), VARCHAR (empty, inlined, 12-byte, long, UTF-8, embedded NUL; the 2.0 107/108/109 string layout), BLOB, JSON, GEOMETRY (WKB), UUID (sign-bit edges), DATE (BC, year > 9999, ±infinity), TIME (incl. 24:00:00), TIME_NS, TIMETZ, TIMESTAMP (BC, ±infinity), TIMESTAMP_S/_MS/_NS, TIMESTAMPTZ, TIMESTAMPTZ_NS, INTERVAL, BIT, BIGNUM, ENUM, LIST (INTEGER, VARCHAR, nested), ARRAY, STRUCT, TUPLE (`row(...)`), MAP, UNION, VARIANT (unshredded) |
| `select_constant` | constant expressions incl. a SQLNULL column (the server still sends FLAT vectors) |
| `select_nulls` | all-NULL columns of SQLNULL, INTEGER, VARCHAR and LIST type |
| `select_multirow` | a 5-row, 3-column result |
| `select_variant_columns` | columns cast to VARIANT (shredded inside DuckDB) still travel unshredded; like `select_variant`, rejected by the decoder |
| `select_multistatement` | `SELECT 1; SELECT 2` answers with the first result |
| `error_catalog`, `error_parser`, `error_conversion` | ERROR_RESPONSE with `exception_type` (`Catalog`, `Parser`, `Conversion`) and `extra_info` (`name`, `type`, `error_subtype`, `candidates`) |
| `multibatch_prepare` | PREPARE with `inline_rows` 1 of a 6000-row result in a session with `quack_target_batch_bytes = 1`: one 2048-row batch is inlined (a whole batch, overshooting inline_rows) and `needs_more_fetch` is set |
| `fetch_missing_batch_index` | FETCH without batch_index → `FETCH_REQUEST is missing its batch index` |
| `multibatch_fetch_01`, `_02` | FETCH batch_index i, ack_index i-1 → FETCH_RESPONSE with batch_index i and one raw DataChunk |
| `multibatch_fetch_01_retry` | FETCH of batch 1 again (ack_index 0) → the same bytes as `multibatch_fetch_01` |
| `multibatch_fetch_03` | terminal FETCH_RESPONSE: no chunks, `total_batches` 2 (the FETCH data batches, not counting the inlined one) |
| `senddata_prepare` | PREPARE of `INSERT INTO golden_send SELECT * FROM scan_data_from_quack_client('golden-stream-1', NULL::STRUCT(...), ordered := true)` with `inline_rows` 0 → empty schema, `needs_more_fetch` |
| `senddata_batch_1`, `_2` | SEND_DATA_REQUEST batch_index 1 (one chunk) and 2 (two chunks) of BIGINT/VARCHAR/BOOLEAN rows with NULLs → SEND_DATA_RESPONSE with unset `accept_budget` |
| `senddata_terminal` | SEND_DATA_REQUEST with only `total_batches` 2 |
| `senddata_fetch` | FETCH 1 → one row, BIGINT 12 (the inserted row count) |
| `senddata_fetch_end` | FETCH 2 → terminal response, `total_batches` 1 |
| `senddata_verify` | the inserted rows read back |
| `senddata_unknown_stream` | SEND_DATA for a stream no statement reads → `No active data stream '...'` |
| `cancel_prepare`, `cancel` | PREPARE (`inline_rows` 0) of a long count, then CANCEL_REQUEST with its query UUID → SUCCESS_RESPONSE |
| `cancel_fetch` | FETCH of the cancelled statement → `Interrupted: query was cancelled` (`INTERRUPT`) |
| `cancel_wildcard` | CANCEL_REQUEST with the zero UUID (wildcard) → SUCCESS_RESPONSE |
| `ack_prepare`, `ack` | ACKNOWLEDGEMENT of a finished result → SUCCESS_RESPONSE (a no-op without `quack_enable_reconnects`) |
| `ack_default` | ACKNOWLEDGEMENT with the default zero UUID: the field is omitted, the body is empty |
| `disconnect` | DISCONNECT_MESSAGE → SUCCESS_RESPONSE |
| `after_disconnect` | HEARTBEAT on the closed connection → `Invalid connection id` |
