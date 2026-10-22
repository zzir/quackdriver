//go:build integration

package sqlfmt

import (
	"database/sql"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/value"
)

func TestPlaceholderForms(t *testing.T) {
	db := openDB(t)
	runPlaceholderCases(t, db, []placeholderCase{
		{"sequential", "SELECT ?, ?, ?", []any{"a", int64(2), true}, []any{"a", int32(2), true}},
		{"question numbered", "SELECT ?2, ?1, ?2", []any{"a", "b"}, []any{"b", "a", "b"}},
		{"dollar numbered", "SELECT $2, $1, $1", []any{"a", "b"}, []any{"b", "a", "a"}},
		{"numbered beyond 9", "SELECT ?10, ?1, ?2+?3+?4+?5+?6+?7+?8+?9", []any{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, []any{int32(10), int32(1), int32(44)}},
		{"mixed numbered forms", "SELECT ?1, $1, ?2", []any{"x", "y"}, []any{"x", "x", "y"}},
		{"named", "SELECT $name, $other, $name",
			[]any{sql.Named("name", "n"), sql.Named("other", -1)}, []any{"n", int32(-1), "n"}},
		{"named case-insensitive", "SELECT $Name, $NAME",
			[]any{sql.Named("name", "n")}, []any{"n", "n"}},
		{"named with digits and underscore", "SELECT $a_1, $b_2x9",
			[]any{sql.Named("a_1", 1), sql.Named("b_2X9", 2)}, []any{int32(1), int32(2)}},
		{"named and positional", "SELECT ?, $x, ?",
			[]any{"p1", sql.Named("x", "nx"), "p2"}, []any{"p1", "nx", "p2"}},
		{"named and numbered", "SELECT $2, $x, $1",
			[]any{"p1", sql.Named("x", "nx"), "p2"}, []any{"p2", "nx", "p1"}},
		{"named followed by cast", "SELECT $v::VARCHAR", []any{sql.Named("v", 12)}, []any{"12"}},
		{"sequential then cast and index", "SELECT ?::INTEGER + 1, [?, ?][2], {'k': ?}.k",
			[]any{"41", 1, 2, "s"}, []any{int32(42), int32(2), "s"}},
		{"adjacent operators", "SELECT 1-?, 2*?, ?||?", []any{-5, -3, "a", "b"}, []any{int32(6), int32(-6), "ab"}},
		{"no spaces", "SELECT(?)AS a,?AS b", []any{1, "x"}, []any{int32(1), "x"}},
	})
}

// TestPlaceholderUnmatchedNamed checks that $name without a matching
// sql.Named argument is left for DuckDB, which resolves nothing for it in a
// plain query: the server, not the driver, reports it.
func TestPlaceholderUnmatchedNamed(t *testing.T) {
	db := openDB(t)
	_, err := queryRow(db, "SELECT $unbound_name")
	if err == nil {
		t.Fatal("SELECT $unbound_name succeeded")
	}
	if strings.Contains(err.Error(), "sqlfmt") {
		t.Fatalf("driver rejected $unbound_name itself: %v", err)
	}
	t.Logf("server error: %v", err)

	// With a named argument for a different name the unmatched one is
	// still left untouched while the matched one is substituted.
	_, err = queryRow(db, "SELECT $a, $unbound_name", sql.Named("a", 1))
	if err == nil || strings.Contains(err.Error(), "sqlfmt") {
		t.Fatalf("SELECT $a, $unbound_name: %v", err)
	}
}

func TestPlaceholdersNotSubstituted(t *testing.T) {
	db := openDB(t)
	runPlaceholderCases(t, db, []placeholderCase{
		{"string", "SELECT '?', ?, '$1 ?1 $x'", []any{"v"}, []any{"?", "v", "$1 ?1 $x"}},
		{"string with doubled quote", "SELECT 'it''s ?', ?", []any{"v"}, []any{"it's ?", "v"}},
		{"string with backslash", `SELECT 'a\', ?, '\?'`, []any{"v"}, []any{`a\`, "v", `\?`}},
		{"E-string escapes", `SELECT E'\'?', ?, e'\\', ?`, []any{"v", "w"}, []any{"'?", "v", `\`, "w"}},
		{"E-string backslash before quote pair", `SELECT E'a\\''?', ?`, []any{"v"}, []any{`a\'?`, "v"}},
		{"identifier", `SELECT 1 AS "?", ? AS "$1 ""?"""`, []any{"v"}, []any{int32(1), "v"}},
		{"backtick identifier", "SELECT 1 AS `?`, ? AS `a``?`", []any{"v"}, []any{int32(1), "v"}},
		{"dollar quoted", "SELECT $$?$1$$, ?, $tag$ $$ ? $tag$", []any{"v"}, []any{"?$1", "v", " $$ ? "}},
		{"dollar quoted with digits and unicode tag", "SELECT $t1$?$t1$, $é$?$é$, ?", []any{"v"}, []any{"?", "?", "v"}},
		{"line comment", "SELECT ? -- ? $1 '\n, ?", []any{"v", "w"}, []any{"v", "w"}},
		{"line comment CR", "SELECT ? -- ? '\r, ?", []any{"v", "w"}, []any{"v", "w"}},
		{"block comment", "SELECT /* ? ' */ ?, /* $x */ ?", []any{"v", "w"}, []any{"v", "w"}},
		{"nested block comment", "SELECT /* a /* ? */ ? */ ?", []any{"v"}, []any{"v"}},
		{"identifier with dollar", "SELECT ? AS a$1, ? AS b$x", []any{"v", "w"}, []any{"v", "w"}},
		{"special string prefixes", `SELECT X'3F', N'?', ?`, []any{"v"}, []any{[]byte("?"), "?", "v"}},
	})
}

// TestPlaceholderPreparedStatement reuses one prepared statement with
// different arguments, including the same numbered argument twice.
func TestPlaceholderPreparedStatement(t *testing.T) {
	db := openDB(t)
	stmt, err := db.Prepare("SELECT $1 || '-' || $1, ?2")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for _, s := range []string{"a", "it's", "", "?", "$1"} {
		var a, b string
		if err := stmt.QueryRow(s, "x"+s).Scan(&a, &b); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if a != s+"-"+s || b != "x"+s {
			t.Errorf("%q: got %q, %q", s, a, b)
		}
	}
}

// TestScannerAgreesWithDuckDB runs queries whose placeholder positions
// depend on DuckDB's exact tokenization; a disagreement shows up as a
// wrong value, an argument-count error or a syntax error.
func TestScannerAgreesWithDuckDB(t *testing.T) {
	db := openDB(t)
	runPlaceholderCases(t, db, []placeholderCase{
		{"identifier ending in dollar digit dollar", "SELECT 1 AS a$1$, ?", []any{"v"}, []any{int32(1), "v"}},
		{"dollar quote right after keyword", "SELECT 1 AS x, $q$?$q$, ?", []any{"v"}, []any{int32(1), "?", "v"}},
		{"keyword before string prefix letter", "SELECT 'a' AS e, ?", []any{"v"}, []any{"a", "v"}},
		{"bitwise and", "SELECT 6&?, 6 & ?", []any{3, -1}, []any{int32(2), int32(6)}},
		{"comment between minus signs", "SELECT 1 -/* ? */- ?", []any{-2}, []any{int32(-1)}},
		{"operator chars around placeholder", "SELECT ?||'?'||?", []any{"a", "b"}, []any{"a?b"}},
		{"json arrow", `SELECT '{"a": 1}'::JSON->>?`, []any{"$.a"}, []any{"1"}},
		{"lambda", "SELECT list_transform([1, 2], lambda x: x * ?)", []any{-1}, []any{[]any{int32(-1), int32(-2)}}},
		{"number then placeholder", "SELECT 1.5e3+?", []any{1}, []any{float64(1501)}},
		{"string then placeholder", "SELECT 'x'||?", []any{"y"}, []any{"xy"}},
		{"empty string and empty identifier-like quotes", "SELECT '', ?, ''''", []any{"v"}, []any{"", "v", "'"}},
		{"tab and form feed whitespace", "SELECT\t?\f,\v?", []any{1, 2}, []any{int32(1), int32(2)}},
		{"placeholder at end", "SELECT ?", []any{"end"}, []any{"end"}},
		{"named at end", "SELECT $last", []any{sql.Named("last", "end")}, []any{"end"}},
	})
}

// TestNegativeNumbers puts negative arguments where a bare "-5" could fuse
// with the surrounding text: after a minus sign ("--" comment), before a
// cast or an operator that binds tighter than unary minus, and in the usual
// WHERE / LIMIT / VALUES / function-argument positions.
func TestNegativeNumbers(t *testing.T) {
	db := openDB(t)
	runPlaceholderCases(t, db, []placeholderCase{
		{"after minus", "SELECT 10-?, 10 - ?, -?", []any{-3, -3, -3}, []any{int32(13), int32(13), int32(3)}},
		{"double minus", "SELECT 1--?\n", []any{}, []any{int32(1)}},
		{"cast", "SELECT ?::VARCHAR, ?::DOUBLE, ?::INTEGER", []any{-5, -2.5, -7}, []any{"-5", -2.5, int32(-7)}},
		{"power binds tighter than unary minus", "SELECT ?^2, ?**2", []any{-2, -3}, []any{float64(4), float64(9)}},
		{"float power", "SELECT ?^2", []any{-1.5}, []any{2.25}},
		{"function args", "SELECT abs(?), greatest(?, ?), round(?, 2), round(?, ?)",
			[]any{-4, -1, -2, -1.256, -1.256, 1}, []any{int32(4), int32(-1), -1.26, -1.3}},
		{"substring and index", "SELECT substr('hello', ?, ?), 'abc'[?], [1, 2, 3][?]",
			[]any{-3, 2, -1, -2}, []any{"ll", "c", int32(2)}},
		{"range", "SELECT list(range) FROM range(?, ?, ?)", []any{-1, -7, -3}, []any{[]any{int64(-1), int64(-4)}}},
		{"between", "SELECT -5 BETWEEN ? AND ?", []any{-6, -4}, []any{true}},
		{"in list", "SELECT -5 IN (?, ?)", []any{-4, -5}, []any{true}},
		{"min int64 negated", "SELECT -(?::HUGEINT)", []any{int64(math.MinInt64)}, []any{bigInt("9223372036854775808")}},
		{"negative zero double", "SELECT 1 / ?", []any{math.Copysign(0, -1)}, []any{math.Inf(-1)}},
		{"negative big.Int", "SELECT ? + 1", []any{bigInt("-170141183460469231731687303715884105727")},
			[]any{bigInt("-170141183460469231731687303715884105726")}},
		{"negative rational", "SELECT ? * 2", []any{bigRat("-1/4")}, []any{"-0.50"}},
		{"negative interval", "SELECT TIMESTAMP '2024-01-01' + ?", []any{-36 * time.Hour},
			[]any{time.Date(2023, 12, 30, 12, 0, 0, 0, time.UTC)}},
		{"negative interval value", "SELECT DATE '2024-03-31' + ?",
			[]any{value.IntervalValue{Months: -1, Days: -1}}, []any{time.Date(2024, 2, 28, 0, 0, 0, 0, time.UTC)}},
		{"BC timestamp in arithmetic", "SELECT ? + INTERVAL 1 DAY", []any{time.Date(-43, 3, 15, 0, 0, 0, 0, time.UTC)},
			[]any{time.Date(-43, 3, 16, 0, 0, 0, 0, time.UTC)}},
	})
}

// TestNegativeNumbersInTable compares stored values with negative and
// extreme arguments of every numeric kind.
func TestNegativeNumbersInTable(t *testing.T) {
	db := openDB(t)
	livetest.Exec(t, db, "CREATE OR REPLACE TABLE sqlfmt_neg (x INTEGER, d DOUBLE, h HUGEINT, m DECIMAL(10,2))")
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS sqlfmt_neg") })
	livetest.Exec(t, db, "INSERT INTO sqlfmt_neg VALUES (?, ?, ?, ?), (?, ?, ?, ?), (?,?,?,?)",
		-1, -1.5, bigInt("-170141183460469231731687303715884105728"), bigRat("-12345678.91"),
		-2, math.Inf(-1), int64(math.MinInt64), -0.125,
		3, math.NaN(), uint64(math.MaxUint64), bigRat("-1/3"))
	t.Run("reads", func(t *testing.T) { // finishes before the table changes below
		runPlaceholderCases(t, db, []placeholderCase{
			{"where equal double", "SELECT x FROM sqlfmt_neg WHERE d = ?", []any{-1.5}, []any{int32(-1)}},
			{"where equal infinity", "SELECT x FROM sqlfmt_neg WHERE d = ?", []any{math.Inf(-1)}, []any{int32(-2)}},
			{"where nan", "SELECT x FROM sqlfmt_neg WHERE d = ?", []any{math.NaN()}, []any{int32(3)}},
			{"where minus", "SELECT count(*) FROM sqlfmt_neg WHERE x-? = 0", []any{-1}, []any{int64(1)}},
			{"hugeint min", "SELECT x FROM sqlfmt_neg WHERE h = ?", []any{bigInt("-170141183460469231731687303715884105728")}, []any{int32(-1)}},
			{"int64 min", "SELECT x FROM sqlfmt_neg WHERE h = ?", []any{int64(math.MinInt64)}, []any{int32(-2)}},
			{"uint64 max", "SELECT x FROM sqlfmt_neg WHERE h = ?", []any{uint64(math.MaxUint64)}, []any{int32(3)}},
			{"decimals", "SELECT string_agg(m::VARCHAR, ' ' ORDER BY x) FROM sqlfmt_neg", nil, []any{"-0.13 -12345678.91 -0.33"}},
			{"compare decimal with rational", "SELECT x FROM sqlfmt_neg WHERE m = ?", []any{bigRat("-1234567891/100")}, []any{int32(-1)}},
			{"compare decimal with float", "SELECT x FROM sqlfmt_neg WHERE m = ?", []any{-0.13}, []any{int32(-2)}},
			{"limit and offset", "SELECT x FROM sqlfmt_neg ORDER BY x LIMIT ? OFFSET ?", []any{1, 1}, []any{int32(-1)}},
		})
	})
	if row := mustRow(t, db, "UPDATE sqlfmt_neg SET x = ? WHERE x = ? RETURNING x", -30, 3); row[0] != int32(-30) {
		t.Errorf("UPDATE ... RETURNING = %v", row)
	}
	if _, err := queryRow(db, "SELECT x FROM sqlfmt_neg LIMIT ?", -1); err == nil || !strings.Contains(err.Error(), "cannot be negative") {
		t.Errorf("LIMIT -1: want the server's negative LIMIT error, got %v", err)
	}
}

// TestUnicodeSpaceInjectionRefused: a Unicode space in the query text that
// DuckDB turns into an ASCII space before tokenizing must not let a string
// argument run a stacked statement.
func TestUnicodeSpaceInjectionRefused(t *testing.T) {
	db := openDB(t)
	for i, c := range []struct{ q, arg string }{
		{"SELECT\u200bE'\\\\', ? AS v", "; CREATE TABLE sqlfmt_pwned1(x INT); --"},
		{"SELECT\u00a0$x$, ? AS v", "$x$ AS a; CREATE TABLE sqlfmt_pwned2(x INT); --"},
	} {
		if _, err := queryRow(db, c.q, c.arg); err == nil || !strings.Contains(err.Error(), "Unicode space") {
			t.Errorf("case %d: %v, want a Unicode space refusal", i, err)
		}
	}
	if row := mustRow(t, db, "SELECT count(*) FROM duckdb_tables() WHERE table_name LIKE 'sqlfmt_pwned%'"); row[0] != int64(0) {
		t.Fatalf("%v injected tables", row[0])
	}
}

// TestStringContinuationKeepsArgument: an argument after an E'...' literal
// and a newline is not joined into it (with its escapes applied).
func TestStringContinuationKeepsArgument(t *testing.T) {
	db := openDB(t)
	_, err := queryRow(db, "SELECT E'x'\n? AS v", `\x41`)
	if qe := (*quackdriver.Error)(nil); !errors.As(err, &qe) || qe.Type != "Parser" {
		t.Fatalf("got %v, want a syntax error", err)
	}
	if row := mustRow(t, db, "SELECT E'x' ||\n? AS v", `\x41`); row[0] != `x\x41` {
		t.Fatalf("got %q", row[0])
	}
}
