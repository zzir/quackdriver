//go:build integration

package protocol

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/livetest"
)

// TestServerRestartReconnects restarts the server on the same port: a
// dedicated connection reports driver.ErrBadConn, and database/sql replaces
// the pooled connections transparently. While the server is down,
// statements fail instead of hanging.
func TestServerRestartReconnects(t *testing.T) {
	port := livetest.FreePort(t)
	srv := livetest.StartServer(t, livetest.ServerOptions{Port: port})
	db := livetest.OpenDB(t, srv.DSN("heartbeat_timeout=60s"))
	db.SetMaxIdleConns(4)
	c := livetest.Conn(t, db)
	for range 3 { // several idle pooled connections
		livetest.Exec(t, livetest.Conn(t, db), "SELECT 1")
	}
	scalar(t, c, "SELECT 1")
	srv.Stop()
	srv = livetest.StartServer(t, livetest.ServerOptions{Port: port})
	if _, err := c.ExecContext(context.Background(), "SELECT 1"); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("dedicated conn after restart: %v, want driver.ErrBadConn", err)
	}
	for i := range 10 {
		if v := scalar(t, db, "SELECT ?::BIGINT", i); v != int64(i) {
			t.Fatalf("after restart: %d", v)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	srv.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	within(t, 9*time.Second, "statement against a stopped server", func() {
		if _, err := db.ExecContext(ctx, "SELECT 1"); err == nil {
			t.Fatal("statement succeeded with the server down")
		}
	})
	livetest.StartServer(t, livetest.ServerOptions{Port: port})
	scalar(t, db, "SELECT 2")
}

// TestServerDiesMidResult kills the server while a result is streamed: the
// reader gets an error instead of hanging or a silently short result.
func TestServerDiesMidResult(t *testing.T) {
	for _, fa := range []int{0, 4} {
		t.Run(fmt.Sprintf("fetch_ahead=%d", fa), func(t *testing.T) {
			srv, db := newDB(t, "inline_rows=10", fmt.Sprintf("fetch_ahead=%d", fa))
			c := smallBatches(t, db)
			rows, err := c.QueryContext(context.Background(), "SELECT i FROM range(10000000) t(i)")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				if n++; n == 1000 {
					srv.Stop()
				}
			}
			if rows.Err() == nil || n >= 10000000 {
				t.Fatalf("no error after %d rows", n)
			}
			within(t, 5*time.Second, "Rows.Close", func() { _ = rows.Close() })
			if _, err := c.ExecContext(context.Background(), "SELECT 1"); err == nil {
				t.Fatal("statement on a conn whose server died succeeded")
			}
		})
	}
}
