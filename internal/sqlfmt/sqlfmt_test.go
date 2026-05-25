package sqlfmt

import (
	"database/sql/driver"
	"math"
	"strings"
	"testing"
	"time"
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
		{int64(-1), "-1"},
		{uint64(math.MaxUint64), "18446744073709551615"},
		{float64(3.5), "3.5"},
		{float64(math.NaN()), "'nan'::DOUBLE"},
		{float64(math.Inf(1)), "'infinity'::DOUBLE"},
		{float64(math.Inf(-1)), "'-infinity'::DOUBLE"},
		{"hello", "'hello'"},
		{"O'Brien", "'O''Brien'"},
		{"", "''"},
		{[]byte{0x01, 0x02, 0xff}, "'\\x0102ff'::BLOB"},
	}
	for _, c := range cases {
		got, err := FormatValue(c.v)
		if err != nil {
			t.Fatalf("%v: %v", c.v, err)
		}
		if got != c.want {
			t.Fatalf("FormatValue(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestFormatValueTime(t *testing.T) {
	in := time.Date(2026, 5, 25, 12, 34, 56, 789_000_000, time.UTC)
	got, err := FormatValue(in)
	if err != nil {
		t.Fatal(err)
	}
	if got != "TIMESTAMP '2026-05-25 12:34:56.789000'" {
		t.Fatalf("timestamp: %q", got)
	}
}

func TestFormatQueryPositional(t *testing.T) {
	q, err := FormatQuery("SELECT * FROM t WHERE id = ? AND name = ? AND raw = ?", []driver.NamedValue{
		{Ordinal: 1, Value: 42},
		{Ordinal: 2, Value: "Bob's"},
		{Ordinal: 3, Value: []byte{0x00, 0xff}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT * FROM t WHERE id = 42 AND name = 'Bob''s' AND raw = '\\x00ff'::BLOB"
	if q != want {
		t.Fatalf("got %q\nwant %q", q, want)
	}
}

func TestFormatQueryQuestionInsideLiteral(t *testing.T) {
	q, err := FormatQuery("SELECT '?' AS lit, ? AS v", []driver.NamedValue{
		{Ordinal: 1, Value: int64(7)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if q != "SELECT '?' AS lit, 7 AS v" {
		t.Fatalf("got %q", q)
	}
}

func TestFormatQueryDoubledQuoteInLiteral(t *testing.T) {
	q, err := FormatQuery("SELECT 'a''?b', ?", []driver.NamedValue{{Ordinal: 1, Value: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if q != "SELECT 'a''?b', 1" {
		t.Fatalf("got %q", q)
	}
}

func TestFormatQueryComments(t *testing.T) {
	q, err := FormatQuery("SELECT 1, -- ?\n?", []driver.NamedValue{{Ordinal: 1, Value: int64(2)}})
	if err != nil {
		t.Fatal(err)
	}
	if q != "SELECT 1, -- ?\n2" {
		t.Fatalf("got %q", q)
	}

	q, err = FormatQuery("/* ? hidden */SELECT ?", []driver.NamedValue{{Ordinal: 1, Value: int64(9)}})
	if err != nil {
		t.Fatal(err)
	}
	if q != "/* ? hidden */SELECT 9" {
		t.Fatalf("got %q", q)
	}
}

func TestFormatQueryMismatches(t *testing.T) {
	if _, err := FormatQuery("SELECT ?", nil); err == nil {
		t.Fatal("expected too-few-args error")
	}
	if _, err := FormatQuery("SELECT 1", []driver.NamedValue{{Ordinal: 1, Value: 1}}); err == nil {
		t.Fatal("expected too-many-args error")
	}
	if _, err := FormatQuery("SELECT ?", []driver.NamedValue{{Name: "x", Value: 1}}); err == nil {
		t.Fatal("expected named-parameter error")
	}
}

func TestFormatStringFuzzNoUnescapedQuotes(t *testing.T) {
	// Sanity-check: any single quote in the input must become two single
	// quotes in the literal — i.e. the output must contain no lone `'` between
	// the outer quotes.
	cases := []string{
		"a'b",
		"a''b",
		"a'''",
		"'''",
		string([]byte{0x00, 0x27, 0xff}),
		strings.Repeat("'", 64),
		"normal string with no quotes",
	}
	for _, c := range cases {
		got, err := FormatValue(c)
		if err != nil {
			t.Fatalf("%q: %v", c, err)
		}
		if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
			t.Fatalf("%q: not enclosed in single quotes: %q", c, got)
		}
		inner := got[1 : len(got)-1]
		// Every embedded `'` must be paired.
		for i := 0; i < len(inner); i++ {
			if inner[i] != '\'' {
				continue
			}
			if i+1 >= len(inner) || inner[i+1] != '\'' {
				t.Fatalf("%q -> %q: lone single quote at offset %d", c, got, i+1)
			}
			i++ // skip the paired quote
		}
	}
}
