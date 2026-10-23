//go:build integration

// Package types holds end-to-end tests of result decoding: every DuckDB type
// and every vector format a DuckDB 2.0 server sends over Quack, checked
// against the server's own VARCHAR rendering of the same values.
package types_test

import (
	"database/sql"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/value"
)

// env is one server and a pool (of at most one connection, so session
// settings stick) whose responses are recorded.
type env struct {
	db   *sql.DB
	rec  *livetest.Recorder
	mark int // start of the responses chunks and raw look at
}

func newEnv(t *testing.T, params ...string) *env {
	t.Helper()
	srv := livetest.StartServer(t, livetest.ServerOptions{})
	rec := livetest.NewRecorder()
	db := livetest.OpenDB(t, srv.DSN(params...), quackdriver.WithHTTPClient(&http.Client{Transport: rec}))
	db.SetMaxOpenConns(1)
	return &env{db: db, rec: rec}
}

// reset forgets the responses recorded so far.
func (e *env) reset() { e.mark = e.rec.Mark() }

// raw returns the response bodies recorded since reset, concatenated.
func (e *env) raw() []byte {
	var out []byte
	for _, x := range e.rec.Since(e.mark) {
		out = append(out, x.Resp...)
	}
	return out
}

// chunks decodes the responses recorded since reset and returns their
// result chunks.
func (e *env) chunks(t *testing.T) []message.DataChunk {
	t.Helper()
	var out []message.DataChunk
	for _, x := range e.rec.Since(e.mark) {
		_, body, err := message.Decode(x.Resp)
		if err != nil {
			t.Fatalf("decode recorded response: %v", err)
		}
		switch m := body.(type) {
		case message.PrepareResponse:
			out = append(out, m.Results...)
		case message.FetchResponse:
			out = append(out, m.Results...)
		}
	}
	return out
}

// columnKinds counts the column implementations of the first column in the
// chunks recorded since reset, e.g. {"dictColumn": 3}.
func (e *env) columnKinds(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, c := range e.chunks(t) {
		out[strings.TrimPrefix(fmt.Sprintf("%T", c.Columns[0]), "*message.")]++
	}
	return out
}

func (e *env) exec(t *testing.T, query string) {
	t.Helper()
	livetest.Exec(t, e.db, query)
}

// rows runs query and returns every row as decoded values.
func (e *env) rows(t *testing.T, query string) [][]any {
	t.Helper()
	return livetest.Rows(t, e.db, query)
}

// one runs a single-row, single-column query.
func (e *env) one(t *testing.T, query string) any {
	t.Helper()
	rows := e.rows(t, query)
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%q: got %d rows, want 1x1", query, len(rows))
	}
	return rows[0][0]
}

// checkRendered selects each expression together with its type and the
// server's VARCHAR rendering, and compares that rendering with render(value).
// Every expression is checked as a literal (CONSTANT vector) and, mixed
// with a NULL row, as a FLAT vector.
func (e *env) checkRendered(t *testing.T, exprs ...string) {
	t.Helper()
	for _, expr := range exprs {
		for _, form := range []string{
			"SELECT x, typeof(x), x::VARCHAR FROM (SELECT " + expr + " AS x)",
			"SELECT x, typeof(x), x::VARCHAR FROM (SELECT CASE WHEN i = 1 THEN " + expr + " END AS x FROM range(3) t(i)) ORDER BY x NULLS FIRST",
		} {
			for _, row := range e.rows(t, form) {
				typ, _ := row[1].(string)
				checkText(t, expr, typ, row[0], row[2])
			}
		}
	}
}

// checkText compares the rendering of a decoded value of type typ with the
// server's VARCHAR rendering want (both may be NULL).
func checkText(t *testing.T, what, typ string, got, want any) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Errorf("%s: decoded %#v, server renders %#v", what, got, want)
		}
		return
	}
	s, err := render(got, typ)
	if err != nil {
		t.Errorf("%s (%s): %v", what, typ, err)
		return
	}
	w := want.(string)
	// DuckDB drops the integer digit of DECIMAL(w,w) values (".5"); the
	// driver keeps it ("0.5").
	if strings.HasPrefix(typ, "DECIMAL") && (strings.HasPrefix(w, ".") || strings.HasPrefix(w, "-.")) {
		w = strings.Replace(w, ".", "0.", 1)
	}
	if s != w {
		t.Errorf("%s (%s): Go value %s renders as %q, server says %q", short(what), typ, short(fmt.Sprintf("%#v", got)), short(s), short(w))
	}
}

