//go:build integration

package appendit

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
)

// typeCase is one column type the Appender supports, with Go values and the
// SQL literals that denote the same values on the server.
type typeCase struct {
	name string
	// typ is the AppendColumn.Type.
	typ string
	// col is the table column type ("" = typ).
	col  string
	vals []any
	lits []string
}

func bigInt(s string) *big.Int {
	b, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic(s)
	}
	return b
}

func mustTimeTz(t *testing.T, tod time.Duration, off int32) quackdriver.TimeTzValue {
	t.Helper()
	v, err := quackdriver.NewTimeTz(tod, off)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func floatLits(bits int, vals ...float64) ([]any, []string) {
	var out []any
	var lits []string
	for _, f := range vals {
		var s string
		switch {
		case math.IsNaN(f):
			s = "'nan'"
		case math.IsInf(f, 1):
			s = "'inf'"
		case math.IsInf(f, -1):
			s = "'-inf'"
		default:
			s = "'" + strconv.FormatFloat(f, 'g', -1, bits) + "'"
		}
		if bits == 32 {
			out = append(out, float32(f))
		} else {
			out = append(out, f)
		}
		lits = append(lits, s)
	}
	return out, lits
}

func typeCases(t *testing.T) []typeCase {
	t.Helper()
	plus14 := time.FixedZone("plus14", 14*3600)
	minus0930 := time.FixedZone("minus0930", -(9*3600 + 30*60))
	f32, f32l := floatLits(32, 1.5, -0.25, math.MaxFloat32, math.SmallestNonzeroFloat32, math.Inf(1), math.Inf(-1), math.NaN(), 0.1)
	f64, f64l := floatLits(64, 0.1, -1e308, math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1), math.NaN(), 1)

	enumLabels := make([]string, 300)
	enumQuoted := make([]string, 300)
	for i := range enumLabels {
		enumLabels[i] = fmt.Sprintf("l%d", i)
		enumQuoted[i] = "'" + enumLabels[i] + "'"
	}
	bigEnum := "ENUM(" + strings.Join(enumQuoted, ", ") + ")"
	hugeLabels := make([]string, 70000)
	for i := range hugeLabels {
		hugeLabels[i] = fmt.Sprintf("'h%d'", i)
	}
	hugeEnum := "ENUM(" + strings.Join(hugeLabels, ", ") + ")"

	hugeMin := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 127))
	hugeMax := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 127), big.NewInt(1))
	uhugeMax := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))

	return []typeCase{
		{name: "boolean", typ: "BOOLEAN", vals: []any{true, false}, lits: []string{"true", "false"}},
		{name: "tinyint", typ: "TINYINT", vals: []any{-128, int8(127), uint8(0)}, lits: []string{"-128", "127", "0"}},
		{name: "smallint", typ: "INT2", vals: []any{math.MinInt16, int16(math.MaxInt16), 7}, lits: []string{"-32768", "32767", "7"}},
		{name: "integer", typ: "INTEGER", vals: []any{int32(math.MinInt32), math.MaxInt32, uint16(9)}, lits: []string{"-2147483648", "2147483647", "9"}},
		{name: "bigint", typ: "BIGINT", vals: []any{int64(math.MinInt64), int64(math.MaxInt64), uint32(5)}, lits: []string{"-9223372036854775808", "9223372036854775807", "5"}},
		{name: "utinyint", typ: "UTINYINT", vals: []any{0, 255}, lits: []string{"0", "255"}},
		{name: "usmallint", typ: "USMALLINT", vals: []any{0, 65535}, lits: []string{"0", "65535"}},
		{name: "uinteger", typ: "UINTEGER", vals: []any{0, uint32(math.MaxUint32)}, lits: []string{"0", "4294967295"}},
		{name: "ubigint", typ: "UBIGINT", vals: []any{0, uint64(math.MaxUint64)}, lits: []string{"0", "18446744073709551615"}},
		{name: "hugeint", typ: "HUGEINT", vals: []any{hugeMin, hugeMax, 0, -1, "123456789012345678901234567890", *big.NewInt(math.MinInt64), uint64(math.MaxUint64)},
			lits: []string{"'" + hugeMin.String() + "'", "'" + hugeMax.String() + "'", "0", "-1", "'123456789012345678901234567890'", "-9223372036854775808", "18446744073709551615"}},
		{name: "uhugeint", typ: "UHUGEINT", vals: []any{0, uhugeMax, new(big.Int).Lsh(big.NewInt(1), 64)},
			lits: []string{"0", "'" + uhugeMax.String() + "'", "'18446744073709551616'"}},
		{name: "float", typ: "FLOAT", vals: f32, lits: f32l},
		{name: "double", typ: "DOUBLE", vals: f64, lits: f64l},
		{name: "decimal4_2", typ: "DECIMAL(4,2)", vals: []any{"12.34", "-99.99", 5, 1.25, "0.5"}, lits: []string{"'12.34'", "'-99.99'", "5", "1.25", "0.5"}},
		{name: "decimal9_3", typ: "NUMERIC(9,3)", vals: []any{"999999.999", "-0.001", big.NewInt(42)}, lits: []string{"'999999.999'", "'-0.001'", "0.042"}},
		{name: "decimal18_4", typ: "DECIMAL(18,4)", vals: []any{"99999999999999.9999", "-99999999999999.9999", "1"}, lits: []string{"'99999999999999.9999'", "'-99999999999999.9999'", "1"}},
		{name: "decimal38_10", typ: "DECIMAL(38,10)", vals: []any{"9999999999999999999999999999.9999999999", "-9999999999999999999999999999.9999999999", "0.0000000001"},
			lits: []string{"'9999999999999999999999999999.9999999999'", "'-9999999999999999999999999999.9999999999'", "'0.0000000001'"}},
		{name: "decimal_default", typ: "DECIMAL", vals: []any{"1.234"}, lits: []string{"'1.234'"}},
		{name: "varchar", typ: "VARCHAR", vals: []any{"", "a", "exactly12chr", "longer than twelve bytes", "üñíçødé 🦆", "it's \"q\"", "line\nbreak", "a\x00b", []byte("bytes")},
			lits: []string{"''", "'a'", "'exactly12chr'", "'longer than twelve bytes'", "'üñíçødé 🦆'", "'it''s \"q\"'", "'line' || chr(10) || 'break'", "'a' || chr(0) || 'b'", "'bytes'"}},
		{name: "text_alias", typ: "TEXT", col: "VARCHAR", vals: []any{"x"}, lits: []string{"'x'"}},
		{name: "json", typ: "JSON", col: "JSON", vals: []any{`{"a":1}`, `[1,2,"x"]`, `"s"`}, lits: []string{`'{"a":1}'`, `'[1,2,"x"]'`, `'"s"'`}},
		{name: "blob", typ: "BLOB", vals: []any{[]byte{0, 1, 2, 255}, []byte{}, "text", make([]byte, 100)},
			lits: []string{`'\x00\x01\x02\xFF'::BLOB`, "''::BLOB", "'text'::BLOB", "repeat('\\x00'::BLOB, 100)"}},
		{name: "bit", typ: "BIT", vals: []any{"1", "0101", "1111000011110000111100001111000011110000", "100000001"},
			lits: []string{"'1'", "'0101'", "'1111000011110000111100001111000011110000'", "'100000001'"}},
		{name: "bignum", typ: "BIGNUM", vals: []any{bigInt("1" + strings.Repeat("0", 50)), bigInt("-" + "1" + strings.Repeat("0", 50)), 0, 42, -1, "-255", "256"},
			lits: []string{"'1" + strings.Repeat("0", 50) + "'", "'-1" + strings.Repeat("0", 50) + "'", "'0'", "'42'", "'-1'", "'-255'", "'256'"}},
		{name: "uuid", typ: "UUID", vals: []any{"00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff", "550E8400-E29B-41D4-A716-446655440000",
			"7fffffff-ffff-ffff-ffff-ffffffffffff", "80000000-0000-0000-0000-000000000000"},
			lits: []string{"'00000000-0000-0000-0000-000000000000'", "'ffffffff-ffff-ffff-ffff-ffffffffffff'", "'550e8400-e29b-41d4-a716-446655440000'",
				"'7fffffff-ffff-ffff-ffff-ffffffffffff'", "'80000000-0000-0000-0000-000000000000'"}},
		{name: "date", typ: "DATE", vals: []any{time.Unix(0, 0), time.Date(2024, 2, 29, 23, 0, 0, 0, time.UTC), time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2024, 1, 1, 5, 0, 0, 0, plus14), time.Unix(-2147483646*86400, 0), time.Unix(2147483646*86400+86399, 0)},
			lits: []string{"'1970-01-01'", "'2024-02-29'", "'0001-01-01'", "'1969-12-31'", "'9999-12-31'", "'0001-01-01 (BC)'", "'2023-12-31'",
				"'5877642-06-25 (BC)'", "'5881580-07-10'"}},
		{name: "time", typ: "TIME", vals: []any{time.Duration(0), 24 * time.Hour, 12*time.Hour + 34*time.Minute + 56*time.Second + 789*time.Millisecond,
			23*time.Hour + 59*time.Minute + 59*time.Second + 999999*time.Microsecond, time.Date(2024, 1, 1, 10, 0, 0, 1500, minus0930)},
			lits: []string{"'00:00:00'", "'24:00:00'", "'12:34:56.789'", "'23:59:59.999999'", "'19:30:00.000001'"}},
		{name: "time_ns", typ: "TIME_NS", vals: []any{12*time.Hour + 34*time.Minute + 56*time.Second + 123456789, time.Duration(1)},
			lits: []string{"'12:34:56.123456789'", "'00:00:00.000000001'"}},
		{name: "timetz", typ: "TIME WITH TIME ZONE", vals: []any{mustTimeTz(t, 12*time.Hour, 3600), mustTimeTz(t, 0, -(15*3600 + 59*60)),
			mustTimeTz(t, 23*time.Hour+59*time.Minute, 15*3600+59*60), time.Date(2024, 1, 1, 10, 0, 0, 1000, minus0930)},
			lits: []string{"'12:00:00+01'", "'00:00:00-15:59'", "'23:59:00+15:59'", "'10:00:00.000001-09:30'"}},
		{name: "timetz_alias", typ: "TIMETZ", vals: []any{mustTimeTz(t, time.Hour, 0)}, lits: []string{"'01:00:00+00'"}},
		{name: "timestamp", typ: "TIMESTAMP", vals: []any{time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC), time.Unix(0, 0), time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), time.Date(2024, 1, 1, 5, 0, 0, 0, plus14),
			time.Unix(-1, 999999000), time.UnixMicro(math.MaxInt64 - 1), time.UnixMicro(-9223372022400000000)},
			// Digits below the column's precision round to the nearest unit,
			// halves away from the epoch, like the server's own casts.
			lits: []string{"'2024-01-02 03:04:05.123457'", "'1970-01-01'", "'1900-01-01'", "'0001-01-01'", "'9999-12-31 23:59:59.999999'", "'2023-12-31 15:00:00'",
				"'1969-12-31 23:59:59.999999'", "'294247-01-10 04:00:54.775806'", "'290309-12-22 (BC) 00:00:00'"}},
		{name: "timestamp_s", typ: "TIMESTAMP_S", vals: []any{time.Date(2024, 1, 2, 3, 4, 5, 999999999, time.UTC), time.Unix(-1, 500)},
			lits: []string{"'2024-01-02 03:04:06'", "'1969-12-31 23:59:59'"}},
		{name: "timestamp_ms", typ: "TIMESTAMP_MS", vals: []any{time.Date(2024, 1, 2, 3, 4, 5, 123999999, time.UTC), time.Unix(-1, 1000000)},
			lits: []string{"'2024-01-02 03:04:05.124'", "'1969-12-31 23:59:59.001'"}},
		{name: "timestamp_ns", typ: "TIMESTAMP_NS", vals: []any{time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC), time.Unix(0, -1),
			time.Unix(0, math.MaxInt64-1), time.Unix(0, math.MinInt64+2)},
			lits: []string{"'2024-01-02 03:04:05.123456789'", "make_timestamp_ns(-1)", "make_timestamp_ns(9223372036854775806)", "make_timestamp_ns(-9223372036854775806)"}},
		{name: "timestamptz", typ: "TIMESTAMP WITH TIME ZONE", vals: []any{time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC), time.Date(2024, 1, 1, 5, 0, 0, 0, plus14)},
			lits: []string{"'2024-01-02 03:04:05.123456+00'", "'2024-01-01 05:00:00+14'"}},
		{name: "timestamptz_alias", typ: "TIMESTAMPTZ", vals: []any{time.Unix(1, 0)}, lits: []string{"'1970-01-01 00:00:01+00'"}},
		{name: "timestamptz_ns", typ: "TIMESTAMPTZ_NS", vals: []any{time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC), time.Date(2024, 1, 1, 5, 0, 0, 1, minus0930)},
			lits: []string{"'2024-01-02 03:04:05.123456789+00'", "'2024-01-01 14:30:00.000000001+00'"}},
		{name: "interval", typ: "INTERVAL", vals: []any{quackdriver.IntervalValue{Months: 14, Days: -3, Micros: 123456789}, 90 * time.Minute, -time.Microsecond,
			quackdriver.IntervalValue{Months: math.MinInt32, Days: math.MaxInt32, Micros: math.MaxInt64}},
			lits: []string{"to_months(14) + to_days(-3) + to_microseconds(123456789)", "INTERVAL 90 MINUTE", "to_microseconds(-1)",
				"to_months(-2147483648) + to_days(2147483647) + to_microseconds(9223372036854775807)"}},
		{name: "enum_quotes", typ: `ENUM('a', 'it''s', 'quo"te', 'ü 🦆', 'with, comma')`, vals: []any{"a", "it's", 2, "ü 🦆", "with, comma"},
			lits: []string{"'a'", "'it''s'", `'quo"te'`, "'ü 🦆'", "'with, comma'"}},
		{name: "enum_300", typ: bigEnum, vals: []any{"l0", "l299", 256}, lits: []string{"'l0'", "'l299'", "'l256'"}},
		{name: "enum_70000", typ: hugeEnum, vals: []any{"h0", "h69999", 65536}, lits: []string{"'h0'", "'h69999'", "'h65536'"}},
		{name: "list", typ: "INTEGER[]", vals: []any{[]any{1, 2, nil}, []any{}, []any{nil}}, lits: []string{"[1, 2, NULL]", "[]", "[NULL]"}},
		{name: "list_list", typ: "VARCHAR[][]", vals: []any{[]any{[]any{"a"}, nil, []any{}}, []any{}}, lits: []string{"[['a'], NULL, []]", "[]"}},
		{name: "array", typ: "INTEGER[3]", vals: []any{[]any{1, nil, 3}, []any{nil, nil, nil}}, lits: []string{"[1, NULL, 3]", "[NULL, NULL, NULL]"}},
		{name: "array_decimal", typ: "DECIMAL(10,2)[2]", vals: []any{[]any{"1.25", "-3.50"}, []any{nil, 12345678.9}}, lits: []string{"['1.25', '-3.50']", "[NULL, 12345678.9]"}},
		{name: "array_decimal38", typ: "DECIMAL(38,5)[2]", vals: []any{[]any{"123456789012345678901234567890123.12345", "-1"}},
			lits: []string{"['123456789012345678901234567890123.12345', '-1']"}},
		{name: "list_array", typ: "VARCHAR[2][]", vals: []any{[]any{[]any{"a", "b"}, nil}}, lits: []string{"[['a', 'b'], NULL]"}},
		{name: "struct_names", typ: `STRUCT(a INTEGER, "b c" VARCHAR, "ü""x" DOUBLE, "ÄÖÜ 中文" BOOLEAN)`,
			vals: []any{map[string]any{"a": 1, "b c": "x", `ü"x`: 1.5, "ÄÖÜ 中文": true}, map[string]any{}, map[string]any{"b c": "only"}},
			lits: []string{`{'a': 1, 'b c': 'x', 'ü"x': 1.5, 'ÄÖÜ 中文': true}`, `{'a': NULL, 'b c': NULL, 'ü"x': NULL, 'ÄÖÜ 中文': NULL}`, `{'a': NULL, 'b c': 'only', 'ü"x': NULL, 'ÄÖÜ 中文': NULL}`}},
		{name: "row_alias", typ: "ROW(x INTEGER)", vals: []any{map[string]any{"x": 1}}, lits: []string{"{'x': 1}"}},
		{name: "struct_list_struct", typ: "STRUCT(items STRUCT(k VARCHAR, v BIGINT)[], n INTEGER)",
			vals: []any{map[string]any{"items": []any{map[string]any{"k": "a", "v": 1}, nil, map[string]any{"k": nil, "v": int64(-5)}}, "n": 3},
				map[string]any{"items": []any{}, "n": nil}},
			lits: []string{"{'items': [{'k': 'a', 'v': 1}, NULL, {'k': NULL, 'v': -5}], 'n': 3}", "{'items': [], 'n': NULL}"}},
		{name: "tuple", typ: "TUPLE(INTEGER, VARCHAR)", vals: []any{[]any{1, "x"}, []any{nil, nil}}, lits: []string{"row(1, 'x')", "row(NULL, NULL)"}},
		{name: "map_list", typ: "MAP(VARCHAR, INTEGER[])", vals: []any{map[string]any{"a": []any{1, 2}, "b": nil, "": []any{}}, map[string]any{}},
			lits: []string{"MAP {'': [], 'a': [1, 2], 'b': NULL}", "MAP {}"}},
		{name: "map_pairs", typ: "MAP(INTEGER, VARCHAR)", vals: []any{[]any{map[string]any{"key": 3, "value": "c"}, map[string]any{"key": 1, "value": nil}}},
			lits: []string{"MAP {3: 'c', 1: NULL}"}},
		{name: "user_type", typ: "append_mood", vals: []any{"ok", 0}, lits: []string{"'ok'", "'sad'"}},
		{name: "kitchen_sink", typ: `STRUCT(q VARCHAR[], bl BLOB[], m MAP(VARCHAR, DECIMAL(20,5)), e ENUM('p', 'q''r')[2], tz TIMESTAMPTZ_NS, tt TIME WITH TIME ZONE, b BIT, n BIGNUM, u UUID, h HUGEINT, iv INTERVAL)`,
			vals: []any{map[string]any{
				"q":  []any{"it's", `back\slash`, "{x}=[y], z: 'w'", "NULL", "", nil},
				"bl": []any{[]byte{0, '\\', 0xff, 'a'}},
				"m":  map[string]any{"k'=1": "123456789012345.12345"},
				"e":  []any{"q'r", "p"},
				"tz": time.Unix(0, 5),
				"tt": mustTimeTz(t, time.Minute, -3600),
				"b":  "101",
				"n":  bigInt("-123456789012345678901234567890"),
				"u":  "550e8400-e29b-41d4-a716-446655440000",
				"h":  hugeMin,
				"iv": quackdriver.IntervalValue{Months: 1, Days: 2, Micros: 3},
			}},
			lits: []string{`{'q': ['it''s', 'back\slash', '{x}=[y], z: ''w''', 'NULL', '', NULL], 'bl': ['\x00\x5C\xFFa'::BLOB],
				'm': MAP {'k''=1': '123456789012345.12345'}, 'e': ['q''r', 'p'],
				'tz': make_timestamp_ns(5)::TIMESTAMPTZ_NS, 'tt': '00:01:00-01'::TIMETZ, 'b': '101'::BIT, 'n': '-123456789012345678901234567890'::BIGNUM,
				'u': '550e8400-e29b-41d4-a716-446655440000'::UUID, 'h': '` + hugeMin.String() + `'::HUGEINT, 'iv': to_months(1) + to_days(2) + to_microseconds(3)}`}},
	}
}

