package quackdriver

import (
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/value"
)

func tp(id logicaltype.ID) logicaltype.Type { return logicaltype.Type{ID: id} }

func listOf(child logicaltype.Type) logicaltype.Type {
	return logicaltype.Type{ID: logicaltype.IDList, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraList, Child: &child}}
}

func arrayOf(child logicaltype.Type, n uint32) logicaltype.Type {
	return logicaltype.Type{ID: logicaltype.IDArray, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraArray, Child: &child, ArraySize: n}}
}

func structOf(id logicaltype.ID, children ...logicaltype.ChildType) logicaltype.Type {
	return logicaltype.Type{ID: id, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraStruct, Children: children}}
}

func mapOf(k, v logicaltype.Type) logicaltype.Type {
	entry := structOf(logicaltype.IDStruct, logicaltype.ChildType{Name: "key", Type: k}, logicaltype.ChildType{Name: "value", Type: v})
	return logicaltype.Type{ID: logicaltype.IDMap, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraList, Child: &entry}}
}

func decimalOf(width, scale uint8) logicaltype.Type {
	return logicaltype.Type{ID: logicaltype.IDDecimal, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraDecimal, Width: width, Scale: scale}}
}

func enumOf(labels ...string) logicaltype.Type {
	return logicaltype.Type{ID: logicaltype.IDEnum, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraEnum, EnumValues: labels}}
}

func child(name string, t logicaltype.Type) logicaltype.ChildType {
	return logicaltype.ChildType{Name: name, Type: t}
}