// render formats a decoded scalar the way DuckDB casts the value of type typ
// (as printed by typeof) to VARCHAR.
func render(v any, typ string) (string, error) {
	switch {
	case typ == "DATE":
		tm, ok := v.(time.Time)
		if !ok {
			return "", fmt.Errorf("DATE decoded as %T", v)
		}
		return renderDate(tm), nil
	case strings.HasPrefix(typ, "TIMESTAMP"):
		tm, ok := v.(time.Time)
		if !ok {
			return "", fmt.Errorf("%s decoded as %T", typ, v)
		}
		s := renderTimestamp(tm)
		if (strings.Contains(typ, "TIME ZONE") || strings.HasPrefix(typ, "TIMESTAMPTZ")) && s != "infinity" && s != "-infinity" {
			s += "+00"
		}
		return s, nil
	case typ == "TIME" || typ == "TIME_NS":
		d, ok := v.(time.Duration)
		if !ok {
			return "", fmt.Errorf("%s decoded as %T", typ, v)
		}
		return renderTimeOfDay(d), nil
	case strings.HasPrefix(typ, "DECIMAL"):
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("DECIMAL decoded as %T", v)
		}
		return s, nil
	case strings.HasPrefix(typ, "ENUM"):
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("ENUM decoded as %T", v)
		}
		return s, nil
	}
	switch x := v.(type) {
	case bool:
		return strconv.FormatBool(x), nil
	case int8, int16, int32, int64, uint8, uint16, uint32, uint64:
		return fmt.Sprint(x), nil
	case *big.Int:
		return x.String(), nil
	case float32:
		return renderFloat(float64(x), 32), nil
	case float64:
		return renderFloat(x, 64), nil
	case string:
		return x, nil
	case []byte:
		return renderBlob(x), nil
	case quackdriver.IntervalValue:
		return x.String(), nil
	case quackdriver.TimeTzValue:
		return x.String(), nil
	}
	return "", fmt.Errorf("no renderer for %T", v)
}

// renderFloat mimics DuckDB's float to string cast: the shortest
// round-tripping digits, positional notation for decimal exponents in
// [-4, 15] with at least one fractional digit, scientific notation with a
// two-digit exponent otherwise.
func renderFloat(f float64, bits int) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	e := strconv.FormatFloat(f, 'e', -1, bits) // e.g. -1.2345e+06
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	neg := strings.HasPrefix(mant, "-")
	mant = strings.TrimPrefix(mant, "-")
	digits := strings.Replace(mant, ".", "", 1)
	sign := ""
	if neg {
		sign = "-"
	}
	if exp < -4 || exp > 15 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		es := "+"
		if exp < 0 {
			es = "-"
			exp = -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, m, es, exp)
	}
	var intPart, frac string
	if exp >= 0 {
		for len(digits) < exp+1 {
			digits += "0"
		}
		intPart, frac = digits[:exp+1], digits[exp+1:]
	} else {
		intPart, frac = "0", strings.Repeat("0", -exp-1)+digits
	}
	if frac == "" {
		frac = "0"
	}
	return sign + intPart + "." + frac
}

// renderBlob mimics DuckDB's BLOB to VARCHAR cast: printable ASCII except
// the quotes and the backslash as is, everything else as \xHH.
func renderBlob(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c <= 0x7e && c != '\'' && c != '"' && c != '\\' {
			sb.WriteByte(c)
		} else {
			fmt.Fprintf(&sb, "\\x%02X", c)
		}
	}
	return sb.String()
}

// renderDateParts formats a proleptic Gregorian date with astronomical year
// numbering (Go year 0 is 1 BC) the way DuckDB does.
func renderDate(t time.Time) string {
	if s, ok := infinityText(t); ok {
		return s
	}
	y, m, d := t.Date()
	bc := ""
	if y <= 0 {
		y = 1 - y
		bc = " (BC)"
	}
	return fmt.Sprintf("%04d-%02d-%02d%s", y, int(m), d, bc)
}

func renderTimestamp(t time.Time) string {
	if s, ok := infinityText(t); ok {
		return s
	}
	tod := time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second + time.Duration(t.Nanosecond())
	return renderDate(t) + " " + renderTimeOfDay(tod)
}

// renderTimeOfDay formats HH:MM:SS with the fraction's trailing zeros
// trimmed.
func renderTimeOfDay(d time.Duration) string {
	h := d / time.Hour
	m := d / time.Minute % 60
	s := d / time.Second % 60
	ns := d % time.Second
	out := fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	if ns != 0 {
		out += strings.TrimRight(fmt.Sprintf(".%09d", ns), "0")
	}
	return out
}

func infinityText(t time.Time) (string, bool) {
	switch {
	case t.Equal(value.InfinityTime):
		return "infinity", true
	case t.Equal(value.NegInfinityTime):
		return "-infinity", true
	}
	return "", false
}

// short truncates s for error messages.
func short(s string) string {
	if len(s) > 300 {
		return s[:300] + fmt.Sprintf("...(%d bytes)", len(s))
	}
	return s
}
