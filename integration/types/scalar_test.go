//go:build integration

package types_test

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// TestScalarsRendered compares every scalar type's decoded value with the
// server's own VARCHAR rendering, as a CONSTANT and as a FLAT vector.
func TestScalarsRendered(t *testing.T) {
	e := newEnv(t)
	groups := map[string][]string{
		"bool": {"true", "false"},
		"int": {
			"(-128)::TINYINT", "127::TINYINT", "0::TINYINT",
			"(-32768)::SMALLINT", "32767::SMALLINT",
			"(-2147483648)::INTEGER", "2147483647::INTEGER",
			"(-9223372036854775808)::BIGINT", "9223372036854775807::BIGINT",
			"255::UTINYINT", "65535::USMALLINT", "4294967295::UINTEGER", "18446744073709551615::UBIGINT",
			"0::UBIGINT",
		},
		"hugeint": {
			"(-170141183460469231731687303715884105728)::HUGEINT", "170141183460469231731687303715884105727::HUGEINT",
			"(-1)::HUGEINT", "0::HUGEINT", "18446744073709551616::HUGEINT", "(-18446744073709551616)::HUGEINT",
			"340282366920938463463374607431768211455::UHUGEINT", "0::UHUGEINT", "18446744073709551615::UHUGEINT",
		},
		"float": {
			"1.5::FLOAT", "0.1::FLOAT", "'-0.0'::FLOAT", "'nan'::FLOAT", "'inf'::FLOAT", "'-inf'::FLOAT",
			"3.4028235e38::FLOAT", "1.4e-45::FLOAT", "1.17549435e-38::FLOAT", "16777217::FLOAT",
			"1.5::DOUBLE", "0.1::DOUBLE", "'-0.0'::DOUBLE", "'nan'::DOUBLE", "'inf'::DOUBLE", "'-inf'::DOUBLE",
			"1.7976931348623157e308::DOUBLE", "5e-324::DOUBLE", "2.2250738585072014e-308::DOUBLE",
			"1e15::DOUBLE", "1e16::DOUBLE", "123456789012345678::DOUBLE", "0.0001::DOUBLE", "0.00001::DOUBLE",
			"(-1.0/3)::DOUBLE", "(1.0/3)::FLOAT", "1e-5::FLOAT", "1e7::FLOAT",
		},
		"decimal": {
			"0.5::DECIMAL(1,1)", "(-9)::DECIMAL(1,0)", "9.9::DECIMAL(2,1)",
			"(-999.9)::DECIMAL(4,1)", "9999::DECIMAL(4,0)", "(-0.0001)::DECIMAL(4,4)",
			"99999.9999::DECIMAL(9,4)", "(-999999999)::DECIMAL(9,0)", "(-0.000000001)::DECIMAL(9,9)",
			"999999999999999999::DECIMAL(18,0)", "(-99999999.9999999999)::DECIMAL(18,10)", "(-0.000000000000000001)::DECIMAL(18,18)",
			"'99999999999999999999999999999999999999'::DECIMAL(38,0)",
			"'-99999999999999999999999999999999999999'::DECIMAL(38,0)",
			"'-0.00000000000000000000000000000000000001'::DECIMAL(38,38)",
			"'0.99999999999999999999999999999999999999'::DECIMAL(38,38)",
			"(-1234567890123456789.0123456789)::DECIMAL(38,10)", "0::DECIMAL(38,10)", "0::DECIMAL(4,2)",
			"12.30::DECIMAL(10,2)", "(-12.3)::DECIMAL(19,2)", "1::DECIMAL(19,18)",
		},
		"string": {
			"''", "'hello'", "'it''s'", "'ünïcødé 漢字 😀'", "chr(0)", "'a' || chr(0) || 'b'", "'trailing space '",
			"repeat('x', 12)", "repeat('y', 13)", "repeat('é', 1000)", "'\"quoted\"'", "'line1' || chr(10) || 'line2'",
			"'NULL'", "'{\"a\": [1, 2.5, null, \"x\"]}'::JSON", "'[]'::JSON", "'\"s\"'::JSON",
			"'abc'::CHAR", "'x'::VARCHAR COLLATE NOCASE",
		},
		"blob": {
			"''::BLOB", "'abc'::BLOB", "from_hex('00FF7F80')", "from_hex('5C27')", "'\\xDE\\xAD\\xBE\\xEF'::BLOB",
			"from_hex((SELECT string_agg(printf('%02X', i), '' ORDER BY i) FROM range(256) t(i)))", "from_hex(repeat('00', 100))",
		},
		"bit": {"'0'::BIT", "'1'::BIT", "'10101010'::BIT", "'101010101'::BIT", "repeat('10', 100)::BIT", "'0000000'::BIT"},
		"bignum": {
			"0::BIGNUM", "1::BIGNUM", "(-1)::BIGNUM", "255::BIGNUM", "256::BIGNUM", "(-256)::BIGNUM", "(-255)::BIGNUM",
			"'" + strings.Repeat("9", 400) + "'::BIGNUM", "'-" + strings.Repeat("9", 400) + "'::BIGNUM",
			"(-170141183460469231731687303715884105728)::HUGEINT::BIGNUM",
		},
		"uuid": {
			"'00000000-0000-0000-0000-000000000000'::UUID", "'ffffffff-ffff-ffff-ffff-ffffffffffff'::UUID",
			"'7fffffff-ffff-ffff-ffff-ffffffffffff'::UUID", "'80000000-0000-0000-0000-000000000000'::UUID",
			"'A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11'::UUID", "uuidv7()", "gen_random_uuid()",
		},
		"enum": {
			"'b'::ENUM('a', 'b', 'it''s')", "'it''s'::ENUM('a', 'b', 'it''s')",
			"'v299'::ENUM(" + enumLabels(300) + ")", "'v69999'::ENUM(" + enumLabels(70000) + ")",
		},
		"interval": {
			"INTERVAL 1 DAY", "INTERVAL '1 year 2 months 3 days 04:05:06.789'", "INTERVAL '-1 year -2 months'",
			"INTERVAL '14 months'", "INTERVAL '-14 months'", "INTERVAL '-00:00:00.000001'", "INTERVAL 0 SECOND",
			"INTERVAL 1 MONTH - INTERVAL 1 DAY", "INTERVAL '1 day -1 hour'", "INTERVAL '2562047788 hours'",
			"to_microseconds(9223372036854775807)", "to_microseconds(-9223372036854775807)",
			"to_months(2147483647)", "to_months(-2147483647)", "to_days(-2147483647)",
			"INTERVAL '123:45:06.5'", "INTERVAL '1 millennium 1 century'",
		},
	}
	for name, exprs := range groups {
		t.Run(name, func(t *testing.T) { e.checkRendered(t, exprs...) })
	}
}

