package message

import (
	"bytes"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/value"
)

// Expected bytes below are derived by hand from DuckDB v2.0.0
// Vector::Serialize for a FLAT vector at StorageVersion::V2_0_0:
//   - 100 has_validity_mask, 101 the mask (64-row LE words, bits past the
//     count stay set because ValidityMask::Initialize fills with ones);
//   - fixed-size data in 102, NULL rows holding NullValue<T>() (the type's
//     minimum for signed integers, 0x80 for BOOLEAN, NaN for floats, 0 for
//     unsigned integers);
//   - strings in 107 (byte_data_length, an optional_idx that is always set),
//     108 (u32 length per row, 0 for NULL) and 109 (the concatenated bytes).
func TestEncodeVectorExactBytes(t *testing.T) {
	integer := tp(logicaltype.IDInteger)
	cases := []struct {
		name string
		typ  logicaltype.Type
		vals []any
		want []byte
	}{
		{"INTEGER with NULL", integer, []any{int32(1), nil, int32(3)},
			obj(fld(100, 1), fld(101, 8, 0xfd, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff),
				fld(102, 12, 1, 0, 0, 0, 0, 0, 0, 0x80, 3, 0, 0, 0))},
		{"INTEGER without NULL", integer, []any{int32(-1)},
			obj(fld(100, 0), fld(102, 4, 0xff, 0xff, 0xff, 0xff))},
		{"BOOLEAN NULL is 0x80", tp(logicaltype.IDBoolean), []any{true, nil, false},
			obj(fld(100, 1), fld(101, blob(mask(3, 1))), fld(102, 3, 1, 0x80, 0))},
		{"UBIGINT NULL is 0", tp(logicaltype.IDUBigint), []any{nil},
			obj(fld(100, 1), fld(101, blob(mask(1, 0))), fld(102, blob(make([]byte, 8))))},
		{"DOUBLE NULL is NaN", tp(logicaltype.IDDouble), []any{nil, 1.0},
			obj(fld(100, 1), fld(101, blob(mask(2, 0))), fld(102, blob(le(uint64(0x7ff8000000000000), math.Float64bits(1)))))},
		{"FLOAT NULL is NaN", tp(logicaltype.IDFloat), []any{nil},
			obj(fld(100, 1), fld(101, blob(mask(1, 0))), fld(102, blob(le(uint32(0x7fc00000)))))},
		{"HUGEINT NULL is (upper min, lower 0)", tp(logicaltype.IDHugeint), []any{nil, big.NewInt(-2)},
			obj(fld(100, 1), fld(101, blob(mask(2, 0))), fld(102, blob(le(uint64(0), uint64(1<<63), uint64(math.MaxUint64-1), int64(-1)))))},
		{"INTERVAL NULL", tp(logicaltype.IDInterval), []any{nil},
			obj(fld(100, 1), fld(101, blob(mask(1, 0))), fld(102, blob(le(uint32(1<<31), uint32(1<<31), uint64(1<<63)))))},
		{"VARCHAR", tp(logicaltype.IDVarchar), []any{"a", nil, "", "xyz"},
			obj(fld(100, 1), fld(101, 8, 0xfd, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff),
				fld(107, 4), fld(108, 16, le(uint32(1), uint32(0), uint32(0), uint32(3))), fld(109, 4, "axyz"))},
		{"empty VARCHAR vector", tp(logicaltype.IDVarchar), []any{},
			obj(fld(100, 0), fld(107, 0), fld(108, 0), fld(109, 0))},
		{"BLOB", tp(logicaltype.IDBlob), []any{[]byte{0, 1}, "s"},
			obj(fld(100, 0), fld(107, 3), fld(108, 8, le(uint32(2), uint32(1))), fld(109, 3, 0, 1, "s"))},
		{"BIT", tp(logicaltype.IDBit), []any{"101"},
			obj(fld(100, 0), fld(107, 2), fld(108, 4, le(uint32(2))), fld(109, 2, 5, 0xfd))},
		{"BIGNUM", tp(logicaltype.IDBignum), []any{big.NewInt(-1), 1},
			obj(fld(100, 0), fld(107, 8), fld(108, 8, le(uint32(4), uint32(4))), fld(109, 8, 0x7f, 0xff, 0xfe, 0xfe, 0x80, 0x00, 0x01, 0x01))},
		{"LIST<INTEGER>", listOf(integer), []any{[]any{int32(1), int32(2)}, nil, []any{}},
			obj(fld(100, 1), fld(101, blob(mask(3, 1))),
				fld(104, 2),
				fld(105, 3, obj(fld(100, 0), fld(101, 2)), obj(fld(100, 0), fld(101, 0)), obj(fld(100, 2), fld(101, 0))),
				fld(106, obj(fld(100, 0), fld(102, 8, 1, 0, 0, 0, 2, 0, 0, 0))))},
		{"STRUCT", structOf(logicaltype.IDStruct, field("a", integer), field("b", tp(logicaltype.IDVarchar))),
			[]any{map[string]any{"a": int32(1), "b": "x"}, nil},
			obj(fld(100, 1), fld(101, blob(mask(2, 1))), fld(103, 2,
				obj(fld(100, 1), fld(101, blob(mask(2, 1))), fld(102, blob(le(int32(1), int32(math.MinInt32))))),
				obj(fld(100, 1), fld(101, blob(mask(2, 1))), fld(107, 1), fld(108, blob(le(uint32(1), uint32(0)))), fld(109, 1, "x"))))},
		{"ARRAY", arrayOf(integer, 2), []any{[]any{int32(1), int32(2)}},
			obj(fld(100, 0), fld(103, 2), fld(104, obj(fld(100, 0), fld(102, blob(le(int32(1), int32(2)))))))},
		{"SQLNULL", tp(logicaltype.IDSQLNull), []any{nil},
			obj(fld(100, 1), fld(101, blob(mask(1, 0))), fld(102, blob(le(int32(math.MinInt32)))))},
	}
	for _, c := range cases {
		got, err := encodeVec(c.typ, c.vals)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("%s:\n got  % x\n want % x", c.name, got, c.want)
		}
	}

	// A validity mask spans whole 64-row words.
	vals := make([]any, 65)
	for i := range vals {
		vals[i] = int8(i)
	}
	vals[64] = nil
	got := mustEncodeVec(t, tp(logicaltype.IDTinyint), vals)
	wantMask := fld(101, 16, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	if !bytes.Contains(got, wantMask) {
		t.Fatalf("65-row mask: % x", got)
	}
}

