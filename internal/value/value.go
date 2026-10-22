// Package value holds the Go representations of DuckDB values that have no
// natural standard-library counterpart (INTERVAL, TIME WITH TIME ZONE) and
// the conversions from DuckDB's in-memory value layouts that the column
// decoder and the VARIANT decoder share. The public API re-exports the types.
package value

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// IntervalValue mirrors DuckDB's INTERVAL type: months + days + microseconds.
// Months and days are kept separate from micros because their length depends
// on the calendar (months especially).
type IntervalValue struct {
	Months int32
	Days   int32
	Micros int64
}

// ApproxDuration returns a best-effort time.Duration ignoring calendar drift.
// Months are treated as 30 days. Use only for rough comparisons or sorting;
// for calendar-correct math, add Months/Days to a time.Time explicitly.
func (iv IntervalValue) ApproxDuration() time.Duration {
	return time.Duration(int64(iv.Months)*30*24*int64(time.Hour) +
		int64(iv.Days)*24*int64(time.Hour) +
		iv.Micros*int64(time.Microsecond))
}

// String renders the interval exactly like DuckDB's INTERVAL to VARCHAR
// cast (core IntervalToStringCast::Format), e.g.
// "1 year 2 months 3 days 04:05:06.5" or "-1 day -00:00:01". Months are
// split into years and months, each part keeps its own sign, and the time
// part's fraction has its trailing zeros trimmed. The arithmetic runs on
// non-positive micros, so MinInt64 needs no special case.
func (iv IntervalValue) String() string {
	var b strings.Builder
	part := func(v int32, unit string) {
		if v == 0 {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.FormatInt(int64(v), 10))
		b.WriteByte(' ')
		b.WriteString(unit)
		if v != 1 && v != -1 {
			b.WriteByte('s')
		}
	}
	years := iv.Months / 12
	part(years, "year")
	part(iv.Months-years*12, "month")
	part(iv.Days, "day")
	if iv.Micros == 0 {
		if b.Len() == 0 {
			return "00:00:00"
		}
		return b.String()
	}
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	const (
		usPerSec  = int64(1_000_000)
		usPerMin  = 60 * usPerSec
		usPerHour = 60 * usPerMin
	)
	micros := iv.Micros
	if micros < 0 {
		b.WriteByte('-')
	} else {
		micros = -micros
	}
	hour := -(micros / usPerHour)
	micros += hour * usPerHour
	minute := -(micros / usPerMin)
	micros += minute * usPerMin
	sec := -(micros / usPerSec)
	micros += sec * usPerSec
	micros = -micros
	fmt.Fprintf(&b, "%02d:%02d:%02d", hour, minute, sec)
	if micros != 0 {
		b.WriteString(strings.TrimRight(fmt.Sprintf(".%06d", micros), "0"))
	}
	return b.String()
}

// DuckDB packs TIME WITH TIME ZONE (dtime_tz_t) into one 64-bit word: the
// upper 40 bits hold microseconds since midnight, the lower 24 bits hold the
// UTC offset encoded as maxTimeTzOffset - offsetSeconds (so that sorting the
// raw bits orders by UTC instant within a day).
const (
	timeTzOffsetBits = 24
	timeTzOffsetMask = 1<<timeTzOffsetBits - 1
	maxTimeTzOffset  = 16*60*60 - 1 // 15:59:59
)

// maxTimeOfDayMicros is 24:00:00, the largest time of day DuckDB accepts.
const maxTimeOfDayMicros = 24 * 60 * 60 * 1_000_000

// TimeTzValue is a DuckDB TIME WITH TIME ZONE: a time of day plus a UTC
// offset. Build one with NewTimeTz; the zero value is 00:00:00+00.
type TimeTzValue struct {
	// x is the dtime_tz_t wire word XOR the encoding of a zero offset, so
	// that the zero value means 00:00:00+00.
	x uint64
}

// TimeTzFromBits wraps a raw dtime_tz_t word as sent on the wire. It is not
// validated (see Validate).
func TimeTzFromBits(bits uint64) TimeTzValue { return TimeTzValue{x: bits ^ maxTimeTzOffset} }

// Bits returns the raw dtime_tz_t word as sent on the wire: the time of day
// in microseconds in the upper 40 bits, 15:59:59 minus the UTC offset in
// seconds in the lower 24 bits.
func (t TimeTzValue) Bits() uint64 { return t.x ^ maxTimeTzOffset }

