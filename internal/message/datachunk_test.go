package message

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// The vectors in this file are built by hand from DuckDB v2.0.0
// Vector::Serialize / Deserialize (src/common/types/vector.cpp) and the
// buffer serializers in src/common/vector/{dictionary,sequence}_vector.cpp.

func bigInt(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic(s)
	}
	return v
}

// TestDecodeFixedTypes covers the FLAT fixed-size layout; the boxing of
// every type is checked against real server data in golden_test.go.
func TestDecodeFixedTypes(t *testing.T) {
	junk8 := uint64(0xaaaaaaaaaaaaaaaa)
	// A NULL row's data is ignored, whatever it holds.
	col := mustDecodeVec(t, tp(logicaltype.IDBigint), 3, flatFixed(withNulls(3, 1), le(int64(-1), junk8, int64(math.MaxInt64))))
	checkValues(t, "BIGINT", col, []any{int64(-1), nil, int64(math.MaxInt64)})
	col = mustDecodeVec(t, tp(logicaltype.IDInterval), 2, flatFixed(withNulls(2, 0), le(junk8, junk8, int32(-1), int32(2), int64(-3))))
	checkValues(t, "INTERVAL", col, []any{nil, IntervalValue{Months: -1, Days: 2, Micros: -3}})

	// Without a validity mask every row is valid; a mask may also be sent
	// although no row is NULL (CanHaveNull).
	col = mustDecodeVec(t, tp(logicaltype.IDInteger), 2, flatFixed(noNulls, le(int32(1), int32(2))))
	checkValues(t, "no validity", col, []any{int32(1), int32(2)})
	col = mustDecodeVec(t, tp(logicaltype.IDInteger), 2, flatFixed(withNulls(2), le(int32(1), int32(2))))
	checkValues(t, "all-valid mask", col, []any{int32(1), int32(2)})

	// A wide ENUM uses a 16-bit index.
	labels := make([]string, 300)
	for i := range labels {
		labels[i] = "l" + itoa(i)
	}
	col = mustDecodeVec(t, enumOf(labels...), 2, flatFixed(noNulls, le(uint16(299), uint16(0))))
	checkValues(t, "ENUM uint16", col, []any{labels[299], labels[0]})

	// SQLNULL is physically INT32 and always NULL.
	col = mustDecodeVec(t, tp(logicaltype.IDSQLNull), 2, flatFixed(noNulls, le(int32(1), int32(2))))
	checkValues(t, "SQLNULL", col, []any{nil, nil})
}

func itoa(i int) string { return big.NewInt(int64(i)).String() }