// roundTripChunk is a chunk with every encodable type, NULLs in every
// column, and values written in the form the decoder returns them.
func roundTripChunk() DataChunk {
	tz, _ := value.NewTimeTz(13*time.Hour+30*time.Minute, -5*3600)
	ts := time.Date(2024, 2, 29, 12, 34, 56, 789123456, time.UTC)
	json := logicaltype.Type{ID: logicaltype.IDVarchar, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraString, Alias: "JSON"}}
	cols := []struct {
		typ  logicaltype.Type
		vals Values
	}{
		{tp(logicaltype.IDBoolean), Values{true, false, nil}},
		{tp(logicaltype.IDTinyint), Values{int8(-128), int8(127), nil}},
		{tp(logicaltype.IDSmallint), Values{int16(-32768), int16(1), nil}},
		{tp(logicaltype.IDInteger), Values{int32(math.MaxInt32), int32(0), nil}},
		{tp(logicaltype.IDBigint), Values{int64(math.MinInt64), int64(5), nil}},
		{tp(logicaltype.IDUTinyint), Values{uint8(255), uint8(0), nil}},
		{tp(logicaltype.IDUSmallint), Values{uint16(65535), uint16(1), nil}},
		{tp(logicaltype.IDUInteger), Values{uint32(math.MaxUint32), uint32(2), nil}},
		{tp(logicaltype.IDUBigint), Values{uint64(math.MaxUint64), uint64(3), nil}},
		{tp(logicaltype.IDHugeint), Values{bigInt("-170141183460469231731687303715884105728"), bigInt("170141183460469231731687303715884105727"), nil}},
		{tp(logicaltype.IDUHugeint), Values{bigInt("340282366920938463463374607431768211455"), big.NewInt(0), nil}},
		{tp(logicaltype.IDFloat), Values{float32(1.25), float32(math.Inf(-1)), nil}},
		{tp(logicaltype.IDDouble), Values{math.MaxFloat64, -0.5, nil}},
		{decimalOf(4, 2), Values{"-99.99", "0.01", nil}},
		{decimalOf(9, 0), Values{"999999999", "-1", nil}},
		{decimalOf(18, 6), Values{"123456789012.345678", "-0.000001", nil}},
		{decimalOf(38, 37), Values{"9.9999999999999999999999999999999999999", "-0.0000000000000000000000000000000000001", nil}},
		{tp(logicaltype.IDVarchar), Values{"héllo", "", nil}},
		{tp(logicaltype.IDChar), Values{"c", "", nil}},
		{json, Values{`{"a":1}`, "null", nil}},
		{tp(logicaltype.IDBlob), Values{[]byte{0, 0xff}, []byte{}, nil}},
		{tp(logicaltype.IDGeometry), Values{[]byte{1, 2, 3}, []byte{}, nil}},
		{tp(logicaltype.IDBit), Values{"1", "0000000011111111", nil}},
		{tp(logicaltype.IDBignum), Values{bigInt("-123456789012345678901234567890"), big.NewInt(0), nil}},
		{tp(logicaltype.IDUUID), Values{"00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff", nil}},
		{tp(logicaltype.IDDate), Values{time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), time.Date(-100, 1, 1, 0, 0, 0, 0, time.UTC), nil}},
		{tp(logicaltype.IDTime), Values{23*time.Hour + 59*time.Minute + time.Microsecond, time.Duration(0), nil}},
		{tp(logicaltype.IDTimeNs), Values{time.Nanosecond, 24 * time.Hour, nil}},
		{tp(logicaltype.IDTimeTz), Values{tz, value.TimeTzValue{}, nil}},
		{tp(logicaltype.IDTimestampSec), Values{ts.Truncate(time.Second), time.Unix(-1, 0).UTC(), nil}},
		{tp(logicaltype.IDTimestampMs), Values{ts.Truncate(time.Millisecond), time.Unix(0, 0).UTC(), nil}},
		{tp(logicaltype.IDTimestamp), Values{ts.Truncate(time.Microsecond), time.UnixMicro(-1).UTC(), nil}},
		{tp(logicaltype.IDTimestampTz), Values{ts.Truncate(time.Microsecond), time.UnixMicro(1).UTC(), nil}},
		{tp(logicaltype.IDTimestampNs), Values{ts, time.Unix(0, -1).UTC(), nil}},
		{tp(logicaltype.IDTimestampTzNs), Values{ts, time.Unix(0, 1).UTC(), nil}},
		{tp(logicaltype.IDInterval), Values{IntervalValue{Months: -1, Days: 2, Micros: -3}, IntervalValue{}, nil}},
		{enumOf("red", "green"), Values{"green", "red", nil}},
		{listOf(tp(logicaltype.IDVarchar)), Values{[]any{"a", nil, ""}, []any{}, nil}},
		{listOf(listOf(tp(logicaltype.IDInteger))), Values{[]any{[]any{int32(1)}, nil, []any{}}, []any{nil}, nil}},
		{arrayOf(tp(logicaltype.IDDouble), 3), Values{[]any{1.0, nil, 3.0}, []any{0.0, 0.0, 0.0}, nil}},
		{structOf(logicaltype.IDStruct, field("i", tp(logicaltype.IDInteger)), field("s", listOf(tp(logicaltype.IDBlob)))),
			Values{map[string]any{"i": int32(1), "s": []any{[]byte("x")}}, map[string]any{"i": nil, "s": nil}, nil}},
		{structOf(logicaltype.IDTuple, field("", tp(logicaltype.IDVarchar)), field("", tp(logicaltype.IDBigint))),
			Values{[]any{"t", int64(1)}, []any{nil, nil}, nil}},
		{mapOf(tp(logicaltype.IDVarchar), tp(logicaltype.IDInteger)),
			Values{[]any{map[string]any{"key": "a", "value": int32(1)}, map[string]any{"key": "b", "value": nil}}, []any{}, nil}},
		{unionOf(field("n", tp(logicaltype.IDInteger)), field("s", tp(logicaltype.IDVarchar))),
			Values{map[string]any{"": uint8(1), "n": nil, "s": "str"}, map[string]any{"": uint8(0), "n": int32(7), "s": nil}, nil}},
		{tp(logicaltype.IDSQLNull), Values{nil, nil, nil}},
	}
	chunk := DataChunk{RowCount: 3}
	for _, c := range cols {
		chunk.Types = append(chunk.Types, c.typ)
		chunk.Columns = append(chunk.Columns, c.vals)
	}
	return chunk
}

