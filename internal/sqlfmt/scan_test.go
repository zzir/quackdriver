package sqlfmt

import (
	"fmt"
	"strings"
	"testing"
)

// describeSpans renders spans compactly for comparisons: kind:text.
func describeSpans(q string, spans []span) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		var k string
		switch s.kind {
		case spanString:
			k = "str"
		case spanEscapeString:
			k = "estr"
		case spanDollarString:
			k = "dstr"
		case spanQuotedIdent:
			k = "ident"
		case spanLineComment:
			k = "line"
		case spanBlockComment:
			k = "block"
		case spanParam:
			switch s.param {
			case paramSequential:
				k = "seq"
			case paramNumbered:
				k = fmt.Sprintf("num%d", s.num)
			case paramNamed:
				k = "named(" + s.name + ")"
			}
		}
		if s.unterminated {
			k += "!"
		}
		out = append(out, k+":"+q[s.start:s.end])
	}
	return out
}

func TestScanSQL(t *testing.T) {
	cases := []struct {
		q    string
		want []string
	}{
		{"SELECT 1", nil},
		{"SELECT ?", []string{"seq:?"}},
		{"'a''b' 'c'", []string{"str:'a''b'", "str:'c'"}},
		{`E'a\'b' e'\\' E'x`, []string{`estr:E'a\'b'`, `estr:e'\\'`, `estr!:E'x`}},
		{`'a\' ?`, []string{`str:'a\'`, "seq:?"}},
		{"N'a' X'0f' B'01' n'x' x'y' b'z'", []string{"str:N'a'", "str:X'0f'", "str:B'01'", "str:n'x'", "str:x'y'", "str:b'z'"}},
		// A prefix letter in the middle of a word does not start a special string.
		{`fooE'\' ?`, []string{`str:'\'`, "seq:?"}},
		{`_E'\' ?`, []string{`str:'\'`, "seq:?"}},
		{`_E'\'' ?`, []string{`str!:'\'' ?`}},
		// After a number token the letter starts a new token.
		{`1E'\'' ?`, []string{`estr:E'\''`, "seq:?"}},
		{`1.5e'x'`, []string{"estr:e'x'"}},
		{`1e5'x'`, []string{"str:'x'"}},
		{`"a""b" "?"`, []string{`ident:"a""b"`, `ident:"?"`}},
		{"`a``b` `?`", []string{"ident:`a``b`", "ident:`?`"}},
		{"`it's` ?", []string{"ident:`it's`", "seq:?"}},
		{`"x`, []string{`ident!:"x`}},
		{"-- a ? \n?", []string{"line:-- a ? \n", "seq:?"}},
		{"-- a ? \r\n?", []string{"line:-- a ? \r", "seq:?"}},
		{"-- tail ?", []string{"line:-- tail ?"}},
		{"/* a /* b */ ? */ ?", []string{"block:/* a /* b */ ? */", "seq:?"}},
		{"/*/ ? */ ?", []string{"block:/*/ ? */", "seq:?"}},
		{"/* /* */ ?", []string{"block!:/* /* */ ?"}},
		{"1-- c\n?", []string{"line:-- c\n", "seq:?"}},
		{"a/* c */?", []string{"block:/* c */", "seq:?"}},
		{"1e--c\n?", []string{"line:--c\n", "seq:?"}},
		{"$$ ? $$ ?", []string{"dstr:$$ ? $$", "seq:?"}},
		{"$a$ $$ $b$ ? $a$ ?", []string{"dstr:$a$ $$ $b$ ? $a$", "seq:?"}},
		{"$a$x$a ?", []string{"dstr!:$a$x$a ?"}},
		{"$_é1$ ? $_é1$", []string{"dstr:$_é1$ ? $_é1$"}},
		{"$1 $23 ?4 ?01", []string{"num1:$1", "num23:$23", "num4:?4", "num1:?01"}},
		{"$name $Name_2 $é", []string{"named(name):$name", "named(Name_2):$Name_2", "named(é):$é"}},
		// A '$' inside an identifier is part of it.
		{"a$1 a$b $c$d", []string{"dstr!:$c$d"}},
		// $label stops at a non-identifier character; a following '$' would
		// have made it a dollar quote, so it only continues past one when the
		// tag scan was interrupted by a byte that is not a tag character.
		{"$ab.c", []string{"named(ab):$ab"}},
		{"$ab\x01$x ?", []string{"named(ab\x01$x):$ab\x01$x", "seq:?"}},
		{"$ E'x'", []string{"estr:E'x'"}},
		{"$E'x'", []string{"estr:E'x'"}},
		{"$Ex", []string{"named(Ex):$Ex"}},
		{"$(1)", nil},
		{"$", nil},
		{"? ?", []string{"seq:?", "seq:?"}},
		{"??", []string{"seq:?", "seq:?"}},
		{"x?y", []string{"seq:?"}},
		{"?;?", []string{"seq:?", "seq:?"}},
	}
	for _, c := range cases {
		spans, err := scanSQL(c.q)
		if err != nil {
			t.Fatalf("%q: %v", c.q, err)
		}
		got := describeSpans(c.q, spans)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Fatalf("%q:\ngot  %q\nwant %q", c.q, got, c.want)
		}
	}
}

