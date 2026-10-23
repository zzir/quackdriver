package sqlfmt

import (
	"fmt"
	"strconv"
	"strings"
)

// This file ports the parts of DuckDB 2.0's SQL tokenizer
// (src/parser/peg/tokenizer/base_tokenizer.cpp, Tokenizer::TokenizeInputInternal)
// that decide where string literals, quoted identifiers, comments and
// parameter placeholders begin and end. FormatQuery only substitutes
// placeholders that DuckDB itself would see as parameter tokens, so the scanner
// must agree with the server byte for byte on those boundaries: a mismatch
// would either leave a placeholder unsubstituted or, far worse, substitute
// text that the server considers part of a literal or comment.
//
// Tokens that cannot contain a placeholder (keywords, numbers, operators) are
// skipped with the same state transitions DuckDB uses, because they decide
// whether a following `E'` starts a backslash-escaped string and whether a `$`
// starts a dollar-quoted string, a parameter, or is part of an identifier.

// spanKind classifies a region of a query found by scanSQL.
type spanKind uint8

const (
	spanString       spanKind = iota + 1 // '...' and the N'...', X'...', B'...' variants
	spanEscapeString                     // E'...' (backslash escapes)
	spanDollarString                     // $$...$$ or $tag$...$tag$
	spanQuotedIdent                      // "..." or `...`
	spanLineComment                      // -- ... up to and including \n or \r
	spanBlockComment                     // /* ... */, nested
	spanParam                            // ?, ?N, $N or $name
)

// paramKind distinguishes the placeholder forms.
type paramKind uint8

const (
	paramSequential paramKind = iota + 1 // ?
	paramNumbered                        // ?N or $N (1-based)
	paramNamed                           // $name
)

// span is a region of the query, [start, end) in bytes.
type span struct {
	kind         spanKind
	start, end   int
	unterminated bool // literal, identifier or block comment runs to the end of input

	// Placeholder details (kind == spanParam).
	param paramKind
	num   uint64 // paramNumbered: the 1-based parameter number
	name  string // paramNamed: the label after '$'
}

// maxParamNumber bounds ?N / $N. DuckDB accepts any positive int64, but a
// driver call can never carry more arguments than this, so larger numbers are
// always a "missing argument" error anyway; the bound keeps the arithmetic
// trivially safe.
const maxParamNumber = 1 << 31

// scanSQL splits query into the regions that matter for parameter
// substitution. Regions not reported (whitespace, keywords, numbers,
// operators) never contain a placeholder. It never panics; the only errors are
// malformed numbered placeholders such as "$1.5" or "?1e3", which DuckDB
// itself would reject or misinterpret.
func scanSQL(q string) ([]span, error) {
	var spans []span
	n := len(q)
	for i := 0; i < n; {
		c := q[i]
		switch {
		case c == '\'':
			end, ok := scanQuoted(q, i+1, false)
			spans = append(spans, span{kind: spanString, start: i, end: end, unterminated: !ok})
			i = end
		case c == '"' || c == '`':
			// Double quotes are the SQL standard identifier delimiter; DuckDB 2.0
			// also accepts backticks, with the same doubling rule.
			end, ok := scanDelimited(q, i+1, c)
			spans = append(spans, span{kind: spanQuotedIdent, start: i, end: end, unterminated: !ok})
			i = end
		case c == '$':
			s, next, err := scanDollar(q, i)
			if err != nil {
				return nil, err
			}
			if s.kind != 0 {
				spans = append(spans, s)
			}
			i = next
		case c == '-' && i+1 < n && q[i+1] == '-':
			end := n
			if k := strings.IndexAny(q[i+2:], "\n\r"); k >= 0 {
				end = i + 2 + k + 1
			}
			spans = append(spans, span{kind: spanLineComment, start: i, end: end})
			i = end
		case c == '/' && i+1 < n && q[i+1] == '*':
			end, ok := scanBlockComment(q, i+2)
			spans = append(spans, span{kind: spanBlockComment, start: i, end: end, unterminated: !ok})
			i = end
		case c == '?':
			if i+1 < n && isDigit(q[i+1]) {
				s, err := scanNumberedParam(q, i)
				if err != nil {
					return nil, err
				}
				spans = append(spans, s)
				i = s.end
				continue
			}
			spans = append(spans, span{kind: spanParam, start: i, end: i + 1, param: paramSequential})
			i++
		case isSpace(c) || isSingleByteOperator(c) || c == ';':
			i++
		case isInitialNumber(c):
			i = scanNumber(q, i)
		case isSpecialStringPrefix(c) && i+1 < n && q[i+1] == '\'':
			// N'..', X'..', B'..' are ordinary quoted strings; only E'..'
			// enables backslash escapes. This is recognised only at the start
			// of a token, which is why keywords and numbers are skipped with
			// DuckDB's own rules below.
			escape := c == 'E' || c == 'e'
			end, ok := scanQuoted(q, i+2, escape)
			kind := spanString
			if escape {
				kind = spanEscapeString
			}
			spans = append(spans, span{kind: kind, start: i, end: end, unterminated: !ok})
			i = end
		case isOperatorChar(c):
			// DuckDB collects operator characters into one token, but inside
			// an operator it still recognises "--" and "/*" as comment starts,
			// so advancing one byte at a time is equivalent.
			i++
		default:
			i = scanKeyword(q, i)
		}
	}
	return spans, nil
}