func TestDataChunkRoundTripAllTypes(t *testing.T) {
	chunk := roundTripChunk()
	w := codec.NewWriter()
	if err := writeDataChunk(w, chunk); err != nil {
		t.Fatal(err)
	}
	enc := w.Bytes()
	r := codec.NewReader(enc)
	dec, err := decodeDataChunk(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AssertEOF(); err != nil {
		t.Fatal(err)
	}
	if dec.RowCount != chunk.RowCount || len(dec.Types) != len(chunk.Types) {
		t.Fatalf("shape: %d rows, %d types", dec.RowCount, len(dec.Types))
	}
	for i := range chunk.Columns {
		checkValues(t, chunk.Types[i].SQL(), dec.Columns[i], chunk.Columns[i].(Values))
	}
	// Decoded values encode back to the same bytes.
	w2 := codec.NewWriter()
	if err := writeDataChunk(w2, dec); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc, w2.Bytes()) {
		t.Fatal("re-encoding the decoded chunk changed the bytes")
	}
	// The whole message path, too.
	msg := mustEncode(t, Header{}, FetchResponse{Results: []DataChunk{chunk}, BatchIndex: Idx(1)})
	_, body := mustDecode(t, msg)
	if !sameColumns(body.(FetchResponse).Results[0], chunk) {
		t.Fatal("FETCH_RESPONSE round trip differs")
	}
}

