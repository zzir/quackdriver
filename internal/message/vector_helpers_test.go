package message

import (
	"bytes"
	"encoding/binary"
	"math/big"
	"reflect"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// Helpers for hand-building serialized vectors. A vector object is a list of
// (u16 LE field id, value) pairs closed by 0xFFFF; see vector_decode.go for
// the field table derived from DuckDB v2.0.0 Vector::Serialize.

// fld is a field id followed by its value bytes (parts as in cat).
func fld(id uint16, parts ...any) []byte {
	return cat(append([]any{[]byte{byte(id), byte(id >> 8)}}, parts...)...)
}

// obj closes parts with the end-of-object marker.
func obj(parts ...any) []byte { return cat(append(parts, end)...) }

func uleb(v uint64) []byte {
	w := codec.NewWriter()
	w.WriteULEB128(v)
	return w.Bytes()
}

func sleb(v int64) []byte {
	w := codec.NewWriter()
	w.WriteSLEB128(v)
	return w.Bytes()
}

// blob is a ULEB length-prefixed byte string.
func blob(b []byte) []byte { return cat(uleb(uint64(len(b))), b) }

// le concatenates fixed-size values in little-endian order.
func le(vals ...any) []byte {
	var b bytes.Buffer
	for _, v := range vals {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			panic(err)
		}
	}
	return b.Bytes()
}

// mask is a DuckDB validity mask for count rows with the given rows NULL:
// 64-row little-endian words, every other bit set (also past count).
func mask(count int, nulls ...int) []byte {
	out := bytes.Repeat([]byte{0xff}, (count+63)/64*8)
	for _, n := range nulls {
		out[n/8] &^= 1 << (n % 8)
	}
	return out
}

// noNulls is "100 has_validity_mask = false".
var noNulls = fld(100, 0x00)

// withNulls is "100 true, 101 mask".
func withNulls(count int, nulls ...int) []byte {
	return cat(fld(100, 0x01), fld(101, blob(mask(count, nulls...))))
}

// flatFixed is a FLAT fixed-size vector object.
func flatFixed(validity []byte, data []byte) []byte {
	return obj(validity, fld(102, blob(data)))
}

// flatStrings is a FLAT VARCHAR-physical vector object in the 2.0 layout.
func flatStrings(validity []byte, lengths []uint32, data string) []byte {
	var l []byte
	for _, n := range lengths {
		l = append(l, le(n)...)
	}
	return obj(validity, fld(107, uleb(uint64(len(data)))), fld(108, blob(l)), fld(109, blob([]byte(data))))
}

func typeBytes(t logicaltype.Type) []byte {
	w := codec.NewWriter()
	logicaltype.Encode(w, t)
	return w.Bytes()
}

// rawChunk is a raw DataChunk object with the given vector objects.
func rawChunk(rows int, types []logicaltype.Type, vectors ...[]byte) []byte {
	out := cat(fld(100, uleb(uint64(rows))), fld(101, uleb(uint64(len(types)))))
	for _, t := range types {
		out = cat(out, typeBytes(t))
	}
	out = cat(out, fld(102, uleb(uint64(len(vectors)))))
	for _, v := range vectors {
		out = cat(out, v)
	}
	return cat(out, end)
}

// decodeVec decodes one vector object of count rows and requires that it
// consumes the whole input.
func decodeVec(typ logicaltype.Type, count int, raw []byte) (Column, error) {
	r := codec.NewReader(raw)
	col, err := decodeVector(r, typ, count, 0)
	if err != nil {
		return nil, err
	}
	if err := r.AssertEOF(); err != nil {
		return nil, err
	}
	return col, nil
}

func mustDecodeVec(t *testing.T, typ logicaltype.Type, count int, raw []byte) Column {
	t.Helper()
	col, err := decodeVec(typ, count, raw)
	if err != nil {
		t.Fatalf("decode %s: %v\nbytes: % x", typ.Name(), err, raw)
	}
	if col.Len() != count {
		t.Fatalf("decode %s: Len() = %d, want %d", typ.Name(), col.Len(), count)
	}
	return col
}

// encodeVec encodes vals as one vector object.
func encodeVec(typ logicaltype.Type, vals []any) ([]byte, error) {
	w := codec.NewWriter()
	if err := encodeVector(w, typ, vals, 0); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}

func mustEncodeVec(t *testing.T, typ logicaltype.Type, vals []any) []byte {
	t.Helper()
	b, err := encodeVec(typ, vals)
	if err != nil {
		t.Fatalf("encode %s: %v", typ.Name(), err)
	}
	return b
}

// sameColumns reports whether two chunks hold the same rows.
func sameColumns(a, b DataChunk) bool {
	if a.RowCount != b.RowCount || len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if !equalValues([]any(Materialize(a.Columns[i])), []any(Materialize(b.Columns[i]))) {
			return false
		}
	}
	return true
}

func checkValues(t *testing.T, label string, col Column, want []any) {
	t.Helper()
	got := []any(Materialize(col))
	if !equalValues(got, want) {
		t.Errorf("%s:\n got  %#v\n want %#v", label, got, want)
	}
}

// Type constructors.

func tp(id logicaltype.ID) logicaltype.Type { return logicaltype.Type{ID: id} }

func listOf(child logicaltype.Type) logicaltype.Type {
	return logicaltype.Type{ID: logicaltype.IDList, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraList, Child: &child}}
}

func arrayOf(child logicaltype.Type, size uint32) logicaltype.Type {
	return logicaltype.Type{ID: logicaltype.IDArray, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraArray, Child: &child, ArraySize: size}}
}

func structOf(id logicaltype.ID, children ...logicaltype.ChildType) logicaltype.Type {
	return logicaltype.Type{ID: id, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraStruct, Children: children}}
}

func field(name string, t logicaltype.Type) logicaltype.ChildType {
	return logicaltype.ChildType{Name: name, Type: t}
}

func mapOf(k, v logicaltype.Type) logicaltype.Type {
	entry := structOf(logicaltype.IDStruct, field("key", k), field("value", v))
	return logicaltype.Type{ID: logicaltype.IDMap, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraList, Child: &entry}}
}

func unionOf(members ...logicaltype.ChildType) logicaltype.Type {
	return structOf(logicaltype.IDUnion, append([]logicaltype.ChildType{field("", tp(logicaltype.IDUTinyint))}, members...)...)
}

func decimalOf(width, scale uint8) logicaltype.Type {
	return logicaltype.Type{ID: logicaltype.IDDecimal, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraDecimal, Width: width, Scale: scale}}
}

func enumOf(labels ...string) logicaltype.Type {
	return logicaltype.Type{ID: logicaltype.IDEnum, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraEnum, EnumValues: labels}}
}

func variantType() logicaltype.Type { return logicaltype.Type{ID: logicaltype.IDVariant} }

// equalValues is reflect.DeepEqual, except that *big.Int values compare by
// value (their internal word slices differ between equal numbers).
func equalValues(a, b any) bool {
	switch x := a.(type) {
	case *big.Int:
		y, ok := b.(*big.Int)
		return ok && (x == nil) == (y == nil) && (x == nil || x.Cmp(y) == 0)
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return false
		}
		for i := range x {
			if !equalValues(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !equalValues(v, w) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}
