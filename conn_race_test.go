package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/message"
)

// TestCloseSendsDisconnect closes a connection from several goroutines at
// once (run with -race): exactly one DISCONNECT ends the session.
func TestCloseSendsDisconnect(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Close()
			_ = c.IsValid()
		}()
	}
	wg.Wait()
	if n := s.Count(message.TypeDisconnectMessage); n != 1 || slices.Contains(s.Connections(), c.connectionID) {
		t.Fatalf("DISCONNECT sent %d times, sessions %v", n, s.Connections())
	}
	if _, err := c.ExecContext(context.Background(), "SELECT 1", nil); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Exec after Close: %v", err)
	}
	if _, err := c.PrepareContext(context.Background(), "SELECT 1"); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Prepare after Close: %v", err)
	}
	checkNoLeaks(t)
}

// TestCloseRacesRowsNext: Conn.Close may be called from another goroutine
// while Rows.Next ends the stream (run with -race).
func TestCloseRacesRowsNext(t *testing.T) {
	s := newServer(t)
	res := intResult(300, 10)
	res.BeforeFetch = func(ctx context.Context, _ uint64) {
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
		}
	}
	s.Script("SELECT big", res)
	for i := range 40 {
		conn, err := Driver{}.Open(s.DSN("inline_rows=100", "fetch_ahead=0"))
		if err != nil {
			t.Fatal(err)
		}
		c := conn.(*Conn)
		ctx, cancel := context.WithCancel(context.Background())
		r, err := c.QueryContext(ctx, "SELECT big", nil)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			dest := make([]driver.Value, 1)
			for {
				if r.Next(dest) != nil {
					return
				}
			}
		}()
		time.Sleep(time.Duration(500+i*37) * time.Microsecond)
		cancel()
		_ = c.Close()
		<-done
		_ = r.Close()
	}
}
