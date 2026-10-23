//go:build integration

// Package appendit holds the integration tests of the bulk-insert path
// (Conn.Append and the streaming Appender) against a real DuckDB 2.0 server.
package appendit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/message"
)

// env is one server plus a pool whose traffic is recorded.
type env struct {
	srv *livetest.Server
	rec *livetest.Recorder
	db  *sql.DB
}

// newEnv starts a dedicated server and opens a recorded pool on it. On
// cleanup it checks that the server never answered with an INTERNAL/FATAL
// error or an invalidation and that the database still works.
func newEnv(t *testing.T, params ...string) *env {
	t.Helper()
	srv := livetest.StartServer(t, livetest.ServerOptions{})
	rec := livetest.NewRecorder()
	e := &env{srv: srv, rec: rec}
	t.Cleanup(func() { e.checkHealthy(t) }) // runs after the pool is closed
	e.db = livetest.OpenDB(t, srv.DSN(params...), quackdriver.WithHTTPClient(&http.Client{Transport: rec}))
	return e
}

func (e *env) checkHealthy(t *testing.T) {
	t.Helper()
	for _, x := range e.rec.Since(0) {
		if _, b, err := message.Decode(x.Resp); err == nil {
			if er, ok := b.(message.ErrorResponse); ok && (er.MustInvalidate || strings.Contains(er.Message, "invalidated") ||
				strings.EqualFold(er.ExceptionType, "INTERNAL") || strings.EqualFold(er.ExceptionType, "FATAL")) {
				t.Errorf("server answered with a fatal error: %+v", er)
			}
		}
	}
	db, err := sql.Open("quack", e.srv.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(new(int)); err != nil {
		t.Errorf("health check: %v", err)
	}
	if out := e.srv.Output(); strings.Contains(out, "INTERNAL") || strings.Contains(out, "invalidated") {
		t.Errorf("server output mentions an internal error:\n%s", out)
	}
}

// conn returns a dedicated connection of the pool.
func (e *env) conn(t *testing.T) *sql.Conn {
	t.Helper()
	return livetest.Conn(t, e.db)
}

// raw runs fn with the driver connection of c.
func raw(c *sql.Conn, fn func(*quackdriver.Conn) error) error {
	return c.Raw(func(dc any) error { return fn(dc.(*quackdriver.Conn)) })
}

// appendRows runs Conn.Append on c.
func appendRows(c *sql.Conn, table string, cols []quackdriver.AppendColumn, rows [][]any) error {
	return raw(c, func(qc *quackdriver.Conn) error { return qc.Append(context.Background(), table, cols, rows) })
}

// appender streams rows through an Appender on c and returns Close's count.
func appender(c *sql.Conn, schema, table string, cols []quackdriver.AppendColumn, n int, row func(i int) []any, opts ...quackdriver.AppenderOption) (int64, error) {
	var count int64
	err := raw(c, func(qc *quackdriver.Conn) error {
		a, err := qc.NewAppender(context.Background(), schema, table, cols, opts...)
		if err != nil {
			return err
		}
		defer func() { _ = a.Abort() }()
		for i := range n {
			if err := a.AppendRow(row(i)...); err != nil {
				return err
			}
		}
		count, err = a.Close()
		return err
	})
	return count, err
}

func (e *env) exec(t *testing.T, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		livetest.Exec(t, e.db, s)
	}
}

func (e *env) queryInt(t *testing.T, q string) int64 {
	t.Helper()
	return livetest.Scalar[int64](t, e.db, q)
}

func (e *env) queryString(t *testing.T, q string) string {
	t.Helper()
	return livetest.Scalar[sql.NullString](t, e.db, q).String
}

// queryStrings returns the first column of a query, NULL as "<NULL>".
func (e *env) queryStrings(t *testing.T, q string) []string {
	t.Helper()
	var out []string
	for _, r := range livetest.Rows(t, e.db, q) {
		if r[0] == nil {
			r[0] = "<NULL>"
		}
		out = append(out, fmt.Sprint(r[0]))
	}
	return out
}

