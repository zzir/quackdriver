//go:build integration

package appendit

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/value"
)

// TestAppendSameAsInsert: client-side conversions of the Appender store the
// same values as the server's casts of the substituted literals: floats to
// DECIMAL, times rounded to lower-precision timestamps (halves away from
// the epoch), ±infinity and nil []byte (NULL).
func TestAppendSameAsInsert(t *testing.T) {
	e := newEnv(t)
	sameOnBothPaths(t, e, "append_fd", []quackdriver.AppendColumn{
		{Name: "a", Type: "DECIMAL(18,18)"}, {Name: "b", Type: "DECIMAL(10,2)"}, {Name: "c", Type: "DECIMAL(10,0)"}, {Name: "d", Type: "DECIMAL(38,10)"},
	}, [][]any{
		{0.1, 2.675, 2.5, 1e20 / 3},
		{0.1, 1.125, -0.5, -123.45678901234},
		{float32(0.1), float32(0.125), float32(3.5), float32(1.1)},
		{0.3, 0.005, 0.49999999999999994, 9.999999999e27},
	})

	tsCols := []quackdriver.AppendColumn{{Name: "s", Type: "TIMESTAMP_S"}, {Name: "m", Type: "TIMESTAMP_MS"}, {Name: "u", Type: "TIMESTAMP"}, {Name: "z", Type: "TIMESTAMPTZ"}}
	var tsRows [][]any
	for _, tm := range []time.Time{
		time.Date(1969, 12, 31, 23, 59, 59, 500600000, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 500000000, time.UTC),
		time.Date(1969, 12, 31, 23, 59, 59, 500000000, time.UTC),
		time.Date(2026, 5, 25, 12, 34, 56, 789000000, time.UTC),
		time.Date(1960, 1, 1, 0, 0, 0, 500000000, time.UTC),
		time.Date(1969, 12, 31, 23, 59, 59, 999500000, time.UTC),
	} {
		tsRows = append(tsRows, []any{tm, tm, tm, tm})
	}
	sameOnBothPaths(t, e, "append_ts", tsCols, tsRows)
	// Sub-microsecond times are TIMESTAMP_NS literals, which DuckDB cannot
	// cast to TIMESTAMP_S.
	tsRows = nil
	for _, tm := range []time.Time{
		time.Date(1969, 12, 31, 23, 59, 59, 999999500, time.UTC),
		time.Date(2026, 5, 25, 12, 34, 56, 789500500, time.UTC),
		time.Date(1969, 12, 31, 23, 59, 59, 500600500, time.UTC),
		time.Date(1969, 12, 31, 23, 59, 59, 998500001, time.UTC),
	} {
		tsRows = append(tsRows, []any{tm, tm})
	}
	sameOnBothPaths(t, e, "append_tsns", tsCols[1:3], tsRows)

	// ±infinity of every DATE/TIMESTAMP type, as decoded from DuckDB.
	var infCols []quackdriver.AppendColumn
	var pos, neg []any
	for i, typ := range []string{"DATE", "TIMESTAMP", "TIMESTAMPTZ", "TIMESTAMP_S", "TIMESTAMP_MS", "TIMESTAMP_NS", "TIMESTAMPTZ_NS"} {
		infCols = append(infCols, quackdriver.AppendColumn{Name: fmt.Sprintf("c%d", i), Type: typ})
		pos, neg = append(pos, quackdriver.InfinityTime), append(neg, quackdriver.NegInfinityTime)
	}
	sameOnBothPaths(t, e, "append_inf", infCols, [][]any{pos, neg})
	if n := e.queryInt(t, `SELECT count(*) FROM append_inf WHERE concat_ws(',', c0, c1, c2, c3, c4, c5, c6) IN ('infinity,infinity,infinity,infinity,infinity,infinity,infinity',
			'-infinity,-infinity,-infinity,-infinity,-infinity,-infinity,-infinity')`); n != 4 {
		t.Errorf("%d of 4 rows hold ±infinity", n)
	}

	sameOnBothPaths(t, e, "append_nilbytes", []quackdriver.AppendColumn{{Name: "v", Type: "BLOB"}, {Name: "s", Type: "VARCHAR"}},
		[][]any{{[]byte(nil), []byte(nil)}, {[]byte{}, []byte{}}})
	if n := e.queryInt(t, "SELECT count(*) FROM append_nilbytes WHERE (id = 0) = (v IS NULL AND s IS NULL) AND (id = 1) = coalesce(v = ''::BLOB AND s = '', false)"); n != 4 {
		t.Errorf("nil []byte: %d of 4 rows as expected", n)
	}
}

// TestAppendInvalidTimeTz: raw TIMETZ words DuckDB cannot render are
// rejected client-side; stored, they would invalidate the server database
// the first time they are rendered (newEnv's cleanup checks that).
func TestAppendInvalidTimeTz(t *testing.T) {
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_tt (v TIMETZ)")
	sc := e.conn(t)
	for _, bits := range []uint64{math.MaxUint64, uint64(25*3600*1_000_000)<<24 | 57599, uint64(3600*1_000_000)<<24 | 0xFFFFFF} {
		err := appendRows(sc, "append_tt", []quackdriver.AppendColumn{{Name: "v", Type: "TIMETZ"}}, [][]any{{value.TimeTzFromBits(bits)}})
		if ve := (*quackdriver.ValueError)(nil); !errors.As(err, &ve) {
			t.Errorf("bits %#x: %v, want a *ValueError", bits, err)
		}
	}
	if n := e.queryInt(t, "SELECT count(*) FROM append_tt"); n != 0 {
		t.Fatalf("%d rows stored", n)
	}
}

