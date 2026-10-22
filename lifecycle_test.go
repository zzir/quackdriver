package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/quacktest"
)

// TestUserErrorLookingLikeSessionGone: a statement error whose text merely
// starts like a session-gone answer is not retried on another connection
// (the statement ran) and does not break the connection.
func TestUserErrorLookingLikeSessionGone(t *testing.T) {
	s := newServer(t)
	const q = "INSERT INTO g VALUES (1); SELECT error('Invalid connection id (from user data)')"
	s.Script(q, quacktest.Result{Error: &message.ErrorResponse{
		Message: "Invalid connection id (from user data)", ExceptionType: "Invalid Input"}})
	db := openDB(t, s)
	_, err := db.Exec(q)
	var qe *Error
	if !errors.As(err, &qe) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("got %s", errString(err))
	}
	if n := len(slices.DeleteFunc(s.EventsOf(message.TypePrepareRequest), func(e quacktest.Event) bool { return e.SQL != q })); n != 1 {
		t.Fatalf("statement sent %d times", n)
	}
	if n := s.Count(message.TypeConnectionRequest); n != 1 {
		t.Fatalf("%d sessions opened", n)
	}
}

// TestUnclosedAppenderDiscardsConn: an Appender left open when the
// connection goes back to the pool would make every later statement on it
// fail with ErrConnBusy; database/sql must discard it instead.
func TestUnclosedAppenderDiscardsConn(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT 1", intResult(1, 1))
	db := openDB(t, s)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	sc, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := sc.Raw(func(dc any) error {
		c := dc.(*Conn)
		id = c.connectionID
		a, err := c.NewAppender(ctx, "", "t", bigintCol)
		if err != nil {
			return err
		}
		return a.AppendRow(int64(1)) // neither Close nor Abort
	}); err != nil {
		t.Fatal(err)
	}
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := db.Exec("SELECT 1"); err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
	}
	eventually(t, "DISCONNECT of the abandoned Appender's session", func() bool {
		return !slices.Contains(s.Connections(), id)
	})
}

// slowConnect delays every CONNECTION_REQUEST by d before the fake server
// handles it.
func slowConnect(s *quacktest.Server, d time.Duration) {
	s.SetRawHook(func(_ http.ResponseWriter, _ message.Header, body message.Body) bool {
		if _, ok := body.(message.ConnectionRequest); ok {
			time.Sleep(d)
		}
		return false
	})
}

