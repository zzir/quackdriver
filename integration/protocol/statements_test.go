//go:build integration

package protocol

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/livetest"
)

// TestTransactions checks commit, rollback, isolation, aborted
// transactions, write-write conflicts and rejected options.
func TestTransactions(t *testing.T) {
	_, db := newDB(t)
	other := livetest.Conn(t, db)
	livetest.Exec(t, db, "CREATE TABLE protocol_tx (i INTEGER)")
	count := func() int64 { return scalar(t, other, "SELECT count(*) FROM protocol_tx") }
	begin := func() *sql.Tx {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}

	tx := begin()
	livetest.Exec(t, tx, "INSERT INTO protocol_tx VALUES (1), (2)")
	if n := scalar(t, tx, "SELECT count(*) FROM protocol_tx"); n != 2 || count() != 0 {
		t.Fatalf("own uncommitted rows %d, another session sees %d", n, count())
	}
	if err := tx.Commit(); err != nil || count() != 2 {
		t.Fatalf("after commit: %d, %v", count(), err)
	}

	tx = begin()
	livetest.Exec(t, tx, "DELETE FROM protocol_tx")
	if err := tx.Rollback(); err != nil || count() != 2 {
		t.Fatalf("after rollback: %d, %v", count(), err)
	}

	// A failed statement aborts the transaction; DuckDB answers its COMMIT
	// like a ROLLBACK, and the connection stays usable.
	tx = begin()
	livetest.Exec(t, tx, "INSERT INTO protocol_tx VALUES (10)")
	if _, err := tx.Exec("INSERT INTO protocol_tx VALUES ('x')"); err == nil {
		t.Fatal("bad insert succeeded")
	}
	_ = tx.Commit()
	if n := count(); n != 2 {
		t.Fatalf("aborted transaction left %d rows", n)
	}

	t1, t2 := begin(), begin()
	livetest.Exec(t, t1, "UPDATE protocol_tx SET i = i + 1 WHERE i = 1")
	_, err := t2.Exec("UPDATE protocol_tx SET i = i + 2 WHERE i = 1")
	if qe := (*quackdriver.Error)(nil); !errors.As(err, &qe) || qe.Type != "TransactionContext" {
		t.Errorf("conflicting update: %v, want a TransactionContext error", err)
	}
	_ = t2.Rollback()
	if err := t1.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, opts := range []*sql.TxOptions{{ReadOnly: true}, {Isolation: sql.LevelSerializable}} {
		if _, err := db.BeginTx(context.Background(), opts); err == nil {
			t.Errorf("BeginTx(%+v) accepted", opts)
		}
	}
}

// slowTransport delays every response, so that a short context ends while a
// request is in flight.
type slowTransport struct{ rt http.RoundTripper }

func (s slowTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.rt.RoundTrip(r)
	time.Sleep(150 * time.Millisecond)
	return resp, err
}

// TestCancelledBeginDoesNotLoseWrites: a BEGIN whose context ended while it
// was in flight may have run; the connection must not return to the pool
// inside that transaction, where a later autocommit write would be lost.
func TestCancelledBeginDoesNotLoseWrites(t *testing.T) {
	srv, setup := newDB(t)
	livetest.Exec(t, setup, "CREATE TABLE bt (i INTEGER)")
	db := livetest.OpenDB(t, srv.DSN(), quackdriver.WithHTTPClient(&http.Client{Transport: slowTransport{&http.Transport{MaxIdleConnsPerHost: 8}}}))
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := db.BeginTx(ctx, nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BeginTx: %v", err)
	}
	livetest.Exec(t, db, "INSERT INTO bt VALUES (1)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin after a cancelled BEGIN: %v", err)
	}
	_ = tx.Rollback()
	_ = db.Close()
	if n := scalar(t, setup, "SELECT count(*) FROM bt"); n != 1 {
		t.Fatalf("the autocommit INSERT was lost (%d rows)", n)
	}
}

// TestTypedErrors checks the exception type the server reports for common
// failures, on the Exec and the Query path.
func TestTypedErrors(t *testing.T) {
	_, db := newDB(t)
	livetest.Exec(t, db, "CREATE TABLE protocol_err (id INTEGER PRIMARY KEY, v VARCHAR NOT NULL); INSERT INTO protocol_err VALUES (1, 'a')")
	for _, c := range []struct{ sql, typ, contains string }{
		{"SELECT * FROM protocol_missing", "Catalog", "protocol_missing"},
		{"SELEC 1", "Parser", "syntax error"},
		{"SELECT nope FROM protocol_err", "Binder", "nope"},
		{"INSERT INTO protocol_err VALUES (1, 'b')", "Constraint", "Duplicate key"},
		{"INSERT INTO protocol_err VALUES (2, NULL)", "Constraint", "NOT NULL"},
		{"SELECT 'abc'::INTEGER", "Conversion", "abc"},
		{"SELECT error('boom')", "Invalid Input", "boom"},
		{"SELECT 9223372036854775807::BIGINT + 1", "Out of Range", "Overflow"},
	} {
		_, execErr := db.Exec(c.sql)
		var qe *quackdriver.Error
		for _, err := range []error{execErr, db.QueryRow(c.sql).Scan(new(any))} {
			if !errors.As(err, &qe) || qe.Type != c.typ || !strings.Contains(qe.Message, c.contains) || qe.MustInvalidate {
				t.Errorf("%q: %v, want a %s *Error containing %q", c.sql, err, c.typ, c.contains)
			} else if !strings.HasPrefix(err.Error(), "quackdriver: "+c.typ+" Error: ") {
				t.Errorf("%q: Error() = %q", c.sql, err.Error())
			}
		}
	}
	// ExtraInfo carries the server's structured details.
	_, err := db.Exec("SELECT * FROM protocol_missing")
	if qe := (*quackdriver.Error)(nil); !errors.As(err, &qe) || qe.ExtraInfo["name"] != "protocol_missing" {
		t.Errorf("extra info of %v", err)
	}

	// A statement error whose text merely looks like a session-gone answer
	// is not retried on another connection (the INSERT before it ran).
	_, err = db.Exec("INSERT INTO protocol_err VALUES (5, 'x'); SELECT error('Invalid connection id (from user data)')")
	if qe := (*quackdriver.Error)(nil); !errors.As(err, &qe) || strings.Contains(err.Error(), "bad connection") {
		t.Fatalf("got %v", err)
	}
	if n := scalar(t, db, "SELECT count(*) FROM protocol_err WHERE id = 5"); n != 1 {
		t.Fatalf("the INSERT ran %d times", n)
	}
}

