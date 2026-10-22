// Package sqlfmt substitutes parameters into a SQL query as DuckDB literals.
//
// The Quack protocol (v3, DuckDB 2.0) has no server-side parameter-bind path:
// a PREPARE request carries only SQL text. This package is therefore the
// *only* injection-defense surface of the driver. Placeholders are located
// with a port of DuckDB 2.0's own tokenizer (see scan.go) so that text inside
// string literals, quoted identifiers and comments is never touched, and every
// value is rendered as a self-delimiting literal: strings are single-quoted
// with embedded quotes doubled (DuckDB's standard strings never treat a
// backslash specially), and every other type uses a fixed ASCII spelling.
package sqlfmt

import (
	"database/sql/driver"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/value"
)

// FormatQuery rewrites query by replacing each parameter placeholder with the
// corresponding argument formatted as a SQL literal.
//
// Supported placeholders (as in DuckDB 2.0's grammar):
//
//	?        sequential: the i-th '?' takes the i-th positional argument
//	?N, $N   numbered (1-based) positional argument; may be repeated
//	$name    the argument passed as sql.Named("name", v), matched
//	         ASCII-case-insensitively like DuckDB identifiers; a $name
//	         with no such argument is left untouched, so DuckDB
//	         session-variable parameters keep working
//
// Positional arguments are the ones without a Name, in order. Mixing '?' with
// numbered placeholders is an error, as is a placeholder without an argument
// and an argument that no placeholder uses.
//
// Placeholders inside '...' / E'...' / $tag$...$tag$ strings, "..." or `...`
// identifiers, -- comments and (nested) /* */ comments are ignored. A space is
// inserted around a substituted literal where it would otherwise fuse with the
// neighbouring text (for example "E?" or "1-?").
func FormatQuery(query string, args []driver.NamedValue) (string, error) {
	spans, err := scanSQL(query)
	if err != nil {
		return "", err
	}

	// Split the arguments into positional and named ones.
	var positional []int // indexes into args
	var named []int
	for i, a := range args {
		if a.Name == "" {
			positional = append(positional, i)
			continue
		}
		for _, j := range named {
			if ciEquals(args[j].Name, a.Name) {
				return "", fmt.Errorf("sqlfmt: duplicate named argument %q", a.Name)
			}
		}
		named = append(named, i)
	}

	// The placeholders to substitute: an index into spans and one into args.
	type sub struct{ span, arg int }
	var (
		subs        []sub
		used        = make([]bool, len(args))
		nSequential int
		nNumbered   int
	)
	for si, s := range spans {
		if s.kind != spanParam {
			continue
		}
		arg := -1
		switch s.param {
		case paramSequential:
			if nSequential >= len(positional) {
				return "", fmt.Errorf("sqlfmt: not enough arguments: placeholder ? at offset %d has no argument (%d positional arguments given)", s.start, len(positional))
			}
			arg = positional[nSequential]
			nSequential++
		case paramNumbered:
			if s.num > uint64(len(positional)) {
				return "", fmt.Errorf("sqlfmt: placeholder %s at offset %d has no argument (%d positional arguments given)", query[s.start:s.end], s.start, len(positional))
			}
			arg = positional[s.num-1]
			nNumbered++
		case paramNamed:
			for _, k := range named {
				if ciEquals(args[k].Name, s.name) {
					arg = k
					break
				}
			}
			if arg < 0 {
				// Not ours: leave it for DuckDB (e.g. a session variable).
				continue
			}
		}
		if nSequential > 0 && nNumbered > 0 {
			return "", fmt.Errorf("sqlfmt: cannot mix sequential ? placeholders with numbered ?N/$N placeholders")
		}
		subs = append(subs, sub{si, arg})
		used[arg] = true
	}
	for i, ok := range used {
		if ok {
			continue
		}
		if args[i].Name != "" {
			return "", fmt.Errorf("sqlfmt: named argument %q is not used by any $%s placeholder", args[i].Name, args[i].Name)
		}
		return "", fmt.Errorf("sqlfmt: too many arguments: positional argument %d is not used by any placeholder", positionOf(positional, i)+1)
	}
	if len(subs) == 0 {
		return query, nil
	}

	// Format each argument once, even when a numbered placeholder repeats
	// it, and record the literal substituted for each span.
	lits := make(map[int]string, len(subs))
	substituted := make(map[int]string, len(subs))
	for _, sb := range subs {
		lit, ok := lits[sb.arg]
		if !ok {
			var err error
			if lit, err = FormatValue(args[sb.arg].Value); err != nil {
				return "", fmt.Errorf("sqlfmt: %s: %w", argLabel(args, positional, sb.arg), err)
			}
			lits[sb.arg] = lit
		}
		substituted[sb.span] = lit
	}

	var out strings.Builder
	out.Grow(len(query) + 16*len(subs))
	type litRange struct{ start, end, arg int }
	ranges := make([]litRange, 0, len(subs))
	last := 0
	for _, sb := range subs {
		s := spans[sb.span]
		out.WriteString(query[last:s.start])
		lit := substituted[sb.span]
		if continuesString(query, spans, substituted, sb.span, lit) {
			lit = "(" + lit + ")"
		}
		if b := lastByte(&out); b != 0 && !safeBeforeLiteral(b) {
			out.WriteByte(' ')
		}
		start := out.Len()
		out.WriteString(lit)
		ranges = append(ranges, litRange{start, out.Len(), sb.arg})
		if s.end < len(query) && !safeAfterLiteral(query[s.end]) {
			out.WriteByte(' ')
		}
		last = s.end
	}
	out.WriteString(query[last:])
	result := out.String()

	// DuckDB replaces Unicode spaces with ' ' before tokenizing, using a
	// simpler quoting model than the tokenizer. Refuse queries where that
	// would silently rewrite the content of a substituted literal.
	r := 0
	for _, us := range unicodeSpaces(result) {
		for r < len(ranges) && ranges[r].end <= us.pos {
			r++
		}
		if r < len(ranges) && us.pos >= ranges[r].start {
			return "", fmt.Errorf("sqlfmt: %s: contains a Unicode space character that DuckDB's query normalizer would replace in this query (move quotes out of comments or E'' strings before the placeholder)", argLabel(args, positional, ranges[r].arg))
		}
	}
	if err := checkNormalizedTokens(result); err != nil {
		return "", err
	}
	return result, nil
}