func enumLabels(n int) string {
	var sb strings.Builder
	for i := range n {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("'v")
		sb.WriteString(itoa(i))
		sb.WriteString("'")
	}
	return sb.String()
}

func itoa(i int) string { return big.NewInt(int64(i)).String() }

// TestScalarGoValues pins the documented Go type and exact value of the
// extremes that a rendering comparison could hide (signed zero, NaN, the
// precise big integers).
func TestScalarGoValues(t *testing.T) {
	e := newEnv(t)
	row := e.rows(t, `SELECT '-0.0'::DOUBLE, '-0.0'::FLOAT, 'nan'::DOUBLE, 'nan'::FLOAT,
		(-170141183460469231731687303715884105728)::HUGEINT, 340282366920938463463374607431768211455::UHUGEINT,
		(-9223372036854775808)::BIGINT, 18446744073709551615::UBIGINT, ''::BLOB, ''::VARCHAR`)[0]
	if v, ok := row[0].(float64); !ok || v != 0 || !math.Signbit(v) {
		t.Errorf("-0.0::DOUBLE = %#v, want float64 -0", row[0])
	}
	if v, ok := row[1].(float32); !ok || v != 0 || !math.Signbit(float64(v)) {
		t.Errorf("-0.0::FLOAT = %#v, want float32 -0", row[1])
	}
	if v, ok := row[2].(float64); !ok || !math.IsNaN(v) {
		t.Errorf("nan::DOUBLE = %#v", row[2])
	}
	if v, ok := row[3].(float32); !ok || !math.IsNaN(float64(v)) {
		t.Errorf("nan::FLOAT = %#v", row[3])
	}
	minHuge, _ := new(big.Int).SetString("-170141183460469231731687303715884105728", 10)
	if v, ok := row[4].(*big.Int); !ok || v.Cmp(minHuge) != 0 {
		t.Errorf("min HUGEINT = %#v", row[4])
	}
	maxUHuge, _ := new(big.Int).SetString("340282366920938463463374607431768211455", 10)
	if v, ok := row[5].(*big.Int); !ok || v.Cmp(maxUHuge) != 0 {
		t.Errorf("max UHUGEINT = %#v", row[5])
	}
	if row[6] != int64(math.MinInt64) || row[7] != uint64(math.MaxUint64) {
		t.Errorf("BIGINT/UBIGINT extremes = %#v, %#v", row[6], row[7])
	}
	if v, ok := row[8].([]byte); !ok || v == nil || len(v) != 0 {
		t.Errorf("empty BLOB = %#v, want non-nil []byte{}", row[8])
	}
	if row[9] != "" {
		t.Errorf("empty VARCHAR = %#v", row[9])
	}
}