// TestAppendText: every accepted Go value renders as the text DuckDB's cast
// from VARCHAR reads as the intended value (checked against a real server
// by integration/append).
func TestAppendText(t *testing.T) {
	ts := time.Date(2026, 5, 25, 12, 34, 56, 789123456, time.UTC)
	tz := time.FixedZone("x", -(3*3600 + 30*60))
	ttz, _ := value.NewTimeTz(10*time.Hour+30*time.Minute, -(3*3600 + 30*60))
	huge, _ := new(big.Int).SetString("-170141183460469231731687303715884105728", 10)
	st := structOf(logicaltype.IDStruct, child("a", tp(logicaltype.IDInteger)), child("b c", tp(logicaltype.IDVarchar)))
	for _, c := range []struct {
		typ  logicaltype.Type
		in   any
		want string
	}{
		{tp(logicaltype.IDBoolean), true, "true"},
		{tp(logicaltype.IDTinyint), int8(-5), "-5"},
		{tp(logicaltype.IDBigint), int64(math.MinInt64), "-9223372036854775808"},
		{tp(logicaltype.IDUBigint), uint64(math.MaxUint64), "18446744073709551615"},
		{tp(logicaltype.IDHugeint), huge, huge.String()},
		{tp(logicaltype.IDHugeint), *big.NewInt(7), "7"},
		{tp(logicaltype.IDUHugeint), "340282366920938463463374607431768211455", "340282366920938463463374607431768211455"},
		{tp(logicaltype.IDBignum), -1, "-1"},
		{tp(logicaltype.IDFloat), 1.5, "1.5"},
		{tp(logicaltype.IDDouble), float32(0.1), "0.10000000149011612"},
		{tp(logicaltype.IDDouble), math.NaN(), "nan"},
		{tp(logicaltype.IDDouble), math.Inf(-1), "-inf"},
		{tp(logicaltype.IDDouble), math.Copysign(0, -1), "-0"},
		{tp(logicaltype.IDDouble), 3, "3"},
		{decimalOf(10, 2), "12.345", "12.345"}, // DuckDB rounds
		{decimalOf(10, 2), 5, "5"},
		{decimalOf(10, 2), big.NewInt(-1234), "-12.34"},
		{decimalOf(10, 2), 2.675, "2.68"}, // like DuckDB's DOUBLE cast: 2.675 * 100 is 267.5 in double arithmetic
		{decimalOf(10, 2), float32(0.125), "0.13"},
		{tp(logicaltype.IDVarchar), "it's", "it's"},
		{tp(logicaltype.IDVarchar), []byte("bytes"), "bytes"},
		{tp(logicaltype.IDBlob), []byte{0, 'a', '\\', 0xff}, `\x00a\x5C\xFF`},
		{tp(logicaltype.IDBlob), "ok'", "ok'"},
		{tp(logicaltype.IDBit), "0101", "0101"},
		{tp(logicaltype.IDUUID), "A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11", "A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11"},
		{tp(logicaltype.IDDate), ts.In(tz), "2026-05-25"},
		{tp(logicaltype.IDDate), time.Date(-43, 3, 15, 0, 0, 0, 0, time.UTC), "0044-03-15 (BC)"},
		{tp(logicaltype.IDDate), time.Date(12345, 1, 2, 0, 0, 0, 0, time.UTC), "12345-01-02"},
		{tp(logicaltype.IDDate), value.InfinityTime, "infinity"},
		{tp(logicaltype.IDTime), 13*time.Hour + 1500*time.Nanosecond, "13:00:00.000001500"},
		{tp(logicaltype.IDTimeNs), 24 * time.Hour, "24:00:00.000000000"},
		{tp(logicaltype.IDTime), ts.In(tz), "12:34:56.789123456"},
		{tp(logicaltype.IDTimeTz), ttz, "10:30:00-03:30"},
		{tp(logicaltype.IDTimeTz), time.Date(2000, 1, 1, 10, 30, 0, 0, tz), "10:30:00-03:30"},
		{tp(logicaltype.IDTimestamp), ts, "2026-05-25 12:34:56.789123000"},
		{tp(logicaltype.IDTimestampSec), ts, "2026-05-25 12:34:57.000000000"},
		{tp(logicaltype.IDTimestampMs), ts, "2026-05-25 12:34:56.789000000"},
		{tp(logicaltype.IDTimestampNs), ts, "2026-05-25 12:34:56.789123456"},
		{tp(logicaltype.IDTimestampMs), time.Unix(0, -1500000), "1969-12-31 23:59:59.998000000"}, // halves away from the epoch
		{tp(logicaltype.IDTimestampTz), ts.In(tz), "2026-05-25 12:34:56.789123000+00"},
		{tp(logicaltype.IDTimestampTzNs), ts.In(tz), "2026-05-25 12:34:56.789123456+00"},
		{tp(logicaltype.IDTimestamp), time.Date(-43, 3, 15, 12, 0, 0, 0, time.UTC), "0044-03-15 (BC) 12:00:00.000000000"},
		{tp(logicaltype.IDTimestamp), value.NegInfinityTime, "-infinity"},
		{tp(logicaltype.IDInterval), value.IntervalValue{Months: 1, Days: -2, Micros: 3}, "1 months -2 days 3 microseconds"},
		{tp(logicaltype.IDInterval), 90 * time.Minute, "5400000000 microseconds"},
		{enumOf("a", "it's"), "it's", "it's"},
		{enumOf("a", "it's"), uint8(1), "it's"},
		// Nested values are quoted, with backslash escapes.
		{listOf(tp(logicaltype.IDVarchar)), []any{"it's", `a\b`, nil, "NULL", ""}, `['it\'s', 'a\\b', NULL, 'NULL', '']`},
		{listOf(tp(logicaltype.IDBlob)), []any{[]byte{0, '\\'}}, `['\\x00\\x5C']`},
		{arrayOf(tp(logicaltype.IDInteger), 2), []any{1, nil}, `['1', NULL]`},
		{listOf(listOf(tp(logicaltype.IDInteger))), []any{[]any{1}, nil, []any{}}, `[['1'], NULL, []]`},
		{st, map[string]any{"a": 1}, `{'a': '1', 'b c': NULL}`},
		{listOf(st), []any{map[string]any{"b c": "x'y"}}, `[{'a': NULL, 'b c': 'x\'y'}]`},
		{structOf(logicaltype.IDTuple, child("", tp(logicaltype.IDInteger)), child("", tp(logicaltype.IDVarchar))), []any{1, "t"}, `('1', 't')`},
		{mapOf(tp(logicaltype.IDVarchar), tp(logicaltype.IDInteger)), map[string]any{"k2": 2, "k'1": nil}, `{'k\'1'=NULL, 'k2'='2'}`},
		{mapOf(tp(logicaltype.IDInteger), tp(logicaltype.IDVarchar)), []any{map[string]any{"key": 3, "value": "c"}}, `{'3'='c'}`},
	} {
		got, err := appendText(c.typ, c.in)
		if err != nil || got != c.want {
			t.Errorf("%s %#v: %#v, %v; want %q", c.typ.SQL(), c.in, got, err, c.want)
		}
	}
	for _, null := range []any{nil, []byte(nil)} {
		if got, err := appendText(tp(logicaltype.IDBlob), null); got != nil || err != nil {
			t.Errorf("%#v: %#v, %v; want NULL", null, got, err)
		}
	}
}

