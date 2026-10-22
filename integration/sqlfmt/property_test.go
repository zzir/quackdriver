//go:build integration

package sqlfmt

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/sqlfmt"
)

// fragments are the pieces adversarial strings are built from: every quote,
// escape, comment, placeholder and dollar-quote token the scanner knows,
// whitespace and Unicode spaces DuckDB's normalizer rewrites, and SQL that
// would do damage if it ever escaped its literal.
var fragments = []string{
	"'", "''", "'''", `"`, "`", `\`, `\'`, `\\`, "$", "$$", "$a$", "$tag$", "$1", "$x", "?", "?1", "??",
	"--", "-- ", "/*", "*/", "/* x */", "\n", "\r", "\r\n", "\t", " ", ";", "E'", "e'", "N'", "X'", "B'",
	"U&'", "::", "||", "(", ")", "-", "0", "1e5", ".5",
	"'; DROP TABLE sqlfmt_prop; --", "' OR '1'='1", "'); SELECT 1; --", "$$; SELECT 2; $$",
	"\u00a0", "\u2000", "\u200b", "\u2028", "\u202f", "\u205f", "\u3000", "\ufeff",
	"é", "日本", "😀", "\u0301", "\ufffd", "\U0010ffff",
	"SELECT", "FROM", "a", "Z",
}