func TestEncodeAcceptsLogicalAndRawValues(t *testing.T) {
	ts := time.Date(2001, 2, 3, 4, 5, 6, 7008009, time.FixedZone("x", 3600))
	cases := []struct {
		typ  logicaltype.Type
		in   []any
		want []any
	}{
		// Raw integers are stored as is (the v0.1 driver passed these).
		{tp(logicaltype.IDDate), []any{int32(1), int64(-1)}, []any{time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC), time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC)}},
		{tp(logicaltype.IDTimestamp), []any{int64(1), ts}, []any{time.UnixMicro(1).UTC(), ts.UTC().Truncate(time.Microsecond)}},
		{tp(logicaltype.IDTimestampTzNs), []any{ts, 5}, []any{ts.UTC(), time.Unix(0, 5).UTC()}},
		{tp(logicaltype.IDDate), []any{ts}, []any{time.Date(2001, 2, 3, 0, 0, 0, 0, time.UTC)}},
		// Lower precisions round to the nearest unit, halves away from the
		// epoch, like DuckDB's timestamp casts.
		{tp(logicaltype.IDTimestampMs), []any{time.Unix(-1, 999999999), time.Unix(-1, 998_500_000), time.Unix(0, 1_500_000)},
			[]any{time.UnixMilli(0).UTC(), time.UnixMilli(-2).UTC(), time.UnixMilli(2).UTC()}},
		{tp(logicaltype.IDTimestampSec), []any{time.Unix(-1, 500_600_000), time.Unix(0, 500_000_000)},
			[]any{time.Unix(0, 0).UTC(), time.Unix(1, 0).UTC()}},
		{tp(logicaltype.IDDate), []any{time.Unix(-1, 0)}, []any{time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC)}},
		{tp(logicaltype.IDTime), []any{int64(1), time.Second}, []any{time.Microsecond, time.Second}},
		{tp(logicaltype.IDTimeTz), []any{uint64(1 << 63), int64(5)}, []any{value.TimeTzFromBits(1 << 63), value.TimeTzFromBits(5)}},
		{tp(logicaltype.IDInterval), []any{90 * time.Minute}, []any{IntervalValue{Micros: 5400000000}}},
		{tp(logicaltype.IDInteger), []any{int8(-1), uint64(7), 9}, []any{int32(-1), int32(7), int32(9)}},
		{tp(logicaltype.IDUTinyint), []any{int64(255)}, []any{uint8(255)}},
		{tp(logicaltype.IDDouble), []any{float32(0.5), 3}, []any{0.5, 3.0}},
		{tp(logicaltype.IDHugeint), []any{int64(-5), uint64(math.MaxUint64)}, []any{big.NewInt(-5), new(big.Int).SetUint64(math.MaxUint64)}},
		{tp(logicaltype.IDBignum), []any{int8(-3)}, []any{big.NewInt(-3)}},
		// DECIMAL: decimal strings or the unscaled integer.
		{decimalOf(5, 2), []any{"1.5", "+2", "-.25", "3.1400", int32(314), big.NewInt(-1)}, []any{"1.50", "2.00", "-0.25", "3.14", "3.14", "-0.01"}},
		{decimalOf(30, 0), []any{"123"}, []any{"123"}},
		{decimalOf(38, 2), []any{int64(-5), uint8(7), big.NewInt(1)}, []any{"-0.05", "0.07", "0.01"}},
		{decimalOf(4, 0), []any{9999, -9999}, []any{"9999", "-9999"}},
		{tp(logicaltype.IDUHugeint), []any{uint64(math.MaxUint64), 3}, []any{new(big.Int).SetUint64(math.MaxUint64), big.NewInt(3)}},
		// UUID: any case, optional dashes and braces, or the raw hugeint.
		{tp(logicaltype.IDUUID), []any{"{550E8400E29B41D4A716446655440000}", mustUUID(t, "550e8400-e29b-41d4-a716-446655440000")},
			[]any{"550e8400-e29b-41d4-a716-446655440000", "550e8400-e29b-41d4-a716-446655440000"}},
		{enumOf("a", "b"), []any{1, uint8(0), "b"}, []any{"b", "a", "b"}},
		{tp(logicaltype.IDVarchar), []any{[]byte("bytes")}, []any{"bytes"}},
		{tp(logicaltype.IDGeometry), []any{"wkb"}, []any{[]byte("wkb")}},
		// UNION: the tag may be omitted when exactly one member is set.
		{unionOf(field("n", tp(logicaltype.IDInteger)), field("s", tp(logicaltype.IDVarchar))), []any{map[string]any{"s": "x"}},
			[]any{map[string]any{"": uint8(1), "n": nil, "s": "x"}}},
		// LIST/ARRAY accept Values too; STRUCT fields may be omitted.
		{listOf(tp(logicaltype.IDInteger)), []any{Values{int32(1)}}, []any{[]any{int32(1)}}},
		{structOf(logicaltype.IDStruct, field("a", tp(logicaltype.IDInteger)), field("b", tp(logicaltype.IDInteger))),
			[]any{map[string]any{"b": int32(2)}}, []any{map[string]any{"a": nil, "b": int32(2)}}},
	}
	for _, c := range cases {
		raw, err := encodeVec(c.typ, c.in)
		if err != nil {
			t.Errorf("%s %v: %v", c.typ.SQL(), c.in, err)
			continue
		}
		col := mustDecodeVec(t, c.typ, len(c.in), raw)
		checkValues(t, c.typ.SQL(), col, c.want)
	}
}

