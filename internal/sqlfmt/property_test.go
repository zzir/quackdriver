package sqlfmt

import (
	"database/sql/driver"
	"math/rand"
	"strings"
	"testing"
)

// Injection-safety properties: whatever bytes a string or []byte argument
// contains, the formatted query must tokenize (with the port of DuckDB's
// tokenizer) as the template's own tokens plus exactly one string literal per
// placeholder, and each literal must decode to the original value.

// hostileAlphabet is weighted towards bytes that matter to the tokenizer.
var hostileAlphabet = []string{
	"'", "''", `"`, "`", `\`, `\'`, "$", "$$", "$a$", "$1", "-", "--", "/", "/*", "*/", "*", "?", "?1", ";",
	"\n", "\r", "\x00", "E", "e", "E'", "N", "X", "B", ":", "::", "#", "(", ")", "@", "%", " ", "\t",
	"\xff", "\xc0", "\xc2", "\xa0", "\xc2\xa0", "\xe2\x80\x8b", "\xef\xbb\xbf", "a", "Z", "0", "9", ".",
	"é", "日本", "\U0001F600",
}

func randomHostileString(r *rand.Rand) string {
	n := r.Intn(24)
	var b strings.Builder
	for range n {
		if r.Intn(5) == 0 {
			b.WriteByte(byte(r.Intn(256)))
			continue
		}
		b.WriteString(hostileAlphabet[r.Intn(len(hostileAlphabet))])
	}
	return b.String()
}

// literalStringPart returns the quoted string part of a literal produced by
// FormatValue for a string ('...') or a blob ('...'::BLOB).
func literalStringPart(lit string) string {
	return strings.TrimSuffix(lit, "::BLOB")
}

// checkSubstitution verifies that query out is the template tmpl with the
// i-th placeholder replaced by lits[i] (optionally padded by one space on
// either side), that the tokenizer sees each substituted string literal as
// exactly one terminated string token decoding to want[i], and that every
// other token of out corresponds one-to-one to a non-placeholder token of
// tmpl.
func checkSubstitution(t *testing.T, tmpl, out string, named []string, lits []string, want []string) {
	t.Helper()
	tspans, err := scanSQL(tmpl)
	if err != nil {
		t.Fatalf("template %q: %v", tmpl, err)
	}
	ospans, err := scanSQL(out)
	if err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	checkSpanInvariants(t, out, ospans)

	type litPos struct{ start, end int }
	var litSpans []litPos
	var params []span
	for _, s := range tspans {
		if isSubstituted(s, named) {
			params = append(params, s)
		}
	}
	if len(params) != len(lits) {
		t.Fatalf("template %q has %d placeholders, test supplied %d literals", tmpl, len(params), len(lits))
	}
	// Walk template and output in parallel.
	ti, oi := 0, 0
	for k, p := range params {
		chunk := tmpl[ti:p.start]
		if !strings.HasPrefix(out[oi:], chunk) {
			t.Fatalf("%q -> %q: template text %q not found at output offset %d", tmpl, out, chunk, oi)
		}
		oi += len(chunk)
		if strings.HasPrefix(out[oi:], " ") && !strings.HasPrefix(lits[k], " ") {
			oi++
		}
		// A literal that DuckDB would join onto a neighbouring string
		// literal is parenthesized (see continuesString).
		paren := strings.HasPrefix(out[oi:], "(") && !strings.HasPrefix(lits[k], "(")
		if paren {
			oi++
		}
		if !strings.HasPrefix(out[oi:], lits[k]) {
			t.Fatalf("%q -> %q: literal %q not found at output offset %d", tmpl, out, lits[k], oi)
		}
		strPart := literalStringPart(lits[k])
		litSpans = append(litSpans, litPos{oi, oi + len(strPart)})
		oi += len(lits[k])
		if paren {
			if !strings.HasPrefix(out[oi:], ")") {
				t.Fatalf("%q -> %q: unbalanced parenthesis after literal %d", tmpl, out, k)
			}
			oi++
		}
		ti = p.end
		if ti < len(tmpl) && !strings.HasPrefix(tmpl[ti:], " ") && strings.HasPrefix(out[oi:], " ") {
			oi++
		}
	}
	if out[oi:] != tmpl[ti:] {
		t.Fatalf("%q -> %q: tail %q != %q", tmpl, out, out[oi:], tmpl[ti:])
	}

	// Each literal must be exactly one terminated standard string token.
	found := make([]bool, len(litSpans))
	var other []span
	for _, s := range ospans {
		matched := false
		for k, l := range litSpans {
			if s.start == l.start {
				if s.kind != spanString || s.end != l.end || s.unterminated {
					t.Fatalf("%q -> %q: literal %d tokenizes as %v %q", tmpl, out, k,
						describeSpans(out, []span{s}), out[s.start:s.end])
				}
				dec, ok := decodeStandardString(out[s.start:s.end])
				if !ok || dec != want[k] {
					t.Fatalf("%q -> %q: literal %d decodes to %q (ok=%v), want %q", tmpl, out, k, dec, ok, want[k])
				}
				found[k] = true
				matched = true
			} else if s.start < l.end && l.start < s.end {
				t.Fatalf("%q -> %q: token %v overlaps literal %d", tmpl, out, describeSpans(out, []span{s}), k)
			}
		}
		if !matched {
			other = append(other, s)
		}
	}
	for k, ok := range found {
		if !ok {
			t.Fatalf("%q -> %q: literal %d not found as a token", tmpl, out, k)
		}
	}
	// All remaining tokens must be the template's own, unchanged.
	var tother []span
	for _, s := range tspans {
		if !isSubstituted(s, named) {
			tother = append(tother, s)
		}
	}
	if len(other) != len(tother) {
		t.Fatalf("%q -> %q: %d other tokens %v, template has %d %v", tmpl, out, len(other),
			describeSpans(out, other), len(tother), describeSpans(tmpl, tother))
	}
	for i := range other {
		if out[other[i].start:other[i].end] != tmpl[tother[i].start:tother[i].end] || other[i].kind != tother[i].kind {
			t.Fatalf("%q -> %q: token %q differs from template token %q", tmpl, out,
				out[other[i].start:other[i].end], tmpl[tother[i].start:tother[i].end])
		}
	}
}