// checkNormalizedTokens refuses a query whose string literals, quoted
// identifiers, comments or placeholders DuckDB would delimit differently from
// the scanner. DuckDB replaces Unicode spaces with ' ' before it tokenizes
// (Parser::StripUnicodeSpaces), while the scanner, like DuckDB's tokenizer,
// treats those bytes as identifier characters; so a Unicode space in the
// query text can split a token the scanner saw whole, for example turning
// "SELECT<U+200B>E'\', ?" into an E'...' string that swallows the
// substituted literal. The query is scanned again as DuckDB will see it and
// every region must be unchanged.
func checkNormalizedTokens(q string) error {
	us := unicodeSpaces(q)
	if len(us) == 0 {
		return nil
	}
	var norm strings.Builder
	norm.Grow(len(q))
	last := 0
	for _, u := range us {
		norm.WriteString(q[last:u.pos])
		norm.WriteByte(' ')
		last = u.pos + u.size
	}
	norm.WriteString(q[last:])
	// mapPos translates an offset of q that does not fall inside a replaced
	// character to the corresponding offset of the normalized text.
	mapPos := func(p int) int {
		for _, u := range us {
			if u.pos >= p {
				break
			}
			p -= u.size - 1
		}
		return p
	}
	errMismatch := fmt.Errorf("sqlfmt: the query contains Unicode space characters (such as U+00A0 or U+200B) " +
		"outside string literals that change how DuckDB tokenizes it; replace them with ASCII spaces")
	before, err := scanSQL(q)
	if err != nil {
		return errMismatch
	}
	after, err := scanSQL(norm.String())
	if err != nil || len(before) != len(after) {
		return errMismatch
	}
	for i, b := range before {
		a := after[i]
		if a.kind != b.kind || a.unterminated != b.unterminated || a.start != mapPos(b.start) || a.end != mapPos(b.end) {
			return errMismatch
		}
	}
	return nil
}