// scanQuoted returns the end (exclusive) of a single-quoted string whose
// body starts at from, and whether the closing quote was found. A doubled
// quote is an escaped quote; with escape set (E'...'), a backslash also
// escapes the following byte.
func scanQuoted(q string, from int, escape bool) (int, bool) {
	n := len(q)
	for i := from; i < n; i++ {
		c := q[i]
		if escape && c == '\\' && i+1 < n {
			i++
			continue
		}
		if c == '\'' {
			if i+1 < n && q[i+1] == '\'' {
				i++
				continue
			}
			return i + 1, true
		}
	}
	return n, false
}

// scanDelimited returns the end (exclusive) of a quoted identifier with the
// given delimiter whose body starts at from. A doubled delimiter is escaped.
func scanDelimited(q string, from int, delim byte) (int, bool) {
	n := len(q)
	for i := from; i < n; i++ {
		if q[i] != delim {
			continue
		}
		if i+1 < n && q[i+1] == delim {
			i++
			continue
		}
		return i + 1, true
	}
	return n, false
}

// scanBlockComment returns the end (exclusive) of a block comment whose body
// starts at from. Block comments nest.
func scanBlockComment(q string, from int) (int, bool) {
	n := len(q)
	depth := 1
	for i := from; i < n; {
		switch {
		case q[i] == '/' && i+1 < n && q[i+1] == '*':
			depth++
			i += 2
		case q[i] == '*' && i+1 < n && q[i+1] == '/':
			depth--
			i += 2
			if depth == 0 {
				return i, true
			}
		default:
			i++
		}
	}
	return n, false
}

// scanDollar handles a '$' at position i. It returns the span it found (a
// zero span for a bare '$' operator) and the position to continue at.
func scanDollar(q string, i int) (span, int, error) {
	n := len(q)
	if i+1 >= n {
		return span{}, n, nil
	}
	if isDigit(q[i+1]) {
		// $N is a numbered parameter, never a dollar quote.
		s, err := scanNumberedParam(q, i)
		return s, s.end, err
	}
	// $tag$ opens a dollar-quoted string when the tag consists of valid tag
	// characters (the first one is known not to be a digit) up to the next '$'.
	j := i + 1
	for j < n && isDollarTagChar(q[j]) {
		j++
	}
	if j < n && q[j] == '$' {
		marker := q[i : j+1]
		body := j + 1
		if k := strings.Index(q[body:], marker); k >= 0 {
			end := body + k + len(marker)
			return span{kind: spanDollarString, start: i, end: end}, end, nil
		}
		return span{kind: spanDollarString, start: i, end: n, unterminated: true}, n, nil
	}
	// Otherwise '$' is an operator token. When a keyword/identifier token
	// follows immediately, the grammar reads the pair as a named parameter
	// (ColLabelParameter <- '$' ColLabel).
	j = i + 1
	if startsKeyword(q, j) {
		end := scanKeyword(q, j)
		return span{kind: spanParam, start: i, end: end, param: paramNamed, name: q[j:end]}, end, nil
	}
	return span{}, i + 1, nil
}

