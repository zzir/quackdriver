//go:build integration

package sqlfmt

import (
	"database/sql"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/value"
)

// roundTrip is one argument value checked end to end.
type roundTrip struct {
	name string
	arg  any
	// want is the value SELECT ? must decode to (with the Go type the
	// literal's DuckDB type decodes as).
	want any
	// typ is the DuckDB type the substituted literal must have.
	typ string
	// lit is an explicit, hand-written typed literal for the same value; the
	// server must consider it IS NOT DISTINCT FROM the parameter and render
	// both identically as VARCHAR.
	lit string
}

// check runs the round trip against the server.
func (c roundTrip) check(t *testing.T, db *sql.DB) {
	t.Helper()
	row, err := queryRow(db, "SELECT ?", c.arg)
	if err != nil {
		t.Fatalf("SELECT ? with %#v: %v", c.arg, err)
	}
	if !same(row[0], c.want) {
		t.Errorf("SELECT ? with %#v decoded to %T %#v, want %T %#v", c.arg, row[0], row[0], c.want, c.want)
	}
	row, err = queryRow(db,
		"SELECT $1 IS NOT DISTINCT FROM ("+c.lit+"), typeof($1), $1::VARCHAR, ("+c.lit+")::VARCHAR", c.arg)
	if err != nil {
		t.Fatalf("compare %#v with %s: %v", c.arg, c.lit, err)
	}
	if row[0] != true {
		t.Errorf("%#v: parameter IS DISTINCT FROM %s (server renders %v vs %v)", c.arg, c.lit, row[2], row[3])
	}
	if row[1] != c.typ {
		t.Errorf("%#v: parameter has type %v, want %s", c.arg, row[1], c.typ)
	}
	if row[2] != row[3] {
		t.Errorf("%#v: server renders parameter as %q, literal %s as %q", c.arg, row[2], c.lit, row[3])
	}
}

func runRoundTrips(t *testing.T, cases []roundTrip) {
	t.Helper()
	db := openDB(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.check(t, db)
		})
	}
}

func TestRoundTripStrings(t *testing.T) {
	str := func(name, s, lit string) roundTrip {
		return roundTrip{name: name, arg: s, want: s, typ: "VARCHAR", lit: lit + "::VARCHAR"}
	}
	runRoundTrips(t, []roundTrip{
		str("empty", "", "''"),
		str("single quote", "it's", "'it''s'"),
		str("backslash quote", `\'`, `'\'''`),
		str("newlines", "a\nb\r\nc\td", "concat('a', chr(10), 'b', chr(13), chr(10), 'c', chr(9), 'd')"),
		str("E-string bait", `E'\''`, `'E''\'''''`),
		str("unicode", "héllo wörld 日本語 😀 Ω", "'héllo wörld 日本語 😀 Ω'"),
		str("unicode spaces", "a\u00a0b\u2003c\u3000d\ufeffe\u200bf", "concat('a', chr(160), 'b', chr(8195), 'c', chr(12288), 'd', chr(65279), 'e', chr(8203), 'f')"),
		str("long", strings.Repeat("x'y\\", 10000), "repeat('x''y\\', 10000)"),
	})
}

func TestRoundTripIntegers(t *testing.T) {
	runRoundTrips(t, []roundTrip{
		// Integer arguments become untyped integer literals, which DuckDB
		// types by magnitude (and which can still be compared with or
		// assigned to any integer column).
		{name: "zero", arg: 0, want: int32(0), typ: "INTEGER", lit: "0::INTEGER"},
		{name: "int8 min", arg: int8(math.MinInt8), want: int32(-128), typ: "INTEGER", lit: "(-128)::TINYINT::INTEGER"},
		{name: "uint16 max", arg: uint16(math.MaxUint16), want: int32(65535), typ: "INTEGER", lit: "65535::USMALLINT::INTEGER"},
		{name: "int32 min", arg: int32(math.MinInt32), want: int32(math.MinInt32), typ: "INTEGER", lit: "(-2147483648)::INTEGER"},
		{name: "uint32 max", arg: uint32(math.MaxUint32), want: int64(math.MaxUint32), typ: "BIGINT", lit: "4294967295::UINTEGER::BIGINT"},
		{name: "int64 min", arg: int64(math.MinInt64), want: int64(math.MinInt64), typ: "BIGINT", lit: "(-9223372036854775808)::BIGINT"},
		{name: "uint64 max", arg: uint64(math.MaxUint64), want: bigInt("18446744073709551615"), typ: "HUGEINT", lit: "18446744073709551615::UBIGINT::HUGEINT"},
		{name: "uint64 2^63", arg: uint64(1) << 63, want: bigInt("9223372036854775808"), typ: "HUGEINT", lit: "9223372036854775808::HUGEINT"},
		{name: "big.Int small", arg: big.NewInt(-42), want: int32(-42), typ: "INTEGER", lit: "(-42)::INTEGER"},
		{name: "big.Int hugeint min", arg: bigInt("-170141183460469231731687303715884105728"), want: bigInt("-170141183460469231731687303715884105728"), typ: "HUGEINT", lit: "'-170141183460469231731687303715884105728'::HUGEINT"},
		{name: "big.Int uhugeint max", arg: bigInt("340282366920938463463374607431768211455"), want: bigInt("340282366920938463463374607431768211455"), typ: "UHUGEINT", lit: "'340282366920938463463374607431768211455'::UHUGEINT"},
		{name: "big.Int bignum", arg: bigInt("123456789012345678901234567890123456789012345678901234567890"), want: bigInt("123456789012345678901234567890123456789012345678901234567890"), typ: "BIGNUM", lit: "'123456789012345678901234567890123456789012345678901234567890'::BIGNUM"},
		{name: "big.Int value", arg: *bigInt("-5"), want: int32(-5), typ: "INTEGER", lit: "(-5)::INTEGER"},
		{name: "nil big.Int", arg: (*big.Int)(nil), want: nil, typ: "\"NULL\"", lit: "NULL"},
	})
}