// continuesString reports whether the literal lit substituted for spans[idx]
// would be joined with a neighbouring single-quoted string literal. DuckDB
// 2.0 concatenates a standard '...' literal onto a preceding single-quoted
// literal (of any kind: E'...', X'...', '...', or the string of a typed
// literal such as TIMESTAMP '...') when only whitespace with a newline, or
// line comments, separate them; the result keeps the first literal's kind, so
// an argument following E'...' would get backslash escapes applied and one
// following X'...' would be read as hex. A bound parameter in that position
// is a syntax error; the caller wraps the literal in parentheses, which keeps
// it a separate expression (and the query an error).
func continuesString(query string, spans []span, substituted map[int]string, idx int, lit string) bool {
	// Walk the whitespace and comments between the two tokens: a block
	// comment prevents the continuation, a newline (or a line comment, which
	// ends with one) is required.
	if strings.HasPrefix(lit, "'") {
		newline := false
		pos := spans[idx].start
		for j := idx - 1; ; j-- {
			from := 0
			if j >= 0 {
				from = spans[j].end
			}
			gap := query[from:pos]
			if !allSpace(gap) {
				break
			}
			newline = newline || strings.ContainsAny(gap, "\n\r")
			if j < 0 {
				break
			}
			p := spans[j]
			if p.kind == spanLineComment {
				newline = true
				pos = p.start
				continue
			}
			if !newline {
				break
			}
			switch p.kind {
			case spanString, spanEscapeString:
				return true
			case spanParam:
				if prev, ok := substituted[j]; ok && strings.HasSuffix(prev, "'") {
					return true
				}
			}
			break
		}
	}
	if strings.HasSuffix(lit, "'") {
		newline := false
		pos := spans[idx].end
		for j := idx + 1; j < len(spans); j++ {
			if !allSpace(query[pos:spans[j].start]) {
				return false
			}
			newline = newline || strings.ContainsAny(query[pos:spans[j].start], "\n\r")
			n := spans[j]
			if n.kind == spanLineComment {
				newline = true
				pos = n.end
				continue
			}
			// Only a standard '...' literal continues a string (a following
			// placeholder is handled when it is substituted).
			return newline && n.kind == spanString && query[n.start] == '\''
		}
	}
	return false
}

func allSpace(s string) bool {
	for i := range len(s) {
		if !isSpace(s[i]) {
			return false
		}
	}
	return true
}

func positionOf(positional []int, argIndex int) int {
	for p, i := range positional {
		if i == argIndex {
			return p
		}
	}
	return argIndex
}

func argLabel(args []driver.NamedValue, positional []int, j int) string {
	if args[j].Name != "" {
		return fmt.Sprintf("argument %q", args[j].Name)
	}
	return fmt.Sprintf("argument %d", positionOf(positional, j)+1)
}

func lastByte(b *strings.Builder) byte {
	s := b.String()
	if s == "" {
		return 0
	}
	return s[len(s)-1]
}

// safeBeforeLiteral reports whether a literal may directly follow byte b
// without the two fusing into a different token (E'..' escape strings, "--"
// comments, "$1" parameters, "1'x'" and so on).
func safeBeforeLiteral(b byte) bool {
	switch b {
	case '(', ',', '[', '{', '=', '<', '>':
		return true
	}
	return isSpace(b)
}

// safeAfterLiteral reports whether byte b may directly follow a literal.
func safeAfterLiteral(b byte) bool {
	switch b {
	case ')', ',', ']', '}', ';', ':':
		return true
	}
	return isSpace(b)
}

// FormatValue returns the DuckDB SQL literal form of v. Every result is a
// single self-delimiting expression: negative numbers are parenthesized so
// that neighbouring operators and casts cannot change their meaning.
//
// Supported types: nil, bool, all Go integer types (untyped integer
// literals), float types (DOUBLE), string, []byte (BLOB; a nil []byte is
// NULL), time.Time (see formatTimestamp), time.Duration (INTERVAL, truncated
// to microseconds), value.IntervalValue (INTERVAL), value.TimeTzValue
// (TIMETZ), *big.Int and big.Int (integer literals) and *big.Rat (DECIMAL,
// see formatRat).
func FormatValue(v driver.Value) (string, error) {
	if v == nil {
		return "NULL", nil
	}
	switch x := v.(type) {
	case bool:
		if x {
			return "TRUE", nil
		}
		return "FALSE", nil
	case int:
		return formatInt(int64(x)), nil
	case int8:
		return formatInt(int64(x)), nil
	case int16:
		return formatInt(int64(x)), nil
	case int32:
		return formatInt(int64(x)), nil
	case int64:
		return formatInt(x), nil
	case uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return formatFloat(float64(x)), nil
	case float64:
		return formatFloat(x), nil
	case string:
		// A standard SQL string literal: only the single quote needs
		// escaping (by doubling), DuckDB's standard strings give no meaning
		// to backslashes, and the E'...' form is never emitted. Every other
		// byte, including NUL and invalid UTF-8, is copied verbatim; DuckDB
		// rejects queries that are not valid UTF-8 as a whole.
		return logicaltype.QuoteString(x), nil
	case []byte:
		if x == nil {
			// database/sql scans a NULL into a nil []byte; writing it
			// back must keep the NULL.
			return "NULL", nil
		}
		return formatBlob(x), nil
	case time.Time:
		return formatTimestamp(x)
	case time.Duration:
		return formatInterval(0, 0, x.Microseconds()), nil
	case value.IntervalValue:
		return formatInterval(x.Months, x.Days, x.Micros), nil
	case value.TimeTzValue:
		return formatTimeTz(x)
	case *big.Int:
		if x == nil {
			return "NULL", nil
		}
		return signed(x.String()), nil
	case big.Int:
		return signed(x.String()), nil
	case *big.Rat:
		if x == nil {
			return "NULL", nil
		}
		return formatRat(x)
	}
	return "", fmt.Errorf("sqlfmt: cannot format value of type %T", v)
}

