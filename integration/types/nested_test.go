//go:build integration

package types_test

import (
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/value"
)

// nestedElementExprs are scalar expressions placed inside LIST, ARRAY,
// STRUCT and MAP values; each element is compared with the server's
// rendering of that element.
var nestedElementExprs = []string{
	"true", "(-128)::TINYINT", "(-2147483648)::INTEGER", "9223372036854775807::BIGINT", "18446744073709551615::UBIGINT",
	"(-170141183460469231731687303715884105728)::HUGEINT", "340282366920938463463374607431768211455::UHUGEINT",
	"'nan'::DOUBLE", "'-inf'::FLOAT", "0.1::DOUBLE",
	"(-1.5)::DECIMAL(4,1)", "(-1.5)::DECIMAL(9,1)", "(-1.5)::DECIMAL(18,1)", "(-1.5)::DECIMAL(38,1)",
	"''", "'ünïcødé 漢字'", "repeat('long', 10)", "from_hex('00FF5C')", "'101'::BIT", "'-123456789012345678901234567890'::BIGNUM",
	"'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::UUID", "'b'::ENUM('a', 'b')",
	"DATE '0001-01-01 (BC)'", "'infinity'::DATE", "TIMESTAMP '2024-01-02 03:04:05.678901'", "'-infinity'::TIMESTAMP",
	"'2024-01-02 03:04:05'::TIMESTAMP_S", "'2024-01-02 03:04:05.123'::TIMESTAMP_MS", "'2024-01-02 03:04:05.123456789'::TIMESTAMP_NS",
	"TIMESTAMPTZ '2024-01-02 03:04:05+02'", "'2024-01-02 03:04:05.123456789+00'::TIMESTAMPTZ_NS",
	"TIME '12:34:56.5'", "'12:34:56.123456789'::TIME_NS", "TIMETZ '12:34:56-07:30'", "INTERVAL '-1 year 2 days -00:00:00.5'",
	"'{\"k\": [1, 2]}'::JSON",
}

// TestNestedElementsRendered checks every element type inside LIST and
// ARRAY vectors (with NULL elements and NULL rows) against the server's
// rendering of each element.
func TestNestedElementsRendered(t *testing.T) {
	e := newEnv(t)
	e.exec(t, "SET TimeZone = 'UTC'")
	for _, el := range nestedElementExprs {
		for _, list := range []string{
			"[" + el + ", NULL, " + el + "]",
			"array_value(" + el + ", NULL, " + el + ")",
			"CASE WHEN i % 3 = 1 THEN NULL WHEN i % 3 = 2 THEN [] ELSE [" + el + ", NULL] END",
		} {
			q := fmt.Sprintf(`SELECT x, typeof(x), list_transform(x, lambda e: e::VARCHAR)
				FROM (SELECT %s AS x FROM range(5) t(i))`, list)
			for _, row := range e.rows(t, q) {
				if row[0] == nil {
					if row[2] != nil {
						t.Errorf("%s: NULL row decoded, server has %v", list, row[2])
					}
					continue
				}
				got, ok := row[0].([]any)
				want, ok2 := row[2].([]any)
				if !ok || !ok2 || len(got) != len(want) {
					t.Errorf("%s: decoded %#v, server elements %#v", list, row[0], row[2])
					continue
				}
				typ := row[1].(string)
				elemType := typ[:strings.LastIndex(typ, "[")]
				for i := range got {
					checkText(t, list, elemType, got[i], want[i])
				}
			}
		}
	}
}

func bigInt(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic(s)
	}
	return v
}

