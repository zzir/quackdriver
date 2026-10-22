//go:build integration

package protocol

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/livetest"
)

// TestConcurrentDB hammers one sql.DB from 64 goroutines with queries of
// different sizes, writes, transactions, early Rows.Close, cancelled
// contexts and pings.
func TestConcurrentDB(t *testing.T) {
	_, db := newDB(t, "heartbeat_timeout=3s", "inline_rows=50")
	db.SetMaxOpenConns(24)
	livetest.Exec(t, db, "CREATE TABLE protocol_conc (g INTEGER, i INTEGER)")
	const workers = 64
	const iterations = 25
	var wg sync.WaitGroup
	errc := make(chan error, workers)
	inserted := make([]int, workers)
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			fail := func(format string, args ...any) {
				errc <- fmt.Errorf("worker %d: "+format, append([]any{w}, args...)...)
			}
			for it := range iterations {
				switch (w + it) % 7 {
				case 0: // a multi-batch result read to the end
					n := 3000 + w*10
					rows, err := db.Query(fmt.Sprintf("SELECT i, i::VARCHAR || '-%d' FROM range(%d) t(i)", w, n))
					if err != nil {
						fail("query: %v", err)
						return
					}
					k := 0
					for rows.Next() {
						var i int64
						var s string
						if err := rows.Scan(&i, &s); err != nil {
							fail("scan: %v", err)
							return
						}
						if i != int64(k) || s != fmt.Sprintf("%d-%d", k, w) {
							fail("row %d: %d %q", k, i, s)
							return
						}
						k++
					}
					if err := rows.Err(); err != nil || k != n {
						fail("rows: %d of %d: %v", k, n, err)
						return
					}
				case 1: // insert
					res, err := db.Exec("INSERT INTO protocol_conc SELECT ?, i FROM range(?) t(i)", w, it+1)
					if err != nil {
						fail("insert: %v", err)
						return
					}
					if n, _ := res.RowsAffected(); n != int64(it+1) {
						fail("RowsAffected %d", n)
						return
					}
					inserted[w] += it + 1
				case 2: // transaction, rolled back half of the time
					tx, err := db.Begin()
					if err != nil {
						fail("begin: %v", err)
						return
					}
					if _, err := tx.Exec("INSERT INTO protocol_conc VALUES (?, -1)", w); err != nil {
						fail("tx insert: %v", err)
						return
					}
					var c int64
					if err := tx.QueryRow("SELECT count(*) FROM protocol_conc WHERE g = ? AND i = -1", w).Scan(&c); err != nil {
						fail("tx select: %v", err)
						return
					}
					if it%2 == 0 {
						err = tx.Rollback()
					} else {
						err = tx.Commit()
						inserted[w]++
					}
					if err != nil {
						fail("end tx: %v", err)
						return
					}
				case 3: // close early
					rows, err := db.Query("SELECT i, i * 2 FROM range(1000000) t(i)")
					if err != nil {
						fail("query: %v", err)
						return
					}
					for range 10 {
						if !rows.Next() {
							break
						}
					}
					if err := rows.Close(); err != nil {
						fail("close: %v", err)
						return
					}
				case 4: // a context that ends mid-statement
					ctx, cancel := context.WithTimeout(context.Background(), time.Duration(1+w%5)*time.Millisecond)
					rows, err := db.QueryContext(ctx, "SELECT i, i::VARCHAR FROM range(2000000) t(i)")
					if err == nil {
						for rows.Next() {
						}
						err = rows.Err()
						_ = rows.Close()
					}
					cancel()
					if err != nil && !errors.Is(err, context.DeadlineExceeded) {
						fail("cancelled query: %v", err)
						return
					}
				case 5: // errors keep the pool healthy
					if _, err := db.Exec("SELECT * FROM protocol_conc_missing"); err == nil {
						fail("missing table did not fail")
						return
					}
				case 6:
					if err := db.Ping(); err != nil {
						fail("ping: %v", err)
						return
					}
					var v int64
					if err := db.QueryRow("SELECT ?::BIGINT * 2", w).Scan(&v); err != nil || v != int64(2*w) {
						fail("scalar: %d %v", v, err)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	for w := range workers {
		if n := scalar(t, db, "SELECT count(*) FROM protocol_conc WHERE g = ?", w); n != int64(inserted[w]) {
			t.Errorf("worker %d: %d rows, want %d", w, n, inserted[w])
		}
	}
	if st := db.Stats(); st.OpenConnections > 24 {
		t.Errorf("open connections %d", st.OpenConnections)
	}
}