// NewTimeTz packs a time of day and a UTC offset (seconds east of UTC) into a
// TimeTzValue. The offset must lie within ±15:59:59.
func NewTimeTz(timeOfDay time.Duration, offsetSeconds int32) (TimeTzValue, error) {
	if timeOfDay < 0 || timeOfDay > 24*time.Hour {
		return TimeTzValue{}, fmt.Errorf("value: time of day %s out of range", timeOfDay)
	}
	if offsetSeconds < -maxTimeTzOffset || offsetSeconds > maxTimeTzOffset {
		return TimeTzValue{}, fmt.Errorf("value: UTC offset %ds out of range (±%ds)", offsetSeconds, maxTimeTzOffset)
	}
	micros := uint64(timeOfDay / time.Microsecond)
	encoded := uint64(int64(maxTimeTzOffset) - int64(offsetSeconds))
	return TimeTzFromBits(micros<<timeTzOffsetBits | encoded), nil
}

// Validate reports whether the value is a valid DuckDB TIMETZ: a time of day
// of at most 24:00:00 and a UTC offset within ±15:59:59. Values built by
// NewTimeTz always are; raw words (TimeTzFromBits) may not be, and DuckDB
// stores such a word unchecked and may later fail to render it.
func (t TimeTzValue) Validate() error {
	if us := t.Micros(); us > maxTimeOfDayMicros {
		return fmt.Errorf("value: TIMETZ time of day %dµs exceeds 24:00:00", us)
	}
	if off := t.OffsetSeconds(); off < -maxTimeTzOffset || off > maxTimeTzOffset {
		return fmt.Errorf("value: TIMETZ offset %ds out of range (±15:59:59)", off)
	}
	return nil
}

// Micros returns the time of day in microseconds since midnight.
func (t TimeTzValue) Micros() int64 { return int64(t.Bits() >> timeTzOffsetBits) }

// TimeOfDay returns the time of day as a duration since midnight.
func (t TimeTzValue) TimeOfDay() time.Duration {
	return time.Duration(t.Micros()) * time.Microsecond
}

// OffsetSeconds returns the UTC offset in seconds east of UTC (+01:00 is 3600).
func (t TimeTzValue) OffsetSeconds() int32 {
	return int32(maxTimeTzOffset - int64(t.Bits()&timeTzOffsetMask))
}

// Location returns a fixed time zone for the value's UTC offset.
func (t TimeTzValue) Location() *time.Location {
	return time.FixedZone("", int(t.OffsetSeconds()))
}

// String renders the value the way DuckDB displays it, e.g. "12:34:56.5+05:30".
func (t TimeTzValue) String() string {
	us := t.Micros()
	hh := us / 3_600_000_000
	mm := us / 60_000_000 % 60
	ss := us / 1_000_000 % 60
	frac := us % 1_000_000
	var b strings.Builder
	fmt.Fprintf(&b, "%02d:%02d:%02d", hh, mm, ss)
	if frac != 0 {
		b.WriteString(strings.TrimRight(fmt.Sprintf(".%06d", frac), "0"))
	}
	off := t.OffsetSeconds()
	sign := byte('+')
	if off < 0 {
		sign = '-'
		off = -off
	}
	b.WriteByte(sign)
	fmt.Fprintf(&b, "%02d", off/3600)
	if rem := off % 3600; rem != 0 {
		fmt.Fprintf(&b, ":%02d", rem/60)
		if rem%60 != 0 {
			fmt.Fprintf(&b, ":%02d", rem%60)
		}
	}
	return b.String()
}

// DuckDB DATE and TIMESTAMP values are proleptic Gregorian with astronomical
// year numbering, like time.Time: Go year 0 is DuckDB's "0001 (BC)", year -1
// is "0002 (BC)", and so on. Every finite DuckDB date and timestamp fits a
// time.Time exactly (DATE spans about ±5.8 million years, TIMESTAMP about
// ±290 thousand years).
//
// DuckDB also has the special values 'infinity' and '-infinity' for DATE and
// every TIMESTAMP type. They decode to InfinityTime and NegInfinityTime: the
// dates one past DuckDB's DATE sentinels (day ±(2^31-1) from 1970-01-01), which
// are later (earlier) than every finite DuckDB date or timestamp, so ordering
// is preserved. Compare with time.Time.Equal; no finite DuckDB value decodes
// to them, and the driver never encodes them as a finite value.
var (
	// InfinityTime is the time.Time a DuckDB 'infinity' DATE or TIMESTAMP
	// decodes to: 5881580-07-11 00:00:00 UTC.
	InfinityTime = dateEpoch.AddDate(0, 0, math.MaxInt32)
	// NegInfinityTime is the time.Time a DuckDB '-infinity' DATE or
	// TIMESTAMP decodes to: 5877642-06-24 (BC) 00:00:00 UTC, i.e. Go year
	// -5877641.
	NegInfinityTime = dateEpoch.AddDate(0, 0, -math.MaxInt32)
)