func TestRoundTripFloats(t *testing.T) {
	f := func(name string, x float64, lit string) roundTrip {
		return roundTrip{name: name, arg: x, want: x, typ: "DOUBLE", lit: lit}
	}
	tenth, fifth := 0.1, 0.2 // not constants: Go folds constant 0.1+0.2 exactly
	runRoundTrips(t, []roundTrip{
		f("zero", 0, "0::DOUBLE"),
		f("negative zero", math.Copysign(0, -1), "'-0.0'::DOUBLE"),
		f("integral", 3, "3::DOUBLE"),
		f("1.5", 1.5, "1.5::DOUBLE"),
		f("0.1", 0.1, "'0.1'::DOUBLE"),
		f("0.1+0.2", tenth+fifth, "'0.30000000000000004'::DOUBLE"),
		f("1e15", 1e15, "'1e15'::DOUBLE"),
		f("1e21", 1e21, "'1e21'::DOUBLE"),
		f("2^53+2", 1<<53+2, "9007199254740994::DOUBLE"),
		f("max", math.MaxFloat64, "'1.7976931348623157e308'::DOUBLE"),
		f("smallest normal", 0x1p-1022, "'2.2250738585072014e-308'::DOUBLE"),
		f("smallest subnormal", math.SmallestNonzeroFloat64, "'5e-324'::DOUBLE"),
		f("+inf", math.Inf(1), "'infinity'::DOUBLE"),
		f("-inf", math.Inf(-1), "'-infinity'::DOUBLE"),
		f("nan", math.NaN(), "'nan'::DOUBLE"),
		// database/sql widens float32 to float64; the exact float32 value
		// arrives as a DOUBLE.
		{name: "float32", arg: float32(0.1), want: float64(float32(0.1)), typ: "DOUBLE", lit: "'0.1'::FLOAT::DOUBLE"},
		{name: "float32 max", arg: float32(math.MaxFloat32), want: float64(math.MaxFloat32), typ: "DOUBLE", lit: "'3.4028235e38'::FLOAT::DOUBLE"},
	})
}

func TestRoundTripOther(t *testing.T) {
	runRoundTrips(t, []roundTrip{
		{name: "empty blob", arg: []byte{}, want: []byte{}, typ: "BLOB", lit: "''::BLOB"},
		{name: "true", arg: true, want: true, typ: "BOOLEAN", lit: "'t'::BOOLEAN"},
		{name: "false", arg: false, want: false, typ: "BOOLEAN", lit: "'f'::BOOLEAN"},
		{name: "nil", arg: nil, want: nil, typ: "\"NULL\"", lit: "NULL"},
	})
}

