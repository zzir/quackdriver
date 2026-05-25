package logicaltype

import (
	"bytes"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
)

func roundTrip(t *testing.T, in Type) Type {
	t.Helper()
	w := codec.NewWriter()
	Encode(w, in)
	enc := w.Bytes()

	r := codec.NewReader(enc)
	out, err := Decode(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := r.AssertEOF(); err != nil {
		t.Fatalf("trailing bytes: %v", err)
	}

	// Re-encode and ensure byte equality.
	w2 := codec.NewWriter()
	Encode(w2, out)
	if !bytes.Equal(enc, w2.Bytes()) {
		t.Fatalf("re-encode differs:\n  first:  %x\n  second: %x", enc, w2.Bytes())
	}
	return out
}

func TestRoundTripScalars(t *testing.T) {
	cases := []Type{
		{ID: IDBoolean},
		{ID: IDInteger},
		{ID: IDBigint},
		{ID: IDDouble},
		{ID: IDVarchar},
		{ID: IDDate},
		{ID: IDTimestamp},
		{ID: IDInterval},
		{ID: IDUUID},
		{ID: IDHugeint},
	}
	for _, c := range cases {
		got := roundTrip(t, c)
		if got.ID != c.ID {
			t.Fatalf("id mismatch: %v vs %v", got.ID, c.ID)
		}
		if got.Info != nil {
			t.Fatalf("expected no ExtraInfo for %s, got %+v", c.Name(), got.Info)
		}
	}
}

func TestRoundTripDecimal(t *testing.T) {
	in := Type{ID: IDDecimal, Info: &ExtraInfo{Kind: ExtraDecimal, Width: 18, Scale: 4}}
	out := roundTrip(t, in)
	if out.Info == nil || out.Info.Kind != ExtraDecimal || out.Info.Width != 18 || out.Info.Scale != 4 {
		t.Fatalf("decimal mismatch: %+v", out.Info)
	}
	if p := out.PhysicalType(); p != PhysicalInt64 {
		t.Fatalf("decimal(18,4) physical: want INT64, got %d", p)
	}
}

func TestRoundTripList(t *testing.T) {
	child := Type{ID: IDInteger}
	in := Type{ID: IDList, Info: &ExtraInfo{Kind: ExtraList, Child: &child}}
	out := roundTrip(t, in)
	if out.Info == nil || out.Info.Child == nil || out.Info.Child.ID != IDInteger {
		t.Fatalf("list child mismatch: %+v", out.Info)
	}
}

func TestRoundTripStruct(t *testing.T) {
	in := Type{
		ID: IDStruct,
		Info: &ExtraInfo{
			Kind: ExtraStruct,
			Children: []ChildType{
				{Name: "id", Type: Type{ID: IDBigint}},
				{Name: "label", Type: Type{ID: IDVarchar}},
			},
		},
	}
	out := roundTrip(t, in)
	if out.Info == nil || len(out.Info.Children) != 2 {
		t.Fatalf("struct children: %+v", out.Info)
	}
	if out.Info.Children[0].Name != "id" || out.Info.Children[0].Type.ID != IDBigint {
		t.Fatalf("first child: %+v", out.Info.Children[0])
	}
}

func TestPhysicalTypeMapping(t *testing.T) {
	cases := map[ID]PhysicalType{
		IDBoolean:     PhysicalBool,
		IDTinyint:     PhysicalInt8,
		IDInteger:     PhysicalInt32,
		IDBigint:      PhysicalInt64,
		IDUBigint:     PhysicalUint64,
		IDUHugeint:    PhysicalUint128,
		IDHugeint:     PhysicalInt128,
		IDUUID:        PhysicalInt128,
		IDFloat:       PhysicalFloat,
		IDDouble:      PhysicalDouble,
		IDVarchar:     PhysicalVarchar,
		IDBlob:        PhysicalVarchar,
		IDInterval:    PhysicalInterval,
		IDStruct:      PhysicalStruct,
		IDList:        PhysicalList,
		IDArray:       PhysicalArray,
		IDDate:        PhysicalInt32,
		IDTimestamp:   PhysicalInt64,
	}
	for id, want := range cases {
		got := Type{ID: id}.PhysicalType()
		if got != want {
			t.Fatalf("ID %s: want physical %d, got %d", Type{ID: id}.Name(), want, got)
		}
	}
}
