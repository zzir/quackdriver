package quackdriver

import (
	"testing"

	"github.com/zzir/quackdriver/internal/logicaltype"
)

func TestParseTypeSTRUCT(t *testing.T) {
	got, err := parseAppendType("STRUCT(id INTEGER, name VARCHAR, scores DOUBLE[])")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != logicaltype.IDStruct {
		t.Fatalf("id: %d", got.ID)
	}
	if got.Info == nil || len(got.Info.Children) != 3 {
		t.Fatalf("children: %+v", got.Info)
	}
	if got.Info.Children[0].Name != "id" || got.Info.Children[0].Type.ID != logicaltype.IDInteger {
		t.Fatalf("child 0: %+v", got.Info.Children[0])
	}
	if got.Info.Children[2].Type.ID != logicaltype.IDList ||
		got.Info.Children[2].Type.Info.Child.ID != logicaltype.IDDouble {
		t.Fatalf("child 2: %+v", got.Info.Children[2])
	}
}

func TestParseTypeNestedDecimal(t *testing.T) {
	got, err := parseAppendType("STRUCT(amt DECIMAL(18,4), label VARCHAR)")
	if err != nil {
		t.Fatal(err)
	}
	if got.Info.Children[0].Type.ID != logicaltype.IDDecimal {
		t.Fatalf("decimal child: %+v", got.Info.Children[0])
	}
	if got.Info.Children[0].Type.Info.Width != 18 || got.Info.Children[0].Type.Info.Scale != 4 {
		t.Fatalf("decimal w/s: %+v", got.Info.Children[0].Type.Info)
	}
}

func TestParseTypeMAP(t *testing.T) {
	got, err := parseAppendType("MAP(VARCHAR, INTEGER)")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != logicaltype.IDMap {
		t.Fatalf("id: %d", got.ID)
	}
	// MAP child is STRUCT<key,value>.
	entry := got.Info.Child
	if entry == nil || entry.ID != logicaltype.IDStruct {
		t.Fatalf("entry: %+v", entry)
	}
	if entry.Info.Children[0].Name != "key" || entry.Info.Children[0].Type.ID != logicaltype.IDVarchar {
		t.Fatalf("key: %+v", entry.Info.Children[0])
	}
	if entry.Info.Children[1].Name != "value" || entry.Info.Children[1].Type.ID != logicaltype.IDInteger {
		t.Fatalf("value: %+v", entry.Info.Children[1])
	}
}

func TestParseTypeListOfStruct(t *testing.T) {
	got, err := parseAppendType("STRUCT(a INTEGER)[]")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != logicaltype.IDList {
		t.Fatalf("outer id: %d", got.ID)
	}
	if got.Info.Child.ID != logicaltype.IDStruct {
		t.Fatalf("inner id: %d", got.Info.Child.ID)
	}
}

func TestParseTypeArrayOfList(t *testing.T) {
	got, err := parseAppendType("INTEGER[][3]")
	if err != nil {
		t.Fatal(err)
	}
	// outer ARRAY[3] of LIST<INTEGER>
	if got.ID != logicaltype.IDArray || got.Info.ArraySize != 3 {
		t.Fatalf("array: %+v", got)
	}
	if got.Info.Child.ID != logicaltype.IDList {
		t.Fatalf("inner: %d", got.Info.Child.ID)
	}
	if got.Info.Child.Info.Child.ID != logicaltype.IDInteger {
		t.Fatalf("innermost: %d", got.Info.Child.Info.Child.ID)
	}
}

func TestParseTypeErrors(t *testing.T) {
	cases := []string{
		"",
		"STRUCT(",
		"STRUCT(a)",
		"DECIMAL(9)",
		"DECIMAL(50, 2)",
		"INTEGER[abc]",
		"NOTHING",
		"STRUCT(a INTEGER,)",
	}
	for _, s := range cases {
		if _, err := parseAppendType(s); err == nil {
			t.Errorf("expected error for %q", s)
		}
	}
}