// TestNestedGoValues pins the documented Go shapes of STRUCT, TUPLE, MAP,
// UNION, LIST and ARRAY values, nested in each other and with NULLs at every
// level.
func TestNestedGoValues(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		expr string
		want any
	}{
		{"[1, NULL, 3]", []any{int32(1), nil, int32(3)}},
		{"[]::INTEGER[]", []any{}},
		{"[[1], [], NULL, [NULL]]", []any{[]any{int32(1)}, []any{}, nil, []any{nil}}},
		{"[[[['deep']]]]", []any{[]any{[]any{[]any{"deep"}}}}},
		{"[1, 2]::BIGINT[2]", []any{int64(1), int64(2)}},
		{"[[1, 2], NULL, [3, NULL]]::INTEGER[2][3]", []any{[]any{int32(1), int32(2)}, nil, []any{int32(3), nil}}},
		{"[[1, 2], [3]]::INTEGER[][2]", []any{[]any{int32(1), int32(2)}, []any{int32(3)}}},
		{"{'a': 1, 'b': 'x', 'c': NULL}", map[string]any{"a": int32(1), "b": "x", "c": nil}},
		{`{'weird "name"': 1, 'with space': 2}`, map[string]any{`weird "name"`: int32(1), "with space": int32(2)}},
		{"{'s': {'t': [1, 2]}, 'l': [{'u': true}, NULL]}", map[string]any{
			"s": map[string]any{"t": []any{int32(1), int32(2)}},
			"l": []any{map[string]any{"u": true}, nil},
		}},
		{"row(1, 'x', NULL)", []any{int32(1), "x", nil}},
		{"(1, (2.5::DOUBLE, [true]))", []any{int32(1), []any{2.5, []any{true}}}},
		{"[(1, 'a'), (2, 'b')]", []any{[]any{int32(1), "a"}, []any{int32(2), "b"}}},
		{"{'t': (1, 2)}", map[string]any{"t": []any{int32(1), int32(2)}}},
		{"MAP {'k1': 1, 'k2': NULL}", []any{
			map[string]any{"key": "k1", "value": int32(1)},
			map[string]any{"key": "k2", "value": nil},
		}},
		{"MAP {}::MAP(VARCHAR, INTEGER)", []any{}},
		{"MAP {[1, 2]: {'a': DATE '2024-01-01'}}", []any{
			map[string]any{"key": []any{int32(1), int32(2)}, "value": map[string]any{"a": time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}},
		}},
		{"MAP {1: MAP {'x': [1.5::DOUBLE]}}", []any{
			map[string]any{"key": int32(1), "value": []any{map[string]any{"key": "x", "value": []any{1.5}}}},
		}},
		{"union_value(n := 1)::UNION(n INTEGER, s VARCHAR)", map[string]any{"": uint8(0), "n": int32(1), "s": nil}},
		{"union_value(s := 'x')::UNION(n INTEGER, s VARCHAR)", map[string]any{"": uint8(1), "n": nil, "s": "x"}},
		{"union_value(s := NULL::VARCHAR)::UNION(n INTEGER, s VARCHAR)", map[string]any{"": uint8(1), "n": nil, "s": nil}},
		{"[union_value(a := [1])::UNION(a INTEGER[], b STRUCT(c INTEGER)), union_value(b := {'c': 2})::UNION(a INTEGER[], b STRUCT(c INTEGER))]",
			[]any{
				map[string]any{"": uint8(0), "a": []any{int32(1)}, "b": nil},
				map[string]any{"": uint8(1), "a": nil, "b": map[string]any{"c": int32(2)}},
			}},
		{"{'h': 170141183460469231731687303715884105727::HUGEINT, 'i': INTERVAL 1 DAY, 'z': TIMETZ '01:00:00+01'}",
			map[string]any{"h": bigInt("170141183460469231731687303715884105727"), "i": value.IntervalValue{Days: 1},
				"z": mustTimeTz(time.Hour, 3600)}},
		{"NULL::INTEGER[]", nil},
		{"NULL::STRUCT(a INTEGER)", nil},
		{"NULL::MAP(INTEGER, INTEGER)", nil},
		{"NULL::INTEGER[2]", nil},
		{"NULL", nil},
	}
	for _, c := range cases {
		got := e.one(t, "SELECT "+c.expr)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %#v\nwant %#v", c.expr, got, c.want)
		}
		// The same value as a FLAT vector with a NULL row before it.
		rows := e.rows(t, "SELECT CASE WHEN i = 1 THEN "+c.expr+" END FROM range(2) t(i) ORDER BY i")
		if rows[0][0] != nil || !reflect.DeepEqual(rows[1][0], c.want) {
			t.Errorf("%s (flat): got %#v, %#v", c.expr, rows[0][0], rows[1][0])
		}
	}
}

func mustTimeTz(tod time.Duration, off int32) value.TimeTzValue {
	v, err := value.NewTimeTz(tod, off)
	if err != nil {
		panic(err)
	}
	return v
}

