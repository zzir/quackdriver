package value

import (
	"math"
	"math/big"
	"testing"
	"time"
)

func TestIntervalValueStringNormal(t *testing.T) {
	cases := []struct {
		iv   IntervalValue
		want string
	}{
		{IntervalValue{Months: 1, Days: 2, Micros: int64(3*time.Hour+4*time.Minute+5*time.Second+678*time.Millisecond) / 1000},
			"1 month 2 days 03:04:05.678"},
		{IntervalValue{Months: 0, Days: 0, Micros: 0}, "00:00:00"},
		{IntervalValue{Months: 2, Days: 0, Micros: 0}, "2 months"},
		{IntervalValue{Micros: -int64(time.Second / 1000)}, "-00:00:01"},
		// Renderings taken from a DuckDB 2.0 server (INTERVAL::VARCHAR).
		{IntervalValue{Months: 14, Days: 3, Micros: 3_600_000_000}, "1 year 2 months 3 days 01:00:00"},
		{IntervalValue{Months: -12, Days: -2, Micros: -1_500_000}, "-1 year -2 days -00:00:01.5"},
		{IntervalValue{Months: -14}, "-1 year -2 months"},
		{IntervalValue{Months: 1, Days: -1}, "1 month -1 day"},
		{IntervalValue{Micros: 2562047788 * 3_600_000_000}, "2562047788:00:00"},
		{IntervalValue{Micros: math.MaxInt64}, "2562047788:00:54.775807"},
		{IntervalValue{Micros: math.MinInt64}, "-2562047788:00:54.775808"},
		{IntervalValue{Months: math.MaxInt32}, "178956970 years 7 months"},
		{IntervalValue{Months: math.MinInt32 + 1, Days: math.MinInt32 + 1}, "-178956970 years -7 months -2147483647 days"},
		{IntervalValue{Micros: 1}, "00:00:00.000001"},
	}
	for _, c := range cases {
		got := c.iv.String()
		if got != c.want {
			t.Errorf("IntervalValue %+v: got %q, want %q", c.iv, got, c.want)
		}
	}
}

func TestTimeTzRoundTrip(t *testing.T) {
	cases := []struct {
		tod    time.Duration
		offset int32
		want   string
	}{
		{12*time.Hour + 34*time.Minute + 56*time.Second, 0, "12:34:56+00"},
		{12*time.Hour + 34*time.Minute + 56*time.Second + 500*time.Millisecond, 5*3600 + 30*60, "12:34:56.5+05:30"},
		{time.Hour, -8 * 3600, "01:00:00-08"},
		{0, 15*3600 + 59*60 + 59, "00:00:00+15:59:59"},
		{24 * time.Hour, -(15*3600 + 59*60 + 59), "24:00:00-15:59:59"},
	}
	for _, c := range cases {
		v, err := NewTimeTz(c.tod, c.offset)
		if err != nil {
			t.Fatalf("NewTimeTz(%s, %d): %v", c.tod, c.offset, err)
		}
		if v.TimeOfDay() != c.tod || v.OffsetSeconds() != c.offset {
			t.Fatalf("round trip: got (%s, %d), want (%s, %d)", v.TimeOfDay(), v.OffsetSeconds(), c.tod, c.offset)
		}
		if got := v.String(); got != c.want {
			t.Fatalf("String() = %q, want %q", got, c.want)
		}
	}
	// A UTC offset of zero encodes as MAX_OFFSET in the low 24 bits.
	v, _ := NewTimeTz(0, 0)
	if v.Bits() != 57599 {
		t.Fatalf("zero offset bits = %d, want 57599", v.Bits())
	}
	// The zero value is midnight UTC.
	var zero TimeTzValue
	if zero != v || zero.String() != "00:00:00+00" || zero.OffsetSeconds() != 0 || zero.Validate() != nil {
		t.Fatalf("zero value %s (%d)", zero, zero.OffsetSeconds())
	}
	if TimeTzFromBits(57599) != zero || TimeTzFromBits(12345).Bits() != 12345 {
		t.Fatal("TimeTzFromBits round trip")
	}
	for _, bits := range []uint64{
		uint64(25*3600*1_000_000)<<24 | 57599, // 25:00:00+00
		uint64(3600*1_000_000)<<24 | 0xFFFFFF, // offset -4644:20:16
		math.MaxUint64,
	} {
		if err := TimeTzFromBits(bits).Validate(); err == nil {
			t.Errorf("bits %#x must be invalid", bits)
		}
	}
	for _, bits := range []uint64{0, uint64(24*3600*1_000_000)<<24 | 115198} {
		if err := TimeTzFromBits(bits).Validate(); err != nil {
			t.Errorf("bits %#x: %v", bits, err)
		}
	}
	if _, err := NewTimeTz(0, 16*3600); err == nil {
		t.Fatal("expected out-of-range offset error")
	}
}