// isSubstituted reports whether FormatQuery replaces placeholder span s when
// the named arguments are the given names.
func isSubstituted(s span, named []string) bool {
	if s.kind != spanParam {
		return false
	}
	if s.param != paramNamed {
		return true
	}
	for _, n := range named {
		if ciEquals(n, s.name) {
			return true
		}
	}
	return false
}

func templateNames(tmpl string) []string {
	if strings.Contains(tmpl, "$v") {
		return []string{"v"}
	}
	return nil
}

// propertyTemplates each contain exactly one placeholder, placed next to text
// that would fuse with a careless substitution.
var propertyTemplates = []string{
	"?",
	"SELECT ?",
	"SELECT E?",
	"SELECT e?",
	"SELECT N?",
	"SELECT X?",
	"SELECT 1-?",
	"SELECT ?-- trailing comment ?",
	"SELECT x||?||'tail'",
	"/* c ' */ SELECT ? /* d */",
	"SELECT $$a ' $$, ?, $t$b$t$",
	"SELECT `i'd`, \"i'd\", ?",
	`SELECT E'\'', ?`,
	"SELECT ?;SELECT 2",
	"SELECT $1, $1",
	"SELECT ?1",
	"SELECT $v",
	"SELECT a$?",
	"SELECT ??",
	"SELECT '?'?'?'",
	"SELECT 1e?",
	"SELECT ?::BLOB",
	"SELECT\n?\n'continuation'",
}

func templateArgs(tmpl string, vals ...driver.Value) []driver.NamedValue {
	if strings.Contains(tmpl, "$v") {
		return []driver.NamedValue{{Name: "v", Value: vals[0]}}
	}
	return nv(vals...)
}