// serverType asks the server for the LogicalType of NULL::<typ> as it
// serializes it in a PREPARE_RESPONSE.
func serverType(t *testing.T, e *env, typ string) logicaltype.Type {
	t.Helper()
	mark := e.rec.Mark()
	livetest.Scalar[any](t, e.db, "SELECT NULL::"+typ)
	for _, x := range e.rec.Since(mark) {
		if _, b, err := message.Decode(x.Resp); err == nil {
			if p, ok := b.(message.PrepareResponse); ok && len(p.ResultTypes) == 1 {
				return p.ResultTypes[0]
			}
		}
	}
	t.Fatalf("no PREPARE_RESPONSE recorded for NULL::%s", typ)
	return logicaltype.Type{}
}

// TestAppendEveryType appends, for every type the Appender supports, a
// 2048-row chunk with NULLs interleaved, an all-NULL chunk and a partial
// chunk, and checks that the chunks are VARCHAR text and that every value
// read back equals the server's own literal for it.
func TestAppendEveryType(t *testing.T) {
	e := newEnv(t)
	sc := e.conn(t)
	e.exec(t, "CREATE TYPE append_mood AS ENUM ('sad', 'ok')")
	for i, tc := range typeCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.vals) != len(tc.lits) {
				t.Fatalf("case has %d values but %d literals", len(tc.vals), len(tc.lits))
			}
			col := tc.col
			if col == "" {
				col = tc.typ
			}
			tbl, exp := fmt.Sprintf("append_types_%d", i), fmt.Sprintf("append_types_exp_%d", i)
			k := len(tc.vals)
			ins := []string{fmt.Sprintf("(%d, NULL)", k)}
			for j, l := range tc.lits {
				ins = append(ins, fmt.Sprintf("(%d, %s)", j, l))
			}
			e.exec(t, fmt.Sprintf("CREATE TABLE %s (id INTEGER, v %s)", tbl, col),
				fmt.Sprintf("CREATE TABLE %s (i INTEGER, v %s)", exp, col),
				fmt.Sprintf("INSERT INTO %s VALUES %s", exp, strings.Join(ins, ", ")))

			const n = 2*2048 + 7 // rows 2048..4095 are all NULL
			mark := e.rec.Mark()
			count, err := appender(sc, "", tbl, []quackdriver.AppendColumn{{Name: "id", Type: "INTEGER"}, {Name: "v", Type: tc.typ}}, n,
				func(r int) []any {
					if j := r % (k + 1); j < k && (r < 2048 || r >= 4096) {
						return []any{r, tc.vals[j]}
					}
					return []any{r, nil}
				})
			if err != nil || count != n {
				t.Fatalf("append: %d rows, %v", count, err)
			}

			// The rows travel as VARCHAR text, which the INSERT casts.
			if sql := appenderPrepare(t, e, mark).SQL; !strings.Contains(sql, `NULL::STRUCT("c0" VARCHAR, "c1" VARCHAR)`) {
				t.Errorf("prototype of %s", sql)
			}
			var chunks []message.DataChunk
			for _, s := range sent[message.SendDataRequest](t, e, mark) {
				chunks = append(chunks, s.Chunks...)
			}
			if len(chunks) != 3 {
				t.Errorf("sent %d chunks, want 3", len(chunks))
			}
			for _, ch := range chunks {
				if ch.Types[0].ID != logicaltype.IDVarchar || ch.Types[1].ID != logicaltype.IDVarchar {
					t.Fatalf("chunk types %s, %s", ch.Types[0].SQL(), ch.Types[1].SQL())
				}
			}

			// Values: each equal to the server's literal.
			if mism := e.queryInt(t, fmt.Sprintf(`SELECT count(*) FROM %s t JOIN %s x ON t.id %% %d = x.i
				WHERE t.v IS DISTINCT FROM (CASE WHEN t.id BETWEEN 2048 AND 4095 THEN NULL ELSE x.v END)`, tbl, exp, k+1)); mism != 0 {
				t.Errorf("%d rows differ from the literals", mism)
			}
			gotS := e.queryStrings(t, fmt.Sprintf("SELECT v::VARCHAR FROM %s WHERE id <= %d ORDER BY id", tbl, k))
			wantS := e.queryStrings(t, fmt.Sprintf("SELECT v::VARCHAR FROM %s ORDER BY i", exp))
			if !reflect.DeepEqual(gotS, wantS) {
				t.Errorf("VARCHAR rendering:\n got %q\nwant %q", gotS, wantS)
			}
		})
	}
}