func TestTemporalSentinels(t *testing.T) {
	if !InfinityTime.Equal(time.Date(5881580, 7, 11, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("InfinityTime = %v", InfinityTime)
	}
	if !NegInfinityTime.Equal(time.Date(-5877641, 6, 24, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("NegInfinityTime = %v", NegInfinityTime)
	}
	if got := DateFromDays(math.MaxInt32); !got.Equal(InfinityTime) {
		t.Errorf("DateFromDays(max) = %v", got)
	}
	if got := DateFromDays(-math.MaxInt32); !got.Equal(NegInfinityTime) {
		t.Errorf("DateFromDays(-max) = %v", got)
	}
	// The finite DATE extremes ('5881580-07-10', '5877642-06-25 (BC)').
	if got := DateFromDays(math.MaxInt32 - 1); !got.Equal(time.Date(5881580, 7, 10, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("max finite date = %v", got)
	}
	if got := DateFromDays(-math.MaxInt32 + 1); !got.Equal(time.Date(-5877641, 6, 25, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("min finite date = %v", got)
	}
	for _, unit := range []int64{Seconds, Milliseconds, Microseconds, Nanoseconds} {
		if got := TimestampFromUnits(math.MaxInt64, unit); !got.Equal(InfinityTime) {
			t.Errorf("unit %d: +infinity = %v", unit, got)
		}
		if got := TimestampFromUnits(-math.MaxInt64, unit); !got.Equal(NegInfinityTime) {
			t.Errorf("unit %d: -infinity = %v", unit, got)
		}
		if got := TimestampFromUnits(-1, unit); !got.Equal(time.Unix(0, -1e9/unit)) || got.Location() != time.UTC {
			t.Errorf("unit %d: -1 = %v", unit, got)
		}
	}
	cases := []struct {
		v, unit int64
		want    time.Time
	}{
		{-9223372022400000000, Microseconds, time.Date(-290308, 12, 22, 0, 0, 0, 0, time.UTC)},
		{math.MaxInt64 - 1, Microseconds, time.Date(294247, 1, 10, 4, 0, 54, 775806000, time.UTC)},
		{math.MinInt64 + 2, Nanoseconds, time.Date(1677, 9, 21, 0, 12, 43, 145224194, time.UTC)},
		{-9223372022400, Seconds, time.Date(-290308, 12, 22, 0, 0, 0, 0, time.UTC)},
		{-1500, Milliseconds, time.Date(1969, 12, 31, 23, 59, 58, 500000000, time.UTC)},
		// Impossible values clamp instead of wrapping around.
		{math.MaxInt64 - 1, Seconds, InfinityTime},
		{math.MinInt64, Seconds, NegInfinityTime},
	}
	for _, c := range cases {
		if got := TimestampFromUnits(c.v, c.unit); !got.Equal(c.want) {
			t.Errorf("TimestampFromUnits(%d, %d) = %v, want %v", c.v, c.unit, got, c.want)
		}
	}
}

// TestUnixRounded checks DuckDB's precision-lowering rule: nearest unit,
// halves away from the epoch (TryCastTimestampBase).
func TestUnixRounded(t *testing.T) {
	for _, c := range []struct {
		t         time.Time
		perSecond int64
		want      int64
	}{
		{time.Unix(0, 500_000_000), 1, 1},
		{time.Unix(0, 499_999_999), 1, 0},
		{time.Unix(-1, 500_000_000), 1, -1},
		{time.Unix(-1, 500_000_001), 1, 0},
		{time.Unix(-1, 499_999_999), 1, -1},
		{time.Unix(12, 789_000_000), 1, 13},
		{time.Unix(-1, 999_999_999), 1e3, 0},
		{time.Unix(-1, 998_500_000), 1e3, -2},
		{time.Unix(0, 1_500_000), 1e3, 2},
		{time.Unix(0, 1500), 1e6, 2},
		{time.Unix(0, 1499), 1e6, 1},
		{time.Unix(-1, 999_998_500), 1e6, -2},
		{time.Unix(-5, 123), 1e9, -4_999_999_877},
	} {
		if got, ok := UnixRounded(c.t, c.perSecond); !ok || got != c.want {
			t.Errorf("UnixRounded(%s, %d) = %d %v, want %d", c.t.UTC().Format(time.RFC3339Nano), c.perSecond, got, ok, c.want)
		}
	}
	if _, ok := UnixRounded(time.Unix(math.MaxInt64/1000, 0), 1e6); ok {
		t.Error("overflow not reported")
	}
	if _, ok := UnixRounded(time.Unix(math.MinInt64/1000, 0), 1e6); ok {
		t.Error("negative overflow not reported")
	}
}

func TestWireConversions(t *testing.T) {
	if got := UUIDString(0x550e8400e29b41d4-(1<<63), 0xa716446655440000); got != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("UUIDString = %q", got)
	}
	if got := UUIDString(math.MinInt64, 0); got != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("UUIDString(zero) = %q", got)
	}
	if got := HugeInt(-1, 0); got.String() != "-18446744073709551616" {
		t.Errorf("HugeInt = %s", got)
	}
	if got := UHugeInt(math.MaxUint64, math.MaxUint64); got.String() != "340282366920938463463374607431768211455" {
		t.Errorf("UHugeInt = %s", got)
	}
	for _, c := range []struct {
		v     int64
		scale int
		want  string
	}{
		{12345, 2, "123.45"}, {-5, 3, "-0.005"}, {0, 2, "0.00"}, {42, 0, "42"},
		{math.MinInt64, 3, "-9223372036854775.808"},
	} {
		if got := FormatDecimal64(c.v, c.scale); got != c.want {
			t.Errorf("FormatDecimal64(%d, %d) = %q, want %q", c.v, c.scale, got, c.want)
		}
		if got := FormatDecimal(big.NewInt(c.v), c.scale); got != c.want {
			t.Errorf("FormatDecimal(%d, %d) = %q, want %q", c.v, c.scale, got, c.want)
		}
	}

	// BIT: padding count, then the bits MSB first with the padding bits set.
	for _, c := range []struct {
		in   []byte
		want string // "" means invalid
	}{
		{[]byte{4, 0xf5}, "0101"}, {[]byte{0, 0x81}, "10000001"}, {[]byte{7, 0xff}, "1"},
		{[]byte{7, 0xff, 0x67}, "101100111"}, {[]byte{4}, ""}, {[]byte{8, 0xff}, ""},
	} {
		got, err := BitString(c.in)
		if got != c.want || (err != nil) != (c.want == "") {
			t.Errorf("BitString(% x) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}

	// BIGNUM: byte count | 0x800000, then the magnitude; both inverted when negative.
	two64, _ := new(big.Int).SetString("18446744073709551616", 10)
	for _, c := range []struct {
		in   []byte
		want *big.Int // nil means invalid
	}{
		{[]byte{0x80, 0x00, 0x01, 0x00}, big.NewInt(0)},
		{[]byte{0x80, 0x00, 0x01, 0x01}, big.NewInt(1)},
		{[]byte{0x7f, 0xff, 0xfe, 0xfe}, big.NewInt(-1)},
		{[]byte{0x7f, 0xff, 0xfd, 0xfe, 0xff}, big.NewInt(-256)},
		{[]byte{0x80, 0x00, 0x09, 0x01, 0, 0, 0, 0, 0, 0, 0, 0}, two64},
		{[]byte{0x80, 0x00, 0x01}, nil},
		{[]byte{0x80, 0x00, 0x02, 0x01}, nil},
	} {
		got, err := Bignum(c.in)
		if (c.want == nil) != (err != nil) || (c.want != nil && got.Cmp(c.want) != 0) {
			t.Errorf("Bignum(% x) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
}
