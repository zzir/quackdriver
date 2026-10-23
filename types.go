package quackdriver

import (
	"time"

	"github.com/zzir/quackdriver/internal/value"
)

// IntervalValue is DuckDB's INTERVAL: months, days and microseconds kept
// apart because their lengths depend on the calendar. INTERVAL columns scan
// into it, and it can be passed as a query argument.
type IntervalValue = value.IntervalValue

// TimeTzValue is DuckDB's TIME WITH TIME ZONE: a time of day plus a UTC
// offset. TIMETZ columns scan into it, and it can be passed as a query
// argument or appended. Build one with NewTimeTz; the zero value is
// 00:00:00+00. Its methods are TimeOfDay, OffsetSeconds, Micros, Location,
// String, Validate and Bits (the raw dtime_tz_t word as DuckDB stores it).
type TimeTzValue = value.TimeTzValue

// InfinityTime and NegInfinityTime are the times DuckDB's 'infinity' and
// '-infinity' DATE and TIMESTAMP values (of every TIMESTAMP type) scan into:
// 5881580-07-11 and 5877642-06-24 (BC) 00:00:00 UTC, later (earlier) than
// every finite DuckDB date or timestamp. Compare with time.Time.Equal. As a
// query argument or an appended DATE or TIMESTAMP value they are sent as
// 'infinity' and '-infinity' again.
//
// BC dates use astronomical year numbering, as DuckDB does: Go year 0 is
// DuckDB's 0001 (BC).
var (
	InfinityTime    = value.InfinityTime
	NegInfinityTime = value.NegInfinityTime
)

// NewTimeTz builds a TimeTzValue from a time of day (0 to 24h) and a UTC
// offset in seconds east of UTC (within ±15:59:59).
func NewTimeTz(timeOfDay time.Duration, offsetSeconds int32) (TimeTzValue, error) {
	return value.NewTimeTz(timeOfDay, offsetSeconds)
}