func TestDecodeStringLayout(t *testing.T) {
	// 2.0 layout: 107 byte_data_length, 108 u32 per row (0 for NULL), 109 bytes.
	raw := flatStrings(withNulls(5, 1, 3), []uint32{5, 0, 0, 0, 3}, "helloxyz")
	col := mustDecodeVec(t, tp(logicaltype.IDVarchar), 5, raw)
	checkValues(t, "VARCHAR", col, []any{"hello", nil, "", nil, "xyz"})

	// A NULL row's length is ignored, as in Vector::Deserialize; surplus
	// byte data is ignored too.
	raw = flatStrings(withNulls(3, 1), []uint32{1, 99, 2}, "abcTRAILING")
	col = mustDecodeVec(t, tp(logicaltype.IDVarchar), 3, raw)
	checkValues(t, "NULL length ignored", col, []any{"a", nil, "bc"})

	// Lengths running past the byte data are rejected.
	if _, err := decodeVec(tp(logicaltype.IDVarchar), 2, flatStrings(noNulls, []uint32{2, 2}, "abc")); err == nil ||
		!strings.Contains(err.Error(), "string lengths exceed") {
		t.Fatalf("overlong lengths: err = %v", err)
	}
	// A huge length cannot overflow the bounds check.
	if _, err := decodeVec(tp(logicaltype.IDVarchar), 1, flatStrings(noNulls, []uint32{math.MaxUint32}, "abc")); err == nil {
		t.Fatal("huge length accepted")
	}

	// Empty vector (e.g. the child of a LIST whose rows are all empty).
	col = mustDecodeVec(t, tp(logicaltype.IDVarchar), 0, flatStrings(noNulls, nil, ""))
	checkValues(t, "empty", col, []any{})

	// Legacy list layout (102) is still accepted.
	legacy := obj(withNulls(3, 1), fld(102, 3, blob([]byte("ab")), blob([]byte("ignored")), blob(nil)))
	col = mustDecodeVec(t, tp(logicaltype.IDVarchar), 3, legacy)
	checkValues(t, "legacy 102", col, []any{"ab", nil, ""})
	legacyShort := obj(noNulls, fld(102, 2, blob([]byte("a")), blob([]byte("b"))))
	if _, err := decodeVec(tp(logicaltype.IDVarchar), 3, legacyShort); err == nil || !strings.Contains(err.Error(), "string vector has 2 values") {
		t.Fatalf("legacy count mismatch: err = %v", err)
	}

	// Other VARCHAR-physical types use the same layout.
	json := logicaltype.Type{ID: logicaltype.IDVarchar, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraString, Alias: "JSON"}}
	for _, typ := range []logicaltype.Type{tp(logicaltype.IDChar), json} {
		col = mustDecodeVec(t, typ, 2, flatStrings(noNulls, []uint32{2, 0}, "{}"))
		checkValues(t, typ.SQL(), col, []any{"{}", ""})
	}
	for _, id := range []logicaltype.ID{logicaltype.IDBlob, logicaltype.IDAggregateState} {
		col = mustDecodeVec(t, tp(id), 3, flatStrings(withNulls(3, 2), []uint32{2, 0, 0}, "\x00\xff"))
		checkValues(t, tp(id).Name(), col, []any{[]byte{0, 0xff}, []byte{}, nil})
	}

	// BIT and BIGNUM (formats tested in internal/value) are validated on
	// decode, so that Value cannot fail.
	col = mustDecodeVec(t, tp(logicaltype.IDBit), 2, flatStrings(withNulls(2, 1), []uint32{2, 0}, "\x04\xf5"))
	checkValues(t, "BIT", col, []any{"0101", nil})
	col = mustDecodeVec(t, tp(logicaltype.IDBignum), 2, flatStrings(noNulls, []uint32{4, 4}, "\x7f\xff\xfe\xfe\x80\x00\x01\x01"))
	checkValues(t, "BIGNUM", col, []any{big.NewInt(-1), big.NewInt(1)})
	for _, c := range []struct {
		typ  logicaltype.ID
		data string
	}{{logicaltype.IDBit, "\x08\xff"}, {logicaltype.IDBignum, "\x80\x00\x02\x01"}} {
		if _, err := decodeVec(tp(c.typ), 1, flatStrings(noNulls, []uint32{uint32(len(c.data))}, c.data)); err == nil {
			t.Errorf("%s % x: expected error", tp(c.typ).Name(), c.data)
		}
	}
}

func TestDecodeGeometry(t *testing.T) {
	wkb := []byte{0x01, 0x01, 0x00, 0x00, 0x00, 0, 0, 0, 0, 0, 0, 0xf0, 0x3f, 0, 0, 0, 0, 0, 0, 0, 0x40}
	geo := tp(logicaltype.IDGeometry)
	// 99 geometry_format = WKB (1), validity, then 102 list<blob>; the NULL
	// row carries NullValue<string_t>() = "\x80".
	raw := obj(fld(99, 1), withNulls(2, 1), fld(102, 2, blob(wkb), blob([]byte{0x80})))
	col := mustDecodeVec(t, geo, 2, raw)
	checkValues(t, "GEOMETRY", col, []any{wkb, nil})
	enc := mustEncodeVec(t, geo, []any{wkb, nil})
	if !bytes.Equal(enc, raw) {
		t.Fatalf("GEOMETRY encode:\n got  % x\n want % x", enc, raw)
	}
	// Without 99 the data is in the pre-1.5 SPATIAL format, which DuckDB 2.0
	// never sends and the driver cannot convert.
	for _, bad := range [][]byte{
		obj(noNulls, fld(102, 1, blob(wkb))),
		obj(fld(99, 0), noNulls, fld(102, 1, blob(wkb))),
		obj(fld(99, 17), noNulls, fld(102, 1, blob(wkb))),
	} {
		if _, err := decodeVec(geo, 1, bad); err == nil || !strings.Contains(err.Error(), "GEOMETRY storage format") {
			t.Errorf("% x: err = %v", bad, err)
		}
	}
}