// scanNumberedParam parses ?N or $N starting at i (q[i] is '?' or '$' and
// q[i+1] is a digit). DuckDB tokenizes the number with its numeric-literal
// rules, so a number that keeps going ("$1.5", "$1e3", "$1_0") is rejected
// rather than guessed at.
func scanNumberedParam(q string, i int) (span, error) {
	digitsEnd := i + 1
	for digitsEnd < len(q) && isDigit(q[digitsEnd]) {
		digitsEnd++
	}
	if numEnd := scanNumber(q, i+1); numEnd != digitsEnd {
		return span{}, fmt.Errorf("sqlfmt: malformed numbered parameter %q at offset %d", q[i:numEnd], i)
	}
	text := q[i+1 : digitsEnd]
	num, err := strconv.ParseUint(text, 10, 64)
	if err != nil || num > maxParamNumber {
		return span{}, fmt.Errorf("sqlfmt: parameter number %s at offset %d is out of range", text, i)
	}
	if num == 0 {
		return span{}, fmt.Errorf("sqlfmt: parameter numbers start at 1 (%q at offset %d)", q[i:digitsEnd], i)
	}
	return span{kind: spanParam, start: i, end: digitsEnd, param: paramNumbered, num: num}, nil
}

// scanNumber returns the end of the numeric-literal token starting at start
// (q[start] is a digit or '.'), following DuckDB 2.0's NUMERIC tokenizer state
// including its backtracking over a trailing exponent marker or sign.
func scanNumber(q string, start int) int {
	n := len(q)
	for i := start + 1; i < n; i++ {
		c := q[i]
		if isInitialNumber(c) {
			continue
		}
		// A digit separator must sit between two digits.
		if c == '_' && isDigit(q[i-1]) && i+1 < n && isDigit(q[i+1]) {
			continue
		}
		if isScientific(c) && !isScientific(q[i-1]) && (isDigit(q[start]) || isDigit(q[i-1])) {
			continue
		}
		if (c == '+' || c == '-') && isScientific(q[i-1]) {
			continue
		}
		// End of the number: give back any trailing characters that cannot
		// end a number ("1e+" followed by a letter yields "1").
		for !isInitialNumber(q[i-1]) {
			i--
		}
		return i
	}
	// At end of input DuckDB keeps the whole remainder as the number token.
	return n
}

// scanKeyword returns the end of a keyword/identifier token starting at i.
// '$' is a valid non-initial identifier character.
func scanKeyword(q string, i int) int {
	i++
	for i < len(q) && (q[i] == '$' || isKeywordChar(q[i])) {
		i++
	}
	return i
}

// startsKeyword reports whether DuckDB, in its STANDARD state at position j,
// would start a keyword/identifier token there.
func startsKeyword(q string, j int) bool {
	if j >= len(q) {
		return false
	}
	c := q[j]
	if !isKeywordChar(c) || isInitialNumber(c) {
		return false
	}
	if isSpecialStringPrefix(c) && j+1 < len(q) && q[j+1] == '\'' {
		return false
	}
	return true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isSpace mirrors StringUtil::CharacterIsSpace.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

// isInitialNumber mirrors Tokenizer::CharacterIsInitialNumber.
func isInitialNumber(c byte) bool { return isDigit(c) || c == '.' }

// isScientific mirrors Tokenizer::CharacterIsScientific.
func isScientific(c byte) bool { return c == 'e' || c == 'E' }

// isSpecialStringPrefix mirrors Tokenizer::CharacterIsSpecialStringCharacter.
func isSpecialStringPrefix(c byte) bool {
	switch c {
	case 'N', 'n', 'X', 'x', 'E', 'e', 'B', 'b':
		return true
	}
	return false
}

// isSingleByteOperator mirrors Tokenizer::IsSingleByteOperator.
func isSingleByteOperator(c byte) bool {
	switch c {
	case '(', ')', '{', '}', '[', ']', ',', ':', '?', '$', '#':
		return true
	}
	return false
}

// isOperatorChar mirrors StringUtil::CharacterIsOperator: ASCII punctuation
// except '_'.
func isOperatorChar(c byte) bool {
	if c == '_' {
		return false
	}
	return (c >= '!' && c <= '/') || (c >= ':' && c <= '@') || (c >= '[' && c <= '`') || (c >= '{' && c <= '~')
}

// isControlFlow mirrors Tokenizer::CharacterIsControlFlow.
func isControlFlow(c byte) bool {
	return c == '\'' || c == ';' || c == '"' || c == '.'
}

// isKeywordChar mirrors Tokenizer::CharacterIsKeyword: letters, digits, '_',
// bytes >= 0x80 and the remaining control bytes.
func isKeywordChar(c byte) bool {
	return !isSingleByteOperator(c) && !isOperatorChar(c) && !isSpace(c) && !isControlFlow(c)
}

// isDollarTagChar mirrors Tokenizer::IsValidDollarTagCharacter.
func isDollarTagChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || isDigit(c) || c == '_' || c >= 0x80
}

