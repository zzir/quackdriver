//go:build integration

package types_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/value"
)

// TestTemporalRendered compares DATE, TIME and TIMESTAMP values (including
// ±infinity, BC dates and the extremes of each type's range) with the
// server's rendering.
func TestTemporalRendered(t *testing.T) {
	e := newEnv(t)
	e.exec(t, "SET TimeZone = 'UTC'")
	dates := []string{
		"1970-01-01", "2024-02-29", "1969-12-31", "0001-01-01", "0001-01-01 (BC)", "0002-12-31 (BC)",
		"9999-12-31", "10000-01-01", "5881580-07-10", "5877642-06-25 (BC)", "1582-10-04", "1582-10-15",
		"infinity", "-infinity",
	}
	timestamps := []string{
		"1970-01-01 00:00:00", "2024-02-29 12:34:56.789012", "1969-12-31 23:59:59.999999",
		"0001-01-01 00:00:00", "0001-01-01 (BC) 00:00:00", "0044-03-15 (BC) 12:00:00.5",
		"290309-12-22 (BC) 00:00:00", "294247-01-10 04:00:54", "9999-12-31 23:59:59.999999",
		"infinity", "-infinity",
	}
	timestampsNs := []string{
		"1970-01-01 00:00:00", "2024-02-29 12:34:56.123456789", "1969-12-31 23:59:59.999999999",
		"1677-09-22 00:00:00", "2262-04-11 23:47:16.854775", "1900-01-01 00:00:00.000000001",
		"infinity", "-infinity",
	}
	var exprs []string
	for _, d := range dates {
		exprs = append(exprs, "DATE "+livetest.SQLString(d))
	}
	for _, ts := range timestamps {
		for _, typ := range []string{"TIMESTAMP", "TIMESTAMP_S", "TIMESTAMP_MS", "TIMESTAMPTZ"} {
			exprs = append(exprs, "CAST(TIMESTAMP "+livetest.SQLString(ts)+" AS "+typ+")")
		}
	}
	for _, ts := range timestampsNs {
		exprs = append(exprs, livetest.SQLString(ts)+"::TIMESTAMP_NS", livetest.SQLString(ts)+"::TIMESTAMP_NS::TIMESTAMPTZ_NS")
	}
	exprs = append(exprs,
		"TIMESTAMP '294247-01-10 04:00:54.775806'", "TIMESTAMPTZ '294247-01-10 04:00:54.775806+00'",
		"TIME '00:00:00'", "TIME '23:59:59.999999'", "TIME '24:00:00'", "TIME '12:00:00.5'",
		"'00:00:00'::TIME_NS", "'23:59:59.999999999'::TIME_NS", "'01:02:03.000000001'::TIME_NS", "'24:00:00'::TIME_NS",
		"TIMESTAMPTZ '2024-06-01 12:00:00+05:30'", "'2024-06-01 12:00:00.123456789-08'::TIMESTAMPTZ_NS",
	)
	e.checkRendered(t, exprs...)
}

// TestTimeTzOffsets covers TIME WITH TIME ZONE across the whole offset
// range, checking the decoded parts against the server's own extraction.
func TestTimeTzOffsets(t *testing.T) {
	e := newEnv(t)
	var exprs []string
	for _, off := range []string{
		"+00", "+01", "-01", "+05:30", "-03:30", "+05:45", "+14", "-12", "+15:59:59", "-15:59:59",
		"+00:00:01", "-00:00:01", "+09:30:15", "-11:22:33",
	} {
		for _, tod := range []string{"00:00:00", "12:34:56.789", "23:59:59.999999", "24:00:00", "00:00:00.000001"} {
			exprs = append(exprs, "TIMETZ "+livetest.SQLString(tod+off))
		}
	}
	e.checkRendered(t, exprs...)

	// The offset and time of day as the server extracts them.
	rows := e.rows(t, `SELECT x, timezone(x)::INTEGER, epoch_us(x::TIME)
		FROM (VALUES (TIMETZ '12:34:56.5+05:30'), (TIMETZ '00:00:00-15:59:59'), (TIMETZ '24:00:00+15:59:59')) v(x)`)
	for _, r := range rows {
		tz, ok := r[0].(value.TimeTzValue)
		if !ok {
			t.Fatalf("TIMETZ decoded as %T", r[0])
		}
		if want := r[1].(int32); tz.OffsetSeconds() != want {
			t.Errorf("%s: OffsetSeconds = %d, server timezone() = %d", tz, tz.OffsetSeconds(), want)
		}
		if want := r[2].(int64); tz.Micros() != want {
			t.Errorf("%s: Micros = %d, server says %d", tz, tz.Micros(), want)
		}
	}
}