func formatInt(i int64) string { return signed(strconv.FormatInt(i, 10)) }

// signed wraps a negative numeric literal in parentheses. Without them a
// preceding '-' would turn "-5" into a "--" comment, and "-5::VARCHAR" or
// "-2^2" would apply the cast/operator before the negation.
func signed(s string) string {
	if strings.HasPrefix(s, "-") {
		return "(" + s + ")"
	}
	return s
}

func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "'nan'::DOUBLE"
	case math.IsInf(f, 1):
		return "'infinity'::DOUBLE"
	case math.IsInf(f, -1):
		return "'-infinity'::DOUBLE"
	case f == 0 && math.Signbit(f):
		// A numeric literal would lose the sign of negative zero.
		return "'-0.0'::DOUBLE"
	}
	// Exponent notation makes DuckDB type the literal as DOUBLE: a plain
	// "1.5" would become DECIMAL(2,1) and "3" an INTEGER, changing both the
	// decoded type and the arithmetic (0.1 + 0.2 = 0.3 in DECIMAL). The
	// shortest representation parses back to exactly the same double.
	return signed(strconv.FormatFloat(f, 'e', -1, 64))
}

// maxDecimalWidth is DuckDB's Decimal::MAX_WIDTH_DECIMAL.
const maxDecimalWidth = 38

// formatRat renders r as a DECIMAL literal. DuckDB types a decimal literal as
// DECIMAL(width, scale) from its digits, and as DOUBLE once it has more than
// 38 digits, so the literal is kept within 38 digits: the shortest exact
// form when r has one, otherwise r rounded (halves away from zero) to as
// many fractional digits as fit. A zero integer part is written as ".5"
// rather than "0.5", since DuckDB counts that leading zero as a digit.
// Integers become integer literals (exact at any size); a non-integer with
// 38 or more integer digits does not fit any DECIMAL and is an error.
func formatRat(r *big.Rat) (string, error) {
	if r.IsInt() {
		return signed(r.Num().String()), nil
	}
	abs := new(big.Rat).Abs(r)
	intPart := new(big.Int).Quo(abs.Num(), abs.Denom())
	intDigits := 0
	if intPart.Sign() != 0 {
		intDigits = len(intPart.String())
	}
	if intDigits >= maxDecimalWidth {
		return "", fmt.Errorf("sqlfmt: *big.Rat %s does not fit a DECIMAL (at most %d digits)", r.FloatString(3), maxDecimalWidth)
	}
	s := abs.FloatString(maxDecimalWidth - intDigits)
	whole, frac, _ := strings.Cut(s, ".")
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		frac = "0"
	}
	if whole == "0" {
		whole = ""
	} else if len(whole)+len(frac) > maxDecimalWidth {
		// Rounding carried into a new integer digit (9.99... -> 10) and
		// left no fractional digits: the value is that integer.
		frac = ""
	}
	lit := whole
	if frac != "" {
		lit += "." + frac
	}
	if r.Sign() < 0 {
		lit = "-" + lit
	}
	return signed(lit), nil
}

