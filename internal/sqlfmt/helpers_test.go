package sqlfmt

import (
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/value"
)

// The helpers in this file re-implement, independently of the code under
// test, how DuckDB 2.0 reads back the literals FormatValue produces. They are
// ports of the relevant core routines (StringLiteralMatcher, Blob::ToBlob,
// Date::TryConvertDate, Interval::FromCString, Time::TryConvertTimeTZ /
// Timestamp::TryParseUTCOffset) restricted to the shapes FormatValue emits.

func newRand(t *testing.T) *rand.Rand {
	t.Helper()
	const seed = 20261021
	return rand.New(rand.NewSource(seed))
}

// decodeStandardString decodes a complete standard single-quoted literal
// (a doubled quote is an escaped quote, nothing else is special). It reports false when
// lit is not exactly one such literal.
func decodeStandardString(lit string) (string, bool) {
	if len(lit) < 2 || lit[0] != '\'' || lit[len(lit)-1] != '\'' {
		return "", false
	}
	inner := lit[1 : len(lit)-1]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c == '\'' {
			if i+1 >= len(inner) || inner[i+1] != '\'' {
				return "", false // a lone quote would end the literal early
			}
			i++
		}
		b.WriteByte(c)
	}
	return b.String(), true
}

// duckBlobFromString ports Blob::TryGetBlobSize/Blob::ToBlob: \xHH escapes a
// byte, other ASCII bytes stand for themselves, non-ASCII bytes are errors.
func duckBlobFromString(s string) ([]byte, bool) {
	hexVal := func(c byte) int {
		switch {
		case c >= '0' && c <= '9':
			return int(c - '0')
		case c >= 'a' && c <= 'f':
			return int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			return int(c-'A') + 10
		}
		return -1
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			if i+3 >= len(s) {
				return nil, false
			}
			if s[i+1] != 'x' || hexVal(s[i+2]) < 0 || hexVal(s[i+3]) < 0 {
				return nil, false
			}
			out = append(out, byte(hexVal(s[i+2])<<4|hexVal(s[i+3])))
			i += 3
		case s[i] <= 127:
			out = append(out, s[i])
		default:
			return nil, false
		}
	}
	return out, true
}

// parseDuckTimestampLiteral reads "TIMESTAMP '...'" or "TIMESTAMP_NS '...'"
// the way DuckDB's date/time parsers do for the format FormatValue emits:
// YYYY[Y..]-MM-DD[ (BC)] HH:MM:SS.fraction. TIMESTAMP keeps six fractional
// digits, TIMESTAMP_NS nine.
func parseDuckTimestampLiteral(lit string) (time.Time, bool) {
	digits := 6
	body, ok := strings.CutPrefix(lit, "TIMESTAMP ")
	if !ok {
		body, ok = strings.CutPrefix(lit, "TIMESTAMP_NS ")
		if !ok {
			return time.Time{}, false
		}
		digits = 9
	}
	s, ok := decodeStandardString(body)
	if !ok {
		return time.Time{}, false
	}
	readNum := func(minLen, maxLen int) (int, bool) {
		i := 0
		for i < len(s) && i < maxLen && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i < minLen {
			return 0, false
		}
		v, err := strconv.Atoi(s[:i])
		s = s[i:]
		return v, err == nil
	}
	expect := func(prefix string) bool {
		var ok bool
		s, ok = strings.CutPrefix(s, prefix)
		return ok
	}
	year, ok := readNum(1, 9)
	if !ok || !expect("-") {
		return time.Time{}, false
	}
	month, ok := readNum(2, 2)
	if !ok || !expect("-") {
		return time.Time{}, false
	}
	day, ok := readNum(2, 2)
	if !ok {
		return time.Time{}, false
	}
	if expect(" (BC)") {
		if year == 0 {
			return time.Time{}, false
		}
		year = -year + 1 // Date::TryConvertDate
	}
	if !expect(" ") {
		return time.Time{}, false
	}
	hh, ok1 := readNum(2, 2)
	ok2 := expect(":")
	mm, ok3 := readNum(2, 2)
	ok4 := expect(":")
	ss, ok5 := readNum(2, 2)
	ok6 := expect(".")
	frac, ok7 := readNum(digits, digits)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 || s != "" {
		return time.Time{}, false
	}
	ns := frac
	if digits == 6 {
		ns *= 1000
	}
	tm := time.Date(year, time.Month(month), day, hh, mm, ss, ns, time.UTC)
	if tm.Year() != year || int(tm.Month()) != month || tm.Day() != day || tm.Hour() != hh {
		return time.Time{}, false // not normalized: DuckDB would reject it
	}
	return tm, true
}