func mustUUID(t *testing.T, s string) *big.Int {
	t.Helper()
	v, err := UUIDToBig(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEncodeVectorErrors(t *testing.T) {
	integer := tp(logicaltype.IDInteger)
	un := unionOf(field("n", integer), field("s", tp(logicaltype.IDVarchar)))
	cases := []struct {
		name string
		typ  logicaltype.Type
		val  any
		want string
	}{
		{"string for INTEGER", integer, "1", "want an integer"},
		{"INTEGER overflow", integer, int64(1 << 40), "out of range"},
		{"TINYINT overflow", tp(logicaltype.IDTinyint), 128, "out of range"},
		{"negative UINTEGER", tp(logicaltype.IDUInteger), -1, "non-negative"},
		{"UBIGINT from float", tp(logicaltype.IDUBigint), 1.0, "non-negative"},
		{"BOOLEAN from int", tp(logicaltype.IDBoolean), 1, "want bool"},
		{"DOUBLE from string", tp(logicaltype.IDDouble), "1", "want float64"},
		{"DECIMAL precision", decimalOf(4, 2), "100.00", "does not fit"},
		{"DECIMAL int precision", decimalOf(4, 2), 10000, "does not fit"},
		{"DECIMAL negative int precision", decimalOf(18, 0), int64(-1e18), "does not fit"},
		{"DECIMAL big precision", decimalOf(38, 0), new(big.Int).Exp(big.NewInt(10), big.NewInt(38), nil), "does not fit"},
		{"UHUGEINT negative int", tp(logicaltype.IDUHugeint), -1, "out of range"},
		{"DECIMAL rounding", decimalOf(4, 2), "1.005", "fractional digits"},
		{"DECIMAL syntax", decimalOf(4, 2), "1e3", "invalid DECIMAL"},
		{"DECIMAL empty", decimalOf(4, 2), "-", "invalid DECIMAL"},
		{"DECIMAL float", decimalOf(4, 2), 1.5, "decimal string"},
		{"DECIMAL without info", tp(logicaltype.IDDecimal), "1", "cannot encode"},
		{"HUGEINT overflow", tp(logicaltype.IDHugeint), bigInt("170141183460469231731687303715884105728"), "out of range"},
		{"UHUGEINT negative", tp(logicaltype.IDUHugeint), big.NewInt(-1), "out of range"},
		{"nil *big.Int", tp(logicaltype.IDHugeint), (*big.Int)(nil), "want *big.Int"},
		{"UUID syntax", tp(logicaltype.IDUUID), "not-a-uuid", "invalid UUID"},
		{"UUID length", tp(logicaltype.IDUUID), "550e8400", "hex digits"},
		{"UUID int", tp(logicaltype.IDUUID), 5, "want string"},
		{"UUID nil *big.Int", tp(logicaltype.IDUUID), (*big.Int)(nil), "want string"},
		{"UUID out of range", tp(logicaltype.IDUUID), bigInt("170141183460469231731687303715884105728"), "out of range"},
		{"DECIMAL nil *big.Int", decimalOf(20, 0), (*big.Int)(nil), "decimal string"},
		{"BIGNUM nil *big.Int", tp(logicaltype.IDBignum), (*big.Int)(nil), "want *big.Int"},
		{"ENUM label", enumOf("a"), "b", "not in dictionary"},
		{"ENUM index", enumOf("a"), 1, "out of range"},
		{"ENUM float", enumOf("a"), 0.0, "want a string label"},
		{"DATE out of range", tp(logicaltype.IDDate), time.Date(9999999, 1, 1, 0, 0, 0, 0, time.UTC), "out of range"},
		{"TIMESTAMP_NS overflow", tp(logicaltype.IDTimestampNs), time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), "out of range"},
		{"TIMESTAMP from string", tp(logicaltype.IDTimestamp), "2020-01-01", "want time.Time"},
		{"TIME from time.Time", tp(logicaltype.IDTime), time.Now(), "want time.Duration"},
		{"TIME_TZ from Duration", tp(logicaltype.IDTimeTz), time.Second, "want TimeTzValue"},
		{"INTERVAL from int", tp(logicaltype.IDInterval), 5, "want IntervalValue"},
		{"VARCHAR from int", tp(logicaltype.IDVarchar), 5, "want string"},
		{"BLOB from int", tp(logicaltype.IDBlob), 5, "want []byte"},
		{"GEOMETRY from int", tp(logicaltype.IDGeometry), 5, "want []byte"},
		{"BIT invalid", tp(logicaltype.IDBit), "102", "invalid character"},
		{"BIT empty", tp(logicaltype.IDBit), "", "at least one bit"},
		{"BIT from bytes", tp(logicaltype.IDBit), []byte{1}, "want a string"},
		{"BIGNUM from string", tp(logicaltype.IDBignum), "1", "want *big.Int"},
		{"LIST from map", listOf(integer), map[string]any{}, "want []any"},
		{"LIST bad element", listOf(integer), []any{"x"}, "want an integer"},
		{"LIST without child", logicaltype.Type{ID: logicaltype.IDList}, []any{}, "missing child type"},
		{"ARRAY length", arrayOf(integer, 2), []any{int32(1)}, "with 2 values"},
		{"ARRAY size 0", arrayOf(integer, 0), []any{}, "size 0"},
		{"STRUCT from slice", structOf(logicaltype.IDStruct, field("a", integer)), []any{1}, "want map[string]any"},
		{"STRUCT unknown field", structOf(logicaltype.IDStruct, field("a", integer)), map[string]any{"b": 1}, "no field \"b\""},
		{"STRUCT bad field", structOf(logicaltype.IDStruct, field("a", integer)), map[string]any{"a": "x"}, "field \"a\""},
		{"STRUCT without info", tp(logicaltype.IDStruct), map[string]any{}, "missing children"},
		{"TUPLE arity", structOf(logicaltype.IDTuple, field("", integer)), []any{1, 2}, "with 1 values"},
		{"UNION two members", un, map[string]any{"n": 1, "s": "x"}, "sets both"},
		{"UNION nothing", un, map[string]any{}, "no member set"},
		{"UNION tag range", un, map[string]any{"": uint8(2)}, "out of range"},
		{"UNION tag mismatch", un, map[string]any{"": uint8(0), "s": "x"}, "does not match"},
		{"UNION bad type", unionOf(field("n", integer)), map[string]any{"n": 1}, ""},
		{"MAP NULL key", mapOf(integer, integer), []any{map[string]any{"key": nil, "value": 1}}, "non-NULL \"key\""},
		{"MAP entry type", mapOf(integer, integer), []any{1}, "MAP entry 0"},
		{"SQLNULL value", tp(logicaltype.IDSQLNull), 1, "want nil"},
		{"VARIANT", variantType(), int32(1), "VARIANT"},
		{"unsupported type", tp(logicaltype.IDAny), 1, "cannot encode"},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic: %v", c.name, r)
				}
			}()
			_, err := encodeVec(c.typ, []any{c.val})
			if c.name == "UNION bad type" {
				// A one-member UNION with a valid tag type is fine.
				if err != nil {
					t.Errorf("%s: %v", c.name, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
			}
		}()
	}

	// Malformed UNION types are refused.
	badUnion := structOf(logicaltype.IDUnion, field("", tp(logicaltype.IDVarchar)), field("n", integer))
	if _, err := encodeVec(badUnion, []any{map[string]any{"n": 1}}); err == nil {
		t.Error("UNION with VARCHAR tag accepted")
	}

	// Chunk-level errors name the column and row and never panic.
	chunks := []DataChunk{
		{RowCount: 1, Types: []logicaltype.Type{integer}, Columns: []Column{nil}},
		{RowCount: 2, Types: []logicaltype.Type{integer}, Columns: []Column{Values{1}}},
		{RowCount: 1, Types: []logicaltype.Type{integer, integer}, Columns: []Column{Values{1}, Values{"x"}}},
	}
	wants := []string{"column 0 is nil", "has 1 values, want 2", "column 1 (INTEGER): row 0"}
	for i, c := range chunks {
		err := writeDataChunk(codec.NewWriter(), c)
		if err == nil || !strings.Contains(err.Error(), wants[i]) {
			t.Errorf("chunk %d: err = %v, want %q", i, err, wants[i])
		}
	}

	// Deeply nested types are refused instead of recursing without bound.
	deep := integer
	for range maxVectorDepth + 1 {
		deep = listOf(deep)
	}
	var v any = int32(1)
	for range maxVectorDepth + 1 {
		v = []any{v}
	}
	if _, err := encodeVec(deep, []any{v}); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Errorf("deep nesting: err = %v", err)
	}
}

