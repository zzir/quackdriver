package message

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestIntervalValueStringExtreme(t *testing.T) {
	// Regression: MinInt64 micros used to negate to MinInt64 (wrap-around),
	// producing garbage HH:MM:SS digits. Now saturates and still produces a
	// well-formed string with the leading "-" sign.
	iv := IntervalValue{Micros: math.MinInt64}
	s := iv.String()
	if !strings.HasPrefix(s, "-") {
		t.Fatalf("MinInt64 micros: expected leading '-', got %q", s)
	}
	if strings.ContainsAny(s, "?") {
		t.Fatalf("MinInt64 micros produced malformed output: %q", s)
	}

	// Large positive value that would overflow time.Duration(micros) *
	// time.Microsecond (the old implementation), now computed on int64
	// directly.
	iv = IntervalValue{Micros: math.MaxInt64}
	s = iv.String()
	if strings.HasPrefix(s, "-") {
		t.Fatalf("MaxInt64 micros should be positive, got %q", s)
	}
}

func TestIntervalValueStringNormal(t *testing.T) {
	cases := []struct {
		iv   IntervalValue
		want string
	}{
		{IntervalValue{Months: 1, Days: 2, Micros: int64(3*time.Hour + 4*time.Minute + 5*time.Second + 678*time.Millisecond) / 1000},
			"1 month 2 days 03:04:05.678000"},
		{IntervalValue{Months: 0, Days: 0, Micros: 0}, "00:00:00"},
		{IntervalValue{Months: 2, Days: 0, Micros: 0}, "2 months"},
		{IntervalValue{Micros: -int64(time.Second / 1000)}, "-00:00:01"},
	}
	for _, c := range cases {
		got := c.iv.String()
		if got != c.want {
			t.Errorf("IntervalValue %+v: got %q, want %q", c.iv, got, c.want)
		}
	}
}
