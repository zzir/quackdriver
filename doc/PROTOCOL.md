# Quack v3 wire-format notes

What this driver implements: Quack protocol version 3, as spoken by DuckDB
2.0 (duckdb-quack `f96420e`, DuckDB core serialization
`StorageVersion::V2_0_0`). Quack v1 (DuckDB 1.5) is not supported.

There is **no official Quack wire-protocol specification**. DuckDB's
documentation describes the protocol only at a high level: HTTP `POST
/quack`, bodies in DuckDB's own binary serialization (the docs call it
`application/duckdb`; the server and the official client label it
`application/vnd.duckdb`), client-driven request/response, results in FETCH
batches. The official FAQ says the protocol is not yet stable. Everything
below is derived from the de-facto schema, in priority order:

1. duckdb-quack `src/include/quack_message.json` (the message schema) and
   the code generated from it, `src/serialize_quack_message.cpp`; server
   behaviour from `src/quack_server.cpp`.
2. DuckDB core v2.0.0 `BinarySerializer` (`src/common/serializer/`) and
   Vector/DataChunk serialization (`src/common/types/vector.cpp`,
   `data_chunk.cpp`).
3. The fixtures in `testdata/golden/`, captured from the official v2.0.0
   release CLI, which the unit tests decode and re-encode byte for byte.

Notation: **R** is a property DuckDB always writes (`WriteProperty`); **D** is
written only when it differs from its default (`WritePropertyWithDefault`),
and is omitted otherwise. The driver's encoder follows the same rule.

## Transport

- `POST /quack`, HTTP/1.1 (the server is cpp-httplib: no HTTP/2, no
  compression). Responses are `application/vnd.duckdb`. `GET /` answers 200
  with a text banner (used as a readiness probe).
- Protocol errors arrive as **HTTP 200 + ERROR_RESPONSE**. Other statuses
  come from the HTTP layer (413 above the 100 MiB request body limit, a
  proxy, ...).
- Default port 9494. URIs: `quack:host[:port]`, `quack://host[:port]`,
  `quack:[::1]:9494`.
- Keep-alive 300 s (`quack_server_keep_alive_timeout`), one server thread per
  TCP connection, at most `quack_server_max_connections` (1024).
- Authentication is in-band (`auth_string` of CONNECTION_REQUEST); afterwards
  the `connection_id` is the credential.
- TLS: local hosts are `localhost`, `127.0.0.1` and `::1`. `quack_serve` on
  any other address needs `allow_other_hostname => true` and serves HTTPS by
  default (`disable_ssl => true` turns it off), with `ssl_cert_file` /
  `ssl_key_file` or a self-signed RSA-4096 certificate generated on first
  use. Clients pin it by the SHA-256 of the DER certificate
  (`ssl_fingerprint`), which replaces CA and hostname verification. Clients
  default to plain HTTP for local hosts and HTTPS otherwise.

## BinarySerializer primitives

| Primitive | Encoding |
|---|---|
| field id | little-endian `uint16` |
| object | fields, then the terminator `0xFFFF`; D fields at their default are omitted |
| unsigned / signed integers | ULEB128 / SLEB128 |
| hugeint | `[SLEB upper][ULEB lower]` |
| optional_idx | ULEB; the unset value is 2^64-1 (`ff ff ff ff ff ff ff ff ff 01`) |
| bool | one byte, 0 or 1 |
| nullable<T> | `[bool present][T]` |
| string / blob | `[ULEB length][bytes]` |
| float / double | IEEE-754 little-endian |
| list<T> | `[ULEB count][T]...` |
| unordered_map<string,string> | `[ULEB count]`, then per entry an object `{0: key, 1: value}` |

A message is a `MessageHeader` object followed by the body object.
FETCH_RESPONSE and SEND_DATA_REQUEST are followed by `chunk_count` bare
DataChunk objects (no wrapper, no nullable byte).

## Message types