func TestParseTypeUNION(t *testing.T) {
	got, err := parseAppendType("UNION(num INTEGER, str VARCHAR)")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != logicaltype.IDUnion {
		t.Fatalf("id: %d", got.ID)
	}
	// 3 children: the hidden tag at index 0, then num, then str.
	if got.Info == nil || len(got.Info.Children) != 3 {
		t.Fatalf("children: %+v", got.Info)
	}
	if got.Info.Children[0].Name != "" || got.Info.Children[0].Type.ID != logicaltype.IDUTinyint {
		t.Fatalf("tag child: %+v", got.Info.Children[0])
	}
	if got.Info.Children[1].Name != "num" || got.Info.Children[2].Name != "str" {
		t.Fatalf("variant order: %+v", got.Info.Children)
	}
}

func TestConvertAppendValueUNION(t *testing.T) {
	ut, _ := parseAppendType("UNION(num INTEGER, str VARCHAR)")
	got, err := convertAppendValue(ut, map[string]any{"str": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m[""] != uint8(1) {
		t.Fatalf("tag: %v want 1", m[""])
	}
	if m["str"] != "hi" {
		t.Fatalf("str: %v", m["str"])
	}
	if m["num"] != nil {
		t.Fatalf("num should be nil, got %v", m["num"])
	}

	if _, err := convertAppendValue(ut, map[string]any{}); err == nil {
		t.Fatal("expected error for empty UNION map")
	}
	if _, err := convertAppendValue(ut, map[string]any{"num": 1, "str": "x"}); err == nil {
		t.Fatal("expected error for multiple non-nil variants")
	}
}

func TestParseTypeENUM(t *testing.T) {
	got, err := parseAppendType("ENUM('sad', 'ok', 'happy')")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != logicaltype.IDEnum {
		t.Fatalf("id: %d", got.ID)
	}
	if got.Info == nil || len(got.Info.EnumValues) != 3 {
		t.Fatalf("values: %+v", got.Info)
	}
	if got.Info.EnumValues[2] != "happy" {
		t.Fatalf("third value: %q", got.Info.EnumValues[2])
	}
}

func TestParseTypeENUMQuoteEscape(t *testing.T) {
	// Single-quote literal inside an enum label uses '' SQL-doubling.
	got, err := parseAppendType("ENUM('it''s ok')")
	if err != nil {
		t.Fatal(err)
	}
	if got.Info.EnumValues[0] != "it's ok" {
		t.Fatalf("unescaped: %q", got.Info.EnumValues[0])
	}
}

func TestParseTypeENUMErrors(t *testing.T) {
	cases := []string{
		"ENUM()",       // empty
		"ENUM('a',)",   // trailing comma
		"ENUM(a, b)",   // bareword instead of string
		"ENUM('unterminated",
	}
	for _, s := range cases {
		if _, err := parseAppendType(s); err == nil {
			t.Errorf("expected error for %q", s)
		}
	}
}

// Regression: lone-quote input used to panic with index-out-of-range because
// the unterminated-string check indexed out[len(out)-1] without first
// verifying out was non-empty.
func TestTokenizeLoneQuoteDoesNotPanic(t *testing.T) {
	cases := []string{
		"'",
		"'foo",
		"  '  ",
	}
	for _, s := range cases {
		if _, err := parseAppendType(s); err == nil {
			t.Errorf("expected error for %q", s)
		}
	}
}

func TestConvertAppendValueENUM(t *testing.T) {
	ut, _ := parseAppendType("ENUM('a', 'b', 'c')")
	got, err := convertAppendValue(ut, "b")
	if err != nil {
		t.Fatal(err)
	}
	if got != uint8(1) {
		t.Fatalf("label 'b' → uint8(1), got %T %v", got, got)
	}
	got, err = convertAppendValue(ut, uint8(2))
	if err != nil {
		t.Fatal(err)
	}
	if got != uint8(2) {
		t.Fatalf("index 2 round-trip: %v", got)
	}
	if _, err := convertAppendValue(ut, "z"); err == nil {
		t.Fatal("unknown label should be rejected")
	}
	if _, err := convertAppendValue(ut, uint8(99)); err == nil {
		t.Fatal("out-of-range index should be rejected")
	}
}

func TestTokenizeWhitespace(t *testing.T) {
	got, err := parseAppendType("  STRUCT( a   INTEGER ,  b VARCHAR )  ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Info.Children[0].Name != "a" || got.Info.Children[1].Name != "b" {
		t.Fatalf("fields: %+v", got.Info.Children)
	}
}
