package quackdriver

import "testing"

func TestClassifyStatement(t *testing.T) {
	cases := []struct {
		sql      string
		readOnly bool
		dml      bool
	}{
		{"SELECT 1", true, false},
		{"  select 1", true, false},
		{"\n\t-- comment\nSELECT 1", true, false},
		{"/* a /* nested */ comment */ FROM t", true, false},
		{"((SELECT 1))", true, false},
		{"VALUES (1)", true, false},
		{"TABLE t", true, false},
		{"SHOW TABLES", true, false},
		{"DESCRIBE t", true, false},
		{"SUMMARIZE t", true, false},
		{"EXPLAIN SELECT 1", true, false},
		{"EXPLAIN ANALYZE SELECT 1", true, false},
		{"EXPLAIN ANALYZE INSERT INTO t VALUES (1)", false, false},
		{"PRAGMA table_info('t')", true, false},
		{"INSERT INTO t VALUES (1)", false, true},
		{"insert into t values (1) returning *", false, true},
		{"UPDATE t SET a = 1", false, true},
		{"DELETE FROM t", false, true},
		{"MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN DELETE", false, true},
		{"COPY t FROM 'x.csv'", false, true},
		{"CREATE TABLE t (a INT)", false, false},
		{"BEGIN", false, false},
		{"", false, false},
		{"-- only a comment", false, false},
		{"'string'", false, false},
		{"WITH x AS (SELECT 1) SELECT * FROM x", true, false},
		{"WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x", false, true},
		{"WITH RECURSIVE x(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM x WHERE n < 3) DELETE FROM t", false, true},
		{"WITH a AS (SELECT ')'), b AS MATERIALIZED (SELECT \"(\" FROM a) UPDATE t SET x = 1", false, true},
		{"WITH a AS NOT MATERIALIZED (SELECT 1) FROM a", true, false},
		{"WITH x AS (SELECT $$ ) $$, E'\\') ' AS q) (SELECT 1)", true, false},
		{"WITH RECURSIVE t(a) USING KEY (a) AS (SELECT 1) SELECT * FROM t", true, false},
		{"WITH x AS (SELECT 1 /* ) */ -- )\n) MERGE INTO t USING x ON true WHEN MATCHED THEN DELETE", false, true},
		{"WITH x AS (SELECT 1", false, false},
		{"WITH x AS (SELECT 1)) SELECT 1", false, false},
		{"WITH x AS (SELECT 1); SELECT 1", false, false},
		{"WITH `odd)` AS (SELECT 1) SELECT 1", true, false},
		{"$tag$ unterminated", false, false},
		{"$1", false, false},
	}
	for _, c := range cases {
		got := classifyStatement(c.sql)
		if got.readOnly != c.readOnly || got.dml != c.dml {
			t.Errorf("%q: got %+v, want readOnly=%v dml=%v", c.sql, got, c.readOnly, c.dml)
		}
	}
	for sql, want := range map[string]stmtInfo{
		"INSERT INTO t VALUES (1) RETURNING *":               {dml: true, returning: true, hasReturning: true},
		"WITH x AS (SELECT 1) UPDATE t SET a = 1":            {dml: true, returning: true},
		"INSERT INTO t VALUES (1)":                           {dml: true, returning: true},
		"update t set a = 1 returning \"Count\"":             {dml: true, returning: true, hasReturning: true},
		"WITH x AS (SELECT 1) DELETE FROM t RETURNING *":     {dml: true, returning: true, hasReturning: true},
		"(INSERT INTO t VALUES (1) RETURNING a)":             {dml: true, returning: true, hasReturning: true},
		"INSERT INTO t SELECT * FROM (SELECT 'returning')":   {dml: true, returning: true},
		"INSERT INTO t (\"returning\") VALUES (1)":           {dml: true, returning: true},
		"DELETE FROM t WHERE a IN (SELECT returning FROM u)": {dml: true, returning: true},
		"INSERT INTO t VALUES (1); SELECT 1 AS returning":    {dml: true, returning: true},
		"INSERT INTO t VALUES (1) -- RETURNING *":            {dml: true, returning: true},
		"SELECT 1 AS returning":                              {readOnly: true},
		"COPY t TO 'x.csv'":                                  {dml: true},
		"CREATE OR REPLACE TABLE t AS SELECT 1":              {create: true},
		"create temp table t as select 1":                    {create: true},
		"WITH x AS (SELECT 1) SELECT * FROM x":               {readOnly: true},
		"EXPLAIN ANALYZE CREATE TABLE t AS SELECT 1 AS a":    {},
	} {
		if got := classifyStatement(sql); got != want {
			t.Errorf("%q: got %+v, want %+v", sql, got, want)
		}
	}
}

func FuzzClassifyStatement(f *testing.F) {
	for _, s := range []string{"SELECT 1", "WITH x AS (SELECT 1) DELETE FROM t", "/* /* */", "E'\\", "$a$ $a", "((("} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_ = classifyStatement(s) // must terminate without panicking
	})
}
