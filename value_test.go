package quackdriver

import (
	"database/sql/driver"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"
)

type intervalValuer struct{ iv IntervalValue }

func (v intervalValuer) Value() (driver.Value, error) { return v.iv, nil }

type ptrValuer struct{ s string }

func (v *ptrValuer) Value() (driver.Value, error) { return v.s, nil }

type failingValuer struct{}

func (failingValuer) Value() (driver.Value, error) { return nil, errors.New("no value") }

type loopValuer struct{}

func (loopValuer) Value() (driver.Value, error) { return loopValuer{}, nil }

type myInt int16

func TestConvertValueTypes(t *testing.T) {
	tz, _ := NewTimeTz(time.Hour, 0)
	bi := big.NewInt(5)
	now := time.Now()
	cases := []struct {
		in, want any
	}{
		{nil, nil},
		{true, true},
		{int(-3), int64(-3)},
		{int8(-3), int64(-3)},
		{int16(-3), int64(-3)},
		{int32(-3), int64(-3)},
		{int64(-3), int64(-3)},
		{uint8(3), int64(3)},
		{uint16(3), int64(3)},
		{uint32(3), int64(3)},
		{uint64(math.MaxUint64), uint64(math.MaxUint64)},
		// A uint above MaxInt64 must not wrap to a negative int64.
		{uint(math.MaxInt64), int64(math.MaxInt64)},
		{uint(math.MaxInt64) + 1, uint64(math.MaxInt64) + 1},
		{uint(math.MaxUint64), uint64(math.MaxUint64)},
		{float32(1.5), float64(1.5)},
		{2.5, 2.5},
		{"s", "s"},
		{now, now},
		{90 * time.Minute, 90 * time.Minute},
		{IntervalValue{Months: 1}, IntervalValue{Months: 1}},
		{tz, tz},
		{bi, bi},
		{intervalValuer{IntervalValue{Days: 2}}, IntervalValue{Days: 2}},
		{&ptrValuer{"p"}, "p"},
		{(*ptrValuer)(nil), nil},
		{myInt(7), int64(7)},
	}
	for _, c := range cases {
		got, err := convertValue(c.in)
		if err != nil {
			t.Errorf("%T: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%T %v: got %T %v, want %T %v", c.in, c.in, got, got, c.want, c.want)
		}
	}
	if got, err := convertValue([]byte{1}); err != nil || got.([]byte)[0] != 1 {
		t.Errorf("[]byte: %v %v", got, err)
	}
	r := big.NewRat(1, 3)
	if got, err := convertValue(r); err != nil || got != r {
		t.Errorf("*big.Rat: %v %v", got, err)
	}
	if _, err := convertValue(failingValuer{}); err == nil {
		t.Error("Valuer error must propagate")
	}
	if _, err := convertValue(loopValuer{}); err == nil {
		t.Error("endless Valuer chain must fail")
	}
	if _, err := convertValue(struct{}{}); err == nil {
		t.Error("struct must be rejected")
	}
	nv := driver.NamedValue{Value: int32(4)}
	if err := (&Conn{}).CheckNamedValue(&nv); err != nil || nv.Value != int64(4) {
		t.Errorf("CheckNamedValue: %v %v", nv.Value, err)
	}
}

func TestNewTimeTz(t *testing.T) {
	tz, err := NewTimeTz(13*time.Hour+30*time.Minute, -3600)
	if err != nil {
		t.Fatal(err)
	}
	if tz.TimeOfDay() != 13*time.Hour+30*time.Minute || tz.OffsetSeconds() != -3600 {
		t.Fatalf("%v %d", tz.TimeOfDay(), tz.OffsetSeconds())
	}
	if _, err := NewTimeTz(25*time.Hour, 0); err == nil {
		t.Fatal("out-of-range time must fail")
	}
}