// TestExoticTypes covers types that rarely appear in results: exported
// aggregate states (aliased types), SQLNULL columns, VARCHAR with a length,
// CHAR, and empty results.
func TestExoticTypes(t *testing.T) {
	e := newEnv(t)
	row := e.rows(t, `SELECT sum(i) EXPORT_STATE, avg(i) EXPORT_STATE, NULL, 'abc'::VARCHAR(10), 'x'::CHAR,
		list(i) EXPORT_STATE FROM range(3) t(i)`)[0]
	if fmt.Sprint(row[0]) != "3" {
		t.Errorf("sum state = %#v", row[0])
	}
	if m, ok := row[1].(map[string]any); !ok || fmt.Sprint(m["count"]) != "3" || fmt.Sprint(m["value"]) != "3" {
		t.Errorf("avg state = %#v", row[1])
	}
	if row[2] != nil || row[3] != "abc" || row[4] != "x" {
		t.Errorf("NULL / VARCHAR(10) / CHAR = %#v %#v %#v", row[2], row[3], row[4])
	}
	if !reflect.DeepEqual(row[5], []any{int64(0), int64(1), int64(2)}) {
		t.Errorf("list state = %#v", row[5])
	}

	// More aggregate states: their types carry extension info holding the
	// aggregate's signature (TYPE and VARIANT values), and ordered
	// aggregates also their ORDER BY spec.
	e.reset()
	row = e.rows(t, `SELECT list(i ORDER BY i DESC) EXPORT_STATE, string_agg(i::VARCHAR, ',') EXPORT_STATE,
		min('x' || i) EXPORT_STATE, arg_max(i, i::VARCHAR) EXPORT_STATE, quantile_cont(i, 0.5) EXPORT_STATE,
		approx_count_distinct(i) EXPORT_STATE, bit_and(i::INTEGER) EXPORT_STATE,
		'POINT (1 2)'::GEOMETRY('OGC:CRS84'), 'POINT (1 2)'::GEOMETRY::BLOB
		FROM range(5) t(i)`)[0]
	if row[2] != "x0" || !reflect.DeepEqual(row[3], map[string]any{"arg": int64(4), "by": "4"}) ||
		!reflect.DeepEqual(row[4], []any{0.0, 1.0, 2.0, 3.0, 4.0}) && !reflect.DeepEqual(row[4], []any{int64(0), int64(1), int64(2), int64(3), int64(4)}) {
		t.Errorf("aggregate states = %#v", row)
	}
	if !bytes.Equal(row[7].([]byte), row[8].([]byte)) {
		t.Errorf("GEOMETRY with CRS = %x, want %x", row[7], row[8])
	}
	// The result types survive an encode/decode round trip byte for byte.
	for _, c := range e.chunks(t) {
		for i, typ := range c.Types {
			w := codec.NewWriter()
			logicaltype.Encode(w, typ)
			back, err := logicaltype.Decode(codec.NewReader(w.Bytes()))
			if err != nil {
				t.Fatalf("type %d: re-decode: %v", i, err)
			}
			w2 := codec.NewWriter()
			logicaltype.Encode(w2, back)
			if !bytes.Equal(w.Bytes(), w2.Bytes()) {
				t.Errorf("type %d (%s): round trip changed the bytes", i, typ.Name())
			}
			if i < 7 && (typ.Info == nil || typ.Info.Alias != "AGGREGATE_STATE" || typ.Info.ExtensionInfo == nil) {
				t.Errorf("type %d: want an AGGREGATE_STATE alias with extension info, got %+v", i, typ.Info)
			}
		}
	}
	if rows := e.rows(t, "SELECT 1::INTEGER, [1], {'a': 1} WHERE false"); len(rows) != 0 {
		t.Errorf("empty result: %d rows", len(rows))
	}
	rows := e.rows(t, "SELECT NULL FROM range(3000)")
	if len(rows) != 3000 || rows[2999][0] != nil {
		t.Errorf("SQLNULL column: %d rows", len(rows))
	}
}