// TestAppendTypeAliases: the server resolves every spelling of a type; the
// INSERT casts to the type it resolved.
func TestAppendTypeAliases(t *testing.T) {
	e := newEnv(t)
	aliases := []string{
		"BOOL", "LOGICAL", "INT1", "SHORT", "INT16", "INT", "INT4", "SIGNED", "INTEGRAL", "INT32", "INT8", "LONG", "OID", "INT64",
		"UINT8", "UINT16", "UINT32", "UINT64", "INT128", "UINT128", "REAL", "FLOAT4", "FLOAT8",
		"STRING", "CHAR", "BPCHAR", "NVARCHAR", "VARCHAR(10)", "CHAR(3)", "BYTEA", "VARBINARY", "BINARY", "GUID",
		"TIME WITHOUT TIME ZONE", "DATETIME", "TIMESTAMP_US", "TIMESTAMP WITHOUT TIME ZONE", "BITSTRING", "VARINT",
		"DEC(5)", "NUMERIC(38, 0)", "decimal(1,1)", "integer[]", "int[2][]", "map(int, varchar)",
		"tuple(int)", "struct(\"A\" int, \"a b\" int)",
	}
	var cols []quackdriver.AppendColumn
	var defs []string
	for i, a := range aliases {
		cols = append(cols, quackdriver.AppendColumn{Name: fmt.Sprintf("c%d", i), Type: a})
		defs = append(defs, fmt.Sprintf("c%d %s", i, a))
	}
	e.exec(t, "CREATE TABLE append_aliases ("+strings.Join(defs, ", ")+")")
	mark := e.rec.Mark()
	if err := appendRows(e.conn(t), "append_aliases", cols, [][]any{make([]any, len(cols))}); err != nil {
		t.Fatal(err)
	}
	sql := appenderPrepare(t, e, mark).SQL
	for i, a := range aliases {
		if cast := fmt.Sprintf(`"c%d"::%s`, i, serverType(t, e, a).SQL()); !strings.Contains(sql, cast) {
			t.Errorf("%s: no %s in %s", a, cast, sql)
		}
	}
}