| ID | Name | Direction |
|---|---|---|
| 1 | CONNECTION_REQUEST | C → S |
| 2 | CONNECTION_RESPONSE | S → C |
| 3 | PREPARE_REQUEST | C → S |
| 4 | PREPARE_RESPONSE | S → C |
| 7 | FETCH_REQUEST | C → S |
| 8 | FETCH_RESPONSE | S → C |
| 9 | SEND_DATA_REQUEST | C → S |
| 10 | SUCCESS_RESPONSE | S → C |
| 11 | DISCONNECT_MESSAGE | C → S |
| 12 | CANCEL_REQUEST | C → S |
| 14 | SEND_DATA_RESPONSE | S → C |
| 15 | ACKNOWLEDGEMENT | C → S |
| 16 | HEARTBEAT_REQUEST | C → S |
| 100 | ERROR_RESPONSE | S → C |

Id 9 was APPEND_REQUEST in v1; the layout is unrelated.

## Fields

| Message | Fields |
|---|---|
| MessageHeader | 1 type (R) · 2 connection_id (D) · 3 client_query_id (R, optional_idx) |
| ConnectionRequest | 1 auth_string · 2 client_duckdb_version · 3 client_platform · 4 min_supported_quack_version · 5 max_supported_quack_version · 6 client_id · 7 heartbeat_timeout_seconds (all D) |
| ConnectionResponse | 1 server_duckdb_version · 2 server_platform · 3 quack_version · 4 heartbeat_timeout_seconds (all D) |
| PrepareRequest | 1 sql_query (D) · 2 query_uuid (R, hugeint) · 3 inline_rows (D, optional_idx) |
| PrepareResponse | 1 result_types (D) · 2 result_names (D) · 3 needs_more_fetch (D) · 4 results (D, list of nullable `{300: DataChunk}`) · 5 query_uuid (R) |
| FetchRequest | 1 query_uuid (R) · 2 batch_index (D) · 3 ack_index (D) |
| FetchResponse | 1 chunk_count (D) · 2 total_batches (D, optional_idx) · 3 batch_index (D, optional_idx), then the chunks |
| SendDataRequest | 1 stream_id (D) · 2 chunk_count (D) · 3 total_batches (D, terminal message only) · 4 batch_index (D, from 1), then the chunks |
| SendDataResponse | 1 accept_budget (R, optional_idx; currently always unset) |
| CancelRequest | 1 query_uuid (R); zero cancels whatever runs on the connection |
| Acknowledgement | 1 query_uuid (D) |
| Heartbeat, Disconnect, Success | empty body |
| ErrorResponse | 1 message · 2 exception_type (e.g. `Catalog`, `Invalid Input`) · 3 extra_info (map) · 4 must_invalidate (bool) (all D) |

Only the handshake response carries a connection id; other responses carry
neither a connection id nor a query id.

## Flows

### Handshake and lease