func TestDecodeVectorFormats(t *testing.T) {
	varchar := tp(logicaltype.IDVarchar)

	// CONSTANT: 90 = 2, then a one-row vector body in the same object.
	raw := obj(fld(90, 2), noNulls, fld(102, blob(le(int32(7)))))
	col := mustDecodeVec(t, tp(logicaltype.IDInteger), 4, raw)
	checkValues(t, "CONSTANT INTEGER", col, []any{int32(7), int32(7), int32(7), int32(7)})
	raw = obj(fld(90, 2), withNulls(1, 0), fld(102, blob(le(int32(0)))))
	col = mustDecodeVec(t, tp(logicaltype.IDInteger), 3, raw)
	checkValues(t, "CONSTANT NULL", col, []any{nil, nil, nil})
	raw = obj(fld(90, 2), fld(100, 0), fld(107, 2), fld(108, blob(le(uint32(2)))), fld(109, blob([]byte("hi"))))
	col = mustDecodeVec(t, varchar, 3, raw)
	checkValues(t, "CONSTANT VARCHAR", col, []any{"hi", "hi", "hi"})

	// A CONSTANT vector does not materialize its rows: 2^32-1 rows cost
	// nothing until read.
	huge := mustDecodeVec(t, tp(logicaltype.IDBigint), math.MaxUint32, obj(fld(90, 2), noNulls, fld(102, blob(le(int64(1))))))
	if huge.Value(math.MaxUint32-1) != int64(1) {
		t.Fatal("constant row")
	}

	// DICTIONARY: 91 selection (u32 per row), 92 dict_count, then the
	// dictionary's vector body in the same object.
	raw = obj(fld(90, 3), fld(91, blob(le(uint32(1), uint32(0), uint32(1), uint32(2), uint32(1)))), fld(92, 3),
		withNulls(3, 2), fld(107, 4), fld(108, blob(le(uint32(1), uint32(3), uint32(0)))), fld(109, blob([]byte("abcd"))))
	col = mustDecodeVec(t, varchar, 5, raw)
	checkValues(t, "DICTIONARY VARCHAR", col, []any{"bcd", "a", "bcd", nil, "bcd"})
	bad := obj(fld(90, 3), fld(91, blob(le(uint32(3)))), fld(92, 3), noNulls, fld(102, blob(le(int32(1), int32(2), int32(3)))))
	if _, err := decodeVec(tp(logicaltype.IDInteger), 1, bad); err == nil || !strings.Contains(err.Error(), "selection 3 out of range") {
		t.Fatalf("dictionary out of range: err = %v", err)
	}
	bad = obj(fld(90, 3), fld(91, blob(le(uint32(0)))), fld(92, 1), noNulls, fld(102, blob(le(int32(1)))))
	if _, err := decodeVec(tp(logicaltype.IDInteger), 2, bad); err == nil || !strings.Contains(err.Error(), "selection vector has 4 bytes") {
		t.Fatalf("short selection: err = %v", err)
	}

	// SEQUENCE: 91 start, 92 increment (signed varints), uint64 wrap-around
	// truncated to the physical width like GenerateSequence.
	seq := func(start, inc int64) []byte { return obj(fld(90, 4), fld(91, sleb(start)), fld(92, sleb(inc))) }
	col = mustDecodeVec(t, tp(logicaltype.IDBigint), 4, seq(10, -3))
	checkValues(t, "SEQUENCE BIGINT", col, []any{int64(10), int64(7), int64(4), int64(1)})
	col = mustDecodeVec(t, tp(logicaltype.IDTinyint), 3, seq(120, 5))
	checkValues(t, "SEQUENCE TINYINT wraps", col, []any{int8(120), int8(125), int8(-126)})
	col = mustDecodeVec(t, tp(logicaltype.IDInteger), 3, seq(-1, 1))
	checkValues(t, "SEQUENCE INTEGER", col, []any{int32(-1), int32(0), int32(1)})
	col = mustDecodeVec(t, decimalOf(9, 2), 2, seq(150, 1))
	checkValues(t, "SEQUENCE DECIMAL", col, []any{"1.50", "1.51"})
	for _, c := range []struct {
		typ logicaltype.Type
		raw []byte
	}{
		{tp(logicaltype.IDTinyint), seq(200, 1)},
		{tp(logicaltype.IDSmallint), seq(0, 40000)},
		{varchar, seq(0, 1)},
		{tp(logicaltype.IDUBigint), seq(0, 1)},
		{tp(logicaltype.IDDouble), seq(0, 1)},
	} {
		if _, err := decodeVec(c.typ, 2, c.raw); err == nil || !strings.Contains(err.Error(), "SEQUENCE") {
			t.Errorf("SEQUENCE %s: err = %v", c.typ.Name(), err)
		}
	}

	// FSST is never serialized; unknown formats are rejected.
	for _, f := range []int{1, 6, 0x7f} {
		if _, err := decodeVec(tp(logicaltype.IDInteger), 1, obj(fld(90, f), noNulls)); err == nil {
			t.Errorf("format %d accepted", f)
		}
	}
	if _, err := decodeVec(tp(logicaltype.IDInteger), 1, obj(fld(90, uleb(300)), noNulls)); err == nil {
		t.Error("format 300 accepted")
	}
}