// TestAppendMapKeys: Append's MAP values go through the server's own cast,
// so keys the server's MAP constructor calls duplicates fail the statement
// with the same error, and distinct keys are inserted.
func TestAppendMapKeys(t *testing.T) {
	e := newEnv(t)
	sc := e.conn(t)
	tz := func(tod time.Duration, off int32) quackdriver.TimeTzValue { return mustTimeTz(t, tod, off) }
	for i, c := range []struct {
		key    string
		k1, k2 any
		l1, l2 string
	}{
		{"INTEGER", 1, int64(1), "1", "1"},
		{"VARCHAR", "a", "A", "'a'", "'A'"},
		{"DOUBLE", 0.0, math.Copysign(0, -1), "'0.0'", "'-0.0'"},
		{"DOUBLE", math.NaN(), math.NaN(), "'nan'", "'nan'"},
		{"INTERVAL", quackdriver.IntervalValue{Months: 1}, quackdriver.IntervalValue{Days: 30}, "INTERVAL 1 MONTH", "INTERVAL 30 DAY"},
		{"INTERVAL", quackdriver.IntervalValue{Days: 1}, 24 * time.Hour, "INTERVAL 1 DAY", "INTERVAL 24 HOUR"},
		{"INTERVAL", quackdriver.IntervalValue{Months: 1, Days: -1}, quackdriver.IntervalValue{Days: 29}, "INTERVAL 1 MONTH - INTERVAL 1 DAY", "INTERVAL 29 DAY"},
		{"INTERVAL", quackdriver.IntervalValue{Days: 1}, quackdriver.IntervalValue{Micros: 86_400_000_001}, "INTERVAL 1 DAY", "to_microseconds(86400000001)"},
		{"STRUCT(a INTEGER, b VARCHAR)", map[string]any{"a": 1}, map[string]any{"a": 1, "b": nil}, "{'a': 1, 'b': NULL}", "{'a': 1, 'b': NULL}"},
		{"STRUCT(a INTEGER, b VARCHAR)", map[string]any{"a": 1}, map[string]any{"b": "1"}, "{'a': 1, 'b': NULL}", "{'a': NULL, 'b': '1'}"},
		{"INTEGER[]", []any{1, nil}, []any{1, nil}, "[1, NULL]", "[1, NULL]"},
		{"VARCHAR[]", []any{"a b"}, []any{"a", "b"}, "['a b']", "['a', 'b']"},
		{"TIME WITH TIME ZONE", tz(12*time.Hour, 3600), tz(11*time.Hour, 0), "'12:00:00+01'", "'11:00:00+00'"},
		{"DECIMAL(10,2)", "1.5", 1.5, "1.5", "1.50"},
		{"UUID", "550e8400-e29b-41d4-a716-446655440000", "550E8400-E29B-41D4-A716-446655440000", "'550e8400-e29b-41d4-a716-446655440000'", "'550E8400-E29B-41D4-A716-446655440000'"},
	} {
		lit := fmt.Sprintf("MAP([%s::%s, %s::%s], [0, 1])", c.l1, c.key, c.l2, c.key)
		_, serverErr := e.db.Exec("SELECT " + lit)
		if serverErr != nil && !strings.Contains(serverErr.Error(), "Map keys must be unique") {
			t.Fatalf("%s: %v", lit, serverErr)
		}
		tbl, typ := fmt.Sprintf("append_mapkeys_%d", i), "MAP("+c.key+", INTEGER)"
		e.exec(t, "CREATE TABLE "+tbl+" (m "+typ+")")
		err := appendRows(sc, tbl, []quackdriver.AppendColumn{{Type: typ}}, [][]any{{[]any{
			map[string]any{"key": c.k1, "value": 0}, map[string]any{"key": c.k2, "value": 1},
		}}})
		switch {
		case serverErr != nil && (serverError(err) == nil || !strings.Contains(err.Error(), "unique")):
			t.Errorf("%s: the server rejects duplicate keys, Append: %v", lit, err)
		case serverErr == nil && err != nil:
			t.Errorf("%s: the server accepts the keys, Append: %v", lit, err)
		case serverErr == nil:
			if got, want := e.queryString(t, "SELECT m::VARCHAR FROM "+tbl), e.queryString(t, "SELECT ("+lit+")::VARCHAR"); got != want {
				t.Errorf("%s: stored %s, want %s", lit, got, want)
			}
			e.queryString(t, "SELECT m::MAP(VARCHAR, BIGINT)::VARCHAR FROM "+tbl) // stays valid through a cast
		}
	}

	// Distinct keys that the INSERT's implicit cast to the table's key type
	// turns into duplicates: an ordinary server error.
	e.exec(t, "CREATE TABLE append_mapcollapse (m MAP(INTEGER, VARCHAR))")
	err := appendRows(sc, "append_mapcollapse", []quackdriver.AppendColumn{{Type: "MAP(DOUBLE, VARCHAR)"}},
		[][]any{{[]any{map[string]any{"key": 1.1, "value": "a"}, map[string]any{"key": 1.2, "value": "b"}}}})
	if qe := serverError(err); qe == nil || qe.Type != "Invalid Input" || qe.MustInvalidate {
		t.Fatalf("collapsing cast: %v", err)
	}
	connUsable(t, sc)
}
