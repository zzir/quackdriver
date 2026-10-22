package quackdriver

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/quacktest"
)

const testToken = "test_token"

var (
	typBigint  = logicaltype.Type{ID: logicaltype.IDBigint}
	typInteger = logicaltype.Type{ID: logicaltype.IDInteger}
	typVarchar = logicaltype.Type{ID: logicaltype.IDVarchar}
)

// newServer starts a fake Quack v3 server for the test.
func newServer(t *testing.T) *quacktest.Server {
	t.Helper()
	s := quacktest.New(testToken)
	t.Cleanup(s.Close)
	return s
}

// openDB opens a database/sql handle on s through NewConnector.
func openDB(t *testing.T, s *quacktest.Server, params ...string) *sql.DB {
	t.Helper()
	c, err := NewConnector(s.DSN(params...))
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openConn opens one driver connection on s (closed at cleanup).
func openConn(t *testing.T, s *quacktest.Server, params ...string) *Conn {
	t.Helper()
	conn, err := Driver{}.Open(s.DSN(params...))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := conn.(*Conn)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// rawConn runs f on the driver connection behind a sql.Conn.
func rawConn(t *testing.T, db *sql.DB, f func(c *Conn)) {
	t.Helper()
	ctx := context.Background()
	sc, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer sc.Close()
	if err := sc.Raw(func(dc any) error {
		f(dc.(*Conn))
		return nil
	}); err != nil {
		t.Fatalf("Raw: %v", err)
	}
}

// fastHeartbeat makes heartbeats fire every d for the test's duration.
func fastHeartbeat(t *testing.T, d time.Duration) {
	t.Helper()
	old := heartbeatIntervalOverride.Swap(int64(d))
	t.Cleanup(func() { heartbeatIntervalOverride.Store(old) })
}

// intRows builds n rows of one BIGINT column holding 0..n-1.
func intRows(n int) [][]any {
	rows := make([][]any, n)
	for i := range rows {
		rows[i] = []any{int64(i)}
	}
	return rows
}

// intResult is a one-column BIGINT result with n rows, cut into batches of
// perBatch rows (one chunk each).
func intResult(n, perBatch int) quacktest.Result {
	return quacktest.Rows([]string{"i"}, []logicaltype.Type{typBigint}, intRows(n), perBatch, 1)
}

// scanInts reads every row of a one-column BIGINT query.
func scanInts(t *testing.T, rows *sql.Rows) []int64 {
	t.Helper()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	return out
}

func checkSequence(t *testing.T, got []int64, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("got %d rows, want %d", len(got), n)
	}
	for i, v := range got {
		if v != int64(i) {
			t.Fatalf("row %d = %d, want %d", i, v, i)
		}
	}
}

// fetchesOf returns the FETCH events of one query.
func fetchesOf(s *quacktest.Server, uuid codec.HugeInt) []quacktest.Event {
	var out []quacktest.Event
	for _, e := range s.EventsOf(message.TypeFetchRequest) {
		if e.QueryUUID == uuid {
			out = append(out, e)
		}
	}
	return out
}

// prepareOf returns the PREPARE event for sql (the last one).
func prepareOf(t *testing.T, s *quacktest.Server, sql string) quacktest.Event {
	t.Helper()
	var found *quacktest.Event
	for _, e := range s.EventsOf(message.TypePrepareRequest) {
		if e.SQL == sql {
			e := e
			found = &e
		}
	}
	if found == nil {
		t.Fatalf("no PREPARE for %q", sql)
	}
	return *found
}

// cancelled reports whether a CANCEL_REQUEST for the query was sent.
func cancelled(s *quacktest.Server, uuid codec.HugeInt) bool {
	for _, e := range s.EventsOf(message.TypeCancelRequest) {
		if e.QueryUUID == uuid {
			return true
		}
	}
	return false
}

// eventually polls cond until it holds or two seconds pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// driverGoroutines lists the stacks of goroutines running driver code that
// must not outlive their connection.
func driverGoroutines() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		for _, fn := range []string{
			"quackdriver.(*Conn).heartbeatLoop",
			"quackdriver.(*fetcher).topUp",
			"quackdriver.(*fetcher).drain",
			"quackdriver.(*query).cancelAsync",
			// background requests: abandoned PREPARE, CANCEL, ACKNOWLEDGEMENT
			"quackdriver.(*Conn).goBackground",
			// an abandoned CONNECTION_REQUEST
			"quackdriver.(*Conn).handshakeRequest",
			"quackdriver.(*Conn).disconnectAbandoned",
		} {
			if strings.Contains(g, fn) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// checkNoLeaks fails when driver goroutines are still running shortly after
// everything was closed.
func checkNoLeaks(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		left := driverGoroutines()
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d driver goroutines leaked:\n%s", len(left), strings.Join(left, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T: %v", err, err)
}