func TestDecodeNested(t *testing.T) {
	integer := tp(logicaltype.IDInteger)
	varchar := tp(logicaltype.IDVarchar)
	entry := func(off, n uint64) []byte { return obj(fld(100, uleb(off)), fld(101, uleb(n))) }

	// Plain FLAT nested vectors are checked against real server data in
	// golden_test.go. A NULL row's LIST entry is ignored even when out of
	// range (DuckDB resets it).
	raw := obj(withNulls(2, 1), fld(104, 1), fld(105, 2, entry(0, 1), entry(50, 50)), fld(106, flatFixed(noNulls, le(int32(9)))))
	col := mustDecodeVec(t, listOf(integer), 2, raw)
	checkValues(t, "LIST NULL entry", col, []any{[]any{int32(9)}, nil})

	st := structOf(logicaltype.IDStruct, field("a", integer), field("b", varchar))
	if _, err := decodeVec(st, 1, obj(noNulls, fld(103, 1, flatFixed(noNulls, le(int32(1)))))); err == nil ||
		!strings.Contains(err.Error(), "has 2 children, got 1") {
		t.Fatalf("struct child count: err = %v", err)
	}

	// DuckDB reads an all-unnamed STRUCT back as a TUPLE; so does the driver.
	raw = obj(noNulls, fld(103, 2, flatFixed(noNulls, le(int32(5))), flatStrings(noNulls, []uint32{2}, "ok")))
	col = mustDecodeVec(t, structOf(logicaltype.IDStruct, field("", integer), field("", varchar)), 1, raw)
	checkValues(t, "unnamed STRUCT", col, []any{[]any{int32(5), "ok"}})

	// The ARRAY size must match the type.
	if _, err := decodeVec(arrayOf(integer, 2), 1, obj(noNulls, fld(103, 3), fld(104, flatFixed(noNulls, le(int32(1), int32(2), int32(3)))))); err == nil ||
		!strings.Contains(err.Error(), "does not match type") {
		t.Fatalf("array size mismatch: err = %v", err)
	}

	// Nested strings below a dictionary inside a LIST: the child is a
	// dictionary vector over three strings.
	dictChild := obj(fld(90, 3), fld(91, blob(le(uint32(2), uint32(2), uint32(0)))), fld(92, 3),
		noNulls, fld(107, 3), fld(108, blob(le(uint32(1), uint32(1), uint32(1)))), fld(109, blob([]byte("xyz"))))
	raw = obj(noNulls, fld(104, 3), fld(105, 2, entry(0, 1), entry(1, 2)), fld(106, dictChild))
	col = mustDecodeVec(t, listOf(varchar), 2, raw)
	checkValues(t, "LIST of DICTIONARY", col, []any{[]any{"z"}, []any{"z", "x"}})

	// A CONSTANT STRUCT whose children are flat one-row vectors.
	raw = obj(fld(90, 2), noNulls, fld(103, 2, flatFixed(noNulls, le(int32(8))), flatStrings(noNulls, []uint32{1}, "c")))
	col = mustDecodeVec(t, st, 2, raw)
	checkValues(t, "CONSTANT STRUCT", col, []any{map[string]any{"a": int32(8), "b": "c"}, map[string]any{"a": int32(8), "b": "c"}})

	// A DICTIONARY over LIST rows.
	raw = obj(fld(90, 3), fld(91, blob(le(uint32(0), uint32(0), uint32(1)))), fld(92, 2),
		noNulls, fld(104, 1), fld(105, 2, entry(0, 1), entry(1, 0)), fld(106, flatFixed(noNulls, le(int32(3)))))
	col = mustDecodeVec(t, listOf(integer), 3, raw)
	checkValues(t, "DICTIONARY LIST", col, []any{[]any{int32(3)}, []any{int32(3)}, []any{}})

	// STRUCT children can themselves be CONSTANT / SEQUENCE (compressed
	// serialization passes down).
	raw = obj(noNulls, fld(103, 2, obj(fld(90, 4), fld(91, sleb(1)), fld(92, sleb(1))),
		obj(fld(90, 2), noNulls, fld(107, 1), fld(108, blob(le(uint32(1)))), fld(109, blob([]byte("k"))))))
	col = mustDecodeVec(t, st, 3, raw)
	checkValues(t, "compressed children", col, []any{
		map[string]any{"a": int32(1), "b": "k"}, map[string]any{"a": int32(2), "b": "k"}, map[string]any{"a": int32(3), "b": "k"},
	})
}

