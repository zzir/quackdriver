//go:build integration

package network

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/livetest"
)

// readAll runs query on c and returns the number of rows.
func readAll(t *testing.T, c *sql.Conn, query string) int {
	t.Helper()
	rows, err := c.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestConnectionReuse counts the TCP connections one session opens while
// statements, FETCH read-ahead and heartbeats run: keep-alive must reuse
// them instead of dialing per request.
func TestConnectionReuse(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprint("tls=", useTLS), func(t *testing.T) {
			t.Parallel()
			var srv *livetest.Server
			params := []string{"heartbeat_timeout=1s"}
			if useTLS {
				ts := livetest.StartTLSServer(t, "127.0.0.1", "")
				srv, params = ts.Server, append(params, "ssl_fingerprint="+ts.Fingerprint)
			} else {
				srv = livetest.StartServer(t, livetest.ServerOptions{})
			}
			fwd := newCounter(t, srv.Addr())
			db := livetest.OpenDB(t, dsn("127.0.0.1", fwd.Port(), srv.Token, append(params, "fetch_ahead=4", "inline_rows=0")...))
			c := livetest.Conn(t, db)
			livetest.Exec(t, c, "SET quack_target_batch_bytes = 16384")

			// Busy phase: multi-batch results with read-ahead while the
			// heartbeat ticks every ~330 ms. The warm-up statement may dial
			// once per waiting request (net/http does not abandon a dial
			// when another connection serves the request first, which shows
			// while TLS handshakes are slow); those connections stay pooled.
			const total = 30000
			query := fmt.Sprintf("SELECT i, 'v' || i FROM range(%d) t(i)", total)
			if n := readAll(t, c, query); n != total {
				t.Fatalf("read %d rows, want %d", n, total)
			}
			warm := fwd.accepted.Load()
			statements := 0
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); statements++ {
				if n := readAll(t, c, query); n != total {
					t.Fatalf("read %d rows, want %d", n, total)
				}
			}
			busy, busyReqs := fwd.accepted.Load(), fwd.requests.Load()
			// The pool holds a connection for the statement, the heartbeat
			// and each read-ahead request; allow two more for dials racing
			// the return of a busy connection. Cancelling in-flight requests
			// (e.g. read-ahead past the end of a result) would cost
			// connections in proportion to the statements instead.
			if busy > 2+4+1 && busy-warm > 2 {
				t.Fatalf("%d statements opened %d TCP connections after a warm-up statement that opened %d", statements, busy-warm, warm)
			}
			if !useTLS && busyReqs < int64(5*statements) {
				t.Fatalf("only %d requests for %d multi-batch statements", busyReqs, statements)
			}

			// Idle phase: heartbeats alone must not dial.
			time.Sleep(2500 * time.Millisecond)
			if n := fwd.accepted.Load(); n != busy {
				t.Fatalf("heartbeats of an idle session opened %d new TCP connections", n-busy)
			}
			if !useTLS && fwd.requests.Load() < busyReqs+4 {
				t.Fatalf("only %d heartbeats in 2.5s with a 1s lease", fwd.requests.Load()-busyReqs)
			}

			// A pool running short statements on default settings reuses the
			// handshake's connection for the statements and a second one for
			// the heartbeat (plus one racing dial).
			db2 := livetest.OpenDB(t, dsn("127.0.0.1", fwd.Port(), srv.Token, params...))
			for range 50 {
				livetest.Scalar[int64](t, db2, "SELECT count(*) FROM range(10)")
			}
			if n := fwd.accepted.Load(); n > busy+3 {
				t.Fatalf("50 pool statements opened %d new TCP connections", n-busy)
			}

			// Closing the pools releases the idle connections.
			_ = c.Close()
			_ = db.Close()
			_ = db2.Close()
			livetest.Eventually(t, 5*time.Second, "TCP connections to close", func() bool { return fwd.open.Load() == 0 })
		})
	}
}

// TestReadAheadEndKeepsConnections: requests the read-ahead issued past the
// end of a result must not cost a TCP connection each (cancelling an
// in-flight HTTP request closes its connection).
func TestReadAheadEndKeepsConnections(t *testing.T) {
	t.Parallel()
	srv := livetest.StartServer(t, livetest.ServerOptions{})
	fwd := newCounter(t, srv.Addr())
	c := livetest.Conn(t, livetest.OpenDB(t, dsn("127.0.0.1", fwd.Port(), srv.Token, "fetch_ahead=4", "inline_rows=0", "heartbeat_timeout=60s")))
	for range 3 { // warm up the connection pool
		livetest.Scalar[int64](t, c, "SELECT 1")
	}
	before := fwd.accepted.Load()
	for range 20 {
		livetest.Scalar[int64](t, c, "SELECT 1")
		livetest.Exec(t, c, "SELECT 1")
	}
	if opened := fwd.accepted.Load() - before; opened > 2 {
		t.Fatalf("40 single-row statements with fetch_ahead=4 opened %d new TCP connections", opened)
	}
}

// TestRequestTimeout: timeout= bounds every HTTP request, so a statement
// that runs longer fails after about that long, naming the DSN timeout; the
// server stops the statement and the session stays usable.
func TestRequestTimeout(t *testing.T) {
	t.Parallel()
	srv := livetest.StartServer(t, livetest.ServerOptions{})
	db := livetest.OpenDB(t, srv.DSN("timeout=1s", "heartbeat_timeout=2s"))
	c := livetest.Conn(t, db)
	if got := livetest.Scalar[int64](t, c, "SELECT count(*) FROM range(1000000)"); got != 1000000 {
		t.Fatal(got)
	}
	var err error
	el := timed(func() {
		err = c.QueryRowContext(context.Background(),
			"SELECT count(*) FROM range(60000) a(i), range(60000) b(j) WHERE (i * 7 + j * 13) % 1000003 = 5").Scan(new(int64))
	})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timeout=1s") {
		t.Fatalf("a statement longer than timeout=1s: %v", err)
	}
	if el < 900*time.Millisecond || el > 3*time.Second {
		t.Fatalf("the statement failed after %v, want about 1s", el)
	}
	if got := livetest.Scalar[string](t, c, "SELECT 'after'"); got != "after" {
		t.Fatal(got)
	}
	livetest.Eventually(t, 10*time.Second, "the timed-out statement to stop", func() bool {
		return livetest.Scalar[int64](t, db, "SELECT count(*) FROM quack_active_connections() WHERE query LIKE '%1000003%' AND query NOT LIKE '%quack_active_connections%'") == 0
	})
}