// parseDuckInterval ports the standard_interval branch of
// Interval::FromCString for the "N unit" components FormatValue emits,
// including its int64 magnitude parsing and checked additions.
func parseDuckInterval(s string) (value.IntervalValue, bool) {
	var iv value.IntervalValue
	found := false
	pos := 0
	isSp := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }
	for {
		for pos < len(s) && isSp(s[pos]) {
			pos++
		}
		if pos >= len(s) {
			break
		}
		neg := false
		if s[pos] == '-' {
			neg = true
			pos++
		}
		start := pos
		for pos < len(s) && s[pos] >= '0' && s[pos] <= '9' {
			pos++
		}
		if pos == start {
			return iv, false
		}
		number, err := strconv.ParseInt(s[start:pos], 10, 64)
		if err != nil {
			return iv, false // TryCast<string_t, int64_t> fails
		}
		if neg {
			number = -number
		}
		for pos < len(s) && isSp(s[pos]) {
			pos++
		}
		start = pos
		for pos < len(s) && ((s[pos] >= 'a' && s[pos] <= 'z') || (s[pos] >= 'A' && s[pos] <= 'Z')) {
			pos++
		}
		add32 := func(target *int32) bool {
			if number < math.MinInt32 || number > math.MaxInt32 {
				return false
			}
			sum := int64(*target) + number
			if sum < math.MinInt32 || sum > math.MaxInt32 {
				return false
			}
			*target = int32(sum)
			return true
		}
		switch strings.ToLower(s[start:pos]) {
		case "months":
			if !add32(&iv.Months) {
				return iv, false
			}
		case "days":
			if !add32(&iv.Days) {
				return iv, false
			}
		case "microseconds":
			sum := iv.Micros + number
			if (number > 0 && sum < iv.Micros) || (number < 0 && sum > iv.Micros) {
				return iv, false
			}
			iv.Micros = sum
		default:
			return iv, false
		}
		found = true
	}
	return iv, found
}

// parseDuckTimeTzLiteral reads "TIMETZ 'HH:MM:SS.ffffff±HH[:MM[:SS]]'" the
// way Time::TryConvertTimeTZ and Timestamp::TryParseUTCOffset do.
func parseDuckTimeTzLiteral(t *testing.T, lit string) (value.TimeTzValue, bool) {
	t.Helper()
	body, ok := strings.CutPrefix(lit, "TIMETZ ")
	if !ok {
		return value.TimeTzValue{}, false
	}
	s, ok := decodeStandardString(body)
	if !ok || len(s) < len("00:00:00.000000+0") {
		return value.TimeTzValue{}, false
	}
	atoi := func(x string) int {
		v, err := strconv.Atoi(x)
		if err != nil {
			return -1
		}
		return v
	}
	if s[2] != ':' || s[5] != ':' || s[8] != '.' {
		return value.TimeTzValue{}, false
	}
	hh, mm, ss, us := atoi(s[0:2]), atoi(s[3:5]), atoi(s[6:8]), atoi(s[9:15])
	if hh < 0 || mm < 0 || mm >= 60 || ss < 0 || ss >= 60 || us < 0 {
		return value.TimeTzValue{}, false
	}
	micros := ((int64(hh)*60+int64(mm))*60+int64(ss))*1_000_000 + int64(us)
	if micros > 24*3600*1_000_000 {
		return value.TimeTzValue{}, false
	}
	off := s[15:]
	sign := off[0]
	if sign != '+' && sign != '-' {
		return value.TimeTzValue{}, false
	}
	parts := strings.Split(off[1:], ":")
	if len(parts) > 3 {
		return value.TimeTzValue{}, false
	}
	vals := [3]int{}
	for i, p := range parts {
		if len(p) < 1 || len(p) > 2 {
			return value.TimeTzValue{}, false
		}
		vals[i] = atoi(p)
		if vals[i] < 0 {
			return value.TimeTzValue{}, false
		}
		if sign == '-' {
			vals[i] = -vals[i]
		}
	}
	offset := (vals[0]*60+vals[1])*60 + vals[2]
	if offset < -maxTimeTzOffset || offset > maxTimeTzOffset {
		return value.TimeTzValue{}, false
	}
	v, err := value.NewTimeTz(time.Duration(micros)*time.Microsecond, int32(offset))
	if err != nil {
		return value.TimeTzValue{}, false
	}
	return v, true
}