// TestHandshakeCancelledDisconnects: a CONNECTION_REQUEST whose caller gave
// up still creates a session on the server; the driver disconnects it.
func TestHandshakeCancelledDisconnects(t *testing.T) {
	s := newServer(t)
	// The server takes 600 ms; the caller gives up after 20 ms. The bound
	// below leaves room for a slow CI runner while still telling the two
	// apart.
	slowConnect(s, 600*time.Millisecond)
	c, err := NewConnector(s.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Connect(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect: %s", errString(err))
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Fatalf("Connect returned after %v, not when ctx ended", d)
	}
	eventually(t, "DISCONNECT of the abandoned session", func() bool {
		return s.Count(message.TypeDisconnectMessage) == 1 && len(s.Connections()) == 0
	})
	ev := s.EventsOf(message.TypeConnectionRequest)
	if len(ev) != 1 || ev[0].Response != message.TypeConnectionResponse {
		t.Fatalf("CONNECTION_REQUEST events %+v", ev)
	}
	checkNoLeaks(t)
}

// TestHandshakeRejectedDisconnects: a session the server created but the
// driver rejects (wrong protocol version, invalid lease) is disconnected.
func TestHandshakeRejectedDisconnects(t *testing.T) {
	for _, c := range []struct {
		name, want string
		setup      func(*quacktest.Server)
	}{
		{"old version", "requires v3 (DuckDB 2.0 or newer)", func(s *quacktest.Server) { s.SetQuackVersion(1) }},
		{"new version", "requires v3", func(s *quacktest.Server) { s.SetQuackVersion(message.QuackVersion + 1) }},
		{"lease overflow", "heartbeat timeout", func(s *quacktest.Server) { s.SetGrantedHeartbeat(9223372037) }},
		{"lease overflow2", "heartbeat timeout", func(s *quacktest.Server) { s.SetGrantedHeartbeat(9223372036854775) }},
		{"lease zero", "heartbeat timeout", func(s *quacktest.Server) { s.SetGrantedHeartbeat(0) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t)
			c.setup(s)
			if _, err := (Driver{}).Open(s.DSN()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if n := s.Count(message.TypeDisconnectMessage); n != 1 || len(s.Connections()) != 0 {
				t.Fatalf("DISCONNECT sent %d times, sessions %v", n, s.Connections())
			}
		})
	}
}

// TestLargestLeaseAccepted: the longest lease a Duration can hold works, and
// a shorter grant than requested is honoured.
func TestLargestLeaseAccepted(t *testing.T) {
	s := newServer(t)
	s.SetGrantedHeartbeat(maxLeaseSeconds)
	c := openConn(t, s)
	if c.serverInfo.Lease <= 0 || heartbeatInterval(c.serverInfo.Lease) <= 0 {
		t.Fatalf("lease %v", c.serverInfo.Lease)
	}
	s.SetGrantedHeartbeat(5)
	if c := openConn(t, s); c.serverInfo.Lease != 5*time.Second {
		t.Fatalf("lease %v, want the granted 5s", c.serverInfo.Lease)
	}
}

// TestHeartbeatRetriesHungRequest: a heartbeat that hangs is abandoned well
// within the lease and retried soon, instead of waiting a whole lease and
// then a whole interval (which lets an idle session expire).
func TestHeartbeatRetriesHungRequest(t *testing.T) {
	s := newServer(t)
	var (
		mu    sync.Mutex
		times []time.Time
	)
	var hung atomic.Bool
	s.SetRawHook(func(_ http.ResponseWriter, _ message.Header, body message.Body) bool {
		if _, ok := body.(message.HeartbeatRequest); !ok {
			return false
		}
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		if hung.CompareAndSwap(false, true) {
			time.Sleep(2500 * time.Millisecond) // the first heartbeat hangs
			panic(http.ErrAbortHandler)
		}
		return false
	})
	fastHeartbeat(t, 50*time.Millisecond)
	c := openConn(t, s, "heartbeat_timeout=3s") // lease 3 s: request timeout 1 s
	eventually3 := time.Now().Add(3 * time.Second)
	for c.heartbeats.Load() == 0 {
		if time.Now().After(eventually3) {
			t.Fatal("no heartbeat succeeded after a hung one")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) < 2 {
		t.Fatalf("%d heartbeats", len(times))
	}
	// Expected about 1 s (the request timeout) plus the retry delay; waiting
	// for the hung request and then a whole interval would take over 3 s.
	if gap := times[1].Sub(times[0]); gap > 2500*time.Millisecond {
		t.Fatalf("retry %v after the hung heartbeat", gap)
	}
	if !c.IsValid() {
		t.Fatal("a hung heartbeat must not break the connection")
	}
}

// TestSlowCancelStaysTracked: a CANCEL the server answers only after the
// synchronous wait (cancelTimeout) is not abandoned: the session's next
// statement is sent after the server answered it, because the server
// interrupts whatever runs on the session when it handles the CANCEL.
func TestSlowCancelStaysTracked(t *testing.T) {
	old := cancelTimeout
	cancelTimeout = 50 * time.Millisecond
	t.Cleanup(func() { cancelTimeout = old })
	s := newServer(t)
	s.Script("SELECT big", intResult(100, 10))
	s.Script("SELECT 1", intResult(1, 1))
	var cancelDone atomic.Int64
	s.SetRawHook(func(_ http.ResponseWriter, _ message.Header, body message.Body) bool {
		if _, ok := body.(message.CancelRequest); ok {
			time.Sleep(1500 * time.Millisecond)
			cancelDone.Store(time.Now().UnixNano())
		}
		return false
	})
	c := openConn(t, s, "inline_rows=10")
	rows, err := c.QueryContext(context.Background(), "SELECT big", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	// Close waits cancelTimeout (50 ms), not the 1.5 s the server takes.
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Rows.Close waited %v for the CANCEL", d)
	}
	if _, err := c.ExecContext(context.Background(), "SELECT 1", nil); err != nil {
		t.Fatal(err)
	}
	next := prepareOf(t, s, "SELECT 1")
	if done := cancelDone.Load(); done == 0 || next.Time.UnixNano() < done {
		t.Fatal("the next statement was sent before the server answered the CANCEL")
	}
	_ = c.Close()
	checkNoLeaks(t)
}

// TestCancelPendingRetriesAfterPrepare: a CANCEL refused because it reached
// the server before the PREPARE installed the statement is sent again when
// the PREPARE answers with a statement that is still running.
func TestCancelPendingRetriesAfterPrepare(t *testing.T) {
	s := newServer(t)
	big := intResult(100, 10)
	big.NeedsMoreFetch = true
	s.Script("SELECT big", big)
	s.SetRawHook(func(_ http.ResponseWriter, _ message.Header, body message.Body) bool {
		if p, ok := body.(message.PrepareRequest); ok && p.SQL == "SELECT big" {
			time.Sleep(300 * time.Millisecond) // authorization, abort of the previous statement
		}
		return false
	})
	c := openConn(t, s, "inline_rows=1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.QueryContext(ctx, "SELECT big", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %s", errString(err))
	}
	eventually(t, "PREPARE", func() bool { return s.Count(message.TypePrepareRequest) == 1 })
	p := prepareOf(t, s, "SELECT big")
	eventually(t, "a CANCEL that applies", func() bool {
		for _, e := range s.EventsOf(message.TypeCancelRequest) {
			if e.QueryUUID == p.QueryUUID && e.Response == message.TypeSuccessResponse {
				return true
			}
		}
		return false
	})
}

// TestAcknowledgeAfterFetchStream: a result read to its end through FETCH
// is acknowledged (a server retaining results for reconnects frees it), and
// so is a large result PREPARE returns whole; small inline results and
// results closed early are not.
func TestAcknowledgeAfterFetchStream(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT big", intResult(30, 10))
	s.Script("SELECT small", intResult(3, 10))
	s.Script("SELECT inline", intResult(ackMinRows, ackMinRows))
	db := openDB(t, s, "inline_rows=10")
	rows, err := db.Query("SELECT big")
	if err != nil {
		t.Fatal(err)
	}
	checkSequence(t, scanInts(t, rows), 30)
	big := prepareOf(t, s, "SELECT big")
	eventually(t, "ACKNOWLEDGEMENT", func() bool {
		for _, e := range s.EventsOf(message.TypeAcknowledgement) {
			if e.QueryUUID == big.QueryUUID && e.ClientQueryID == big.ClientQueryID {
				return e.Response == message.TypeSuccessResponse
			}
		}
		return false
	})
	if _, err := db.Exec("SELECT big"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "ACKNOWLEDGEMENT after Exec", func() bool { return s.Count(message.TypeAcknowledgement) == 2 })

	rows, err = db.Query("SELECT small")
	if err != nil {
		t.Fatal(err)
	}
	checkSequence(t, scanInts(t, rows), 3)
	rows, err = db.Query("SELECT big")
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	_ = rows.Close()
	time.Sleep(50 * time.Millisecond)
	if n := s.Count(message.TypeAcknowledgement); n != 2 {
		t.Fatalf("%d ACKNOWLEDGEMENTs", n)
	}

	// A large result PREPARE returned whole (inline_rows above its size).
	rows, err = db.Query("SELECT inline")
	if err != nil {
		t.Fatal(err)
	}
	checkSequence(t, scanInts(t, rows), ackMinRows)
	inline := prepareOf(t, s, "SELECT inline")
	eventually(t, "ACKNOWLEDGEMENT of an inline result", func() bool {
		for _, e := range s.EventsOf(message.TypeAcknowledgement) {
			if e.QueryUUID == inline.QueryUUID {
				return true
			}
		}
		return false
	})
	if f := fetchesOf(s, inline.QueryUUID); len(f) != 0 {
		t.Fatalf("%d FETCHes for an inline result", len(f))
	}
}

// TestAcknowledgeErrorIgnored: a refused ACKNOWLEDGEMENT does not affect
// the connection.
func TestAcknowledgeErrorIgnored(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT big", intResult(30, 10))
	s.SetRawHook(func(w http.ResponseWriter, _ message.Header, body message.Body) bool {
		if _, ok := body.(message.Acknowledgement); ok {
			quacktest.RawResponse(w, message.Header{}, message.ErrorResponse{
				Message: "Invalid connection id", ExceptionType: "Invalid Input"})
			return true
		}
		return false
	})
	c := openConn(t, s, "inline_rows=10")
	if _, err := c.ExecContext(context.Background(), "SELECT big", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !c.IsValid() || c.gone.Load() {
		t.Fatal("an ACKNOWLEDGEMENT answer must be ignored")
	}
}

// TestTimeZoneParameter: timezone=<name> sets the session's TimeZone right
// after the handshake; a failure fails Connect and ends the session.
func TestTimeZoneParameter(t *testing.T) {
	s := newServer(t)
	s.Script("SET SESSION TimeZone = 'UTC'", quacktest.Success())
	s.Script("SET SESSION TimeZone = 'It''s/Bad'", quacktest.Result{Error: &message.ErrorResponse{
		Message: "Unknown TimeZone 'It's/Bad'", ExceptionType: "Invalid Input"}})
	db := openDB(t, s, "timezone=UTC")
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	if e := prepareOf(t, s, "SET SESSION TimeZone = 'UTC'"); e.ConnectionID == "" {
		t.Fatal("no session")
	}

	_, err := Driver{}.Open(s.DSN("timezone=It's/Bad"))
	if err == nil || !strings.Contains(err.Error(), "timezone=It's/Bad") || !strings.Contains(err.Error(), "Unknown TimeZone") {
		t.Fatalf("got %v", err)
	}
	bad := prepareOf(t, s, "SET SESSION TimeZone = 'It''s/Bad'")
	eventually(t, "DISCONNECT", func() bool { return !slices.Contains(s.Connections(), bad.ConnectionID) })
}