func TestPropertyStringLiteralsScanBack(t *testing.T) {
	r := newRand(t)
	checked := 0
	defer func() {
		if checked < 19000 {
			t.Fatalf("only %d substitutions checked", checked)
		}
	}()
	for iter := range 20000 {
		s := randomHostileString(r)
		lit, err := FormatValue(s)
		if err != nil {
			t.Fatal(err)
		}
		if dec, ok := decodeStandardString(lit); !ok || dec != s {
			t.Fatalf("%q -> %q decodes to %q (ok=%v)", s, lit, dec, ok)
		}
		spans, err := scanSQL(lit)
		if err != nil || len(spans) != 1 || spans[0].kind != spanString || spans[0].start != 0 ||
			spans[0].end != len(lit) || spans[0].unterminated {
			t.Fatalf("%q -> %q scans as %v (err %v)", s, lit, describeSpans(lit, spans), err)
		}

		tmpl := propertyTemplates[iter%len(propertyTemplates)]
		nParams := countParams(t, tmpl)
		vals := make([]driver.Value, 0, 2)
		wants := make([]string, 0, 2)
		lits := make([]string, 0, 2)
		if strings.Contains(tmpl, "??") {
			s2 := randomHostileString(r)
			vals = append(vals, s, s2)
		} else {
			vals = append(vals, s)
		}
		for _, v := range vals {
			l, _ := FormatValue(v)
			lits = append(lits, l)
			wants = append(wants, v.(string))
		}
		for len(lits) < nParams { // numbered placeholders repeating one argument
			lits = append(lits, lits[0])
			wants = append(wants, wants[0])
		}
		out, err := FormatQuery(tmpl, templateArgs(tmpl, vals...))
		if err != nil {
			if strings.Contains(err.Error(), "Unicode space") {
				continue // refused rather than corrupted; checked separately
			}
			t.Fatalf("%q with %q: %v", tmpl, vals, err)
		}
		checkSubstitution(t, tmpl, out, templateNames(tmpl), lits, wants)
		checked++
	}
}

func TestPropertyBlobLiteralsScanBack(t *testing.T) {
	r := newRand(t)
	for iter := range 10000 {
		b := []byte(randomHostileString(r))
		lit, err := FormatValue(b)
		if err != nil {
			t.Fatal(err)
		}
		str, ok := decodeStandardString(literalStringPart(lit))
		if !ok || !strings.HasSuffix(lit, "::BLOB") {
			t.Fatalf("%q -> %q: not a string cast to BLOB", b, lit)
		}
		got, ok := duckBlobFromString(str)
		if !ok || string(got) != string(b) {
			t.Fatalf("%q -> %q -> %q (ok=%v)", b, lit, got, ok)
		}
		tmpl := propertyTemplates[iter%len(propertyTemplates)]
		if strings.Contains(tmpl, "??") || countParams(t, tmpl) != 1 {
			continue
		}
		out, err := FormatQuery(tmpl, templateArgs(tmpl, b))
		if err != nil {
			t.Fatalf("%q: %v", tmpl, err)
		}
		checkSubstitution(t, tmpl, out, templateNames(tmpl), []string{lit}, []string{str})
	}
}

// TestPropertyManyArguments substitutes several hostile strings separated by
// hostile glue text in one query.
func TestPropertyManyArguments(t *testing.T) {
	r := newRand(t)
	glue := []string{
		", ", "", " ", "-", "E", "e", "||", "/* ' */", "--\n", "$", "1", ".", "'x'", `E'\''`, "`'`", "\"'\"",
		"$q$ ' $q$", ";", "(", ")", "::", "\n",
	}
	checked := 0
	defer func() {
		if checked < 1000 {
			t.Fatalf("only %d queries checked", checked)
		}
	}()
	for range 5000 {
		n := 1 + r.Intn(5)
		var tmpl strings.Builder
		tmpl.WriteString("SELECT ")
		vals := make([]driver.Value, n)
		lits := make([]string, n)
		wants := make([]string, n)
		for i := range n {
			tmpl.WriteString(glue[r.Intn(len(glue))])
			tmpl.WriteByte('?')
			s := randomHostileString(r)
			vals[i] = s
			wants[i] = s
			lits[i], _ = FormatValue(s)
		}
		tmpl.WriteString(glue[r.Intn(len(glue))])
		q := tmpl.String()
		spans, err := scanSQL(q)
		if err != nil {
			t.Fatal(err)
		}
		// Glue can swallow placeholders (inside comments or quotes); only
		// check templates whose placeholders all survived.
		if countKind(spans, paramSequential) != n || countKind(spans, paramNamed)+countKind(spans, paramNumbered) != 0 {
			continue
		}
		out, err := FormatQuery(q, nv(vals...))
		if err != nil {
			if strings.Contains(err.Error(), "Unicode space") {
				continue
			}
			t.Fatalf("%q: %v", q, err)
		}
		checkSubstitution(t, q, out, nil, lits, wants)
		checked++
	}
}