func TestRoundTripRationals(t *testing.T) {
	r := func(name, s string, want any, typ, lit string) roundTrip {
		return roundTrip{name: name, arg: bigRat(s), want: want, typ: typ, lit: lit}
	}
	runRoundTrips(t, []roundTrip{
		r("half", "1/2", "0.5", "DECIMAL(1,1)", "0.5::DECIMAL(1,1)"),
		r("decimal", "12345.678", "12345.678", "DECIMAL(8,3)", "12345.678::DECIMAL(8,3)"),
		r("integer", "42", int32(42), "INTEGER", "42"),
		r("huge integer", "123456789012345678901234567890123456789012345678901234567890",
			bigInt("123456789012345678901234567890123456789012345678901234567890"), "BIGNUM",
			"'123456789012345678901234567890123456789012345678901234567890'::BIGNUM"),
		r("38 digits", "1234567890123456789012345678.9012345678", "1234567890123456789012345678.9012345678",
			"DECIMAL(38,10)", "'1234567890123456789012345678.9012345678'::DECIMAL(38,10)"),
		r("38 fraction digits", "0.12345678901234567890123456789012345678", "0.12345678901234567890123456789012345678",
			"DECIMAL(38,38)", "'0.12345678901234567890123456789012345678'::DECIMAL(38,38)"),
		r("negative 38 fraction digits", "-0.00000000000000000000000000000000000001", "-0.00000000000000000000000000000000000001",
			"DECIMAL(38,38)", "'-0.00000000000000000000000000000000000001'::DECIMAL(38,38)"),
		// Rationals without a finite decimal expansion (or with more
		// digits than DECIMAL's 38) are rounded to 38 significant digits.
		r("third", "1/3", "0.33333333333333333333333333333333333333",
			"DECIMAL(38,38)", "'0.33333333333333333333333333333333333333'::DECIMAL(38,38)"),
		r("two thirds negative", "-2/3", "-0.66666666666666666666666666666666666667",
			"DECIMAL(38,38)", "'-0.66666666666666666666666666666666666667'::DECIMAL(38,38)"),
		r("2^-40", "1/1099511627776", "0.00000000000090949470177292823791503906",
			"DECIMAL(38,38)", "'0.00000000000090949470177292823791503906'::DECIMAL(38,38)"),
		r("tiny negative rounds to zero", "-1e-50", "0.0", "DECIMAL(1,1)", "0.0::DECIMAL(1,1)"),
		r("rounding carries", "99999999999999999999999999999999999.9999999", "100000000000000000000000000000000000.0",
			"DECIMAL(37,1)", "'100000000000000000000000000000000000.0'::DECIMAL(37,1)"),
		{name: "nil", arg: (*big.Rat)(nil), want: nil, typ: "\"NULL\"", lit: "NULL"},
	})
}

func TestRoundTripTimestamps(t *testing.T) {
	ts := func(name string, tm time.Time, typ, lit string) roundTrip {
		return roundTrip{name: name, arg: tm, want: tm, typ: typ, lit: lit}
	}
	est := time.FixedZone("EST", -5*3600)
	odd := time.FixedZone("odd", 5*3600+30*60+17)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = est
	}
	runRoundTrips(t, []roundTrip{
		ts("epoch", time.Unix(0, 0).UTC(), "TIMESTAMP", "TIMESTAMP '1970-01-01 00:00:00'"),
		ts("micros", time.Date(2024, 2, 29, 23, 59, 59, 999999000, time.UTC), "TIMESTAMP", "TIMESTAMP '2024-02-29 23:59:59.999999'"),
		ts("nanos", time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC), "TIMESTAMP_NS", "TIMESTAMP_NS '2024-01-02 03:04:05.123456789'"),
		ts("negative nano", time.Unix(0, -1).UTC(), "TIMESTAMP_NS", "TIMESTAMP_NS '1969-12-31 23:59:59.999999999'"),
		ts("ns min", time.Unix(0, math.MinInt64+2).UTC(), "TIMESTAMP_NS", "TIMESTAMP_NS '1677-09-21 00:12:43.145224194'"),
		ts("ns max", time.Unix(0, math.MaxInt64-1).UTC(), "TIMESTAMP_NS", "TIMESTAMP_NS '2262-04-11 23:47:16.854775806'"),
		ts("fixed zone", time.Date(2024, 6, 1, 1, 2, 3, 0, est), "TIMESTAMP", "TIMESTAMP '2024-06-01 06:02:03'"),
		ts("odd zone with nanos", time.Date(2024, 6, 1, 1, 2, 3, 7, odd), "TIMESTAMP_NS", "TIMESTAMP_NS '2024-05-31 19:31:46.000000007'"),
		ts("DST zone", time.Date(2024, 3, 10, 3, 30, 0, 0, ny), "TIMESTAMP", "TIMESTAMP '2024-03-10 07:30:00'"),
		ts("44 BC", time.Date(-43, 3, 15, 12, 0, 0, 0, time.UTC), "TIMESTAMP", "TIMESTAMP '0044-03-15 (BC) 12:00:00'"),
		ts("BC zone crossing", time.Date(1, 1, 1, 1, 0, 0, 0, time.FixedZone("", 2*3600)), "TIMESTAMP", "TIMESTAMP '0001-12-31 (BC) 23:00:00'"),
		ts("timestamp min", time.Date(-290308, 12, 22, 0, 0, 0, 0, time.UTC), "TIMESTAMP", "TIMESTAMP '290309-12-22 (BC) 00:00:00'"),
		ts("year 10000", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), "TIMESTAMP", "TIMESTAMP '10000-01-01 00:00:00'"),
		ts("timestamp max", time.Date(294247, 1, 10, 4, 0, 54, 775806000, time.UTC), "TIMESTAMP", "TIMESTAMP '294247-01-10 04:00:54.775806'"),
	})
}