// unicodeSpace is one multi-byte Unicode space that DuckDB's query
// normalizer would replace with an ASCII space.
type unicodeSpace struct{ pos, size int }

// unicodeSpaces ports Parser::StripUnicodeSpaces (src/parser/parser.cpp),
// which runs over the whole query before tokenization and replaces Unicode
// space characters with ' ' wherever it believes it is outside a quoted
// string, a dollar-quoted string or a line comment. Its notion of "quoted" is
// simpler than the tokenizer's (no E'...' escapes, backticks or block comments),
// so in some queries it rewrites bytes inside a literal. FormatQuery uses this
// to refuse substitutions whose content would be altered that way.
func unicodeSpaces(q string) []unicodeSpace {
	var out []unicodeSpace
	n := len(q)
	pos := 0
	const (
		regular = iota
		inQuotes
		inDollar
		inComment
	)
	state := regular
	var quote byte
	var tag string
	for {
		switch state {
		case regular:
			next := -1
			for ; pos+2 < n; pos++ {
				b := q[pos]
				if b == 0xC2 && q[pos+1] == 0xA0 {
					out = append(out, unicodeSpace{pos, 2}) // U+00A0
				}
				switch {
				case b == 0xE2:
					b1, b2 := q[pos+1], q[pos+2]
					if (b1 == 0x80 && ((b2 >= 0x80 && b2 <= 0x8B) || b2 == 0xAF)) ||
						(b1 == 0x81 && (b2 == 0x9F || b2 == 0xA0)) {
						out = append(out, unicodeSpace{pos, 3}) // U+2000..U+200B, U+202F, U+205F, U+2060
					}
				case b == 0xE3:
					if q[pos+1] == 0x80 && q[pos+2] == 0x80 {
						out = append(out, unicodeSpace{pos, 3}) // U+3000
					}
				case b == 0xEF:
					if q[pos+1] == 0xBB && q[pos+2] == 0xBF {
						out = append(out, unicodeSpace{pos, 3}) // U+FEFF
					}
				case b == '"' || b == '\'':
					quote = b
					pos++
					next = inQuotes
				case b == '$' && (q[pos+1] == '$' || isStripTagFirstChar(q[pos+1])):
					pos++
					start := pos
					next = -2 // end of input unless a tag terminator is found
					for ; pos+2 < n; pos++ {
						if q[pos] == '$' {
							tag = q[start:pos]
							next = inDollar
							break
						}
						if !isStripTagChar(q[pos]) {
							next = regular
							break
						}
					}
				case b == '-' && q[pos+1] == '-':
					next = inComment
				}
				if next != -1 {
					break
				}
			}
			switch next {
			case -1, -2:
				return out
			case regular:
				// Re-examine the byte that ended the tag scan in regular mode.
				state = regular
			default:
				state = next
			}
		case inQuotes:
			state = -1
			for ; pos+1 < n; pos++ {
				if q[pos] == quote {
					if q[pos+1] == quote {
						pos++
						continue
					}
					pos++
					state = regular
					break
				}
			}
			if state != regular {
				return out
			}
		case inDollar:
			state = -1
			for ; pos+2 < n; pos++ {
				if q[pos] == '$' && n-(pos+1) >= len(tag)+1 && q[pos+len(tag)+1] == '$' &&
					q[pos+1:pos+1+len(tag)] == tag {
					pos += len(tag) + 1
					state = regular
					break
				}
			}
			if state != regular {
				return out
			}
		case inComment:
			state = -1
			for ; pos < n; pos++ {
				if q[pos] == '\n' || q[pos] == '\r' {
					state = regular
					break
				}
			}
			if state != regular {
				return out
			}
		default:
			return out
		}
	}
}

func isStripTagFirstChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' || c >= 0x80
}

func isStripTagChar(c byte) bool { return isStripTagFirstChar(c) || isDigit(c) }