func TestEncodeDecodedColumns(t *testing.T) {
	// The encoder accepts any Column, e.g. a decoded one, and iterates Value.
	raw := obj(fld(90, 3), fld(91, blob(le(uint32(1), uint32(0)))), fld(92, 2),
		noNulls, fld(107, 2), fld(108, blob(le(uint32(1), uint32(1)))), fld(109, blob([]byte("ab"))))
	col := mustDecodeVec(t, tp(logicaltype.IDVarchar), 2, raw)
	chunk := DataChunk{RowCount: 2, Types: []logicaltype.Type{tp(logicaltype.IDVarchar)}, Columns: []Column{col}}
	buf := mustEncode(t, Header{}, SendDataRequest{StreamID: "s", Chunks: []DataChunk{chunk}, BatchIndex: Idx(1)})
	_, body := mustDecode(t, buf)
	checkValues(t, "re-encoded dictionary", body.(SendDataRequest).Chunks[0].Columns[0], []any{"b", "a"})
}

func TestEncodeNeverPanics(t *testing.T) {
	types := roundTripChunk().Types
	types = append(types, variantType(), tp(logicaltype.IDAny), tp(logicaltype.IDDecimal), tp(logicaltype.IDEnum),
		logicaltype.Type{ID: logicaltype.IDList}, logicaltype.Type{ID: logicaltype.IDArray}, tp(logicaltype.IDUnion),
		tp(logicaltype.IDTuple), tp(logicaltype.IDMap), structOf(logicaltype.IDUnion), tp(logicaltype.IDPointer))
	values := []any{
		nil, true, 0, -1, int8(-128), uint64(math.MaxUint64), int64(math.MinInt64), math.NaN(), float32(math.Inf(1)),
		"", "x", "1.5", "0101", "550e8400-e29b-41d4-a716-446655440000", []byte{}, []byte{0x80},
		(*big.Int)(nil), big.NewInt(-1), new(big.Int).Lsh(big.NewInt(1), 200),
		time.Time{}, time.Unix(1<<40, 0), time.Duration(math.MinInt64), value.TimeTzFromBits(math.MaxUint64),
		IntervalValue{Months: math.MinInt32}, []any{}, []any{nil}, []any{1, "a"}, Values{nil},
		map[string]any{}, map[string]any{"": uint8(9)}, map[string]any{"key": nil}, struct{}{}, &struct{}{},
	}
	for _, typ := range types {
		for _, v := range values {
			for _, vals := range [][]any{{v}, {v, nil}, {[]any{v}}, {map[string]any{"key": v, "value": v}}} {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("encode %s %#v: panic: %v", typ.Name(), vals, r)
						}
					}()
					_, _ = encodeVec(typ, vals)
				}()
			}
		}
	}
}