func TestDecodeVariantRejected(t *testing.T) {
	// VARIANT vectors are refused before their body is read, wherever the
	// type is nested.
	body := obj(noNulls, fld(102, 0))
	for _, c := range []struct {
		typ logicaltype.Type
		raw []byte
	}{
		{variantType(), body},
		{listOf(variantType()), obj(noNulls, fld(104, 1), fld(105, 1, obj(fld(100, 0), fld(101, 1))), fld(106, body))},
		{structOf(logicaltype.IDStruct, field("v", variantType())), obj(noNulls, fld(103, 1, body))},
		{variantType(), obj(fld(90, 5))}, // SHREDDED
	} {
		if _, err := decodeVec(c.typ, 1, c.raw); !errors.Is(err, ErrVariant) {
			t.Errorf("%s: err = %v, want ErrVariant", c.typ.SQL(), err)
		}
	}
	// SHREDDED (format 5) only ever holds VARIANT.
	if _, err := decodeVec(tp(logicaltype.IDInteger), 1, obj(fld(90, 5))); err == nil || !strings.Contains(err.Error(), "unsupported vector format 5") {
		t.Errorf("SHREDDED INTEGER: err = %v", err)
	}
	if _, err := encodeVec(variantType(), []any{int32(1)}); err == nil {
		t.Fatal("VARIANT encode accepted")
	}
}