// TestNestedNullPatterns checks NULLs inside struct children, list elements
// and map values across many rows (validity masks of more than one 64-row
// word, at every nesting level).
func TestNestedNullPatterns(t *testing.T) {
	e := newEnv(t)
	rows := e.rows(t, `SELECT i,
			CASE WHEN i % 7 = 0 THEN NULL ELSE {'a': CASE WHEN i % 3 = 0 THEN NULL ELSE i END,
				'b': CASE WHEN i % 5 = 0 THEN NULL ELSE [i, NULL, i::VARCHAR::INTEGER] END} END,
			CASE WHEN i % 11 = 0 THEN NULL ELSE MAP {i::VARCHAR: CASE WHEN i % 2 = 0 THEN NULL ELSE i * 2 END} END
		FROM range(3000) t(i) ORDER BY i`)
	if len(rows) != 3000 {
		t.Fatalf("got %d rows", len(rows))
	}
	for _, r := range rows {
		i := r[0].(int64)
		var wantS any
		if i%7 != 0 {
			var a any = i
			if i%3 == 0 {
				a = nil
			}
			var b any = []any{int32(i), nil, int32(i)}
			if i%5 == 0 {
				b = nil
			}
			// i is BIGINT, so the list holds the common supertype BIGINT.
			if b != nil {
				b = []any{i, nil, i}
			}
			wantS = map[string]any{"a": a, "b": b}
		}
		if !reflect.DeepEqual(r[1], wantS) {
			t.Fatalf("row %d struct: got %#v, want %#v", i, r[1], wantS)
		}
		var wantM any
		if i%11 != 0 {
			var v any = i * 2
			if i%2 == 0 {
				v = nil
			}
			wantM = []any{map[string]any{"key": fmt.Sprint(i), "value": v}}
		}
		if !reflect.DeepEqual(r[2], wantM) {
			t.Fatalf("row %d map: got %#v, want %#v", i, r[2], wantM)
		}
	}
}

// TestLargeNested decodes nested values whose child vectors are far larger
// than a 2048-row chunk, very wide structs and deeply nested values.
func TestLargeNested(t *testing.T) {
	e := newEnv(t)
	// One row holding a 300000-element list, and lists of lists.
	l := e.one(t, "SELECT range(300000)").([]any)
	if len(l) != 300000 || l[0] != int64(0) || l[299999] != int64(299999) {
		t.Errorf("large list: len %d", len(l))
	}
	rows := e.rows(t, "SELECT i, list(j::VARCHAR ORDER BY j) FROM range(50) t(i), range(5000) u(j) WHERE j < i * 100 GROUP BY i ORDER BY i")
	for _, r := range rows {
		i := r[0].(int64)
		got := r[1].([]any)
		if int64(len(got)) != i*100 {
			t.Fatalf("row %d: %d elements", i, len(got))
		}
		for j, v := range got {
			if v != fmt.Sprint(j) {
				t.Fatalf("row %d element %d: %#v", i, j, v)
			}
		}
	}
	// A large fixed-size ARRAY, many rows.
	rows = e.rows(t, "SELECT i, list_transform(range(1000), lambda x: x + i)::BIGINT[1000] FROM range(3000) t(i) ORDER BY i")
	for _, r := range rows {
		i := r[0].(int64)
		a := r[1].([]any)
		if len(a) != 1000 || a[0] != i || a[999] != i+999 {
			t.Fatalf("array row %d: len %d", i, len(a))
		}
	}
	// A MAP with many entries.
	m := e.one(t, "SELECT map_from_entries(list({'k': i::VARCHAR, 'v': i})) FROM range(10000) t(i)").([]any)
	if len(m) != 10000 {
		t.Fatalf("map: %d entries", len(m))
	}
	seen := map[string]bool{}
	for _, entry := range m {
		kv := entry.(map[string]any)
		if kv["key"] != fmt.Sprint(kv["value"]) {
			t.Fatalf("map entry %#v", kv)
		}
		seen[kv["key"].(string)] = true
	}
	if len(seen) != 10000 {
		t.Errorf("map keys: %d distinct", len(seen))
	}
	// A struct with 500 fields.
	var fields []string
	for i := range 500 {
		fields = append(fields, fmt.Sprintf("'f%d': %d", i, i))
	}
	s := e.one(t, "SELECT {"+strings.Join(fields, ", ")+"}").(map[string]any)
	if len(s) != 500 || s["f0"] != int32(0) || s["f499"] != int32(499) {
		t.Errorf("wide struct: %d fields", len(s))
	}
	// Deep nesting: 60 levels of LIST and of STRUCT.
	expr := "1"
	for range 60 {
		expr = "[" + expr + "]"
	}
	v := e.one(t, "SELECT "+expr)
	for d := range 60 {
		lst, ok := v.([]any)
		if !ok || len(lst) != 1 {
			t.Fatalf("depth %d: %#v", d, v)
		}
		v = lst[0]
	}
	if v != int32(1) {
		t.Errorf("deep list leaf: %#v", v)
	}
	expr = "1"
	for range 60 {
		expr = "{'x': " + expr + "}"
	}
	v = e.one(t, "SELECT "+expr)
	for range 60 {
		v = v.(map[string]any)["x"]
	}
	if v != int32(1) {
		t.Errorf("deep struct leaf: %#v", v)
	}
}