// TestTemporalInfinity: the ±infinity times DATE and every TIMESTAMP type
// decode to are DuckDB's sentinels on the wire, both ways. A server sends
// ±(2^63-1) for 'infinity'::TIMESTAMP_S too, which time.Unix used to turn
// into a wrapped-around year.
func TestTemporalInfinity(t *testing.T) {
	for _, c := range []struct {
		id    logicaltype.ID
		width int
	}{
		{logicaltype.IDDate, 4}, {logicaltype.IDTimestampSec, 8}, {logicaltype.IDTimestampMs, 8}, {logicaltype.IDTimestamp, 8},
		{logicaltype.IDTimestampTz, 8}, {logicaltype.IDTimestampNs, 8}, {logicaltype.IDTimestampTzNs, 8},
	} {
		typ := tp(c.id)
		data := le(int32(math.MaxInt32), int32(-math.MaxInt32))
		if c.width == 8 {
			data = le(int64(math.MaxInt64), int64(-math.MaxInt64))
		}
		raw := flatFixed(noNulls, data)
		if got := mustEncodeVec(t, typ, []any{value.InfinityTime, value.NegInfinityTime}); !bytes.Equal(got, raw) {
			t.Errorf("%s: encoded % x, want % x", typ.Name(), got, raw)
		}
		checkValues(t, typ.Name(), mustDecodeVec(t, typ, 2, raw), []any{value.InfinityTime, value.NegInfinityTime})
	}
	// TIME types never treat the sentinel times specially.
	if _, err := encodeVec(tp(logicaltype.IDTime), []any{value.InfinityTime}); err == nil {
		t.Errorf("TIME accepted a time.Time")
	}
	// TIMESTAMP_NS at the edges of int64: the smallest one's seconds times
	// 1e9 underflows before the sub-second part is added.
	ns := tp(logicaltype.IDTimestampNs)
	edges := []any{time.Unix(0, math.MinInt64+2).UTC(), time.Unix(0, math.MaxInt64-1).UTC()}
	checkValues(t, "TIMESTAMP_NS edges", mustDecodeVec(t, ns, 2, mustEncodeVec(t, ns, edges)), edges)
	if _, err := encodeVec(ns, []any{time.Unix(0, math.MinInt64+2).Add(-time.Second)}); err == nil {
		t.Errorf("TIMESTAMP_NS below int64 accepted")
	}
}