func TestDecodedValuesAreFresh(t *testing.T) {
	// BLOB values are copies: mutating one never changes the next Value or
	// the input buffer.
	raw := flatStrings(noNulls, []uint32{3}, "abc")
	col := mustDecodeVec(t, tp(logicaltype.IDBlob), 1, raw)
	b := col.Value(0).([]byte)
	b[0] = 'X'
	if got := col.Value(0).([]byte); string(got) != "abc" {
		t.Fatalf("second Value = %q", got)
	}
	if !bytes.Contains(raw, []byte("abc")) {
		t.Fatal("input buffer was modified")
	}

	// Same for CONSTANT and DICTIONARY BLOBs and for nested values.
	constant := obj(fld(90, 2), noNulls, fld(107, 1), fld(108, blob(le(uint32(1)))), fld(109, blob([]byte("q"))))
	col = mustDecodeVec(t, tp(logicaltype.IDBlob), 2, constant)
	col.Value(0).([]byte)[0] = 'Z'
	if string(col.Value(1).([]byte)) != "q" {
		t.Fatal("CONSTANT BLOB rows share memory")
	}
	list := obj(noNulls, fld(104, 1), fld(105, 1, obj(fld(100, 0), fld(101, 1))), fld(106, flatStrings(noNulls, []uint32{1}, "w")))
	col = mustDecodeVec(t, listOf(tp(logicaltype.IDBlob)), 1, list)
	l := col.Value(0).([]any)
	l[0].([]byte)[0] = 'M'
	l[0] = nil
	if got := col.Value(0).([]any); string(got[0].([]byte)) != "w" {
		t.Fatalf("LIST value shared: %v", got)
	}

	// BIGNUM values behind a CONSTANT are distinct *big.Int values.
	bn := obj(fld(90, 2), noNulls, fld(107, 4), fld(108, blob(le(uint32(4)))), fld(109, blob([]byte{0x80, 0x00, 0x01, 0x05})))
	col = mustDecodeVec(t, tp(logicaltype.IDBignum), 2, bn)
	col.Value(0).(*big.Int).SetInt64(99)
	if col.Value(1).(*big.Int).Int64() != 5 || col.Value(0).(*big.Int).Int64() != 5 {
		t.Fatal("BIGNUM values are shared")
	}

	// Decoded chunks do not depend on the input once values are boxed.
	chunk := DataChunk{RowCount: 2, Types: []logicaltype.Type{tp(logicaltype.IDVarchar)}, Columns: []Column{Values{"one", "two"}}}
	w := codec.NewWriter()
	if err := writeDataChunk(w, chunk); err != nil {
		t.Fatal(err)
	}
	dec, err := decodeDataChunk(codec.NewReader(w.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	s := dec.Columns[0].Value(0).(string)
	for i := range w.Bytes() {
		w.Bytes()[i] = 0
	}
	if s != "one" {
		t.Fatalf("string aliases the input: %q", s)
	}
}

func TestColumnIndexPanics(t *testing.T) {
	cols := []Column{
		mustDecodeVec(t, tp(logicaltype.IDInteger), 1, flatFixed(noNulls, le(int32(1)))),
		mustDecodeVec(t, tp(logicaltype.IDVarchar), 1, flatStrings(noNulls, []uint32{1}, "a")),
		mustDecodeVec(t, tp(logicaltype.IDInteger), 1, obj(fld(90, 2), noNulls, fld(102, blob(le(int32(1)))))),
		mustDecodeVec(t, tp(logicaltype.IDBigint), 1, obj(fld(90, 4), fld(91, sleb(0)), fld(92, sleb(1)))),
		Values{1},
	}
	for i, c := range cols {
		for _, idx := range []int{-1, 1} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("column %d: Value(%d) did not panic", i, idx)
					}
				}()
				c.Value(idx)
			}()
		}
	}
}