func formatBlob(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, len(b)*4+len("''::BLOB"))
	out = append(out, '\'')
	for _, c := range b {
		out = append(out, '\\', 'x', hex[c>>4], hex[c&0x0f])
	}
	out = append(out, '\'', ':', ':', 'B', 'L', 'O', 'B')
	return string(out)
}

// formatTimestamp renders t's UTC wall clock as a TIMESTAMP literal, or as a
// TIMESTAMP_NS literal when t has sub-microsecond precision. Years before 1
// use DuckDB's " (BC)" date suffix (Go year 0 is 1 BC); years after 9999 are
// written with as many digits as needed, which DuckDB's date parser accepts.
// value.InfinityTime and value.NegInfinityTime, which DuckDB's ±infinity
// dates and timestamps decode to, become TIMESTAMP 'infinity' / '-infinity'.
func formatTimestamp(t time.Time) (string, error) {
	switch value.Infinity(t) {
	case 1:
		return "TIMESTAMP 'infinity'", nil
	case -1:
		return "TIMESTAMP '-infinity'", nil
	}
	t = t.UTC()
	nsec := t.Nanosecond()
	typ := "TIMESTAMP"
	var frac string
	if nsec%1000 != 0 {
		ns := t.UnixNano()
		// UnixNano is undefined outside int64 nanoseconds; DuckDB further
		// reserves ±(2^63-1) for ±infinity.
		if !time.Unix(0, ns).Equal(t) || ns == math.MaxInt64 || ns <= -math.MaxInt64 {
			return "", fmt.Errorf("sqlfmt: time %s has sub-microsecond precision but is outside the TIMESTAMP_NS range", t.Format(time.RFC3339Nano))
		}
		typ = "TIMESTAMP_NS"
		frac = fmt.Sprintf("%09d", nsec)
	} else {
		frac = fmt.Sprintf("%06d", nsec/1000)
	}
	year, month, day := t.Date()
	bc := ""
	if year <= 0 {
		year = 1 - year
		bc = " (BC)"
	}
	return fmt.Sprintf("%s '%04d-%02d-%02d%s %02d:%02d:%02d.%s'", typ, year, int(month), day, bc,
		t.Hour(), t.Minute(), t.Second(), frac), nil
}

// formatInterval renders an INTERVAL literal of IntervalText.
func formatInterval(months, days int32, micros int64) string {
	return "INTERVAL '" + IntervalText(months, days, micros) + "'"
}

// IntervalText renders an interval as text that DuckDB's
// Interval::FromCString parses back to exactly the same months/days/micros
// triple: each component is added independently ("N months", "N days",
// "N microseconds"), so no normalisation between units takes place.
func IntervalText(months, days int32, micros int64) string {
	parts := make([]string, 0, 4)
	if months != 0 {
		parts = append(parts, strconv.FormatInt(int64(months), 10)+" months")
	}
	if days != 0 {
		parts = append(parts, strconv.FormatInt(int64(days), 10)+" days")
	}
	switch {
	case micros == math.MinInt64:
		// The parser reads the magnitude as an int64 before negating it, so
		// 2^63 itself is out of range; add the value in two steps.
		parts = append(parts, "-9223372036854775807 microseconds", "-1 microseconds")
	case micros != 0 || len(parts) == 0:
		parts = append(parts, strconv.FormatInt(micros, 10)+" microseconds")
	}
	return strings.Join(parts, " ")
}

// maxTimeTzOffset is DuckDB's dtime_tz_t::MAX_OFFSET (15:59:59).
const maxTimeTzOffset = 16*60*60 - 1

// formatTimeTz renders a TIMETZ literal such as
// TIMETZ '12:34:56.000001+05:30:00'.
func formatTimeTz(t value.TimeTzValue) (string, error) {
	if err := t.Validate(); err != nil {
		return "", fmt.Errorf("sqlfmt: %w", err)
	}
	us := t.Micros()
	off := t.OffsetSeconds()
	sign := byte('+')
	if off < 0 {
		sign = '-'
		off = -off
	}
	return fmt.Sprintf("TIMETZ '%02d:%02d:%02d.%06d%c%02d:%02d:%02d'",
		us/3_600_000_000, us/60_000_000%60, us/1_000_000%60, us%1_000_000,
		sign, off/3600, off/60%60, off%60), nil
}

// ciEquals compares two names ASCII-case-insensitively, like DuckDB's
// StringUtil::CIEquals which backs its case-insensitive Identifier.
func ciEquals(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
