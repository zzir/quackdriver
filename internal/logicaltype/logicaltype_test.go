package logicaltype

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
)

func ptr(t Type) *Type { return &t }

func structOf(children ...ChildType) Type {
	return Type{ID: IDStruct, Info: &ExtraInfo{Kind: ExtraStruct, Children: children}}
}

func listOf(c Type) Type { return Type{ID: IDList, Info: &ExtraInfo{Kind: ExtraList, Child: &c}} }

func encodeBytes(t Type) []byte {
	w := codec.NewWriter()
	Encode(w, t)
	return w.Bytes()
}

// roundTrip decodes the encoding of in and checks that it consumes the input
// and re-encodes to the same bytes.
func roundTrip(t *testing.T, in Type) Type {
	t.Helper()
	enc := encodeBytes(in)
	r := codec.NewReader(enc)
	out, err := Decode(r)
	if err != nil {
		t.Fatalf("decode % x: %v", enc, err)
	}
	if err := r.AssertEOF(); err != nil {
		t.Fatal(err)
	}
	if again := encodeBytes(out); !bytes.Equal(enc, again) {
		t.Fatalf("re-encode differs:\n %x\n %x", enc, again)
	}
	return out
}

// The expected bytes are hand-derived from DuckDB 2.0's LogicalType::Serialize
// and serialize_types.cpp: field 101 is a nullable (present byte)
// LogicalTypeInfo object whose "with default" properties are omitted.
func TestEncodeExactBytes(t *testing.T) {
	cases := []struct {
		name string
		in   Type
		want []byte
	}{
		{"INTEGER", Type{ID: IDInteger}, []byte{0x64, 0x00, 0x0d, 0xff, 0xff}},
		{"TIMESTAMP_TZ_NS", Type{ID: IDTimestampTzNs}, []byte{0x64, 0x00, 0x21, 0xff, 0xff}},
		{"DECIMAL(18,4)", Type{ID: IDDecimal, Info: &ExtraInfo{Kind: ExtraDecimal, Width: 18, Scale: 4}}, []byte{
			0x64, 0x00, 0x15, // 100 id = 21
			0x65, 0x00, 0x01, // 101 type_info present
			0x64, 0x00, 0x02, // 100 kind DECIMAL_TYPE_INFO
			0xc8, 0x00, 0x12, // 200 width 18
			0xc9, 0x00, 0x04, // 201 scale 4
			0xff, 0xff, 0xff, 0xff,
		}},
		{"DECIMAL(18,0) omits default scale", Type{ID: IDDecimal, Info: &ExtraInfo{Kind: ExtraDecimal, Width: 18}}, []byte{
			0x64, 0x00, 0x15, 0x65, 0x00, 0x01, 0x64, 0x00, 0x02, 0xc8, 0x00, 0x12, 0xff, 0xff, 0xff, 0xff,
		}},
		{"VARCHAR without collation", Type{ID: IDVarchar, Info: &ExtraInfo{Kind: ExtraString}}, []byte{
			0x64, 0x00, 0x19, 0x65, 0x00, 0x01, 0x64, 0x00, 0x03, 0xff, 0xff, 0xff, 0xff,
		}},
		{"JSON alias", Type{ID: IDVarchar, Info: &ExtraInfo{Kind: ExtraGeneric, Alias: "JSON"}}, []byte{
			0x64, 0x00, 0x19, 0x65, 0x00, 0x01, 0x64, 0x00, 0x01, 0x65, 0x00, 0x04, 'J', 'S', 'O', 'N', 0xff, 0xff, 0xff, 0xff,
		}},
		{"INTEGER[3]", Type{ID: IDArray, Info: &ExtraInfo{Kind: ExtraArray, Child: ptr(Type{ID: IDInteger}), ArraySize: 3}}, []byte{
			0x64, 0x00, 0x6c, 0x65, 0x00, 0x01, 0x64, 0x00, 0x09,
			0xc8, 0x00, 0x64, 0x00, 0x0d, 0xff, 0xff, // 200 child INTEGER
			0xc9, 0x00, 0x03, // 201 size
			0xff, 0xff, 0xff, 0xff,
		}},
		{"ARRAY size 0 omitted", Type{ID: IDArray, Info: &ExtraInfo{Kind: ExtraArray, Child: ptr(Type{ID: IDInteger})}}, []byte{
			0x64, 0x00, 0x6c, 0x65, 0x00, 0x01, 0x64, 0x00, 0x09, 0xc8, 0x00, 0x64, 0x00, 0x0d, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		}},
		{"TUPLE(INTEGER)", Type{ID: IDTuple, Info: &ExtraInfo{Kind: ExtraStruct, Children: []ChildType{{Type: Type{ID: IDInteger}}}}}, []byte{
			0x64, 0x00, 0x6e, 0x65, 0x00, 0x01, 0x64, 0x00, 0x05,
			0xc8, 0x00, 0x01, // 200 child_types, 1 pair
			0x00, 0x00, 0x00, // pair.0 name ""
			0x01, 0x00, 0x64, 0x00, 0x0d, 0xff, 0xff, // pair.1 INTEGER
			0xff, 0xff, // end pair
			0xff, 0xff, 0xff, 0xff,
		}},
		{"empty STRUCT omits child_types", Type{ID: IDStruct, Info: &ExtraInfo{Kind: ExtraStruct}}, []byte{
			0x64, 0x00, 0x64, 0x65, 0x00, 0x01, 0x64, 0x00, 0x05, 0xff, 0xff, 0xff, 0xff,
		}},
		{"ENUM('a')", Type{ID: IDEnum, Info: &ExtraInfo{Kind: ExtraEnum, EnumValues: []string{"a"}}}, []byte{
			0x64, 0x00, 0x68, 0x65, 0x00, 0x01, 0x64, 0x00, 0x06,
			0xc8, 0x00, 0x01, // 200 values_count (R)
			0xc9, 0x00, 0x01, 0x01, 'a', // 201 values
			0xff, 0xff, 0xff, 0xff,
		}},
		{"GEOMETRY no CRS", Type{ID: IDGeometry, Info: &ExtraInfo{Kind: ExtraGeo}}, []byte{
			0x64, 0x00, 0x3c, 0x65, 0x00, 0x01, 0x64, 0x00, 0x0d, 0xc8, 0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		}},
		// LEGACY_AGGREGATE_STATE_TYPE_INFO lost all its properties in 2.0.
		{"LEGACY_AGGREGATE_STATE", Type{ID: IDAggregateState, Info: &ExtraInfo{Kind: ExtraAggregateState, FunctionName: "x"}}, []byte{
			0x64, 0x00, 0x69, 0x65, 0x00, 0x01, 0x64, 0x00, 0x08, 0xff, 0xff, 0xff, 0xff,
		}},
	}
	for _, c := range cases {
		if got := encodeBytes(c.in); !bytes.Equal(got, c.want) {
			t.Errorf("%s:\n got  % x\n want % x", c.name, got, c.want)
			continue
		}
		if c.in.Info == nil || c.in.Info.FunctionName == "" {
			roundTrip(t, c.in)
		}
	}
	// A generic info (alias holder) is fine on any type; names survive.
	roundTrip(t, Type{ID: IDBlob, Info: &ExtraInfo{Kind: ExtraGeneric, Alias: "BYTES"}})
	out := roundTrip(t, structOf(ChildType{Name: "id", Type: Type{ID: IDBigint}}, ChildType{Name: "l", Type: listOf(Type{ID: IDVarchar})}))
	if out.Info.Children[1].Name != "l" || out.Info.Children[1].Type.Info.Child.ID != IDVarchar {
		t.Fatalf("struct round trip: %+v", out.Info)
	}
}