// TestAppendTextErrors: values a column does not accept are rejected
// client-side; ranges and formats are left to the server's casts.
func TestAppendTextErrors(t *testing.T) {
	for _, c := range []struct {
		typ  logicaltype.Type
		in   any
		want string
	}{
		{tp(logicaltype.IDBoolean), "true", "BOOLEAN expects bool, got string"},
		{tp(logicaltype.IDVarchar), 3, "VARCHAR expects string, got int"},
		{tp(logicaltype.IDInteger), 1.5, "INTEGER expects a Go integer, got float64"},
		{tp(logicaltype.IDVarchar), "\xff", "not valid UTF-8"},
		{tp(logicaltype.IDFloat), 1e39, "out of range"},
		{tp(logicaltype.IDTime), -time.Second, "out of range"},
		{tp(logicaltype.IDTimeTz), value.TimeTzFromBits(math.MaxUint64), "TIME"},
		{decimalOf(10, 2), math.Inf(1), "DECIMAL cannot hold"},
		{tp(logicaltype.IDHugeint), (*big.Int)(nil), "HUGEINT expects"},
		{tp(logicaltype.IDTimestamp), time.Date(300000, 1, 1, 0, 0, 0, 0, time.UTC), "out of range"},
		{enumOf("a"), 1, "out of range"},
		{listOf(tp(logicaltype.IDInteger)), []any{1, "x"}, "element 1: INTEGER expects"},
		{structOf(logicaltype.IDStruct, child("a", tp(logicaltype.IDInteger))), map[string]any{"b": 1}, `STRUCT has no field "b"`},
		{structOf(logicaltype.IDTuple, child("", tp(logicaltype.IDInteger))), []any{1, 2}, "TUPLE expects 1 values"},
		{mapOf(tp(logicaltype.IDInteger), tp(logicaltype.IDInteger)), []any{map[string]any{"k": 1}}, "MAP element 0"},
		{mapOf(tp(logicaltype.IDInteger), tp(logicaltype.IDInteger)), 1, "MAP expects"},
	} {
		if got, err := appendText(c.typ, c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %#v: %#v, %v; want an error with %q", c.typ.SQL(), c.in, got, err, c.want)
		}
	}
}

// TestCheckTypeSyntax: a column type cannot end or extend the statement
// that resolves it.
func TestCheckTypeSyntax(t *testing.T) {
	for _, s := range []string{
		"INTEGER", "DECIMAL(10, 2)", "TIMESTAMP WITH TIME ZONE", "INTEGER[3][]", "main.mood",
		`STRUCT("a;b" INTEGER, "x""y" VARCHAR[])`, "ENUM('x;y', 'it''s', '--', '$$')", "MAP(VARCHAR, INTEGER)",
	} {
		if err := checkTypeSyntax(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{
		"", " ", "INTEGER; DROP TABLE t", "INTEGER -- x", "INTEGER /* x */", "$$x$$", "INTEGER\x00",
		`ENUM(E'\'', '; DROP TABLE t; --')`, "ENUM('open", `STRUCT("open INTEGER)`,
	} {
		if err := checkTypeSyntax(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

// TestCheckAppendType: types whose values cannot travel as text are
// refused, also nested.
func TestCheckAppendType(t *testing.T) {
	union := structOf(logicaltype.IDUnion, child("", tp(logicaltype.IDUTinyint)), child("a", tp(logicaltype.IDInteger)))
	for _, typ := range []logicaltype.Type{
		union, listOf(union), structOf(logicaltype.IDStruct, child("u", union)), mapOf(tp(logicaltype.IDVarchar), union),
		tp(logicaltype.IDVariant), tp(logicaltype.IDGeometry), tp(logicaltype.IDSQLNull), listOf(tp(logicaltype.IDVariant)),
	} {
		if err := checkAppendType(typ); err == nil {
			t.Errorf("%s accepted", typ.SQL())
		}
	}
	for _, typ := range []logicaltype.Type{
		tp(logicaltype.IDInteger), decimalOf(38, 10), enumOf("a"), arrayOf(listOf(tp(logicaltype.IDBlob)), 2),
		mapOf(tp(logicaltype.IDVarchar), structOf(logicaltype.IDTuple, child("", tp(logicaltype.IDTimeTz)))),
	} {
		if err := checkAppendType(typ); err != nil {
			t.Errorf("%s: %v", typ.SQL(), err)
		}
	}
}