// TestRowsAffected compares RowsAffected with the server's own change count,
// with results inlined and fetched.
func TestRowsAffected(t *testing.T) {
	csv := filepath.Join(t.TempDir(), "out.csv")
	srv := livetest.StartServer(t, livetest.ServerOptions{})
	for i, params := range []string{"", "inline_rows=0"} {
		db := livetest.OpenDB(t, srv.DSN(params))
		r := []string{"protocol_ra", "protocol_rb"}[i]
		for _, c := range []struct {
			sql  string
			want int64
		}{
			{"CREATE TABLE " + r + " (i INTEGER PRIMARY KEY, v VARCHAR)", 0},
			{"INSERT INTO " + r + " SELECT i, 'v' || i FROM range(100) t(i)", 100},
			{"UPDATE " + r + " SET v = 'x' WHERE i < 30", 30},
			{"WITH s AS (SELECT 1) DELETE FROM " + r + " WHERE i >= 90", 10},
			{"INSERT INTO " + r + " VALUES (200, 'a'), (201, 'b') RETURNING i", 2},
			{"UPDATE " + r + " SET v = 'y' WHERE i >= 200 RETURNING *", 2},
			{"MERGE INTO " + r + " USING (SELECT i FROM range(75, 95) t(i)) s ON " + r + ".i = s.i " +
				"WHEN MATCHED THEN UPDATE SET v = 'm' WHEN NOT MATCHED THEN INSERT VALUES (s.i, 'n')", 20},
			{"INSERT INTO " + r + " VALUES (1, 'dup') ON CONFLICT DO NOTHING", 0},
			{"COPY " + r + " TO '" + csv + "' (HEADER)", 97},
			{"CREATE TABLE " + r + "2 AS SELECT * FROM range(42)", 42},
			{"CREATE OR REPLACE TABLE " + r + "2 AS SELECT * FROM range(0)", 0},
			{"CREATE TABLE " + r + "3 (i INTEGER, v VARCHAR)", 0},
			{"COPY " + r + "3 FROM '" + csv + "' (HEADER)", 97},
			{"SELECT 1 AS \"Count\"", 0},
			{"SELECT count(*) AS \"Count\" FROM " + r, 0},
			{"SET threads = 4", 0},
			// RETURNING counts the returned rows even when the first one is
			// a BIGINT named "Count".
			{`CREATE TABLE ` + r + `c ("Count" BIGINT, id INTEGER); INSERT INTO ` + r + `c VALUES (42, 7), (10, 8)`, 2},
			{`UPDATE ` + r + `c SET "Count" = "Count" - 1 WHERE id = 7 RETURNING "Count"`, 1},
			{`DELETE FROM ` + r + `c WHERE id = 8 RETURNING *`, 1},
			{`INSERT INTO ` + r + `c VALUES (1, 1), (2, 2), (3, 3) RETURNING "Count"`, 3},
			{"DELETE FROM " + r, 97},
		} {
			res := livetest.Exec(t, db, c.sql)
			if n, err := res.RowsAffected(); err != nil || n != c.want {
				t.Errorf("%q: RowsAffected %d, %v; want %d", c.sql, n, err, c.want)
			}
			if _, err := res.LastInsertId(); err == nil {
				t.Errorf("LastInsertId supported?")
			}
		}
	}
}

// TestMultiStatement checks the server's rule: the first statement that
// returns a query result is returned; otherwise the last statement's.
func TestMultiStatement(t *testing.T) {
	_, db := newDB(t)
	c := livetest.Conn(t, db)
	rows, err := c.QueryContext(context.Background(),
		"CREATE TABLE protocol_multi (a INT); INSERT INTO protocol_multi VALUES (1), (2); SELECT a FROM protocol_multi ORDER BY a; SELECT 99")
	if err != nil {
		t.Fatal(err)
	}
	var got []int32
	for rows.Next() {
		var a int32
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
	}
	if err := rows.Err(); err != nil || len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	res := livetest.Exec(t, c, "INSERT INTO protocol_multi VALUES (3); INSERT INTO protocol_multi VALUES (4), (5), (6)")
	if n, _ := res.RowsAffected(); n != 3 {
		t.Fatalf("RowsAffected of the last INSERT: %d, want 3", n)
	}
	// An error in a later statement fails the whole request.
	if _, err := c.ExecContext(context.Background(), "INSERT INTO protocol_multi VALUES (7); SELECT * FROM protocol_nope"); err == nil {
		t.Fatal("no error")
	}
	livetest.Exec(t, c, "SET threads = 2; SET threads = 4")
}
