package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/quacktest"
	"github.com/zzir/quackdriver/internal/transport"
)

func TestErrorFormatting(t *testing.T) {
	for _, c := range []struct {
		e    error
		want string
	}{
		{&Error{Type: "Catalog", Message: "Table with name x does not exist!"}, "quackdriver: Catalog Error: Table with name x does not exist!"},
		{&Error{Message: "no type"}, "quackdriver: no type"},
		{&badConnError{cause: &Error{Type: "Invalid Input", Message: "Invalid connection id"}},
			"quackdriver: bad connection: Invalid Input Error: Invalid connection id"},
	} {
		if c.e.Error() != c.want {
			t.Errorf("%q, want %q", c.e.Error(), c.want)
		}
	}
	// A bad-connection error matches driver.ErrBadConn and unwraps to its cause.
	cause := &Error{Message: "Invalid connection id"}
	var qe *Error
	if err := error(&badConnError{cause: cause}); !errors.Is(err, driver.ErrBadConn) || !errors.As(err, &qe) || qe != cause {
		t.Fatal("badConnError must be driver.ErrBadConn and unwrap to its cause")
	}
}

func TestIsConnGone(t *testing.T) {
	for _, e := range []*Error{
		{Message: "Invalid connection id"},
		{Message: "Connection heartbeat lease expired"},
		{Message: "Database was closed", MustInvalidate: true},
		{Message: "The server database has been invalidated and the server must be restarted before it can be used again. Original error: \"x\"", MustInvalidate: true},
	} {
		if !isConnGone(e) {
			t.Errorf("%q not recognized", e.Message)
		}
	}
	for _, e := range []*Error{
		{Message: "Result has been closed"},
		{Message: "Interrupted: query was cancelled"},
		{Message: "invalid connection id"},
		// Messages raised by SQL (error('...')) that merely start like a
		// session-gone answer: the statement ran, so they must not be
		// retried on another connection.
		{Message: "Invalid connection id (from user data)", Type: "Invalid Input"},
		{Message: "Connection heartbeat lease expired!", Type: "Invalid Input"},
		{Message: "Database was closed by the user", Type: "Invalid Input"},
		// The invalidation answer always carries must_invalidate.
		{Message: "The server database has been invalidated and the server must be restarted", Type: "Invalid Input"},
	} {
		if isConnGone(e) {
			t.Errorf("%q misrecognized", e.Message)
		}
	}
}

func TestClassify(t *testing.T) {
	gone := &Error{Message: "Invalid connection id"}
	inval := &Error{Message: "boom", Type: "Fatal", MustInvalidate: true}
	plain := &Error{Message: "Catalog thing", Type: "Catalog"}
	cases := []struct {
		err     error
		first   bool
		badConn bool
		marks   bool
	}{
		{gone, true, true, true},
		{gone, false, false, true},
		{inval, true, false, true},
		{plain, true, false, false},
		{errors.New("other"), true, false, false},
	}
	for i, c := range cases {
		conn := &Conn{}
		got := conn.classify(c.err, c.first)
		if errors.Is(got, driver.ErrBadConn) != c.badConn || conn.bad.Load() != c.marks {
			t.Errorf("case %d: got %v (bad=%v)", i, got, conn.bad.Load())
		}
		if !c.badConn && !errors.Is(got, c.err) {
			t.Errorf("case %d: error replaced: %v", i, got)
		}
	}
}

// TestNotSentIsBadConn: a request that never reached the server (connection
// refused) is a bad connection for the statement's first request.
func TestNotSentIsBadConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c, err := NewConnector("quack://" + addr + "?token=" + testToken + "&connect_timeout=2s")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	conn := newConn(c)
	conn.connectionID = "fake"
	_, err = conn.ExecContext(context.Background(), "SELECT 1", nil)
	if !errors.Is(err, driver.ErrBadConn) || !transport.NotSent(err) {
		t.Fatalf("got %s", errString(err))
	}
	if conn.IsValid() {
		t.Fatal("conn must be marked bad")
	}
	// A FETCH that never left the client marks the conn bad but is not
	// retried by database/sql (the statement already ran).
	conn2 := newConn(c)
	conn2.connectionID = "fake"
	q := &query{conn: conn2, id: 1, uuid: message.NewQueryUUID()}
	f := q.newFetcher(0)
	_, _, err = f.next(context.Background())
	if errors.Is(err, driver.ErrBadConn) || !transport.NotSent(err) || conn2.IsValid() {
		t.Fatalf("FETCH: %s valid=%v", errString(err), conn2.IsValid())
	}
	_ = conn.Close()
	_ = conn2.Close()
}

