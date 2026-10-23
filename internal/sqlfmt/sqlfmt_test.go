package sqlfmt

import (
	"database/sql/driver"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/value"
)

func TestFormatValueScalars(t *testing.T) {
	cases := []struct {
		v    driver.Value
		want string
	}{
		{nil, "NULL"},
		{true, "TRUE"},
		{false, "FALSE"},
		{int(42), "42"},
		{int8(-8), "(-8)"},
		{int16(16), "16"},
		{int32(-32), "(-32)"},
		{int64(-1), "(-1)"},
		{int64(math.MinInt64), "(-9223372036854775808)"},
		{uint(7), "7"},
		{uint8(255), "255"},
		{uint16(65535), "65535"},
		{uint32(math.MaxUint32), "4294967295"},
		{uint64(math.MaxUint64), "18446744073709551615"},
		{float64(3.5), "3.5e+00"},
		{float64(-3.5), "(-3.5e+00)"},
		{float64(1e300), "1e+300"},
		{float64(-1e-300), "(-1e-300)"},
		{float64(0), "0e+00"},
		{math.Copysign(0, -1), "'-0.0'::DOUBLE"},
		{float32(0.5), "5e-01"},
		{float64(math.NaN()), "'nan'::DOUBLE"},
		{float64(math.Inf(1)), "'infinity'::DOUBLE"},
		{float64(math.Inf(-1)), "'-infinity'::DOUBLE"},
		{"hello", "'hello'"},
		{"O'Brien", "'O''Brien'"},
		{"", "''"},
		{`back\slash`, `'back\slash'`},
		{[]byte{0x01, 0x02, 0xff}, `'\x01\x02\xff'::BLOB`},
		{[]byte{}, "''::BLOB"},
		{[]byte(nil), "NULL"}, // what database/sql scans a NULL into
		{big.NewInt(-5), "(-5)"},
		{*big.NewInt(12), "12"},
		{(*big.Int)(nil), "NULL"},
		{big.NewRat(-1, 4), "(-.25)"},
		{big.NewRat(5, 4), "1.25"},
		{big.NewRat(84, 2), "42"},
		{big.NewRat(-1, 3), "(-.33333333333333333333333333333333333333)"},
		{big.NewRat(20, 3), "6.6666666666666666666666666666666666667"},
		{new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(50), nil)), ".0"},
		{new(big.Rat).SetFrac(new(big.Int).Sub(new(big.Int).Exp(big.NewInt(10), big.NewInt(38), nil), big.NewInt(1)), big.NewInt(10)), "9999999999999999999999999999999999999.9"},
		{new(big.Rat).SetFrac(new(big.Int).Sub(new(big.Int).Exp(big.NewInt(10), big.NewInt(39), nil), big.NewInt(1)), big.NewInt(100)), "10000000000000000000000000000000000000"},
		{new(big.Rat).SetFrac(new(big.Int).Exp(big.NewInt(10), big.NewInt(40), nil), big.NewInt(1)), "1" + strings.Repeat("0", 40)},
		{(*big.Rat)(nil), "NULL"},
		// The times DuckDB's ±infinity decode to, in any time zone.
		{value.InfinityTime, "TIMESTAMP 'infinity'"},
		{value.NegInfinityTime.In(time.FixedZone("", 3600)), "TIMESTAMP '-infinity'"},
	}
	for _, c := range cases {
		got, err := FormatValue(c.v)
		if err != nil {
			t.Fatalf("%#v: %v", c.v, err)
		}
		if got != c.want {
			t.Fatalf("FormatValue(%#v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestFormatValueErrors(t *testing.T) {
	for _, v := range []driver.Value{
		struct{}{},
		// 38 integer digits leave no room for a fraction in DECIMAL(38).
		new(big.Rat).SetFrac(new(big.Int).Add(new(big.Int).Exp(big.NewInt(10), big.NewInt(38), nil), big.NewInt(1)), big.NewInt(10)),
		// Sub-microsecond precision outside the TIMESTAMP_NS range.
		time.Date(2300, 1, 1, 0, 0, 0, 1, time.UTC),
		time.Date(1600, 1, 1, 0, 0, 0, 1, time.UTC),
		time.Date(-100, 1, 1, 0, 0, 0, 1, time.UTC),
		// TIMETZ bit patterns DuckDB cannot represent.
		value.TimeTzFromBits(uint64(24*3600*1_000_000+1)<<24 | maxTimeTzOffset),
		value.TimeTzFromBits(1<<24 | (1<<24 - 1)), // offset beyond -15:59:59
	} {
		if got, err := FormatValue(v); err == nil || !strings.HasPrefix(err.Error(), "sqlfmt: ") {
			t.Errorf("FormatValue(%v) = %q, %v; want an error", v, got, err)
		}
	}
}

func TestFormatValueTimestamp(t *testing.T) {
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 5, 25, 12, 34, 56, 789_000_000, time.UTC), "TIMESTAMP '2026-05-25 12:34:56.789000'"},
		{time.Date(2026, 5, 25, 12, 34, 56, 789_001_000, time.UTC), "TIMESTAMP '2026-05-25 12:34:56.789001'"},
		{time.Date(2026, 5, 25, 12, 34, 56, 789_001_234, time.UTC), "TIMESTAMP_NS '2026-05-25 12:34:56.789001234'"},
		{time.Date(1970, 1, 1, 0, 0, 0, 1, time.UTC), "TIMESTAMP_NS '1970-01-01 00:00:00.000000001'"},
		// Converted to the UTC wall clock.
		{time.Date(2026, 1, 1, 1, 0, 0, 0, time.FixedZone("", 2*3600)), "TIMESTAMP '2025-12-31 23:00:00.000000'"},
		{time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), "TIMESTAMP '0001-01-01 00:00:00.000000'"},
		{time.Date(9999, 12, 31, 23, 59, 59, 999_999_000, time.UTC), "TIMESTAMP '9999-12-31 23:59:59.999999'"},
		{time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), "TIMESTAMP '10000-01-01 00:00:00.000000'"},
		{time.Date(294246, 12, 31, 0, 0, 0, 0, time.UTC), "TIMESTAMP '294246-12-31 00:00:00.000000'"},
		// Go year 0 is 1 BC; year -43 is 44 BC.
		{time.Date(0, 12, 31, 23, 59, 59, 0, time.UTC), "TIMESTAMP '0001-12-31 (BC) 23:59:59.000000'"},
		{time.Date(-43, 3, 15, 12, 0, 0, 0, time.UTC), "TIMESTAMP '0044-03-15 (BC) 12:00:00.000000'"},
		{time.Date(-290307, 1, 1, 0, 0, 0, 0, time.UTC), "TIMESTAMP '290308-01-01 (BC) 00:00:00.000000'"},
		// The TIMESTAMP_NS extremes, and whole microseconds beyond them.
		{time.Unix(0, math.MaxInt64-1), "TIMESTAMP_NS '2262-04-11 23:47:16.854775806'"},
		{time.Unix(0, -math.MaxInt64+1), "TIMESTAMP_NS '1677-09-21 00:12:43.145224194'"},
		{time.Date(2300, 1, 1, 0, 0, 0, 1000, time.UTC), "TIMESTAMP '2300-01-01 00:00:00.000001'"},
	}
	for _, c := range cases {
		got, err := FormatValue(c.in)
		if err != nil {
			t.Fatalf("%v: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("FormatValue(%v) = %q, want %q", c.in, got, c.want)
		}
		// DuckDB's date parser must read back the same instant.
		back, ok := parseDuckTimestampLiteral(got)
		if !ok || !back.Equal(c.in) {
			t.Fatalf("%q parses back as %v (ok=%v), want %v", got, back, ok, c.in)
		}
	}
}