func TestSQL(t *testing.T) {
	mapType := func(k, v Type) Type {
		entry := structOf(ChildType{Name: "key", Type: k}, ChildType{Name: "value", Type: v})
		return Type{ID: IDMap, Info: &ExtraInfo{Kind: ExtraList, Child: &entry}}
	}
	array := func(c Type, n uint32) Type {
		return Type{ID: IDArray, Info: &ExtraInfo{Kind: ExtraArray, Child: &c, ArraySize: n}}
	}
	union := Type{ID: IDUnion, Info: &ExtraInfo{Kind: ExtraStruct, Children: []ChildType{
		{Name: "", Type: Type{ID: IDUTinyint}},
		{Name: "num", Type: Type{ID: IDInteger}},
		{Name: "err", Type: Type{ID: IDVarchar}},
	}}}
	deep := Type{ID: IDInteger}
	for range maxDepth + 10 {
		deep = listOf(deep)
	}
	cases := []struct {
		in   Type
		want string
	}{
		// Plain names are EnumUtil::ToString(LogicalTypeId) in DuckDB 2.0.
		{Type{ID: IDBoolean}, "BOOLEAN"},
		{Type{ID: IDTinyint}, "TINYINT"},
		{Type{ID: IDSmallint}, "SMALLINT"},
		{Type{ID: IDInteger}, "INTEGER"},
		{Type{ID: IDBigint}, "BIGINT"},
		{Type{ID: IDUTinyint}, "UTINYINT"},
		{Type{ID: IDUSmallint}, "USMALLINT"},
		{Type{ID: IDUInteger}, "UINTEGER"},
		{Type{ID: IDUBigint}, "UBIGINT"},
		{Type{ID: IDHugeint}, "HUGEINT"},
		{Type{ID: IDUHugeint}, "UHUGEINT"},
		{Type{ID: IDFloat}, "FLOAT"},
		{Type{ID: IDDouble}, "DOUBLE"},
		{Type{ID: IDVarchar}, "VARCHAR"},
		{Type{ID: IDVarchar, Info: &ExtraInfo{Kind: ExtraString, Collation: "nocase"}}, "VARCHAR"},
		{Type{ID: IDChar}, "CHAR"},
		{Type{ID: IDBlob}, "BLOB"},
		{Type{ID: IDBit}, "BIT"},
		{Type{ID: IDBignum}, "BIGNUM"},
		{Type{ID: IDUUID}, "UUID"},
		{Type{ID: IDDate}, "DATE"},
		{Type{ID: IDTime}, "TIME"},
		{Type{ID: IDTimeNs}, "TIME_NS"},
		{Type{ID: IDTimeTz}, "TIME WITH TIME ZONE"},
		{Type{ID: IDTimestamp}, "TIMESTAMP"},
		{Type{ID: IDTimestampSec}, "TIMESTAMP_S"},
		{Type{ID: IDTimestampMs}, "TIMESTAMP_MS"},
		{Type{ID: IDTimestampNs}, "TIMESTAMP_NS"},
		{Type{ID: IDTimestampTz}, "TIMESTAMP WITH TIME ZONE"},
		{Type{ID: IDTimestampTzNs}, "TIMESTAMPTZ_NS"},
		{Type{ID: IDInterval}, "INTERVAL"},
		{Type{ID: IDVariant}, "VARIANT"},
		{Type{ID: IDAggregateState}, "LEGACY_AGGREGATE_STATE"},
		{Type{ID: IDAny}, "ANY"},
		{Type{ID: 200}, "ID(200)"},
		{Type{ID: IDGeometry}, "GEOMETRY"},
		{Type{ID: IDGeometry, Info: &ExtraInfo{Kind: ExtraGeo, CRSDefinition: "OGC:CRS84"}}, "GEOMETRY('OGC:CRS84')"},
		{Type{ID: IDSQLNull}, `"NULL"`},
		{Type{ID: IDDecimal, Info: &ExtraInfo{Kind: ExtraDecimal, Width: 18, Scale: 4}}, "DECIMAL(18,4)"},
		{Type{ID: IDDecimal}, "DECIMAL"},
		{listOf(listOf(Type{ID: IDInteger})), "INTEGER[][]"},
		{Type{ID: IDList}, "LIST"},
		{array(Type{ID: IDInteger}, 3), "INTEGER[3]"},
		{listOf(array(Type{ID: IDDouble}, 2)), "DOUBLE[2][]"},
		{array(Type{ID: IDInteger}, 0), "INTEGER[ANY]"},
		{structOf(ChildType{Name: "a", Type: Type{ID: IDInteger}}, ChildType{Name: "b", Type: Type{ID: IDVarchar}}),
			`STRUCT("a" INTEGER, "b" VARCHAR)`},
		{structOf(ChildType{Name: `we"ird name`, Type: Type{ID: IDInteger}}), `STRUCT("we""ird name" INTEGER)`},
		{structOf(ChildType{Name: "Mixed", Type: listOf(structOf(ChildType{Name: "x", Type: Type{ID: IDTimestampTz}}))}),
			`STRUCT("Mixed" STRUCT("x" TIMESTAMP WITH TIME ZONE)[])`},
		{Type{ID: IDStruct}, "STRUCT"},
		{Type{ID: IDTuple, Info: &ExtraInfo{Kind: ExtraStruct, Children: []ChildType{{Type: Type{ID: IDInteger}}, {Type: Type{ID: IDVarchar}}}}},
			"TUPLE(INTEGER, VARCHAR)"},
		{Type{ID: IDTuple}, "TUPLE"},
		{mapType(Type{ID: IDInteger}, listOf(Type{ID: IDVarchar})), "MAP(INTEGER, VARCHAR[])"},
		{Type{ID: IDMap}, "MAP"},
		{union, `UNION("num" INTEGER, "err" VARCHAR)`},
		{Type{ID: IDUnion}, "UNION"},
		{Type{ID: IDEnum, Info: &ExtraInfo{Kind: ExtraEnum, EnumValues: []string{"a", "it's"}}}, "ENUM('a', 'it''s')"},
		{Type{ID: IDVarchar, Info: &ExtraInfo{Kind: ExtraGeneric, Alias: "my type"}}, `"my type"`},
		{listOf(Type{ID: IDVarchar, Info: &ExtraInfo{Kind: ExtraGeneric, Alias: "JSON"}}), "JSON[]"},
	}
	for _, c := range cases {
		if got := c.in.SQL(); got != c.want {
			t.Errorf("SQL(%s) = %q, want %q", c.in.Name(), got, c.want)
		}
	}
	// A hand-built type deeper than Decode allows renders without recursing forever.
	if s := deep.SQL(); !strings.HasSuffix(s, "[][]") {
		t.Errorf("deep SQL ends in %q", s[len(s)-10:])
	}
}