// TestDroppedSessionRetried: after a server restart the session id is
// unknown; database/sql retries the statement on a fresh connection.
func TestDroppedSessionRetried(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT 1", intResult(1, 1))
	db := openDB(t, s)
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	s.DropConnections()
	var v int64
	if err := db.QueryRow("SELECT 1").Scan(&v); err != nil {
		t.Fatalf("query after restart: %v", err)
	}
	if n := s.Count(message.TypeConnectionRequest); n != 2 {
		t.Fatalf("%d handshakes, want 2", n)
	}
	var invalid int
	for _, e := range s.EventsOf(message.TypePrepareRequest) {
		if e.Error == "Invalid connection id" {
			invalid++
		}
	}
	if invalid != 1 {
		t.Fatalf("%d PREPAREs hit the dropped session", invalid)
	}
}

func TestLeaseExpiry(t *testing.T) {
	s := newServer(t)
	s.SetLeaseUnit(20 * time.Millisecond)
	fastHeartbeat(t, time.Hour) // no heartbeats: let the lease lapse
	s.Script("SELECT 1", intResult(1, 1))
	db := openDB(t, s, "heartbeat_timeout=1s")
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := db.Exec("SELECT 1"); err != nil {
		t.Fatalf("Exec after expiry: %v", err)
	}
	var expired bool
	for _, e := range s.EventsOf(message.TypePrepareRequest) {
		if e.Error == "Connection heartbeat lease expired" {
			expired = true
		}
	}
	if !expired || s.Count(message.TypeConnectionRequest) != 2 {
		t.Fatalf("expired=%v handshakes=%d", expired, s.Count(message.TypeConnectionRequest))
	}
}

func TestMustInvalidateDiscardsConn(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT fatal", quacktest.Result{Error: &message.ErrorResponse{
		Message: "the disk is gone", ExceptionType: "Fatal", MustInvalidate: true,
	}})
	s.Script("SELECT 1", intResult(1, 1))
	db := openDB(t, s)
	_, err := db.Exec("SELECT fatal")
	var qe *Error
	if !errors.As(err, &qe) || !qe.MustInvalidate || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("got %s", errString(err))
	}
	if n := s.Count(message.TypePrepareRequest); n != 1 {
		t.Fatalf("must_invalidate must not be retried (%d PREPAREs)", n)
	}
	if _, err := db.Exec("SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if n := s.Count(message.TypeConnectionRequest); n != 2 {
		t.Fatalf("the invalidated conn was reused (%d handshakes)", n)
	}
}

func TestInvalidatedServer(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT 1", intResult(1, 1))
	db := openDB(t, s)
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	s.Invalidate("FATAL Error: out of disk")
	_, err := db.Exec("SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "invalidated") {
		t.Fatalf("got %s", errString(err))
	}
	var qe *Error
	if !errors.As(err, &qe) || !qe.MustInvalidate {
		t.Fatalf("expected the server error, got %s", errString(err))
	}
}

func TestFetchSessionGoneNotRetried(t *testing.T) {
	s := newServer(t)
	res := intResult(9, 3)
	res.BeforeFetch = func(ctx context.Context, index uint64) {
		if index == 2 {
			s.DropConnections()
		}
	}
	// BeforeFetch runs after the session lookup, so batch 2 is still served;
	// batch 3 then hits the dropped session.
	s.Script("SELECT gone", res)
	db := openDB(t, s, "inline_rows=0", "fetch_ahead=0")
	rows, err := db.Query("SELECT gone")
	if err != nil {
		t.Fatal(err)
	}
	got := scanInts(t, rows)
	err = rows.Err()
	var qe *Error
	if !errors.As(err, &qe) || qe.Message != "Invalid connection id" || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("rows %v err %s", got, errString(err))
	}
	if n := s.Count(message.TypePrepareRequest); n != 1 {
		t.Fatalf("the statement was re-run (%d PREPAREs)", n)
	}
	// the broken conn is discarded
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	if n := s.Count(message.TypeConnectionRequest); n != 2 {
		t.Fatalf("%d handshakes", n)
	}
}
