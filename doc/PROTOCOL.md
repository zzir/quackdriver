# Quack wire-format notes

Authoritative sources, in priority order:

1. `references/duckdb-quack/src/include/quack_message.json` — message schema, field IDs.
2. `references/quack-protocol-ts/src/{binary,messages,logical-types,vector}.ts` — clean algorithm reference.
3. `references/quack-jdbc/src/main/java/com/gizmodata/quack/jdbc/` — structural reference (parallel layering).

## Transport

- HTTP/2, `POST /quack`, `Content-Type: application/duckdb`.
- Default port `9494`. URI scheme `quack:` or `quack://`. IPv6: `quack:[::1]:9494`.
- SSL: separate flag — DSN `?tls=true` → `https://...`.

## BinarySerializer primitives

| Primitive       | Encoding                                                        |
|-----------------|-----------------------------------------------------------------|
| field id        | little-endian `uint16`                                          |
| ULEB128         | unsigned, up to 10 bytes (64 bits)                              |
| SLEB128         | signed, sign-extended on terminator                             |
| string          | `[ULEB len][bytes]` UTF-8                                       |
| blob            | `[ULEB len][bytes]`                                             |
| hugeint         | `[SLEB upper][ULEB lower]` — upper-first on the wire            |
| optional_idx    | `ULEB`, sentinel `0xFFFFFFFFFFFFFFFF` = absent                  |
| bool            | one byte (0 or 1, strict)                                       |
| nullable<T>     | `[bool present][T body]`                                        |
| object framing  | body + `0xFFFF` terminator; absent fields omitted (skip-ahead)  |
| fixed integers  | little-endian, two's complement                                 |
| float32/64      | IEEE-754 little-endian                                          |
| `SerializationCompatibility::FromIndex(7)` is pinned for v1.5.3 |

## Message types

| ID  | Name                  | Direction  |
|-----|-----------------------|------------|
| 0   | INVALID               | —          |
| 1   | CONNECTION_REQUEST    | C → S      |
| 2   | CONNECTION_RESPONSE   | S → C      |
| 3   | PREPARE_REQUEST       | C → S      |
| 4   | PREPARE_RESPONSE      | S → C      |
| 7   | FETCH_REQUEST         | C → S      |
| 8   | FETCH_RESPONSE        | S → C      |
| 9   | APPEND_REQUEST        | C → S      |
| 10  | SUCCESS_RESPONSE      | S → C      |
| 11  | DISCONNECT_MESSAGE    | C → S      |
| 100 | ERROR_RESPONSE        | S → C      |

`MessageHeader` fields (1-based):

1. `type` — ULEB
2. `connection_id` — optional string
3. `client_query_id` — optional_idx (always present, sentinel = absent)

## DataChunk encoding

```
DataChunk wrapper (field id 300, nullable):
  field 100: row_count (ULEB)
  field 101: types (list<LogicalType>)
  field 102: columns (list<Vector>, length == |types|)
```

A `Vector` is an object:

```
field 90 (optional, default FLAT): format (ULEB)
   0=FLAT, 1=FSST, 2=CONSTANT, 3=DICTIONARY, 4=SEQUENCE
FLAT body:
  field 99 (GEOMETRY only): unused version (ULEB)
  field 100: has_validity_mask (bool)
  field 101 (if 100): validity mask (blob, ⌈count/64⌉×8 bytes, 1=valid LSB-first)
  for fixed-size physical types:
    field 102: data blob (size × count bytes, little-endian)
  for VARCHAR-physical:
    field 102: list<blob>
  for STRUCT-physical:
    field 103: list<Vector>  (one per child, all `count` rows)
  for LIST-physical:
    field 104: list_size (ULEB)
    field 105: list<{field 100: offset, field 101: length}>
    field 106: child Vector(list_size)
  for ARRAY-physical:
    field 103: array_size (ULEB; must match type metadata)
    field 104: child Vector(array_size × count)
CONSTANT body: nested Vector of length 1 (or 0)
DICTIONARY body:
  field 91: selection blob (count × uint32 little-endian)
  field 92: dictionary_count (ULEB)
  nested Vector(dictionary_count)
SEQUENCE body:
  field 91: start (SLEB)
  field 92: increment (SLEB)
```

## Logical-type encoding

```
LogicalType (object):
  field 100: id (ULEB)
  field 101 (optional, nullable): ExtraInfo

ExtraInfo (object):
  field 100: kind (ULEB)
  field 101 (optional): alias (string)
  field 103 (optional, nullable): extension metadata — we reject
  variant-specific fields starting at 200
```

ExtraInfo kinds: `INVALID=0, GENERIC=1, DECIMAL=2, STRING=3, LIST=4, STRUCT=5, ENUM=6, UNBOUND=7, AGGREGATE_STATE=8, ARRAY=9, ANY=10, INTEGER_LITERAL=11, TEMPLATE=12, GEO=13`.

## Quirks worth remembering

- `bool` is strict (0 or 1). Anything else is a protocol error.
- `HugeInt` order on the wire is **upper first** (`SLEB` then `ULEB`).
- `optional_idx` is always emitted in the header; the sentinel encodes "absent".
- Validity mask is allocated in 64-row words — pad to a multiple of 8 bytes.
- `STRUCT` children share the chunk's row count; the validity-masked outer rows are still serialized as zero-bytes inside child vectors.
- `UUID` upper word has its high bit flipped for display — see `uuidFromParts` for the round-trip.
- `Quack` does **not** transmit `RowsAffected` or `LastInsertId`.