// typeBytes builds a serialized LogicalType with type info of the given
// kind; body writes the info's own properties.
func typeBytes(id ID, kind ExtraInfoType, body func(w *codec.Writer)) []byte {
	w := codec.NewWriter()
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(100, func(w *codec.Writer) { w.WriteULEB128(uint64(id)) })
		w.WriteField(101, func(w *codec.Writer) {
			w.WriteBool(true)
			w.WriteObject(func(w *codec.Writer) {
				w.WriteField(100, func(w *codec.Writer) { w.WriteULEB128(uint64(kind)) })
				body(w)
			})
		})
	})
	return w.Bytes()
}

func TestDecodeRejects(t *testing.T) {
	deep := Type{ID: IDInteger}
	for range maxDepth + 5 {
		deep = listOf(deep)
	}
	idOnly := codec.NewWriter()
	idOnly.WriteObject(func(w *codec.Writer) { w.WriteField(100, func(w *codec.Writer) { w.WriteULEB128(300) }) })
	cases := map[string][]byte{
		// Type info that does not belong to the type id, and DECIMALs
		// DuckDB itself rejects (LogicalType::Deserialize).
		"VARCHAR with LIST info":    encodeBytes(Type{ID: IDVarchar, Info: &ExtraInfo{Kind: ExtraList, Child: ptr(Type{ID: IDInteger})}}),
		"INTEGER with DECIMAL info": encodeBytes(Type{ID: IDInteger, Info: &ExtraInfo{Kind: ExtraDecimal, Width: 4}}),
		"DECIMAL(0,0)":              encodeBytes(Type{ID: IDDecimal, Info: &ExtraInfo{Kind: ExtraDecimal}}),
		"DECIMAL(39,0)":             encodeBytes(Type{ID: IDDecimal, Info: &ExtraInfo{Kind: ExtraDecimal, Width: 39}}),
		"DECIMAL(5,6)":              encodeBytes(Type{ID: IDDecimal, Info: &ExtraInfo{Kind: ExtraDecimal, Width: 5, Scale: 6}}),
		"type id 300":               idOnly.Bytes(),
		"too deep":                  encodeBytes(deep),
		"2^62 struct children": typeBytes(IDStruct, ExtraStruct, func(w *codec.Writer) {
			w.WriteField(200, func(w *codec.Writer) { w.WriteULEB128(1 << 62) })
		}),
		"enum count mismatch": typeBytes(IDEnum, ExtraEnum, func(w *codec.Writer) {
			w.WriteField(200, func(w *codec.Writer) { w.WriteULEB128(3) })
			w.WriteField(201, func(w *codec.Writer) { w.WriteULEB128(1); w.WriteString("a") })
		}),
		// 102 is the deleted modifiers property; 103 announces an extension
		// info object that is not there.
		"modifiers": typeBytes(IDVarchar, ExtraGeneric, func(w *codec.Writer) {
			w.WriteField(102, func(w *codec.Writer) { w.WriteBool(true) })
		}),
		"bad extension info": typeBytes(IDVarchar, ExtraGeneric, func(w *codec.Writer) {
			w.WriteField(103, func(w *codec.Writer) { w.WriteBool(true) })
		}),
	}
	for name, raw := range cases {
		if _, err := Decode(codec.NewReader(raw)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDecodeLegacyAggregateState(t *testing.T) {
	// An older writer sends the (now deleted) 200/201/202 properties.
	raw := typeBytes(IDAggregateState, ExtraAggregateState, func(w *codec.Writer) {
		w.WriteField(200, func(w *codec.Writer) { w.WriteString("sum") })
		w.WriteField(201, func(w *codec.Writer) { Encode(w, Type{ID: IDHugeint}) })
		w.WriteField(202, func(w *codec.Writer) {
			w.WriteULEB128(1)
			Encode(w, Type{ID: IDInteger})
		})
	})
	r := codec.NewReader(raw)
	got, err := Decode(r)
	if err != nil || r.AssertEOF() != nil {
		t.Fatal(err)
	}
	if got.Info.FunctionName != "sum" || got.Info.ReturnType.ID != IDHugeint || len(got.Info.BoundArgumentTypes) != 1 {
		t.Fatalf("decoded %+v", got.Info)
	}
}

func TestDecodeMalformedNeverPanics(t *testing.T) {
	full := encodeBytes(structOf(
		ChildType{Name: "a", Type: Type{ID: IDDecimal, Info: &ExtraInfo{Kind: ExtraDecimal, Width: 9, Scale: 2}}},
		ChildType{Name: "b", Type: Type{ID: IDEnum, Info: &ExtraInfo{Kind: ExtraEnum, EnumValues: []string{"x", "y"}}}},
		ChildType{Name: "c", Type: Type{ID: IDArray, Info: &ExtraInfo{Kind: ExtraArray, Child: ptr(Type{ID: IDUUID}), ArraySize: 4}}},
		ChildType{Name: "d", Type: Type{ID: IDGeometry, Info: &ExtraInfo{Kind: ExtraGeo, CRSDefinition: "EPSG:4326"}}},
	))
	for i := range full {
		if _, err := Decode(codec.NewReader(full[:i])); err == nil {
			t.Fatalf("%d-byte prefix decoded", i)
		}
		for _, v := range []byte{0x00, 0x02, 0x80, 0xff} {
			mutated := append([]byte(nil), full...)
			mutated[i] = v
			_, _ = Decode(codec.NewReader(mutated)) // must not panic
		}
	}
}

// val writes a serialized Value; typ is nil when the parent implies it and
// payload nil for NULL.
func val(typ *Type, payload func(w *codec.Writer)) func(w *codec.Writer) {
	return func(w *codec.Writer) {
		w.WriteObject(func(w *codec.Writer) {
			if typ != nil {
				w.WriteField(100, func(w *codec.Writer) { Encode(w, *typ) })
			}
			w.WriteField(101, func(w *codec.Writer) { w.WriteBool(payload == nil) })
			if payload != nil {
				w.WriteField(102, payload)
			}
		})
	}
}

// nested is the payload of a LIST, STRUCT or ARRAY Value.
func nested(children ...func(w *codec.Writer)) func(w *codec.Writer) {
	return func(w *codec.Writer) {
		w.WriteObject(func(w *codec.Writer) {
			w.WriteField(100, func(w *codec.Writer) {
				w.WriteULEB128(uint64(len(children)))
				for _, c := range children {
					c(w)
				}
			})
		})
	}
}

// TestDecodeExtensionInfo: a type carrying ExtensionTypeInfo, as every
// "agg(x) EXPORT_STATE" column does, decodes and re-encodes unchanged.
func TestDecodeExtensionInfo(t *testing.T) {
	variantT := structOf(ChildType{Name: "keys", Type: listOf(Type{ID: IDVarchar})}, ChildType{Name: "data", Type: Type{ID: IDBlob}})
	variantT.ID = IDVariant
	stateT := listOf(structOf(ChildType{Name: "type", Type: Type{ID: IDType}}, ChildType{Name: "value", Type: variantT}))
	geo := func(w *codec.Writer) { // GEOMETRY: the payload, then 103 geometry_format
		w.WriteObject(func(w *codec.Writer) {
			w.WriteField(100, func(w *codec.Writer) { Encode(w, Type{ID: IDGeometry}) })
			w.WriteField(101, func(w *codec.Writer) { w.WriteBool(false) })
			w.WriteField(102, func(w *codec.Writer) { w.WriteString("\x01") })
			w.WriteField(103, func(w *codec.Writer) { w.WriteULEB128(1) })
		})
	}
	props := []func(w *codec.Writer){
		// LIST(STRUCT(type TYPE, value VARIANT)), as aggregate states use.
		val(&stateT, nested(val(nil, nested(
			val(nil, func(w *codec.Writer) { Encode(w, Type{ID: IDBigint}) }),
			val(nil, nested(val(nil, nested(val(nil, func(w *codec.Writer) { w.WriteString("k") }))),
				val(nil, func(w *codec.Writer) { w.WriteBlob([]byte{0, 1}) }))))))),
		val(&Type{ID: IDDecimal, Info: &ExtraInfo{Kind: ExtraDecimal, Width: 38, Scale: 2}},
			func(w *codec.Writer) { w.WriteHugeInt(codec.HugeInt{Upper: -1, Lower: 5}) }),
		val(&Type{ID: IDUHugeint}, func(w *codec.Writer) { w.WriteULEB128(1); w.WriteULEB128(2) }),
		val(&Type{ID: IDArray, Info: &ExtraInfo{Kind: ExtraArray, Child: ptr(Type{ID: IDDouble}), ArraySize: 2}}, nested(
			val(nil, func(w *codec.Writer) { w.WriteBytes(binary.LittleEndian.AppendUint64(nil, math.Float64bits(1.5))) }),
			val(nil, nil))),
		val(&Type{ID: IDInterval}, func(w *codec.Writer) {
			w.WriteObject(func(w *codec.Writer) {
				w.WriteField(1, func(w *codec.Writer) { w.WriteSLEB128(14) })
				w.WriteField(3, func(w *codec.Writer) { w.WriteSLEB128(-7) })
			})
		}),
		geo,
		// LIST(ANY): each child carries its own type.
		val(ptr(listOf(Type{ID: IDAny})), nested(
			val(&Type{ID: IDBoolean}, func(w *codec.Writer) { w.WriteBool(true) }),
			val(&Type{ID: IDFloat}, func(w *codec.Writer) { w.WriteBytes([]byte{0, 0, 0x20, 0x40}) }),
			val(&Type{ID: IDUBigint}, func(w *codec.Writer) { w.WriteULEB128(1 << 63) }))),
	}
	ext := codec.NewWriter()
	ext.WriteObject(func(w *codec.Writer) {
		w.WriteField(100, func(w *codec.Writer) { // modifiers
			w.WriteULEB128(2)
			w.WriteObject(func(w *codec.Writer) {
				w.WriteField(100, val(&Type{ID: IDInteger}, func(w *codec.Writer) { w.WriteSLEB128(-5) }))
				w.WriteField(101, func(w *codec.Writer) { w.WriteString("label") })
			})
			w.WriteObject(func(w *codec.Writer) { w.WriteField(100, val(&Type{ID: IDVarchar}, nil)) })
		})
		w.WriteField(101, func(w *codec.Writer) { // properties
			w.WriteULEB128(uint64(len(props)))
			for i, p := range props {
				w.WriteObject(func(w *codec.Writer) {
					w.WriteField(0, func(w *codec.Writer) { w.WriteString(string(rune('a' + i))) })
					w.WriteField(1, p)
				})
			}
		})
	})

	typ := structOf(ChildType{Name: "count", Type: Type{ID: IDBigint}})
	typ.Info.Alias = "AGGREGATE_STATE"
	typ.Info.ExtensionInfo = ext.Bytes()
	got := roundTrip(t, typ)
	if got.Info.Alias != "AGGREGATE_STATE" || !bytes.Equal(got.Info.ExtensionInfo, ext.Bytes()) || got.Info.Children[0].Name != "count" {
		t.Fatalf("decoded %+v", got.Info)
	}
	raw := encodeBytes(typ)
	for n := range raw {
		if _, err := Decode(codec.NewReader(raw[:n])); err == nil {
			t.Fatalf("truncated to %d bytes: no error", n)
		}
	}
}
