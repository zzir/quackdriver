package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/quacktest"
)

// TestInterleavedRowsInTx iterates a multi-batch result inside a
// transaction while executing updates on the same connection. Quack aborts
// a statement when the next one is prepared, so the driver must read the
// rest of the result first.
func TestInterleavedRowsInTx(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT id FROM t", intResult(20, 4))
	s.ScriptFunc(func(sql string) (quacktest.Result, bool) {
		if strings.HasPrefix(sql, "UPDATE t SET seen = true WHERE id = ") {
			return quacktest.Count(1), true
		}
		return quacktest.Result{}, false
	})
	db := openDB(t, s, "inline_rows=1")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query("SELECT id FROM t")
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
		res, err := tx.Exec("UPDATE t SET seen = true WHERE id = ?", id)
		if err != nil {
			t.Fatalf("update %d: %v", id, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("RowsAffected %d", n)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	checkSequence(t, got, 20)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Every FETCH of the SELECT happened before the first UPDATE's PREPARE,
	// and none of them failed.
	p := prepareOf(t, s, "SELECT id FROM t")
	events := s.Events()
	firstUpdate := -1
	lastFetch := -1
	for i, e := range events {
		if e.Type == message.TypePrepareRequest && strings.HasPrefix(e.SQL, "UPDATE") && firstUpdate < 0 {
			firstUpdate = i
		}
		if e.Type == message.TypeFetchRequest && e.QueryUUID == p.QueryUUID {
			lastFetch = i
			if e.Error != "" {
				t.Fatalf("FETCH failed: %s", e.Error)
			}
		}
	}
	if firstUpdate < 0 || lastFetch < 0 || lastFetch > firstUpdate {
		t.Fatalf("last FETCH at %d, first UPDATE at %d", lastFetch, firstUpdate)
	}
	if s.Count(message.TypeCancelRequest) != 0 {
		t.Fatal("buffering must not cancel the statement")
	}
}

// TestBufferedRowsKeepError: a failure while buffering is reported by the
// old Rows after the rows read so far, and the new statement still runs.
func TestBufferedRowsKeepError(t *testing.T) {
	s := newServer(t)
	res := intResult(9, 3)
	res.FetchErrors = map[uint64]*message.ErrorResponse{2: {Message: "disk on fire", ExceptionType: "IO"}}
	s.Script("SELECT bad", res)
	s.Script("SELECT good", intResult(2, 2))
	c := openConn(t, s, "inline_rows=0", "fetch_ahead=0")
	ctx := context.Background()
	r1, err := c.QueryContext(ctx, "SELECT bad", nil)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c.QueryContext(ctx, "SELECT good", nil)
	if err != nil {
		t.Fatalf("second statement: %v", err)
	}
	defer r2.Close()
	dest := make([]driver.Value, 1)
	for i := range 3 {
		if err := r1.Next(dest); err != nil || dest[0] != int64(i) {
			t.Fatalf("buffered row %d: %v %v", i, dest[0], err)
		}
	}
	var qe *Error
	if err := r1.Next(dest); !errors.As(err, &qe) || qe.Type != "IO" {
		t.Fatalf("buffered error: %v", err)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r2.Next(dest); err != nil {
			t.Fatal(err)
		}
	}
	if err := r2.Next(dest); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
	}
}

// TestExclusiveHook: acquiring the connection for an Appender buffers an
// open Rows (the Appender's PREPARE would abort its statement), and the
// holder itself can still run statements.
func TestExclusiveHook(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT 1", intResult(1, 1))
	s.Script("SELECT big", intResult(6, 2))
	c := openConn(t, s, "inline_rows=1")
	ctx := context.Background()
	open, err := c.QueryContext(ctx, "SELECT big", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.acquireExclusive(ctx); err != nil {
		t.Fatal(err)
	}
	if c.active != nil {
		t.Fatal("acquiring must buffer the open Rows")
	}
	if _, err := c.BeginTx(ctx, driver.TxOptions{}); !errors.Is(err, ErrConnBusy) || c.inTx {
		t.Fatalf("BeginTx while held: %v, inTx=%v", err, c.inTx)
	}
	q, err := c.prepare(ctx, "SELECT 1", message.OptionalIdx{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.exec(ctx); err != nil {
		t.Fatal(err)
	}
	c.releaseExclusive()
	dest := make([]driver.Value, 1)
	for i := range 6 {
		if err := open.Next(dest); err != nil || dest[0] != int64(i) {
			t.Fatalf("row %d: %v %v", i, dest[0], err)
		}
	}
	_ = open.Close()
}

func TestExecRowsAffected(t *testing.T) {
	s := newServer(t)
	s.Script("INSERT INTO t VALUES (1), (2), (3)", quacktest.Count(3))
	s.Script("WITH old AS (SELECT 1) DELETE FROM t WHERE id IN (SELECT * FROM old)", quacktest.Count(2))
	s.Script("UPDATE t SET x = 1", quacktest.Count(7))
	s.Script("SELECT count(*) AS Count FROM t", quacktest.Count(5))
	s.Script("CREATE TABLE t (id INTEGER)", quacktest.Success())
	s.Script("SELECT big", intResult(50, 5))
	twoRows := quacktest.Rows([]string{"Count"}, []logicaltype.Type{typBigint}, [][]any{{int64(1)}, {int64(2)}}, 0, 0)
	s.Script("INSERT INTO t SELECT 1 UNION ALL SELECT 2 RETURNING 1 AS Count", twoRows)
	db := openDB(t, s, "inline_rows=0")
	cases := []struct {
		sql  string
		want int64
	}{
		{"INSERT INTO t VALUES (1), (2), (3)", 3},
		{"WITH old AS (SELECT 1) DELETE FROM t WHERE id IN (SELECT * FROM old)", 2},
		{"UPDATE t SET x = 1", 7}, // the Count arrives through FETCH (inline_rows=0)
		{"SELECT count(*) AS Count FROM t", 0},
		{"CREATE TABLE t (id INTEGER)", 0},
		{"SELECT big", 0},
		// RETURNING counts the returned rows, even of a BIGINT "Count".
		{"INSERT INTO t SELECT 1 UNION ALL SELECT 2 RETURNING 1 AS Count", 2},
	}
	for _, c := range cases {
		res, err := db.Exec(c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		n, err := res.RowsAffected()
		if err != nil || n != c.want {
			t.Errorf("%s: RowsAffected %d %v, want %d", c.sql, n, err, c.want)
		}
		if _, err := res.LastInsertId(); err == nil {
			t.Errorf("%s: LastInsertId must be unsupported", c.sql)
		}
	}
	// Exec drained every batch of the SELECT.
	p := prepareOf(t, s, "SELECT big")
	f := fetchesOf(s, p.QueryUUID)
	if len(f) != 11 || f[len(f)-1].Response != message.TypeFetchResponse {
		t.Fatalf("Exec issued %d FETCHes", len(f))
	}
}

func TestExecServerError(t *testing.T) {
	s := newServer(t)
	s.Script("INSERT INTO t VALUES (1)", quacktest.Result{Error: &message.ErrorResponse{
		Message:       "Duplicate key \"id: 1\" violates primary key constraint.",
		ExceptionType: "Constraint",
		ExtraInfo:     map[string]string{"error_subtype": "DUPLICATE_KEY"},
	}})
	db := openDB(t, s)
	_, err := db.Exec("INSERT INTO t VALUES (1)")
	var qe *Error
	if !errors.As(err, &qe) {
		t.Fatalf("got %s", errString(err))
	}
	if qe.Type != "Constraint" || qe.ExtraInfo["error_subtype"] != "DUPLICATE_KEY" || qe.MustInvalidate {
		t.Fatalf("error %+v", qe)
	}
	want := "quackdriver: Constraint Error: Duplicate key \"id: 1\" violates primary key constraint."
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
	if errors.Is(err, driver.ErrBadConn) {
		t.Fatal("a statement error is not a bad connection")
	}
	if _, err := db.Exec("SELECT nothing"); err == nil {
		t.Fatal("unscripted query must fail")
	}
	// one connection served both statements
	if n := s.Count(message.TypeConnectionRequest); n != 1 {
		t.Fatalf("%d connections", n)
	}
}

func TestQueryContextCancelDuringPrepare(t *testing.T) {
	s := newServer(t)
	gate := make(chan struct{})
	defer close(gate)
	s.Script("SELECT slow", quacktest.Result{Names: []string{"i"}, Types: []logicaltype.Type{typBigint}, PrepareGate: gate})
	s.Script("SELECT 1", intResult(1, 1))
	c := openConn(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.QueryContext(ctx, "SELECT slow", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %s", errString(err))
	}
	p := prepareOf(t, s, "SELECT slow")
	eventually(t, "CANCEL_REQUEST", func() bool {
		for _, e := range s.EventsOf(message.TypeCancelRequest) {
			if e.QueryUUID == p.QueryUUID && e.ClientQueryID == p.ClientQueryID {
				return true
			}
		}
		return false
	})
	if !c.IsValid() {
		t.Fatal("a cancelled statement must not break the connection")
	}
	if _, err := c.ExecContext(context.Background(), "SELECT 1", nil); err != nil {
		t.Fatal(err)
	}
	// The abandoned PREPARE was not aborted but answered (the CANCEL
	// released it), and the next statement was only sent after the CANCEL:
	// a request that arrives late would otherwise hit the next statement.
	events := s.Events()
	slow, cancelAt, next := -1, -1, -1
	for i, e := range events {
		switch {
		case e.Type == message.TypePrepareRequest && e.SQL == "SELECT slow":
			slow = i
		case e.Type == message.TypeCancelRequest && e.QueryUUID == p.QueryUUID && cancelAt < 0:
			cancelAt = i
		case e.Type == message.TypePrepareRequest && e.SQL == "SELECT 1":
			next = i
		}
	}
	if slow < 0 || cancelAt < 0 || next < 0 || slow >= cancelAt || cancelAt >= next {
		t.Fatalf("event order: PREPARE %d, CANCEL %d, next PREPARE %d", slow, cancelAt, next)
	}
	if e := events[slow]; e.Response != message.TypeErrorResponse || !strings.Contains(e.Error, "cancelled") {
		t.Fatalf("abandoned PREPARE answered with %v %q", e.Response, e.Error)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	checkNoLeaks(t)
}

// TestLeakDetectorSeesBackgroundRequests: checkNoLeaks must notice the
// goroutines of background requests (abandoned PREPARE, CANCEL), which run
// through goBackground.
func TestLeakDetectorSeesBackgroundRequests(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	before := len(driverGoroutines())
	started := make(chan struct{})
	if !c.goBackground(true, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	}) {
		t.Fatal("goBackground refused")
	}
	<-started
	if n := len(driverGoroutines()); n <= before {
		t.Fatalf("leak detector does not see a goBackground goroutine (%d before, %d now)", before, n)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	checkNoLeaks(t)
}

func TestQueryContextCancelDuringFetch(t *testing.T) {
	s := newServer(t)
	res := intResult(9, 3)
	res.BeforeFetch = func(ctx context.Context, index uint64) {
		if index == 2 {
			<-ctx.Done() // never answers batch 2
		}
	}
	s.Script("SELECT stuck", res)
	db := openDB(t, s, "inline_rows=0", "fetch_ahead=0")
	ctx, cancel := context.WithCancel(context.Background())
	rows, err := db.QueryContext(ctx, "SELECT stuck")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if !rows.Next() {
			t.Fatalf("row %d: %v", i, rows.Err())
		}
	}
	time.AfterFunc(50*time.Millisecond, cancel)
	if rows.Next() {
		t.Fatal("expected the stream to stop")
	}
	if err := rows.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("rows.Err() = %s", errString(err))
	}
	_ = rows.Close()
	p := prepareOf(t, s, "SELECT stuck")
	eventually(t, "CANCEL_REQUEST", func() bool { return cancelled(s, p.QueryUUID) })
	checkNoLeaksAfter(t, db)
}

func TestExecContextCancelDuringFetch(t *testing.T) {
	s := newServer(t)
	res := intResult(9, 3)
	res.BeforeFetch = func(ctx context.Context, index uint64) { <-ctx.Done() }
	s.Script("UPDATE slow SET x = 1", res)
	c := openConn(t, s, "inline_rows=0")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.ExecContext(ctx, "UPDATE slow SET x = 1", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %s", errString(err))
	}
	p := prepareOf(t, s, "UPDATE slow SET x = 1")
	eventually(t, "CANCEL_REQUEST", func() bool { return cancelled(s, p.QueryUUID) })
}

func TestRowsCloseEarly(t *testing.T) {
	t.Run("read-only statement is cancelled", func(t *testing.T) {
		s := newServer(t)
		s.Script("  /* hint */ (SELECT * FROM t)", intResult(30, 3))
		db := openDB(t, s, "inline_rows=1", "fetch_ahead=2")
		rows, err := db.Query("  /* hint */ (SELECT * FROM t)")
		if err != nil {
			t.Fatal(err)
		}
		if !rows.Next() {
			t.Fatal(rows.Err())
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		p := prepareOf(t, s, "  /* hint */ (SELECT * FROM t)")
		cancels := s.EventsOf(message.TypeCancelRequest)
		if len(cancels) != 1 || cancels[0].QueryUUID != p.QueryUUID || cancels[0].Response != message.TypeSuccessResponse {
			t.Fatalf("cancels %+v", cancels)
		}
		if f := fetchesOf(s, p.QueryUUID); len(f) > 4 {
			t.Fatalf("closing early still fetched %d batches", len(f))
		}
		checkNoLeaksAfter(t, db)
	})
	t.Run("write statement is drained", func(t *testing.T) {
		s := newServer(t)
		const q = "INSERT INTO t SELECT * FROM range(30) RETURNING *"
		s.Script(q, intResult(30, 3))
		db := openDB(t, s, "inline_rows=1")
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		if !rows.Next() {
			t.Fatal(rows.Err())
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if n := s.Count(message.TypeCancelRequest); n != 0 {
			t.Fatalf("a write must not be cancelled (%d cancels)", n)
		}
		p := prepareOf(t, s, q)
		f := fetchesOf(s, p.QueryUUID)
		last := f[len(f)-1]
		if last.BatchIndex < 10 || last.Response != message.TypeFetchResponse {
			t.Fatalf("drain stopped at batch %d", last.BatchIndex)
		}
	})
	t.Run("drain error is returned", func(t *testing.T) {
		s := newServer(t)
		const q = "DELETE FROM t RETURNING *"
		res := intResult(30, 3)
		res.FetchErrors = map[uint64]*message.ErrorResponse{5: {Message: "boom", ExceptionType: "Constraint"}}
		s.Script(q, res)
		db := openDB(t, s, "inline_rows=1")
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		rows.Next()
		var qe *Error
		if err := rows.Close(); !errors.As(err, &qe) || qe.Type != "Constraint" {
			t.Fatalf("Close: %s", errString(err))
		}
	})
}

func TestPing(t *testing.T) {
	s := newServer(t)
	db := openDB(t, s)
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	if n := s.Count(message.TypeHeartbeatRequest); n != 1 {
		t.Fatalf("Ping sent %d heartbeats", n)
	}
	rawConn(t, db, func(c *Conn) {
		s.DropConnections()
		if err := c.Ping(context.Background()); !errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("Ping on a dropped session: %s", errString(err))
		}
		var qe *Error
		if err := c.Ping(context.Background()); !errors.Is(err, driver.ErrBadConn) || errors.As(err, &qe) {
			t.Fatalf("second Ping: %s", errString(err))
		}
		if c.IsValid() || !errors.Is(c.ResetSession(context.Background()), driver.ErrBadConn) {
			t.Fatal("conn must report bad")
		}
	})
	// database/sql retries Ping on a fresh connection.
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rawConn(t, db, func(c *Conn) {
		if err := c.Ping(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Ping with cancelled ctx: %s", errString(err))
		}
		if !c.IsValid() {
			t.Fatal("a cancelled Ping must not mark the conn bad")
		}
	})
}

// TestCloseDisconnect: Close sends DISCONNECT for a connection that is
// merely unusable (the server reclaims a session only when a message for
// it arrives), but not once the server said the session is gone.
func TestCloseDisconnect(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT 1", intResult(1, 1))
	c := openConn(t, s)
	c.markBad()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if n := s.Count(message.TypeDisconnectMessage); n != 1 || len(s.Connections()) != 0 {
		t.Fatalf("DISCONNECT sent %d times, sessions %v", n, s.Connections())
	}

	c = openConn(t, s)
	s.DropConnections()
	if _, err := c.ExecContext(context.Background(), "SELECT 1", nil); !errors.Is(err, driver.ErrBadConn) || !c.gone.Load() {
		t.Fatalf("Exec on a dropped session: %v, gone=%v", err, c.gone.Load())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if n := s.Count(message.TypeDisconnectMessage); n != 1 {
		t.Fatal("DISCONNECT sent on a gone session")
	}
}

func TestCloseStopsReadAhead(t *testing.T) {
	s := newServer(t)
	res := intResult(30, 3)
	res.BeforeFetch = func(ctx context.Context, index uint64) {
		if index > 1 {
			<-ctx.Done()
		}
	}
	s.Script("SELECT forever", res)
	c := openConn(t, s, "inline_rows=0", "fetch_ahead=3")
	rows, err := c.QueryContext(context.Background(), "SELECT forever", nil)
	if err != nil {
		t.Fatal(err)
	}
	dest := make([]driver.Value, 1)
	for range 3 {
		if err := rows.Next(dest); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on the read-ahead")
	}
	if err := rows.Next(dest); !errors.Is(err, errConnClosed) {
		t.Fatalf("Next after Close: %s", errString(err))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	checkNoLeaks(t)
}

// TestStmtAndArgs covers database/sql prepared statements (client-side
// substitution) and the argument types the driver accepts.
func TestStmtAndArgs(t *testing.T) {
	s := newServer(t)
	var mu sync.Mutex
	var seen []string
	s.ScriptFunc(func(sql string) (quacktest.Result, bool) {
		mu.Lock()
		seen = append(seen, sql)
		mu.Unlock()
		return intResult(1, 1), true
	})
	db := openDB(t, s)
	stmt, err := db.Prepare("SELECT ?, ?, ?, ?")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	tz, _ := NewTimeTz(time.Hour, -3600)
	var v int64
	if err := stmt.QueryRow(IntervalValue{Days: 1}, tz, 90*time.Minute, "it's").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if _, err := stmt.Exec(1, 2, 3, 4); err != nil {
		t.Fatal(err)
	}
	// Copy under the lock and release it: the server's ScriptFunc takes mu
	// too, so holding it across the next statement could deadlock.
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 2 || !strings.Contains(got[0], "INTERVAL") || !strings.Contains(got[0], "TIMETZ") ||
		!strings.Contains(got[0], "'it''s'") {
		t.Fatalf("formatted SQL %q", got)
	}
	if _, err := db.Exec("SELECT ?"); err == nil {
		t.Fatal("missing argument must fail")
	}
}

func TestPrepareResponseValidation(t *testing.T) {
	cases := []struct {
		name string
		hook func(h message.Header, b message.Body) message.Body
		want string
	}{
		{"names/types", func(h message.Header, b message.Body) message.Body {
			p := b.(message.PrepareRequest)
			return message.PrepareResponse{ResultNames: []string{"a", "b"}, ResultTypes: []logicaltype.Type{typBigint}, QueryUUID: p.QueryUUID}
		}, "2 names but 1 types"},
		{"uuid", func(h message.Header, b message.Body) message.Body {
			return message.PrepareResponse{QueryUUID: codec.HugeInt{Lower: 1}}
		}, "another query"},
		{"type", func(h message.Header, b message.Body) message.Body {
			return message.SuccessResponse{}
		}, "unexpected SUCCESS_RESPONSE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t)
			s.SetRawHook(func(w http.ResponseWriter, h message.Header, b message.Body) bool {
				if _, ok := b.(message.PrepareRequest); !ok {
					return false
				}
				quacktest.RawResponse(w, message.Header{ConnectionID: h.ConnectionID}, c.hook(h, b))
				return true
			})
			db := openDB(t, s)
			if _, err := db.Query("SELECT 1"); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

// TestLegacyEntryPoints covers the pre-context driver interfaces.
func TestLegacyEntryPoints(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT ?", intResult(1, 1))
	s.Script("SELECT 7", intResult(3, 1))
	c := openConn(t, s)
	st, err := c.Prepare("SELECT 7")
	if err != nil {
		t.Fatal(err)
	}
	if st.NumInput() != -1 {
		t.Fatalf("NumInput %d", st.NumInput())
	}
	if _, err := st.Exec(nil); err != nil { //nolint:staticcheck // tests the legacy driver.Stmt method
		t.Fatal(err)
	}
	rows, err := st.Query(nil) //nolint:staticcheck // tests the legacy driver.Stmt method
	if err != nil {
		t.Fatal(err)
	}
	dest := make([]driver.Value, 1)
	for i := range 3 {
		if err := rows.Next(dest); err != nil || dest[0] != int64(i) {
			t.Fatalf("row %d: %v %v", i, dest[0], err)
		}
	}
	_ = rows.Close()
	_ = st.Close()
	if _, err := c.Prepare("SELECT ?"); err != nil {
		t.Fatal(err)
	}
	tx, err := c.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if c.inTx {
		t.Fatal("Commit must end the transaction")
	}
}

// TestVariantRejected: a result with a VARIANT column, also a nested one,
// fails with a hint to cast it, and the connection stays usable.
func TestVariantRejected(t *testing.T) {
	s := newServer(t)
	variant := logicaltype.Type{ID: logicaltype.IDVariant}
	list := logicaltype.Type{ID: logicaltype.IDList, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraList, Child: &variant}}
	s.Script("SELECT v", quacktest.Rows([]string{"v"}, []logicaltype.Type{variant}, nil, 1, 1))
	s.Script("SELECT l", quacktest.Rows([]string{"l"}, []logicaltype.Type{list}, nil, 1, 1))
	s.Script("SELECT 1", intResult(1, 1))
	db := openDB(t, s)
	db.SetMaxOpenConns(1)
	for _, q := range []string{"SELECT v", "SELECT l"} {
		if _, err := db.Query(q); err == nil || !strings.Contains(err.Error(), "v::JSON") {
			t.Fatalf("%s: %v, want the VARIANT error", q, err)
		}
	}
	if err := db.QueryRow("SELECT 1").Scan(new(int)); err != nil {
		t.Fatalf("after the VARIANT error: %v", err)
	}
}

// TestRowCounter derives RowsAffected from result shapes: DuckDB's "Count"
// row for DML and CREATE TABLE ... AS, the row count for DML ... RETURNING
// (even when the first returned column is a BIGINT named "Count"), the
// chunk types for a client-data statement whose PREPARE has no schema.
func TestRowCounter(t *testing.T) {
	col := func(typ logicaltype.Type, vals ...any) message.DataChunk {
		return message.DataChunk{RowCount: len(vals), Types: []logicaltype.Type{typ}, Columns: []message.Column{message.Values(vals)}}
	}
	count := col(typBigint, int64(7))
	files := message.DataChunk{RowCount: 1, Types: []logicaltype.Type{typBigint, typVarchar},
		Columns: []message.Column{message.Values{int64(9)}, message.Values{"x"}}}
	big := []string{"Count"}
	bigT := []logicaltype.Type{typBigint}
	for i, c := range []struct {
		sql    string
		names  []string
		types  []logicaltype.Type
		chunks []message.DataChunk
		want   int64
	}{
		{"INSERT INTO t VALUES (1)", big, bigT, []message.DataChunk{count}, 7},
		{"INSERT INTO t VALUES (1)", []string{"i"}, []logicaltype.Type{typVarchar}, []message.DataChunk{col(typVarchar, "a", "b"), col(typVarchar, "c")}, 3},
		{"INSERT INTO t SELECT * FROM scan_data_from_quack_client('s', NULL::STRUCT(a INTEGER))", nil, nil, []message.DataChunk{col(typBigint, int64(4))}, 4},
		{"INSERT INTO t SELECT * FROM scan_data_from_quack_client('s', NULL::STRUCT(a INTEGER))", nil, nil, []message.DataChunk{col(typVarchar, "x")}, 0},
		{"COPY t TO 'x.csv' (RETURN_FILES)", []string{"Count", "Files"}, []logicaltype.Type{typBigint, typVarchar}, []message.DataChunk{files}, 9},
		{"COPY t TO 'x.csv'", []string{"a"}, []logicaltype.Type{typVarchar}, []message.DataChunk{col(typVarchar, "a")}, 0},
		{"CREATE TABLE t AS SELECT 1", big, bigT, []message.DataChunk{count}, 7},
		{"CREATE TABLE t AS SELECT 1", big, bigT, nil, 0},
		{"SELECT 1", big, bigT, []message.DataChunk{count}, 0},
		{`UPDATE inv SET "Count" = "Count" - 1 WHERE id = 7 RETURNING "Count"`, big, bigT, []message.DataChunk{col(typBigint, int64(41))}, 1},
		{`DELETE FROM inv WHERE id = 8 RETURNING *`, big, bigT, []message.DataChunk{col(typBigint, int64(5), int64(5), int64(5))}, 3},
		{`WITH x AS (SELECT 1) INSERT INTO inv SELECT 41, 7 RETURNING *`, big, bigT, []message.DataChunk{count}, 1},
	} {
		rc := newRowCounter(&query{info: classifyStatement(c.sql), prep: message.PrepareResponse{ResultNames: c.names, ResultTypes: c.types}})
		if err := rc.add(c.chunks); err != nil || rc.result() != c.want {
			t.Errorf("case %d %s: %d %v, want %d", i, c.sql, rc.result(), err, c.want)
		}
	}
	rc := newRowCounter(&query{info: stmtInfo{dml: true}})
	if err := rc.add([]message.DataChunk{{RowCount: 2, Types: bigT, Columns: []message.Column{message.Values{int64(1)}}}}); err == nil {
		t.Fatal("malformed Count chunk must fail")
	}
}

// TestMaxBufferedRows: a statement on a connection whose open Rows still has
// more than max_buffered_rows rows on the server fails with an error naming
// the parameter, and nothing is sent; the Rows stays readable and keeps its
// stream, and once it is closed the statement runs. With max_buffered_rows=0
// the rest is buffered whatever its size.
func TestMaxBufferedRows(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT big", intResult(60, 4))
	s.Script("SELECT 1", intResult(1, 1))
	ctx := context.Background()
	for _, c := range []struct {
		param  string
		failed bool
	}{{"max_buffered_rows=10", true}, {"max_buffered_rows=0", false}, {"max_buffered_rows=1000", false}} {
		t.Run(c.param, func(t *testing.T) {
			s.ResetEvents()
			conn := openConn(t, s, "inline_rows=4", "fetch_ahead=0", c.param)
			rows, err := conn.QueryContext(ctx, "SELECT big", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = conn.ExecContext(ctx, "SELECT 1", nil)
			if c.failed {
				if err == nil || !strings.Contains(err.Error(), "max_buffered_rows") || errors.Is(err, driver.ErrBadConn) {
					t.Fatalf("statement over the limit: %v", err)
				}
				if n := len(s.EventsOf(message.TypePrepareRequest)); n != 1 {
					t.Fatalf("%d PREPAREs: the refused statement must not be sent", n)
				}
				if !conn.IsValid() {
					t.Fatal("the refused statement broke the connection")
				}
			} else if err != nil {
				t.Fatalf("statement within the limit: %v", err)
			}
			// The Rows is complete either way.
			dest := make([]driver.Value, 1)
			for i := range 60 {
				if err := rows.Next(dest); err != nil || dest[0] != int64(i) {
					t.Fatalf("row %d: %v %v", i, dest[0], err)
				}
			}
			if err := rows.Next(dest); !errors.Is(err, io.EOF) {
				t.Fatalf("end: %v", err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(ctx, "SELECT 1", nil); err != nil {
				t.Fatalf("after the Rows ended: %v", err)
			}
			if s.Count(message.TypeCancelRequest) != 0 {
				t.Fatal("no statement should have been cancelled")
			}
		})
	}
}