func TestScanNumber(t *testing.T) {
	// Expected token text, following DuckDB 2.0's NUMERIC tokenizer state.
	cases := map[string]string{
		"1":       "1",
		"123abc":  "123",
		"1.5":     "1.5",
		".5":      ".5",
		"1.":      "1.",
		"1.x":     "1.",
		"1e5":     "1e5",
		"1e-5":    "1e-5",
		"1E+5x":   "1E+5",
		"1e":      "1e",
		"1ex":     "1",
		"1e+x":    "1",
		"1e--":    "1",
		"1ee5":    "1",
		".e5":     ".",
		".1e5":    ".1e5",
		"1_000":   "1_000",
		"1__0":    "1",
		"1_":      "1",
		"1._5":    "1.",
		"12-3":    "12",
		"1.2.3":   "1.2.3",
		"9e9e9":   "9e9e9", // accepted by the tokenizer, rejected later by the number parser
		"1e5-3":   "1e5",
		"1_0_0e1": "1_0_0e1",
	}
	for in, want := range cases {
		if got := in[:scanNumber(in, 0)]; got != want {
			t.Errorf("scanNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScanSQLErrors(t *testing.T) {
	for _, q := range []string{"$1.5", "?1e3", "$1_0", "$0", "?0", "$1e", "$18446744073709551616", "$2147483649"} {
		if _, err := scanSQL(q); err == nil {
			t.Errorf("%q: expected error", q)
		}
	}
	if _, err := scanSQL("$2147483648"); err != nil {
		t.Errorf("max parameter number: %v", err)
	}
}

func TestKeywordCharClasses(t *testing.T) {
	for c := range 256 {
		b := byte(c)
		want := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' ||
			b >= 0x80 || (b < 0x20 && !isSpace(b)) || b == 0x7f
		if got := isKeywordChar(b); got != want {
			t.Errorf("isKeywordChar(%#x) = %v, want %v", b, got, want)
		}
	}
}

// checkSpanInvariants verifies that spans are ordered, non-overlapping and
// within bounds, and that each span's text starts the way its kind requires.
func checkSpanInvariants(t *testing.T, q string, spans []span) {
	t.Helper()
	prev := 0
	for _, s := range spans {
		if s.start < prev || s.end <= s.start || s.end > len(q) {
			t.Fatalf("%q: bad span %+v (prev end %d)", q, s, prev)
		}
		prev = s.end
		text := q[s.start:s.end]
		var ok bool
		switch s.kind {
		case spanString:
			ok = text[0] == '\'' || (len(text) > 1 && text[1] == '\'')
		case spanEscapeString:
			ok = (text[0] == 'E' || text[0] == 'e') && text[1] == '\''
		case spanDollarString, spanParam:
			ok = text[0] == '$' || text[0] == '?'
		case spanQuotedIdent:
			ok = text[0] == '"' || text[0] == '`'
		case spanLineComment:
			ok = strings.HasPrefix(text, "--")
		case spanBlockComment:
			ok = strings.HasPrefix(text, "/*")
		}
		if !ok {
			t.Fatalf("%q: span %+v has text %q", q, s, text)
		}
	}
}

func FuzzScanSQL(f *testing.F) {
	for _, s := range []string{
		"SELECT ?", "$a$ ? $a$", "/* /* */ */ ?", `E'\'' ?`, "`a``b`", "$1 ?2 $x", "1e--x\n?", "$ab\x01$x",
		"'unterminated", "$$", "?", "$", "--", "/*", "e'", "1E'", "\xff\x00'",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		spans, err := scanSQL(q)
		if err != nil {
			if !strings.HasPrefix(err.Error(), "sqlfmt: ") {
				t.Fatalf("error without prefix: %v", err)
			}
			return
		}
		checkSpanInvariants(t, q, spans)
		_ = unicodeSpaces(q)
	})
}
