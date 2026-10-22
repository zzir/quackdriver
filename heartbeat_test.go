package quackdriver

import (
	"context"
	"database/sql"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/quacktest"
)

// TestHeartbeatTimings checks the interval (a third of the lease, ±20%) and
// the per-request bound derived from the lease.
func TestHeartbeatTimings(t *testing.T) {
	for range 1000 {
		if d := heartbeatInterval(time.Minute); d < 16*time.Second || d > 24*time.Second {
			t.Fatalf("interval %v outside lease/3 ±20%%", d)
		}
	}
	if d := heartbeatInterval(-time.Second); d <= 0 {
		t.Errorf("interval for a negative lease %v", d)
	}
	for _, c := range []struct{ lease, want time.Duration }{
		{time.Second, time.Second / 3},
		{3 * time.Second, time.Second},
		{60 * time.Second, maxHeartbeatRequestTimeout},
		{time.Duration(maxLeaseSeconds) * time.Second, maxHeartbeatRequestTimeout},
	} {
		if got := heartbeatRequestTimeout(c.lease); got != c.want {
			t.Errorf("lease %v: timeout %v, want %v", c.lease, got, c.want)
		}
	}
	fastHeartbeat(t, time.Millisecond)
	if heartbeatInterval(time.Minute) != time.Millisecond {
		t.Fatal("override ignored")
	}
}

// TestHeartbeatKeepsBusyConnAlive runs a statement whose PREPARE takes
// several leases: the concurrent heartbeat keeps the session alive so the
// FETCH afterwards succeeds; without heartbeats the lease lapses.
func TestHeartbeatKeepsBusyConnAlive(t *testing.T) {
	for _, c := range []struct {
		interval time.Duration
		alive    bool
	}{{8 * time.Millisecond, true}, {time.Hour, false}} {
		s := newServer(t)
		s.SetLeaseUnit(40 * time.Millisecond) // heartbeat_timeout=1s lasts 40ms
		fastHeartbeat(t, c.interval)
		gate := make(chan struct{})
		time.AfterFunc(250*time.Millisecond, func() { close(gate) })
		res := intResult(6, 3)
		res.PrepareGate = gate
		s.Script("SELECT slow", res)
		db := openDB(t, s, "heartbeat_timeout=1s", "inline_rows=0")
		rows, err := db.Query("SELECT slow")
		if err != nil {
			t.Fatal(err)
		}
		got := scanInts(t, rows)
		if err := rows.Err(); (err == nil) != c.alive {
			t.Fatalf("interval %v: %v", c.interval, err)
		}
		if c.alive {
			checkSequence(t, got, 6)
			if n := s.Count(message.TypeHeartbeatRequest); n < 5 {
				t.Fatalf("only %d heartbeats during a 250ms statement", n)
			}
		}
	}
}

// TestHeartbeatSessionGone: an answer meaning the session is gone (unknown
// id, or must_invalidate) marks the connection bad and ends the loop.
func TestHeartbeatSessionGone(t *testing.T) {
	for name, kill := range map[string]func(*quacktest.Server){
		"dropped": func(s *quacktest.Server) { s.DropConnections() },
		"must_invalidate": func(s *quacktest.Server) {
			s.SetRawHook(func(w http.ResponseWriter, h message.Header, b message.Body) bool {
				if _, ok := b.(message.HeartbeatRequest); !ok {
					return false
				}
				quacktest.RawResponse(w, message.Header{}, message.ErrorResponse{Message: "dying", MustInvalidate: true})
				return true
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t)
			fastHeartbeat(t, 5*time.Millisecond)
			c := openConn(t, s)
			eventually(t, "a heartbeat", func() bool { return c.heartbeats.Load() > 0 })
			kill(s)
			eventually(t, "the conn to turn bad", func() bool { return !c.IsValid() })
			select {
			case <-c.hbDone:
			case <-time.After(2 * time.Second):
				t.Fatal("heartbeat loop kept running on a gone session")
			}
		})
	}
}

func TestHeartbeatSurvivesTransientFailures(t *testing.T) {
	s := newServer(t)
	fastHeartbeat(t, 5*time.Millisecond)
	var failures atomic.Int32
	s.SetRawHook(func(w http.ResponseWriter, h message.Header, b message.Body) bool {
		if _, ok := b.(message.HeartbeatRequest); !ok {
			return false
		}
		switch failures.Add(1) {
		case 1:
			http.Error(w, "proxy hiccup", http.StatusBadGateway)
			return true
		case 2:
			quacktest.RawResponse(w, message.Header{}, message.ErrorResponse{Message: "some other error"})
			return true
		case 3:
			_, _ = w.Write([]byte("garbage"))
			return true
		}
		return false
	})
	c := openConn(t, s)
	eventually(t, "heartbeats after failures", func() bool { return c.heartbeats.Load() >= 2 })
	if !c.IsValid() {
		t.Fatal("transient heartbeat failures must not break the conn")
	}
}

func TestHeartbeatStopsOnClose(t *testing.T) {
	s := newServer(t)
	fastHeartbeat(t, 2*time.Millisecond)
	db := openDB(t, s)
	db.SetMaxIdleConns(4)
	var conns []*sql.Conn
	for range 4 {
		sc, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, sc)
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(driverGoroutines()); n < 4 {
		t.Fatalf("expected 4 heartbeat goroutines, found %d", n)
	}
	for _, sc := range conns {
		_ = sc.Close()
	}
	checkNoLeaksAfter(t, db)
	if n := s.Count(message.TypeDisconnectMessage); n != 4 {
		t.Fatalf("%d DISCONNECTs", n)
	}
}

// TestHeartbeatGivesUpAfterLease: when no answer arrives from the server for
// a whole lease (every heartbeat fails in transit), the session has expired
// there; the connection is marked gone and the loop ends instead of dialing
// an unreachable server forever. No DISCONNECT is sent for a gone session.
func TestHeartbeatGivesUpAfterLease(t *testing.T) {
	s := newServer(t)
	var attempts atomic.Int32 // counted here: an aborted request is not recorded as an event
	s.SetRawHook(func(_ http.ResponseWriter, _ message.Header, b message.Body) bool {
		if _, ok := b.(message.HeartbeatRequest); ok {
			attempts.Add(1)
			panic(http.ErrAbortHandler) // the answer never arrives
		}
		return false
	})
	fastHeartbeat(t, 20*time.Millisecond)
	c := openConn(t, s, "heartbeat_timeout=1s")
	start := time.Now()
	deadline := start.Add(3 * time.Second)
	for c.IsValid() {
		if time.Now().After(deadline) {
			t.Fatal("connection still valid two leases after the server stopped answering")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if el := time.Since(start); el < 900*time.Millisecond {
		t.Fatalf("gave up after %v, before the 1s lease could have expired", el)
	}
	if !c.gone.Load() {
		t.Fatal("an expired lease must mark the session gone")
	}
	select {
	case <-c.hbDone:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat loop kept running")
	}
	if n := attempts.Load(); n < 10 {
		t.Fatalf("only %d heartbeat attempts before giving up", n)
	}
	_ = c.Close()
	if n := s.Count(message.TypeDisconnectMessage); n != 0 {
		t.Fatalf("%d DISCONNECTs for a session the server expired", n)
	}
}