// TestPropertyUnicodeSpaceRefusal checks that whenever FormatQuery accepts a
// query, DuckDB's Unicode-space normalizer leaves every substituted literal
// intact.
func TestPropertyUnicodeSpaceRefusal(t *testing.T) {
	r := newRand(t)
	prefixes := []string{"", "/* ' */ ", `E'\'' `, "`'` ", "$a$b$a$ ", "-- '\n", "'''' ", "$$'$$ "}
	accepted, refused := 0, 0
	defer func() {
		if accepted < 1000 || refused < 1000 {
			t.Fatalf("accepted %d, refused %d", accepted, refused)
		}
	}()
	for range 5000 {
		s := randomHostileString(r) + " " + randomHostileString(r)
		q := "SELECT " + prefixes[r.Intn(len(prefixes))] + "?"
		out, err := FormatQuery(q, nv(s))
		if err != nil {
			if !strings.Contains(err.Error(), "Unicode space") {
				t.Fatalf("%q: %v", q, err)
			}
			refused++
			continue
		}
		accepted++
		lit, _ := FormatValue(s)
		start := strings.LastIndex(out, lit)
		for _, us := range unicodeSpaces(out) {
			if us.pos >= start && us.pos < start+len(lit) {
				t.Fatalf("%q -> %q: normalizer would rewrite offset %d inside the literal", q, out, us.pos)
			}
		}
	}
}

func countParams(t *testing.T, q string) int {
	t.Helper()
	spans, err := scanSQL(q)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range spans {
		if s.kind == spanParam {
			n++
		}
	}
	return n
}

func countKind(spans []span, k paramKind) int {
	n := 0
	for _, s := range spans {
		if s.kind == spanParam && s.param == k {
			n++
		}
	}
	return n
}

func FuzzFormatQuery(f *testing.F) {
	f.Add("SELECT ?, ?", "a'b", "c")
	f.Add("SELECT E?--?\n?", `\'`, "--")
	f.Add("/* ' */ SELECT ? $v ?", "\u00a0", "")
	f.Add("SELECT $$?$$ ?1 ?2 $2", "$$", "'")
	f.Add("SELECT $x, $1, $2", "x", "y")
	f.Fuzz(func(t *testing.T, q, s1, s2 string) {
		vals := []string{s1, s2}
		out, err := FormatQuery(q, nv(s1, s2))
		if err != nil {
			if !strings.HasPrefix(err.Error(), "sqlfmt: ") {
				t.Fatalf("error without prefix: %v", err)
			}
			return
		}
		// FormatQuery succeeded, so the template scans and every placeholder
		// it substituted maps to one of the two string arguments.
		spans, err := scanSQL(q)
		if err != nil {
			t.Fatal(err)
		}
		var lits, wants []string
		seq := 0
		for _, sp := range spans {
			if !isSubstituted(sp, nil) {
				continue
			}
			var v string
			if sp.param == paramSequential {
				v = vals[seq]
				seq++
			} else {
				v = vals[sp.num-1]
			}
			lit, _ := FormatValue(v)
			lits = append(lits, lit)
			wants = append(wants, v)
		}
		checkSubstitution(t, q, out, nil, lits, wants)
	})
}

func FuzzFormatValueString(f *testing.F) {
	for _, s := range []string{"", "'", "''", `\'`, "$$", "--", "/*", "\x00", "\xff", " "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		lit, err := FormatValue(s)
		if err != nil {
			t.Fatal(err)
		}
		if dec, ok := decodeStandardString(lit); !ok || dec != s {
			t.Fatalf("%q -> %q", s, lit)
		}
		for _, tmpl := range propertyTemplates {
			if countParams(t, tmpl) != 1 {
				continue
			}
			out, err := FormatQuery(tmpl, templateArgs(tmpl, s))
			if err != nil {
				if strings.Contains(err.Error(), "Unicode space") {
					continue
				}
				t.Fatalf("%q: %v", tmpl, err)
			}
			checkSubstitution(t, tmpl, out, templateNames(tmpl), []string{lit}, []string{s})
		}
	})
}