func TestDecodeMalformedVectors(t *testing.T) {
	integer := tp(logicaltype.IDInteger)
	entry := obj(fld(100, 0), fld(101, 1))
	cases := []struct {
		name string
		typ  logicaltype.Type
		n    int
		raw  []byte
		want string
	}{
		{"short validity", integer, 1, obj(fld(100, 1), fld(101, blob([]byte{0xff})), fld(102, blob(le(int32(1))))), "validity mask has 1 bytes"},
		{"short data", integer, 2, flatFixed(noNulls, le(int32(1))), "vector data has 4 bytes, expected 8"},
		{"blob longer than input", integer, 1, obj(noNulls, fld(102, uleb(1<<40))), "exceeds"},
		{"bad bool", integer, 1, obj(fld(100, 2)), "invalid boolean"},
		{"missing data", integer, 1, obj(noNulls), "expected field"},
		{"short lengths", tp(logicaltype.IDVarchar), 2, obj(noNulls, fld(107, 0), fld(108, blob(le(uint32(0)))), fld(109, blob(nil))), "string length data"},
		{"byte data shorter than declared", tp(logicaltype.IDVarchar), 1, obj(noNulls, fld(107, 5), fld(108, blob(le(uint32(1)))), fld(109, blob([]byte("a")))), "string byte data"},
		{"absurd row count", integer, maxVectorCount + 1, flatFixed(noNulls, nil), "vector size"},
		{"list entries mismatch", listOf(integer), 2, obj(noNulls, fld(104, 1), fld(105, 1, entry), fld(106, flatFixed(noNulls, le(int32(1))))), "got 1 entries"},
		{"list entry out of range", listOf(integer), 1, obj(noNulls, fld(104, 0), fld(105, 1, entry), fld(106, flatFixed(noNulls, nil))), "LIST entry 0"},
		{"list absurd size", listOf(integer), 1, obj(noNulls, fld(104, uleb(1<<62))), "LIST child size"},
		{"list absurd entry count", listOf(integer), 1, obj(noNulls, fld(104, 0), fld(105, uleb(1<<40))), "exceeds"},
		{"list child short", listOf(integer), 1, obj(noNulls, fld(104, 1), fld(105, 1, entry), fld(106, flatFixed(noNulls, nil))), "vector data"},
		{"array missing info", logicaltype.Type{ID: logicaltype.IDArray}, 1, obj(noNulls, fld(103, 1)), "missing child type"},
		{"array absurd", arrayOf(integer, 1<<31), 1 << 20, obj(noNulls, fld(103, uleb(1<<31))), "ARRAY child size"},
		{"struct missing info", tp(logicaltype.IDStruct), 1, obj(noNulls, fld(103, 0)), "missing type metadata"},
		{"enum index", enumOf("a"), 1, flatFixed(noNulls, le(uint8(1))), "ENUM index 1 out of range"},
		{"decimal missing info", tp(logicaltype.IDDecimal), 1, flatFixed(noNulls, le(int16(1))), "cannot decode"},
		{"unsupported type", tp(logicaltype.IDAny), 1, obj(noNulls), "cannot decode"},
		{"dictionary absurd", integer, 1, obj(fld(90, 3), fld(91, blob(le(uint32(0)))), fld(92, uleb(1<<62))), "dictionary size"},
		{"geometry spatial", tp(logicaltype.IDGeometry), 1, obj(noNulls, fld(102, 1, blob(nil))), "GEOMETRY"},
		{"trailing field", integer, 1, obj(noNulls, fld(102, blob(le(int32(1)))), fld(103, 0)), "end-of-object"},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic: %v", c.name, r)
				}
			}()
			_, err := decodeVec(c.typ, c.n, c.raw)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
			}
		}()
	}

	// Nested CONSTANT formats cannot exhaust the stack.
	var bomb []byte
	for range maxVectorDepth + 2 {
		bomb = append(bomb, fld(90, 2)...)
	}
	if _, err := decodeVec(integer, 1, obj(bomb, noNulls, fld(102, blob(le(int32(1)))))); err == nil ||
		!strings.Contains(err.Error(), "nesting") {
		t.Fatalf("constant bomb: err = %v", err)
	}
}

func TestDataChunkDecode(t *testing.T) {
	// A complete raw DataChunk with three differently encoded columns.
	types := []logicaltype.Type{tp(logicaltype.IDBigint), tp(logicaltype.IDVarchar), tp(logicaltype.IDInteger)}
	raw := rawChunk(3, types,
		obj(fld(90, 4), fld(91, sleb(0)), fld(92, sleb(1))),
		flatStrings(withNulls(3, 1), []uint32{1, 0, 2}, "abc"),
		obj(fld(90, 2), noNulls, fld(102, blob(le(int32(9))))))
	r := codec.NewReader(raw)
	c, err := decodeDataChunk(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AssertEOF(); err != nil {
		t.Fatal(err)
	}
	want := [][]any{{int64(0), "a", int32(9)}, {int64(1), nil, int32(9)}, {int64(2), "bc", int32(9)}}
	got := c.Rows()
	if len(got) != 3 {
		t.Fatalf("rows: %v", got)
	}
	for i := range want {
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Errorf("row %d col %d = %#v, want %#v", i, j, got[i][j], want[i][j])
			}
		}
	}
	// Column count must match the types; the error names the column.
	if _, err := decodeDataChunk(codec.NewReader(rawChunk(1, types[:1], flatFixed(noNulls, le(int32(1)))))); err == nil ||
		!strings.Contains(err.Error(), "column 0 (BIGINT)") {
		t.Fatalf("bad column: err = %v", err)
	}
}