// Infinity reports whether t is InfinityTime (+1) or NegInfinityTime (-1);
// it returns 0 for every other time. Encoders use it to write DuckDB's
// ±infinity sentinels instead of an out-of-range finite value.
func Infinity(t time.Time) int {
	switch {
	case t.Equal(InfinityTime):
		return 1
	case t.Equal(NegInfinityTime):
		return -1
	}
	return 0
}

// dateEpoch is 1970-01-01 UTC, day zero of DuckDB's DATE.
var dateEpoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// DateFromDays converts a DuckDB DATE (days since 1970-01-01) to UTC
// midnight of that day; the ±infinity sentinels (±(2^31-1)) map to
// InfinityTime and NegInfinityTime.
func DateFromDays(days int32) time.Time {
	switch days {
	case math.MaxInt32:
		return InfinityTime
	case -math.MaxInt32:
		return NegInfinityTime
	}
	return dateEpoch.AddDate(0, 0, int(days))
}

// Units per second of the TIMESTAMP family's int64 payloads.
const (
	Seconds      = 1
	Milliseconds = 1_000
	Microseconds = 1_000_000
	Nanoseconds  = 1_000_000_000
)

// TimestampFromUnits converts a DuckDB TIMESTAMP-family value, v units of
// 1/perSecond seconds since 1970-01-01 00:00:00 UTC, to a UTC time.Time.
// perSecond is Seconds (TIMESTAMP_S), Milliseconds (TIMESTAMP_MS),
// Microseconds (TIMESTAMP, TIMESTAMPTZ) or Nanoseconds (TIMESTAMP_NS,
// TIMESTAMPTZ_NS). The ±infinity sentinels (±(2^63-1) in every unit) map to
// InfinityTime and NegInfinityTime.
func TimestampFromUnits(v, perSecond int64) time.Time {
	switch v {
	case math.MaxInt64:
		return InfinityTime
	case -math.MaxInt64:
		return NegInfinityTime
	}
	sec, sub := v/perSecond, v%perSecond
	if sub < 0 {
		sec--
		sub += perSecond
	}
	// Go's internal epoch is year 1; seconds near ±2^63 would overflow it.
	// Finite DuckDB values are far inside that range; clamp the impossible
	// rest instead of wrapping around.
	const maxSec = math.MaxInt64 / 4
	switch {
	case sec > maxSec:
		return InfinityTime
	case sec < -maxSec:
		return NegInfinityTime
	}
	return time.Unix(sec, sub*(Nanoseconds/perSecond)).UTC()
}

// UnixRounded returns t in units of 1/perSecond seconds since the epoch,
// rounded to the nearest unit with halves away from the epoch: the way
// DuckDB lowers the precision of a timestamp (TryCastTimestampBase, used by
// the TIMESTAMP_NS -> TIMESTAMP -> TIMESTAMP_MS -> TIMESTAMP_S casts). A
// time.Time sent as a query argument becomes an exact TIMESTAMP or
// TIMESTAMP_NS literal that the server casts this way into a column of lower
// precision, so values encoded for such a column must round alike. ok is
// false on int64 overflow. perSecond must divide 1e9.
func UnixRounded(t time.Time, perSecond int64) (n int64, ok bool) {
	unit := int64(1e9) / perSecond
	nsec := int64(t.Nanosecond())
	sub, rem := nsec/unit, nsec%unit // sub in [0, perSecond)
	base, ok := unixUnits(t.Unix(), sub, perSecond)
	if !ok {
		return 0, false
	}
	// t lies in (base, base+1) units when rem > 0; base < 0 means t < 0.
	if 2*rem > unit || (rem != 0 && 2*rem == unit && base >= 0) {
		if base == math.MaxInt64 {
			return 0, false
		}
		base++
	}
	return base, true
}

// unixUnits returns sec*perSecond + sub (0 <= sub < perSecond), reporting
// int64 overflow.
func unixUnits(sec, sub, perSecond int64) (int64, bool) {
	if sec >= 0 {
		if sec > (math.MaxInt64-sub)/perSecond {
			return 0, false
		}
		return sec*perSecond + sub, true
	}
	// sec*perSecond + sub == (sec+1)*perSecond - (perSecond-sub), which
	// cannot overflow before the bound is checked.
	s1, r := sec+1, perSecond-sub
	if s1 < (math.MinInt64+r)/perSecond {
		return 0, false
	}
	return s1*perSecond - r, true
}