// TestTemporalGoValues pins the Go representation of the edge cases: BC
// years use astronomical numbering, ±infinity of every DATE/TIMESTAMP type
// map to value.InfinityTime / value.NegInfinityTime, and finite values keep
// their full precision.
func TestTemporalGoValues(t *testing.T) {
	e := newEnv(t)
	for _, typ := range []string{"DATE", "TIMESTAMP", "TIMESTAMP_S", "TIMESTAMP_MS", "TIMESTAMP_NS", "TIMESTAMPTZ", "TIMESTAMPTZ_NS"} {
		row := e.rows(t, fmt.Sprintf("SELECT 'infinity'::%s, '-infinity'::%s", typ, typ))[0]
		if v, ok := row[0].(time.Time); !ok || !v.Equal(value.InfinityTime) {
			t.Errorf("'infinity'::%s = %#v, want value.InfinityTime", typ, row[0])
		}
		if v, ok := row[1].(time.Time); !ok || !v.Equal(value.NegInfinityTime) {
			t.Errorf("'-infinity'::%s = %#v, want value.NegInfinityTime", typ, row[1])
		}
	}
	// Ordering survives: -infinity < min finite < max finite < infinity.
	row := e.rows(t, `SELECT DATE '5877642-06-25 (BC)', DATE '5881580-07-10',
		TIMESTAMP '290309-12-22 (BC) 00:00:00', TIMESTAMP '294247-01-10 04:00:54.775806'`)[0]
	for i, v := range row {
		tm := v.(time.Time)
		if !tm.After(value.NegInfinityTime) || !tm.Before(value.InfinityTime) {
			t.Errorf("finite value %d (%s) is not strictly between the infinities", i, tm)
		}
	}
	cases := []struct {
		expr string
		want time.Time
	}{
		{"DATE '0001-01-01 (BC)'", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"DATE '0044-03-15 (BC)'", time.Date(-43, 3, 15, 0, 0, 0, 0, time.UTC)},
		{"DATE '5881580-07-10'", time.Date(5881580, 7, 10, 0, 0, 0, 0, time.UTC)},
		{"TIMESTAMP '1969-12-31 23:59:59.999999'", time.Date(1969, 12, 31, 23, 59, 59, 999999000, time.UTC)},
		{"'1969-12-31 23:59:59.999'::TIMESTAMP_MS", time.Date(1969, 12, 31, 23, 59, 59, 999000000, time.UTC)},
		{"'1969-12-31 23:59:59'::TIMESTAMP_S", time.Date(1969, 12, 31, 23, 59, 59, 0, time.UTC)},
		{"'1677-09-22 00:00:00.000000001'::TIMESTAMP_NS", time.Date(1677, 9, 22, 0, 0, 0, 1, time.UTC)},
		{"'2024-01-02 03:04:05.123456789+02'::TIMESTAMPTZ_NS", time.Date(2024, 1, 2, 1, 4, 5, 123456789, time.UTC)},
		{"TIMESTAMPTZ '2024-01-02 03:04:05.5-01:30'", time.Date(2024, 1, 2, 4, 34, 5, 500000000, time.UTC)},
	}
	for _, c := range cases {
		got, ok := e.one(t, "SELECT "+c.expr).(time.Time)
		if !ok || !got.Equal(c.want) || got.Location() != time.UTC {
			t.Errorf("%s = %v, want %v (UTC)", c.expr, got, c.want)
		}
	}
	if d := e.one(t, "SELECT '23:59:59.999999999'::TIME_NS"); d != 24*time.Hour-1 {
		t.Errorf("TIME_NS = %#v", d)
	}
	if d := e.one(t, "SELECT TIME '24:00:00'"); d != 24*time.Hour {
		t.Errorf("TIME 24:00 = %#v", d)
	}
	iv := e.one(t, "SELECT INTERVAL '-1 year -2 months 3 days -00:00:01.5'")
	if want := (value.IntervalValue{Months: -14, Days: 3, Micros: -1500000}); iv != want {
		t.Errorf("INTERVAL = %#v, want %#v", iv, want)
	}
}

// TestTimestampTzSessionZone checks that TIMESTAMPTZ values decode to the
// same instant whatever the session time zone.
func TestTimestampTzSessionZone(t *testing.T) {
	e := newEnv(t)
	const q = `SELECT TIMESTAMPTZ '2024-03-10 06:59:59.5+00', '2024-11-03 05:30:00.123456789+00'::TIMESTAMPTZ_NS,
		TIMESTAMPTZ '1900-01-01 00:00:00+00', TIMETZ '12:00:00+02'`
	base := e.rows(t, q)[0]
	for _, tz := range []string{"America/New_York", "Asia/Kolkata", "Pacific/Chatham", "UTC"} {
		e.exec(t, "SET TimeZone = "+livetest.SQLString(tz))
		got := e.rows(t, q)[0]
		for i := range got {
			if !equalValues(got[i], base[i]) {
				t.Errorf("TimeZone %s column %d: %#v, want %#v", tz, i, got[i], base[i])
			}
		}
		// The server's rendering in that zone names the same instant.
		row := e.rows(t, "SELECT x, strftime(x AT TIME ZONE 'UTC', '%Y-%m-%d %H:%M:%S.%f') FROM (SELECT TIMESTAMPTZ '2024-03-10 06:59:59.5+00' AS x)")[0]
		if s := row[0].(time.Time).Format("2006-01-02 15:04:05.000000"); s != row[1] {
			t.Errorf("TimeZone %s: decoded %s, server UTC %s", tz, s, row[1])
		}
	}
}