// sent returns the decoded requests of type T recorded after mark.
func sent[T message.Body](t *testing.T, e *env, mark int) []T {
	t.Helper()
	var out []T
	for _, x := range e.rec.Since(mark) {
		_, b, err := message.Decode(x.Req)
		if err != nil {
			t.Fatalf("decode recorded request: %v", err)
		}
		if m, ok := b.(T); ok {
			out = append(out, m)
		}
	}
	return out
}

// appenderPrepare returns the Appender's INSERT PREPARE sent after mark.
func appenderPrepare(t *testing.T, e *env, mark int) message.PrepareRequest {
	t.Helper()
	for _, p := range sent[message.PrepareRequest](t, e, mark) {
		if strings.Contains(p.SQL, "scan_data_from_quack_client") {
			return p
		}
	}
	t.Fatal("no Appender PREPARE recorded")
	return message.PrepareRequest{}
}

// cancelsFor counts the CANCEL_REQUESTs for the Appender's INSERT sent
// after mark.
func cancelsFor(t *testing.T, e *env, mark int) int {
	t.Helper()
	p := appenderPrepare(t, e, mark)
	n := 0
	for _, c := range sent[message.CancelRequest](t, e, mark) {
		if c.QueryUUID == p.QueryUUID {
			n++
		}
	}
	return n
}

// serverError extracts the *quackdriver.Error of err.
func serverError(err error) *quackdriver.Error {
	var qe *quackdriver.Error
	if errors.As(err, &qe) {
		return qe
	}
	return nil
}

// connUsable checks that c runs statements again.
func connUsable(t *testing.T, c *sql.Conn) {
	t.Helper()
	if n := livetest.Scalar[int](t, c, "SELECT 41 + 1"); n != 42 {
		t.Fatalf("connection not usable: %d", n)
	}
}

// sameOnBothPaths inserts every row through INSERT ... VALUES (?, ...) and
// through Conn.Append into a new table and fails when the server stored
// different values (compared as VARCHAR): the Appender's client-side
// conversions must match the server's casts of the substituted literals.
func sameOnBothPaths(t *testing.T, e *env, table string, cols []quackdriver.AppendColumn, rows [][]any) {
	t.Helper()
	defs := []string{"path VARCHAR", "id INTEGER"}
	for _, c := range cols {
		defs = append(defs, fmt.Sprintf("%q %s", c.Name, c.Type))
	}
	e.exec(t, "CREATE TABLE "+table+" ("+strings.Join(defs, ", ")+")")
	ph := strings.TrimPrefix(strings.Repeat(", ?", len(defs)), ", ")
	appended := make([][]any, len(rows))
	for i, r := range rows {
		livetest.Exec(t, e.db, "INSERT INTO "+table+" VALUES ("+ph+")", append([]any{"exec", i}, r...)...)
		appended[i] = append([]any{"append", i}, r...)
	}
	all := append([]quackdriver.AppendColumn{{Name: "path", Type: "VARCHAR"}, {Name: "id", Type: "INTEGER"}}, cols...)
	if err := appendRows(e.conn(t), table, all, appended); err != nil {
		t.Fatal(err)
	}
	render := func(alias string) string {
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = fmt.Sprintf("coalesce(%s.%q::VARCHAR, 'NULL')", alias, c.Name)
		}
		return strings.Join(parts, " || '|' || ")
	}
	for _, diff := range e.queryStrings(t, "SELECT a.id || ': Append stored ' || "+render("a")+" || ', INSERT stored ' || "+render("b")+
		" FROM "+table+" a JOIN "+table+" b ON a.id = b.id AND a.path = 'append' AND b.path = 'exec' WHERE "+render("a")+" <> "+render("b")) {
		t.Errorf("%s row %s", table, diff)
	}
}