// TestEncodeBitBignum checks the BIT and BIGNUM encoders against
// hand-derived bytes (Bit::ToBit, IntToBignum); internal/value tests the
// decoders on the same bytes.
func TestEncodeBitBignum(t *testing.T) {
	for s, want := range map[string][]byte{
		"0101": {4, 0xf5}, "10000001": {0, 0x81}, "1": {7, 0xff}, "000000001": {7, 0xfe, 0x01},
	} {
		if got, err := encodeBit(s); err != nil || !bytes.Equal(got, want) {
			t.Errorf("encodeBit(%q) = % x, %v; want % x", s, got, err, want)
		}
	}
	for _, c := range []struct {
		n    *big.Int
		want []byte
	}{
		{big.NewInt(0), []byte{0x80, 0x00, 0x01, 0x00}},
		{big.NewInt(-1), []byte{0x7f, 0xff, 0xfe, 0xfe}},
		{big.NewInt(-256), []byte{0x7f, 0xff, 0xfd, 0xfe, 0xff}},
		{bigInt("18446744073709551616"), []byte{0x80, 0x00, 0x09, 0x01, 0, 0, 0, 0, 0, 0, 0, 0}},
	} {
		if got, err := encodeBignum(c.n); err != nil || !bytes.Equal(got, c.want) {
			t.Errorf("encodeBignum(%s) = % x, %v; want % x", c.n, got, err, c.want)
		}
	}
}