func TestFormatValueTimestampRoundTripRandom(t *testing.T) {
	r := newRand(t)
	for range 5000 {
		sec := r.Int63n(2*9_000_000_000_000) - 9_000_000_000_000 // about ±285000 years
		nsec := r.Int63n(1_000_000_000)
		if r.Intn(2) == 0 {
			nsec -= nsec % 1000
		}
		tm := time.Unix(sec, nsec).UTC()
		got, err := FormatValue(tm)
		if err != nil {
			continue // sub-microsecond value outside the TIMESTAMP_NS range
		}
		back, ok := parseDuckTimestampLiteral(got)
		if !ok || !back.Equal(tm) {
			t.Fatalf("%v -> %q -> %v (ok=%v)", tm, got, back, ok)
		}
	}
}

func TestFormatValueInterval(t *testing.T) {
	cases := []struct {
		in   driver.Value
		want string
	}{
		{value.IntervalValue{}, "INTERVAL '0 microseconds'"},
		{value.IntervalValue{Months: 14, Days: 3, Micros: 4_000_005}, "INTERVAL '14 months 3 days 4000005 microseconds'"},
		{value.IntervalValue{Months: -1}, "INTERVAL '-1 months'"},
		{value.IntervalValue{Days: -2, Micros: 1}, "INTERVAL '-2 days 1 microseconds'"},
		{value.IntervalValue{Months: math.MinInt32, Days: math.MinInt32, Micros: math.MinInt64},
			"INTERVAL '-2147483648 months -2147483648 days -9223372036854775807 microseconds -1 microseconds'"},
		{value.IntervalValue{Months: math.MaxInt32, Days: math.MaxInt32, Micros: math.MaxInt64},
			"INTERVAL '2147483647 months 2147483647 days 9223372036854775807 microseconds'"},
		{time.Duration(0), "INTERVAL '0 microseconds'"},
		{90*time.Minute + 1500*time.Nanosecond, "INTERVAL '5400000001 microseconds'"},
		{-time.Second - 999*time.Nanosecond, "INTERVAL '-1000000 microseconds'"},
		{time.Duration(math.MinInt64), "INTERVAL '-9223372036854775 microseconds'"},
	}
	for _, c := range cases {
		got, err := FormatValue(c.in)
		if err != nil {
			t.Fatalf("%v: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("FormatValue(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatValueIntervalRoundTrip(t *testing.T) {
	r := newRand(t)
	check := func(iv value.IntervalValue) {
		t.Helper()
		lit, err := FormatValue(iv)
		if err != nil {
			t.Fatal(err)
		}
		body, ok := strings.CutPrefix(lit, "INTERVAL ")
		if !ok {
			t.Fatalf("%q: missing INTERVAL prefix", lit)
		}
		s, ok := decodeStandardString(body)
		if !ok {
			t.Fatalf("%q: not a string literal", lit)
		}
		back, ok := parseDuckInterval(s)
		if !ok || back != iv {
			t.Fatalf("%+v -> %q -> %+v (ok=%v)", iv, lit, back, ok)
		}
	}
	edges32 := []int32{0, 1, -1, math.MaxInt32, math.MinInt32}
	edges64 := []int64{0, 1, -1, math.MaxInt64, math.MinInt64, math.MinInt64 + 1}
	for _, m := range edges32 {
		for _, d := range edges32 {
			for _, us := range edges64 {
				check(value.IntervalValue{Months: m, Days: d, Micros: us})
			}
		}
	}
	for range 5000 {
		check(value.IntervalValue{Months: int32(r.Uint32()), Days: int32(r.Uint32()), Micros: int64(r.Uint64())})
	}
}

func TestFormatValueTimeTz(t *testing.T) {
	mk := func(tod time.Duration, off int32) value.TimeTzValue {
		v, err := value.NewTimeTz(tod, off)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		in   value.TimeTzValue
		want string
	}{
		{mk(12*time.Hour+34*time.Minute+56*time.Second+1*time.Microsecond, 5*3600+30*60), "TIMETZ '12:34:56.000001+05:30:00'"},
		{mk(0, 0), "TIMETZ '00:00:00.000000+00:00:00'"},
		{mk(24*time.Hour, -(15*3600 + 59*60 + 59)), "TIMETZ '24:00:00.000000-15:59:59'"},
		{mk(time.Second, -1), "TIMETZ '00:00:01.000000-00:00:01'"},
		{mk(23*time.Hour+59*time.Minute+59*time.Second+999999*time.Microsecond, 15*3600+59*60+59), "TIMETZ '23:59:59.999999+15:59:59'"},
	}
	for _, c := range cases {
		got, err := FormatValue(c.in)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Fatalf("FormatValue(%v) = %q, want %q", c.in, got, c.want)
		}
		back, ok := parseDuckTimeTzLiteral(t, got)
		if !ok || back != c.in {
			t.Fatalf("%q parses back as %v (ok=%v), want %v", got, back, ok, c.in)
		}
	}

	r := newRand(t)
	for range 5000 {
		tod := time.Duration(r.Int63n(24*3600*1_000_000+1)) * time.Microsecond
		off := int32(r.Intn(2*maxTimeTzOffset+1) - maxTimeTzOffset)
		v := mk(tod, off)
		got, err := FormatValue(v)
		if err != nil {
			t.Fatal(err)
		}
		back, ok := parseDuckTimeTzLiteral(t, got)
		if !ok || back != v {
			t.Fatalf("%v -> %q -> %v (ok=%v)", v, got, back, ok)
		}
	}
}

func nv(vals ...driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(vals))
	for i, v := range vals {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

func TestFormatQuery(t *testing.T) {
	named := func(name string, v driver.Value) driver.NamedValue {
		return driver.NamedValue{Name: name, Value: v}
	}
	cases := []struct {
		name  string
		query string
		args  []driver.NamedValue
		want  string
	}{
		{"positional", "SELECT * FROM t WHERE id = ? AND name = ? AND raw = ?",
			nv(42, "Bob's", []byte{0x00, 0xff}),
			`SELECT * FROM t WHERE id = 42 AND name = 'Bob''s' AND raw = '\x00\xff'::BLOB`},
		{"no args", "SELECT 1", nil, "SELECT 1"},
		{"no args keeps unmatched named", "SELECT $x", nil, "SELECT $x"},
		{"question inside literal", "SELECT '?' AS lit, ? AS v", nv(int64(7)), "SELECT '?' AS lit, 7 AS v"},
		{"doubled quote", "SELECT 'a''?b', ?", nv(1), "SELECT 'a''?b', 1"},
		{"line comment", "SELECT 1, -- ?\n?", nv(int64(2)), "SELECT 1, -- ?\n2"},
		{"line comment cr", "SELECT 1, -- ?\r?", nv(int64(2)), "SELECT 1, -- ?\r2"},
		{"block comment", "/* ? hidden */SELECT ?", nv(int64(9)), "/* ? hidden */SELECT 9"},
		{"nested block comment", "/* a /* ? */ ? */ SELECT ?", nv(1), "/* a /* ? */ ? */ SELECT 1"},
		{"double-quoted identifier", `SELECT "a?""?" , ?`, nv(1), `SELECT "a?""?" , 1`},
		{"backtick identifier", "SELECT `a?``?` , ?", nv(1), "SELECT `a?``?` , 1"},
		{"dollar quote", "SELECT $$ ? ' $$, ?", nv(1), "SELECT $$ ? ' $$, 1"},
		{"tagged dollar quote", "SELECT $tag$ $$ ? $x$ $tag$, ?", nv(1), "SELECT $tag$ $$ ? $x$ $tag$, 1"},
		{"dollar quote with digits in tag", "SELECT $a1$ ? $a1$, ?", nv(1), "SELECT $a1$ ? $a1$, 1"},
		{"escape string", `SELECT E'\' ?', ?`, nv(1), `SELECT E'\' ?', 1`},
		{"escape string lower", `SELECT e'\\', ?`, nv(1), `SELECT e'\\', 1`},
		{"escape string after number", `SELECT 1E'\' ?', ?`, nv(1), `SELECT 1E'\' ?', 1`},
		{"not an escape string", `SELECT xE'\', ?`, nv(1), `SELECT xE'\', 1`},
		{"national string", `SELECT N'\', ?`, nv(1), `SELECT N'\', 1`},
		{"hex and bit strings", `SELECT X'0a', B'01', ?`, nv(1), `SELECT X'0a', B'01', 1`},
		{"numbered dollar", "SELECT $2, $1, $2", nv("a", "b"), "SELECT 'b', 'a', 'b'"},
		{"numbered question", "SELECT ?2 + ?1", nv(1, 2), "SELECT 2 + 1"},
		{"numbered leading zero", "SELECT $01", nv(1), "SELECT 1"},
		{"named", "SELECT $foo, $bar, $foo", []driver.NamedValue{named("foo", 1), named("bar", "x")}, "SELECT 1, 'x', 1"},
		{"named case-insensitive", "SELECT $Foo", []driver.NamedValue{named("fOO", 1)}, "SELECT 1"},
		{"unknown named untouched", "SELECT $session_var, ?", nv(1), "SELECT $session_var, 1"},
		{"named with sequential", "SELECT ?, $n, ?", []driver.NamedValue{{Ordinal: 1, Value: 1}, named("n", 2), {Ordinal: 3, Value: 3}}, "SELECT 1, 2, 3"},
		{"dollar inside identifier", "SELECT a$1, x$y, ?", nv(1), "SELECT a$1, x$y, 1"},
		{"bare dollar", "SELECT $ , ?", nv(1), "SELECT $ , 1"},
		{"trailing dollar", "SELECT ?, $", nv(1), "SELECT 1, $"},
		{"in list", "SELECT * FROM t WHERE x IN (?,?)", nv(1, 2), "SELECT * FROM t WHERE x IN (1,2)"},
		{"cast", "SELECT ?::INTEGER", nv(-5), "SELECT (-5)::INTEGER"},
		{"struct literal", "SELECT {'a': ?}", nv("v"), "SELECT {'a': 'v'}"},
		{"array literal", "SELECT [?]", nv(1), "SELECT [1]"},
		{"eq without spaces", "SELECT x=?", nv(1), "SELECT x=1"},
		{"semicolon", "SELECT ?;", nv(1), "SELECT 1;"},
		{"unterminated comment swallows", "SELECT 1 /* ?", nil, "SELECT 1 /* ?"},
		{"unterminated string swallows", "SELECT '?", nil, "SELECT '?"},
		{"unterminated dollar swallows", "SELECT $a$ ?", nil, "SELECT $a$ ?"},
		{"comment marker inside operator", "SELECT 1+-- ?\n?", nv(2), "SELECT 1+-- ?\n2"},
		{"block marker inside operator", "SELECT 1+/* ? */?", nv(2), "SELECT 1+/* ? */ 2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FormatQuery(c.query, c.args)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

// TestFormatQuerySeparation checks that a substituted literal never fuses
// with the text around it into a different token.
func TestFormatQuerySeparation(t *testing.T) {
	cases := []struct {
		query string
		args  []driver.NamedValue
		want  string
	}{
		// E followed by a string literal would start a backslash-escape string.
		{"SELECT E?", nv(`\'; DROP TABLE t; --`), `SELECT E '\''; DROP TABLE t; --'`},
		{"SELECT e?", nv("x"), "SELECT e 'x'"},
		{"SELECT N?", nv("x"), "SELECT N 'x'"},
		// "-" followed by a negative number would start a comment.
		{"SELECT 1-?", nv(-5), "SELECT 1- (-5)"},
		{"SELECT 1-?", nv(5), "SELECT 1- 5"},
		// Adjacent placeholders and numbers.
		{"SELECT ??", nv(1, 2), "SELECT 1 2"},
		{"SELECT 1?", nv(2), "SELECT 1 2"},
		{"SELECT ?abc", nv(true), "SELECT TRUE abc"},
		{"SELECT ?.5", nv(1), "SELECT 1 .5"},
		{"SELECT $?", nv(1), "SELECT $ 1"},
		{"SELECT #?", nv(1), "SELECT # 1"},
		{"SELECT ?'x'", nv("a"), "SELECT 'a' 'x'"},
		{"SELECT '?'?", nv("a"), "SELECT '?' 'a'"},
		{"SELECT $x$y$x$?", nv(1), "SELECT $x$y$x$ 1"},
		{"SELECT ?$foo", []driver.NamedValue{{Ordinal: 1, Value: 1}, {Name: "foo", Value: 2}}, "SELECT 1 2"},
		{"SELECT ?--c", nv(1), "SELECT 1 --c"},
		{"SELECT ?/*c*/", nv(1), "SELECT 1 /*c*/"},
		{"SELECT x/?", nv(-1), "SELECT x/ (-1)"},
	}
	for _, c := range cases {
		got, err := FormatQuery(c.query, c.args)
		if err != nil {
			t.Fatalf("%q: %v", c.query, err)
		}
		if got != c.want {
			t.Fatalf("%q:\ngot  %q\nwant %q", c.query, got, c.want)
		}
	}
}

func TestFormatQueryErrors(t *testing.T) {
	named := func(name string, v driver.Value) driver.NamedValue {
		return driver.NamedValue{Name: name, Value: v}
	}
	cases := []struct {
		name  string
		query string
		args  []driver.NamedValue
		want  string
	}{
		{"too few", "SELECT ?", nil, "not enough arguments"},
		{"too few with named", "SELECT ?, ?, $x", nv(1), "not enough arguments"},
		{"too few 2", "SELECT ?, ?", nv(1), "not enough arguments"},
		{"too many", "SELECT 1", nv(1), "too many arguments"},
		{"too many 2", "SELECT ?", nv(1, 2), "positional argument 2 is not used"},
		{"numbered gap", "SELECT $2", nv(1, 2), "positional argument 1 is not used"},
		{"numbered missing", "SELECT $3", nv(1, 2), "has no argument"},
		{"numbered question missing", "SELECT ?1", nil, "has no argument"},
		{"numbered question missing 2", "SELECT ?1, ?2", nv(1), "has no argument"},
		{"numbered zero", "SELECT $0", nv(1), "start at 1"},
		{"numbered zero question", "SELECT ?00", nv(1), "start at 1"},
		{"numbered huge", "SELECT $99999999999999999999999", nv(1), "out of range"},
		{"numbered decimal", "SELECT $1.5", nv(1), "malformed numbered parameter"},
		{"numbered exponent", "SELECT ?1e3", nv(1), "malformed numbered parameter"},
		{"numbered separator", "SELECT $1_0", nv(1), "malformed numbered parameter"},
		{"numbered trailing dot", "SELECT $1.x", nv(1), "malformed numbered parameter"},
		{"mixed ? and $N", "SELECT ?, $1", nv(1), "cannot mix"},
		{"mixed ?N and ?", "SELECT ?1, ?", nv(1), "cannot mix"},
		{"named unused", "SELECT ?", []driver.NamedValue{{Ordinal: 1, Value: 1}, named("x", 2)}, `named argument "x" is not used`},
		{"named only, used as ?", "SELECT ?", []driver.NamedValue{named("x", 1)}, "not enough arguments"},
		{"named duplicate", "SELECT $x", []driver.NamedValue{named("x", 1), named("X", 2)}, "duplicate named argument"},
		{"unformattable", "SELECT ?", nv(struct{}{}), "argument 1: sqlfmt: cannot format"},
		{"unformattable named", "SELECT $v", []driver.NamedValue{named("v", struct{}{})}, `argument "v": sqlfmt: cannot format`},
		{"named in comment only", "SELECT 1 -- $x", []driver.NamedValue{named("x", 1)}, "is not used"},
		// Like DuckDB's identifiers, only ASCII letters fold.
		{"named non-ASCII case", "SELECT $é", []driver.NamedValue{named("É", 1)}, "is not used"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FormatQuery(c.query, c.args)
			if err == nil {
				t.Fatalf("expected error containing %q, got query %q", c.want, got)
			}
			if !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "sqlfmt: ") {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

// TestFormatQueryUnicodeSpaces: DuckDB replaces Unicode spaces with ' '
// before tokenizing (Parser::StripUnicodeSpaces), outside what it believes
// are quotes. Queries where that would rewrite a substituted literal, or
// move a token boundary of the template (E'...' or $tag$ strings that
// swallow a substituted literal), are refused.
func TestFormatQueryUnicodeSpaces(t *testing.T) {
	const val = "a\u00a0b\u3000c"
	for _, c := range []struct{ q, arg, want string }{
		{"SELECT ?", val, "SELECT '" + val + "'"},
		// A quote the normalizer misreads puts the literal "outside quotes".
		{"/* it's */ SELECT ?", val, ""},
		{`SELECT E'\'', ?`, val, ""},
		{"SELECT `it's`, ?", val, ""},
		{"/* it's */ SELECT ?", "plain", "/* it's */ SELECT 'plain'"},
		// Unicode spaces in the template that change token boundaries.
		{"SELECT\u200bE'\\\\', ? AS v", "; SELECT $$INJECTED$$ AS pwned; --", ""},
		{"SELECT\u00a0$x$, ? AS v", "$x$ AS a; SELECT $$INJECTED$$ AS pwned; --", ""},
		{"SELECT 1, \ufeffE'\\', ? AS v", "x", ""},
		{"SELECT\u2000E'\\\\' , 1, ?", "x", ""},
		// ...and ones that do not.
		{"SELECT\u00a0?", "x", "SELECT\u00a0 'x'"},
		{"SELECT 'a\u00a0b', ?", "x", "SELECT 'a\u00a0b', 'x'"},
		{"SELECT ? /* \u00a0 */", "x", "SELECT 'x' /* \u00a0 */"},
		{"SELECT \"c\u00a0d\" FROM t, ?", "x", "SELECT \"c\u00a0d\" FROM t, 'x'"},
		{"SELECT\u00a0x$y FROM t WHERE ?", "x", "SELECT\u00a0x$y FROM t WHERE 'x'"},
	} {
		got, err := FormatQuery(c.q, nv(c.arg))
		if c.want == "" {
			if err == nil || !strings.Contains(err.Error(), "Unicode space") {
				t.Errorf("%q: got %q, %v; want a Unicode space error", c.q, got, err)
			}
		} else if err != nil || got != c.want {
			t.Errorf("%q: got %q, %v; want %q", c.q, got, err, c.want)
		}
	}
}

func TestUnicodeSpacesPort(t *testing.T) {
	cases := []struct {
		q    string
		want []unicodeSpace
	}{
		{"a\u00a0b  ", []unicodeSpace{{1, 2}}},
		{"x\u2000\u200b\u202f\u205f\u2060\u3000\ufeffy", []unicodeSpace{{1, 3}, {4, 3}, {7, 3}, {10, 3}, {13, 3}, {16, 3}, {19, 3}}},
		{"'\u00a0' \u00a0 ", []unicodeSpace{{5, 2}}},
		{"\"\u00a0\" \u00a0 ", []unicodeSpace{{5, 2}}},
		{"'it''s\u00a0' \u00a0 ", []unicodeSpace{{10, 2}}},
		{"$$\u00a0$$ \u00a0 ", []unicodeSpace{{7, 2}}},
		{"$t$\u00a0$t$ \u00a0 ", []unicodeSpace{{9, 2}}},
		{"-- \u00a0\n\u00a0 ", []unicodeSpace{{6, 2}}},
		{"/* ' */ \u00a0 ' \u00a0 ", []unicodeSpace{{13, 2}}},
		// A '$' followed by a non-tag character is not a dollar quote.
		{"$ \u00a0 ", []unicodeSpace{{2, 2}}},
		// The last two bytes are never examined.
		{"\u00a0", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := unicodeSpaces(c.q)
		if len(got) != len(c.want) {
			t.Fatalf("%q: got %v, want %v", c.q, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%q: got %v, want %v", c.q, got, c.want)
			}
		}
	}
}

// TestFormatQueryStringContinuation: DuckDB joins a '...' literal onto a
// preceding single-quoted literal across a newline, keeping the first one's
// kind. A substituted literal in such a position is parenthesized so that it
// stays a separate expression (and the query the syntax error a bound
// parameter would give).
func TestFormatQueryStringContinuation(t *testing.T) {
	ts := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, c := range []struct {
		q    string
		args []driver.Value
		want string
	}{
		{"SELECT E'x'\n? AS v", nv1(`\x41`), "SELECT E'x'\n('\\x41') AS v"},
		{"SELECT X'41'\n?", nv1("42"), "SELECT X'41'\n('42')"},
		{"SELECT 'a' -- c\n?", nv1("b"), "SELECT 'a' -- c\n('b')"},
		{"SELECT 'a' -- c\n  -- d\n ?", nv1("b"), "SELECT 'a' -- c\n  -- d\n ('b')"},
		{"SELECT 'a'\r?", nv1("b"), "SELECT 'a'\r('b')"},
		{"SELECT ?\n?", []driver.Value{"a", "b"}, "SELECT 'a'\n('b')"},
		{"SELECT ?\n'abc'", nv1("a"), "SELECT ('a')\n'abc'"},
		{"SELECT ? -- c\n'abc'", []driver.Value{ts}, "SELECT (TIMESTAMP '2020-01-02 03:04:05.000000') -- c\n'abc'"},
		{"SELECT ?\n?", []driver.Value{ts, "b"}, "SELECT TIMESTAMP '2020-01-02 03:04:05.000000'\n('b')"},
		{"SELECT E'x'\n?", []driver.Value{[]byte{1}}, "SELECT E'x'\n('\\x01'::BLOB)"},
		// No continuation: same line, a block comment, another token, a
		// literal that does not start (end) with a quote.
		{"SELECT 'a' ?", nv1("b"), "SELECT 'a' 'b'"},
		{"SELECT 'a' /* c */\n?", nv1("b"), "SELECT 'a' /* c */\n'b'"},
		{"SELECT 'a'\n/* c */ ?", nv1("b"), "SELECT 'a'\n/* c */ 'b'"},
		{"SELECT 'a',\n?", nv1("b"), "SELECT 'a',\n'b'"},
		{"SELECT E'x'\n?", nv1(5), "SELECT E'x'\n5"},
		{"SELECT ?\nE'x'", nv1("a"), "SELECT 'a'\nE'x'"},
		{"SELECT ?\n'x'", nv1(5), "SELECT 5\n'x'"},
		{"SELECT ?::BLOB\n'x'", nv1([]byte{1}), "SELECT '\\x01'::BLOB::BLOB\n'x'"},
		{"SELECT $$a$$\n?", nv1("b"), "SELECT $$a$$\n'b'"},
		{"SELECT \"a\"\n?", nv1("b"), "SELECT \"a\"\n'b'"},
		{"-- c\n?", nv1("b"), "-- c\n'b'"},
		{"?", nv1("b"), "'b'"},
	} {
		got, err := FormatQuery(c.q, nv(c.args...))
		if err != nil || got != c.want {
			t.Errorf("%q: got %q, %v; want %q", c.q, got, err, c.want)
		}
	}
}

func nv1(v driver.Value) []driver.Value { return []driver.Value{v} }