// TestAppendUnsupportedTypes: UNION (also nested), VARIANT and GEOMETRY
// columns are refused before the INSERT starts; the connection stays usable.
func TestAppendUnsupportedTypes(t *testing.T) {
	e := newEnv(t)
	sc := e.conn(t)
	e.exec(t, "CREATE TABLE append_unsupported (v INTEGER)")
	for typ, want := range map[string]string{
		"UNION(a INTEGER, b VARCHAR)":  "UNION is not supported",
		"STRUCT(u UNION(a INTEGER)[])": "UNION is not supported",
		"VARIANT":                      "VARIANT",
		"GEOMETRY":                     "GEOMETRY",
		"INTEGER); DROP TABLE append_unsupported; --": "unexpected",
	} {
		err := appendRows(sc, "append_unsupported", []quackdriver.AppendColumn{{Name: "v", Type: typ}}, [][]any{{nil}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", typ, err, want)
		}
	}
	connUsable(t, sc)
	if n := e.queryInt(t, "SELECT count(*) FROM append_unsupported"); n != 0 {
		t.Fatalf("%d rows", n)
	}
}

// TestTypeSQLRoundTrip checks logicaltype.Type.SQL on types decoded from
// the server, including types the Appender never sends: the rendered text
// parses back on the server to an equal type.
func TestTypeSQLRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.exec(t,
		"CREATE TYPE append_sql_mood AS ENUM ('a', 'b''c')",
		"CREATE TYPE append_sql_int AS INTEGER",
		`CREATE TYPE "append sql ""struct""" AS STRUCT(x INTEGER)`)
	for _, typ := range []string{
		"DECIMAL(1,0)", "DECIMAL(4,4)", "DECIMAL(38,37)", "TIME_NS", "TIMETZ", "TIMESTAMP_S", "TIMESTAMPTZ_NS",
		"JSON", "GEOMETRY", "INTEGER[][]", "VARCHAR[3]", "INTEGER[2][3]", "INTEGER[3][]",
		`STRUCT("a" INTEGER, "B b" VARCHAR, "c""d" DOUBLE, "ü" BOOLEAN)`, "STRUCT(a STRUCT(b INTEGER[])[])",
		"TUPLE(TUPLE(INTEGER), INTEGER[])", "MAP(INTEGER[], STRUCT(a INTEGER))", `UNION("a" INTEGER, "b c" VARCHAR)`,
		"ENUM('x', 'it''s', 'q\"q', '')", "append_sql_mood", "append_sql_int", `"append sql ""struct"""`,
		"JSON[]", "STRUCT(j JSON, m append_sql_mood)", `"NULL"`,
	} {
		s := serverType(t, e, typ)
		if back := serverType(t, e, s.SQL()); !reflect.DeepEqual(s, back) {
			t.Errorf("%s: server type renders as %s, which the server parses to %s", typ, s.SQL(), back.SQL())
		}
	}
}