// randomString builds a string of up to maxParts fragments, random runes and
// random ASCII bytes. It never contains NUL or invalid UTF-8.
func randomString(rng *rand.Rand, maxParts int) string {
	var b strings.Builder
	for n := rng.Intn(maxParts + 1); n > 0; n-- {
		switch rng.Intn(4) {
		case 0, 1:
			b.WriteString(fragments[rng.Intn(len(fragments))])
		case 2:
			b.WriteByte(byte(1 + rng.Intn(127)))
		case 3:
			r := rune(rng.Intn(utf8.MaxRune + 1))
			if r == 0 || !utf8.ValidRune(r) {
				r = 'x'
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stringTemplates are queries whose column col must be exactly the string
// argument. unicodeRefusal marks templates in which a quote inside a comment
// makes DuckDB's Unicode-space normalizer believe the literal is outside a
// string, so the driver must refuse arguments containing such spaces when
// they would be rewritten.
var stringTemplates = []struct {
	query          string
	col            int // the column holding the argument
	unicodeRefusal bool
}{
	{query: "SELECT ? AS s"},
	{query: "SELECT $1 AS s, $1 = $1 AS same"},
	{query: "SELECT s FROM (VALUES (?)) t(s)"},
	{query: "SELECT ?::VARCHAR AS s -- ' trailing \" comment"},
	{query: "SELECT concat(?, '') AS s, '?' AS q, $$?$$ AS d"},
	{query: "SELECT [?][1] AS s"},
	{query: "SELECT {'k': ?}.k AS s"},
	{query: "SELECT /* ? */ ?||'' AS s"},
	{query: "SELECT E'\\'' AS e, ? AS s", col: 1, unicodeRefusal: true},
	{query: "SELECT /* ' */ ? AS s", unicodeRefusal: true},
}

// hasNormalizedSpace reports whether s contains a character
// Parser::StripUnicodeSpaces replaces.
func hasNormalizedSpace(s string) bool {
	for _, r := range s {
		switch {
		case r == 0xa0, r >= 0x2000 && r <= 0x200b, r == 0x202f, r == 0x205f, r == 0x2060, r == 0x3000, r == 0xfeff:
			return true
		}
	}
	return false
}

// TestPropertyNoInjection sends thousands of random adversarial strings
// through several query shapes: the server must return exactly the input,
// or the driver must refuse it; the query must never be altered.
func TestPropertyNoInjection(t *testing.T) {
	db := openDB(t)
	livetest.Exec(t, db, "CREATE OR REPLACE TABLE sqlfmt_prop (id INTEGER, s VARCHAR)")
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS sqlfmt_prop") })

	const n = 2000
	rng := rand.New(rand.NewSource(20261021))
	inputs := make([]string, n)
	for i := range inputs {
		inputs[i] = randomString(rng, 1+i%40)
	}

	var (
		mu       sync.Mutex
		refusals = map[string]int{}
		wg       sync.WaitGroup
		work     = make(chan int)
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				s := inputs[i]
				tpl := stringTemplates[i%len(stringTemplates)]
				row, err := queryRow(db, tpl.query, s)
				if err != nil {
					if tpl.unicodeRefusal && hasNormalizedSpace(s) && strings.Contains(err.Error(), "Unicode space") {
						// The refusal must be necessary: substituted by
						// hand, the server really receives another value.
						lit, ferr := sqlfmt.FormatValue(s)
						if ferr != nil {
							t.Errorf("FormatValue(%q): %v", s, ferr)
							continue
						}
						raw := strings.Replace(tpl.query, "? AS s", lit+" AS s", 1)
						if row, err := queryRow(db, raw); err == nil && row[tpl.col] == s {
							t.Errorf("input %d %q in %s: refused, but the server keeps it intact", i, s, tpl.query)
						}
						mu.Lock()
						refusals[tpl.query]++
						mu.Unlock()
						continue
					}
					t.Errorf("input %d %q in %s: %v", i, s, tpl.query, err)
					continue
				}
				if got, ok := row[tpl.col].(string); !ok || got != s {
					t.Errorf("input %d in %s: got %q, want %q", i, tpl.query, row[tpl.col], s)
				}
			}
		}()
	}
	for i := range inputs {
		work <- i
	}
	close(work)
	wg.Wait()
	for _, tpl := range stringTemplates {
		if tpl.unicodeRefusal && refusals[tpl.query] == 0 {
			t.Errorf("%s: no input was refused; the Unicode-space check is not exercised", tpl.query)
		}
	}
	t.Logf("%d inputs, refusals for Unicode spaces: %v", n, refusals)

	// Insert them all through the Exec path, many per statement, and read
	// them back.
	const batch = 50
	for start := 0; start < n; start += batch {
		var q strings.Builder
		q.WriteString("INSERT INTO sqlfmt_prop VALUES ")
		var args []any
		for i := start; i < start+batch && i < n; i++ {
			if i > start {
				q.WriteString(", ")
			}
			q.WriteString("(?, ?)")
			args = append(args, i, inputs[i])
		}
		livetest.Exec(t, db, q.String(), args...)
	}
	rows, err := db.Query("SELECT id, s FROM sqlfmt_prop ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id int
		var s string
		if err := rows.Scan(&id, &s); err != nil {
			t.Fatal(err)
		}
		if id != seen {
			t.Fatalf("row %d has id %d", seen, id)
		}
		if s != inputs[id] {
			t.Errorf("stored input %d = %q, want %q", id, s, inputs[id])
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != n {
		t.Fatalf("read back %d rows, want %d", seen, n)
	}

	// The table must still be intact and alone: no injected statement ran.
	if row := mustRow(t, db, "SELECT count(*) FROM duckdb_tables() WHERE table_name = 'sqlfmt_prop'"); row[0] != int64(1) {
		t.Fatalf("sqlfmt_prop tables: %v", row)
	}
}

// TestPropertyNamedAndNumbered repeats the round trip with every value
// referenced through numbered and named placeholders, several per query.
func TestPropertyNamedAndNumbered(t *testing.T) {
	db := openDB(t)
	rng := rand.New(rand.NewSource(7))
	for i := range 100 {
		a, b := randomString(rng, 20), randomString(rng, 20)
		var row []any
		var err error
		if i%2 == 0 {
			row, err = queryRow(db, "SELECT $2 AS b, ?1 AS a, $2 || $1 AS ba", a, b)
		} else {
			row, err = queryRow(db, "SELECT $b AS b, $a AS a, $B || $A AS ba", sql.Named("a", a), sql.Named("b", b))
		}
		if err != nil {
			t.Fatalf("%q, %q: %v", a, b, err)
		}
		if row[0] != b || row[1] != a || row[2] != b+a {
			t.Fatalf("%q, %q: got %q", a, b, row)
		}
	}
}

// TestPropertyBlobs round-trips random byte strings, which may hold any
// byte including NUL, quotes and invalid UTF-8.
func TestPropertyBlobs(t *testing.T) {
	db := openDB(t)
	rng := rand.New(rand.NewSource(99))
	const special = "'\\\x00\xff\"$?"
	for i := range 200 {
		b := make([]byte, rng.Intn(200))
		for j := range b {
			if rng.Intn(4) == 0 {
				b[j] = special[rng.Intn(len(special))]
			} else {
				b[j] = byte(rng.Intn(256))
			}
		}
		row := mustRow(t, db, "SELECT $1 AS b, octet_length($1) AS n, hex($1) AS h", b)
		if !same(row[0], b) {
			t.Fatalf("blob %d: got %x, want %x", i, row[0], b)
		}
		if row[1] != int64(len(b)) || row[2] != strings.ToUpper(fmt.Sprintf("%x", b)) {
			t.Fatalf("blob %d: server sees length %v hex %v, want %d %X", i, row[1], row[2], len(b), b)
		}
	}
}

// TestStringsNULAndInvalidUTF8 covers the bytes a string literal might not
// carry: NUL bytes arrive intact, and invalid UTF-8 is rejected by the
// server (DuckDB VARCHARs must be valid UTF-8). Neither is ever altered.
func TestStringsNULAndInvalidUTF8(t *testing.T) {
	db := openDB(t)
	for _, s := range []string{"a\x00b", "\x00", "'\x00'", "\x00--\n?"} {
		row, err := queryRow(db, "SELECT ? AS s, ? AS t", s, "after")
		if err != nil {
			t.Errorf("%q: %v", s, err)
			continue
		}
		if row[0] != s || row[1] != "after" {
			t.Errorf("%q arrived as %q", s, row)
		}
	}
	for _, s := range []string{"\xff", "a\xc0\x80b", "\xed\xa0\x80", "ok'\xfe"} {
		row, err := queryRow(db, "SELECT ? AS s", s)
		if err == nil {
			t.Errorf("%q: got %q, want an invalid-UTF-8 error", s, row)
		} else if !strings.Contains(err.Error(), "Invalid unicode") {
			t.Errorf("%q: %v, want an invalid-UTF-8 error", s, err)
		}
	}
}
