//go:build integration

package protocol

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/livetest"
)

// slowSQL is an aggregate over an n×n cross join: it produces no row until
// it finishes (about 1e8 pairs per second).
func slowSQL(n int) string {
	return fmt.Sprintf("SELECT count(*) AS c FROM range(%d) a(i), range(%d) b(j) WHERE (i * 7 + j * 13) %% 1000003 = 5", n, n)
}

// TestLongStatementOutlivesLease runs one statement that takes several
// leases: the heartbeat runs concurrently, so the session survives.
func TestLongStatementOutlivesLease(t *testing.T) {
	t.Parallel()
	_, db := newDB(t, "heartbeat_timeout=1s")
	c := livetest.Conn(t, db)
	id := sessionID(t, c)
	const n = 20000
	var want int64
	for i := range n {
		for j := range n {
			if (i*7+j*13)%1000003 == 5 {
				want++
			}
		}
	}
	start := time.Now()
	if got := scalar(t, c, slowSQL(n)); got != want {
		t.Fatalf("count %d, want %d", got, want)
	}
	t.Logf("statement took %v with a 1s lease", time.Since(start))
	if got := sessionID(t, c); got != id {
		t.Fatalf("session changed from %s to %s: the lease expired during the statement", id, got)
	}
}

// TestSlowConsumer sleeps longer than the lease between Rows.Next calls,
// across batch boundaries.
func TestSlowConsumer(t *testing.T) {
	for _, fa := range []int{0, 1, 8} {
		t.Run(fmt.Sprintf("fetch_ahead=%d", fa), func(t *testing.T) {
			t.Parallel()
			srv := livetest.StartServer(t, livetest.ServerOptions{})
			db, rec := recordingDB(t, srv.DSN("heartbeat_timeout=1s", "inline_rows=100", fmt.Sprintf("fetch_ahead=%d", fa)))
			c := smallBatches(t, db)
			id := sessionID(t, c)
			const total = 50000
			rows, err := c.QueryContext(context.Background(), fmt.Sprintf("SELECT i, 'v' || i FROM range(%d) t(i)", total))
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			before := rec.Mark()
			n := int64(0)
			for ; rows.Next(); n++ {
				var i int64
				var s string
				if err := rows.Scan(&i, &s); err != nil || i != n || s != fmt.Sprintf("v%d", n) {
					t.Fatalf("row %d: got (%d, %q), %v", n, i, s, err)
				}
				if n == 50 || n == 20000 {
					time.Sleep(3500 * time.Millisecond)
				}
			}
			if err := rows.Err(); err != nil || n != total {
				t.Fatalf("%d rows: %v", n, err)
			}
			if r := rec.Mark() - before; r < 20 {
				t.Fatalf("only %d requests: the result did not span batches", r)
			}
			if got := sessionID(t, c); got != id {
				t.Fatalf("session changed")
			}
		})
	}
}

// TestCancelDuringStatement lets a context end while PREPARE waits for a
// slow aggregate, or (with inline_rows=0) while a FETCH waits: the driver
// sends CANCEL, the server stops the statement, and the connection is
// immediately usable.
func TestCancelDuringStatement(t *testing.T) {
	for _, inline := range []string{"inline_rows=2048", "inline_rows=0"} {
		t.Run(inline, func(t *testing.T) {
			t.Parallel()
			srv, mon := newDB(t)
			c := livetest.Conn(t, livetest.OpenDB(t, srv.DSN(inline)))
			id := sessionID(t, c)
			for _, mode := range []string{"query", "exec", "cancel"} {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				want := context.DeadlineExceeded
				if mode == "cancel" {
					ctx, cancel = context.WithCancel(context.Background())
					time.AfterFunc(300*time.Millisecond, cancel)
					want = context.Canceled
				}
				start := time.Now()
				var err error
				if mode == "exec" {
					_, err = c.ExecContext(ctx, slowSQL(200000))
				} else if rows, qerr := c.QueryContext(ctx, slowSQL(200000)); qerr != nil {
					err = qerr
				} else {
					// With inline_rows=0, PREPARE answers once the statement
					// is bound, so the cancel hits the FETCH.
					if el := time.Since(start); inline == "inline_rows=0" && el > 250*time.Millisecond {
						t.Fatalf("PREPARE with inline_rows=0 took %v", el)
					}
					rows.Next()
					err = rows.Err()
					_ = rows.Close()
				}
				cancel()
				if !errors.Is(err, want) {
					t.Fatalf("%s: err = %v, want %v", mode, err, want)
				}
				if el := time.Since(start); el > 2500*time.Millisecond {
					t.Fatalf("%s: returned after %v", mode, el)
				}
				waitCancelled(t, mon, id)
				within(t, 3*time.Second, "follow-up statement", func() { scalar(t, c, "SELECT 42") })
			}
		})
	}
}

// TestCancelThenStatement cancels statements at random points and runs the
// next statement on the same session right away. The server handles every
// request on its own thread, so a late CANCEL or a late (aborted) PREPARE
// must not hit the next statement ("Interrupted!", "superseded by a new
// query").
func TestCancelThenStatement(t *testing.T) {
	_, db := newDB(t, "inline_rows=100")
	c := livetest.Conn(t, db)
	livetest.Exec(t, c, "CREATE TABLE protocol_cts (i BIGINT)")
	var inserted int64
	for it := range 300 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(it%7)*300*time.Microsecond)
		if it%2 == 0 {
			if rows, err := c.QueryContext(ctx, "SELECT i, i::VARCHAR FROM range(3000000) t(i)"); err == nil {
				for rows.Next() {
				}
				_ = rows.Close()
			}
		} else {
			_, _ = c.ExecContext(ctx, slowSQL(3000))
		}
		cancel()
		if it%3 != 0 {
			if v := scalar(t, c, "SELECT ?::BIGINT", it); v != int64(it) {
				t.Fatalf("iteration %d: got %d", it, v)
			}
			continue
		}
		tx, err := c.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatalf("iteration %d: BEGIN: %v", it, err)
		}
		livetest.Exec(t, tx, "INSERT INTO protocol_cts VALUES (?)", it)
		if err := tx.Commit(); err != nil {
			t.Fatalf("iteration %d: COMMIT: %v", it, err)
		}
		inserted++
	}
	if n := scalar(t, c, "SELECT count(*) FROM protocol_cts"); n != inserted {
		t.Fatalf("%d rows, want %d", n, inserted)
	}
}