- The client sends min = max = 3, `heartbeat_timeout_seconds` (default 60)
  and an optional `client_id` (empty or at least 4 characters). The server
  answers with `quack_version` 3 and echoes the lease; it rejects other
  versions ("Unsupported Quack version - server only supports version 3 of
  quack"), out-of-range leases and bad tokens.
- The server renews the lease only when a message arrives. After it expires
  the next message gets "Connection heartbeat lease expired", later ones
  "Invalid connection id". A long PREPARE or a slow consumer therefore needs
  concurrent HEARTBEAT_REQUESTs; the driver sends them about every lease/3,
  busy or not.

### Queries

- PREPARE: `{sql, query_uuid = random non-zero, inline_rows}`. The server
  returns the schema and, without `inline_rows`, up to 24576 rows inline
  (`quack_prepare_inline_rows`); it inlines whole batches, so more rows may
  arrive. A new PREPARE aborts the session's previous statement ("superseded
  by a new query").
- Several statements in one query: the result of the first statement that
  returns a query result, else of the last one.
- With `needs_more_fetch`, FETCH batches 1, 2, 3, ... (counted after the
  batches PREPARE inlined), each acknowledging the highest index up to which
  every batch has arrived. The acknowledgement must stay below the index
  requested: the server drops acknowledged batches before serving, and an
  unacknowledged batch is served again on a retry. The response's
  batch_index must equal the request's. A response without chunks ends the
  stream; its `total_batches` (counting FETCH batches only) must equal the
  number received. Several FETCHes may be in flight (read-ahead); a request
  for a batch not produced yet waits for it, and requests past the end get
  the terminal response as soon as the stream has ended.
- Errors: "FETCH_REQUEST is missing its batch index" for index 0, "Result has
  been closed" for another query's uuid.
- CANCEL `{query_uuid}` aborts the statement; later FETCHes report
  "Interrupted: query was cancelled". The server checks the uuid ("Attempted
  to cancel a different query ...") and then interrupts whatever runs on the
  session, and it handles every HTTP request on its own thread, even after
  the client gave up on it. A cancelled statement's late PREPARE or CANCEL
  can therefore hit the session's next statement, so the driver lets them
  finish before it sends the next PREPARE (it never abandons a CANCEL
  client-side; the session is given up when one takes over 10 s).
- The same holds for CONNECTION_REQUEST: the server creates the session even
  when the client gave up, and reclaims a session only when a message for it
  arrives (there is no background reaper). The driver therefore lets an
  abandoned handshake finish and disconnects the session it created, and
  sends DISCONNECT on close unless the server reported the session gone.
- ACKNOWLEDGEMENT only releases cached results (`quack_enable_reconnects`):
  with it, the server retains every result until the session's next PREPARE,
  an ACKNOWLEDGEMENT for it, or `quack_result_ttl`. The driver acknowledges a
  result after reading it to the end through FETCH, and a result of at least
  2048 rows that PREPARE returned whole (smaller ones are not worth a request
  per statement), best effort and without waiting; an ACKNOWLEDGEMENT for a
  query the server no longer retains is refused, harmlessly.
- The session-gone answers ("Invalid connection id", "Connection heartbeat
  lease expired", "Database was closed", "The server database has been
  invalidated ...") carry the same exception type as errors raised by SQL, so
  the driver matches their exact text (the last one with `must_invalidate`).
- DML results are one BIGINT row named `Count` (COPY ... RETURN_FILES adds
  `Files`); the driver reports it as `RowsAffected`.

### Client data (SEND_DATA)

1. The client picks a `stream_id` (a UUID string, unique in the session).
2. PREPARE with `inline_rows = 0`:

   ```sql
   INSERT INTO <table> [(cols)] SELECT * FROM scan_data_from_quack_client(
       '<stream_id>', NULL::STRUCT(c0 T0, c1 T1, ...), ordered := true)
   ```

   The response comes as soon as the scan is bound (empty schema,
   `needs_more_fetch`). The driver declares every prototype member VARCHAR
   and sends each value as text that the SELECT casts (`SELECT "c0"::T0,
   ...`), after resolving T0, ... on the server with
   `SELECT NULL::T0, ... LIMIT 0`.
3. SEND_DATA `{stream_id, batch_index = 1..N, chunk_count = k}` + k chunks.
   Each chunk holds at most 2048 rows, and its types must equal the
   prototype exactly: a mismatch raises an INTERNAL error that can invalidate
   the server's database. Requests stay below 100 MiB (the driver targets 32
   MiB). The server delays its answer while its buffer is full
   (`quack_server_stream_buffer_bytes`), which is the back-pressure.
4. The terminal SEND_DATA `{stream_id, total_batches = N}` has no batch index
   and no chunks.
5. FETCH `{query_uuid, 1, 0}` returns the inserted row count (`Count`).
   Constraint violations surface in SEND_DATA or this FETCH; an unknown
   stream is "No active data stream '<id>'".

A column list in the INSERT lets omitted columns take their defaults, and the
INSERT casts the prototype's types to the table's.

## DataChunk and vectors

```
DataChunk:            100 row_count · 101 types (list<LogicalType>) · 102 columns (list<Vector>)

Vector (object):
  90 vector_type (default FLAT): 0 FLAT, 2 CONSTANT, 3 DICTIONARY, 4 SEQUENCE, 5 SHREDDED
  FLAT:
    99  (GEOMETRY only) storage type, 1 = WKB
    100 has_validity_mask (may be true without NULLs)
    101 validity (if 100): ⌈count/64⌉ × 8 bytes, bit set = valid, LSB first
    fixed size:   102 data (count × width bytes, little-endian)
    GEOMETRY:     102 list<WKB blob>
    VARCHAR-like: 107 byte_data_length (optional_idx) · 108 length_data (count × u32)
                  · 109 byte_data (the non-NULL values concatenated)
                  (VARCHAR, CHAR, BLOB, BIT, BIGNUM, JSON, ...; v1's 102 list<blob>
                  is still accepted on decode)
    STRUCT, UNION, TUPLE: 103 list<Vector> (one child per field, count rows each)
    LIST, MAP:    104 list_size · 105 list<{100 offset, 101 length}> · 106 child Vector
    ARRAY:        103 array_size · 104 child Vector (array_size × count rows)
  CONSTANT:   a vector body of one row, in the same object
  DICTIONARY: 91 selection (count × u32) · 92 dictionary_count, then the dictionary body
  SEQUENCE:   91 start · 92 increment (SLEB)
  SHREDDED:   VARIANT only, which the driver does not decode
```

FSST vectors never appear on the wire. NULL rows of fixed-size vectors hold
`NullValue<T>()` in DuckDB's own output (the type's minimum for signed
integers, NaN for floats, 0x80 for BOOLEAN).

## Logical types

```
LogicalType:      100 id · 101 type_info (nullable LogicalTypeInfo)
LogicalTypeInfo:  100 kind · 101 alias (D) · 103 extension info (D) · kind fields from 200
```

Kinds: `INVALID=0, GENERIC=1, DECIMAL=2, STRING=3, LIST=4, STRUCT=5, ENUM=6,
UNBOUND=7, AGGREGATE_STATE=8, ARRAY=9, ANY=10, INTEGER_LITERAL=11,
TEMPLATE=12, GEO=13`. DuckDB 2.0 checks that the kind fits the type id.

New in 2.0: `TIMESTAMP_TZ_NS = 33` (INT64 nanoseconds, UTC) and `TUPLE = 110`
(a STRUCT whose children are all unnamed; `row(...)` produces it). Id 105 is
now `LEGACY_AGGREGATE_STATE`; `EXPORT_STATE` results are ordinary types
aliased `AGGREGATE_STATE` with extension info, which the driver keeps
verbatim.

Value details:

- TIME WITH TIME ZONE: `micros = bits >> 24`, `offset = 57599 - (bits &
  0xFFFFFF)` seconds east of UTC.
- DATE `±(2^31-1)` days and every TIMESTAMP type's `±(2^63-1)` are
  `infinity` / `-infinity`.
- UUID: the hugeint's top bit is flipped relative to the display form.
- VARIANT: not decoded. Decoding a VARIANT vector fails with
  `message.ErrVariant`, and the driver refuses a result with a VARIANT
  column (also nested) at PREPARE, suggesting a cast such as `v::JSON`.

## Errors

- `driver.ErrBadConn` (the session is gone; nothing ran, so a retry is
  safe): "Invalid connection id", "Connection heartbeat lease expired",
  "Database was closed", "The server database has been invalidated", and
  transport failures before the request was sent. An error with
  `must_invalidate` also discards the connection.
- Everything else is a typed `*Error` with `exception_type` and
  `extra_info`. The server sends INTERNAL and FATAL as they are.

## Settings worth knowing

| Setting | Default | Notes |
|---|---|---|
| `quack_prepare_inline_rows` | 24576 | server; overridden per statement by `inline_rows` |
| `quack_target_batch_bytes` | 32 MiB | size of FETCH batches (server) |
| `quack_server_stream_buffer_bytes` | 256 MiB | SEND_DATA back-pressure |
| `quack_server_max_connections` | 1024 | TCP connections |
| `quack_server_keep_alive_timeout` | 300 s | idle keep-alive |
| `quack_enable_reconnects` | false | result cache + ACKNOWLEDGEMENT |

`quack_serve([listen_uri], disable_ssl, ssl_cert_file, ssl_key_file,
allow_other_hostname, token, ...)` returns `(listen_uri, listen_url,
auth_token)`.