func TestTimestampOutOfRange(t *testing.T) {
	db := openDB(t)
	for _, tm := range []time.Time{
		// One microsecond past TIMESTAMP's range (the next value is
		// +infinity).
		time.Date(294247, 1, 10, 4, 0, 54, 775807000, time.UTC),
		time.Date(300000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(-300000, 1, 1, 0, 0, 0, 0, time.UTC),
		// Sub-microsecond values outside TIMESTAMP_NS's range.
		time.Unix(0, math.MaxInt64).UTC(),
		time.Unix(0, math.MinInt64+1).UTC(),
		time.Date(1500, 1, 1, 0, 0, 0, 1, time.UTC),
	} {
		row, err := queryRow(db, "SELECT ?", tm)
		if err == nil {
			t.Errorf("%v: got %v, want an error", tm, row)
		}
	}
}

func TestRoundTripDurations(t *testing.T) {
	d := func(name string, x time.Duration, micros int64, lit string) roundTrip {
		return roundTrip{name: name, arg: x, want: value.IntervalValue{Micros: micros}, typ: "INTERVAL", lit: lit}
	}
	runRoundTrips(t, []roundTrip{
		d("zero", 0, 0, "INTERVAL '0 seconds'"),
		d("negative", -90*time.Minute, -5400e6, "INTERVAL '-90 minutes'"),
		d("mixed", 49*time.Hour+3*time.Millisecond, 49*3600e6+3000, "to_microseconds(176400003000)"),
		// Durations are truncated to microseconds.
		d("sub-micro", 1999*time.Nanosecond, 1, "INTERVAL '1 microsecond'"),
		d("negative sub-micro", -1999*time.Nanosecond, -1, "INTERVAL '-1 microsecond'"),
		d("max", math.MaxInt64, math.MaxInt64/1000, "to_microseconds(9223372036854775)"),
		d("min", math.MinInt64, math.MinInt64/1000, "to_microseconds(-9223372036854775)"),
	})
}

func TestRoundTripIntervals(t *testing.T) {
	iv := func(name string, v value.IntervalValue, lit string) roundTrip {
		return roundTrip{name: name, arg: v, want: v, typ: "INTERVAL", lit: lit}
	}
	runRoundTrips(t, []roundTrip{
		iv("months", value.IntervalValue{Months: 14}, "to_months(14)"),
		iv("negative mixed", value.IntervalValue{Months: -1, Days: 2, Micros: -3}, "to_months(-1) + to_days(2) + to_microseconds(-3)"),
		// Unnormalized parts must not be folded into larger units.
		iv("45 days", value.IntervalValue{Days: 45}, "to_days(45)"),
		iv("48 hours", value.IntervalValue{Micros: 48 * 3600e6}, "to_hours(48)"),
		iv("extremes", value.IntervalValue{Months: math.MaxInt32, Days: math.MinInt32, Micros: math.MaxInt64},
			"to_months(2147483647) + to_days(-2147483648) + to_microseconds(9223372036854775807)"),
		iv("minimums", value.IntervalValue{Months: math.MinInt32, Days: math.MaxInt32, Micros: math.MinInt64 + 1},
			"to_months(-2147483648) + to_days(2147483647) + to_microseconds(-9223372036854775807)"),
		iv("micros min", value.IntervalValue{Micros: math.MinInt64},
			"INTERVAL '-9223372036854775807 microseconds -1 microseconds'"),
	})
}

func TestRoundTripTimeTz(t *testing.T) {
	tz := func(name string, v value.TimeTzValue, lit string) roundTrip {
		return roundTrip{name: name, arg: v, want: v, typ: "TIME WITH TIME ZONE", lit: lit}
	}
	runRoundTrips(t, []roundTrip{
		tz("utc", timeTz(12*time.Hour+34*time.Minute+56*time.Second+time.Microsecond, 0), "TIMETZ '12:34:56.000001+00'"),
		tz("negative offset with seconds", timeTz(23*time.Hour+59*time.Minute, -(8*3600+7)), "TIMETZ '23:59:00-08:00:07'"),
		tz("midnight max offset", timeTz(0, 15*3600+59*60+59), "TIMETZ '00:00:00+15:59:59'"),
		tz("24:00 min offset", timeTz(24*time.Hour, -(15*3600+59*60+59)), "TIMETZ '24:00:00-15:59:59'"),
	})
}