// HugeInt returns the hugeint_t upper*2^64 + lower.
func HugeInt(upper int64, lower uint64) *big.Int {
	v := big.NewInt(upper)
	v.Lsh(v, 64)
	return v.Add(v, new(big.Int).SetUint64(lower))
}

// UHugeInt returns the uhugeint_t upper*2^64 + lower.
func UHugeInt(upper, lower uint64) *big.Int {
	v := new(big.Int).SetUint64(upper)
	v.Lsh(v, 64)
	return v.Or(v, new(big.Int).SetUint64(lower))
}

// UUIDString formats a UUID stored as hugeint_t as lowercase 8-4-4-4-12 hex.
// DuckDB flips the top bit of the upper word so that signed order matches
// UUID order.
func UUIDString(upper int64, lower uint64) string {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(upper)^(1<<63))
	binary.BigEndian.PutUint64(raw[8:], lower)
	return FormatUUID(raw)
}

// FormatUUID renders the 16 bytes of a UUID as lowercase 8-4-4-4-12 hex.
func FormatUUID(u [16]byte) string {
	var out [36]byte
	hex.Encode(out[0:8], u[0:4])
	hex.Encode(out[9:13], u[4:6])
	hex.Encode(out[14:18], u[6:8])
	hex.Encode(out[19:23], u[8:10])
	hex.Encode(out[24:], u[10:])
	out[8], out[13], out[18], out[23] = '-', '-', '-', '-'
	return string(out[:])
}

// RandomUUID returns the bytes of a random version-4 UUID (RFC 4122
// variant), the shared source of query UUIDs, stream ids and client ids. It
// fails only when the OS entropy source does.
func RandomUUID() ([16]byte, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return u, err
	}
	u[6] = u[6]&0x0f | 0x40 // version 4
	u[8] = u[8]&0x3f | 0x80 // RFC 4122 variant
	return u, nil
}

// FormatDecimal64 renders an unscaled DECIMAL value, e.g. (12345, 2) is
// "123.45" and (-5, 3) is "-0.005".
func FormatDecimal64(v int64, scale int) string {
	u := uint64(v)
	if v < 0 {
		u = -u // two's complement magnitude, correct for MinInt64 too
	}
	var buf [20]byte
	return placeDecimalPoint(strconv.AppendUint(buf[:0], u, 10), scale, v < 0)
}

// FormatDecimal is FormatDecimal64 for a 128-bit (or wider) unscaled value.
func FormatDecimal(v *big.Int, scale int) string {
	return placeDecimalPoint(new(big.Int).Abs(v).Append(nil, 10), scale, v.Sign() < 0)
}

func placeDecimalPoint(digits []byte, scale int, neg bool) string {
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	if scale <= 0 {
		b.Write(digits)
		return b.String()
	}
	if n := len(digits); n <= scale {
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", scale-n))
		b.Write(digits)
		return b.String()
	}
	b.Write(digits[:len(digits)-scale])
	b.WriteByte('.')
	b.Write(digits[len(digits)-scale:])
	return b.String()
}

// BitString renders a DuckDB BIT value as '0'/'1' characters. Byte 0 holds
// the number of padding bits (0..7) at the top of byte 1; the bits follow
// most-significant first (core bit.cpp Bit::ToString).
func BitString(b []byte) (string, error) {
	if len(b) < 2 {
		return "", fmt.Errorf("BIT value has %d bytes, need at least 2", len(b))
	}
	padding := int(b[0])
	if padding > 7 {
		return "", fmt.Errorf("BIT padding %d out of range", padding)
	}
	out := make([]byte, (len(b)-1)*8-padding)
	for k := range out {
		bit := k + padding
		out[k] = '0' + b[1+bit/8]>>(7-bit%8)&1
	}
	return string(out), nil
}

// Bignum decodes a DuckDB BIGNUM value (core bignum.cpp): a 3-byte
// big-endian header holding the data byte count with bit 23 set, followed by
// the big-endian magnitude; for a negative value the header and every data
// byte are bit-inverted.
func Bignum(b []byte) (*big.Int, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("BIGNUM value has %d bytes, need at least 4", len(b))
	}
	neg := b[0]&0x80 == 0
	header := uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	if neg {
		header = ^header & 0xffffff
	}
	if n := header & 0x7fffff; int(n) != len(b)-3 {
		return nil, fmt.Errorf("BIGNUM header declares %d data bytes, value has %d", n, len(b)-3)
	}
	mag := append([]byte(nil), b[3:]...)
	if neg {
		for i := range mag {
			mag[i] = ^mag[i]
		}
	}
	v := new(big.Int).SetBytes(mag)
	if neg {
		v.Neg(v)
	}
	return v, nil
}
