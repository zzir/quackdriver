package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/quacktest"
)

// TestStreamEnds: a result PREPARE returns whole needs no FETCH; a PREPARE
// that inlined every batch but reported needs_more_fetch gets a terminal
// first FETCH; a terminal response may omit total_batches.
func TestStreamEnds(t *testing.T) {
	for _, c := range []struct {
		name    string
		mod     func(*quacktest.Result)
		params  []string
		fetches int
	}{
		{"inline", func(*quacktest.Result) {}, nil, 0},
		{"needs_more_fetch", func(r *quacktest.Result) { r.NeedsMoreFetch = true }, nil, 1},
		{"no total", func(r *quacktest.Result) { r.OmitTotal = true }, []string{"inline_rows=0"}, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t)
			res := intResult(9, 3)
			c.mod(&res)
			s.Script("SELECT x", res)
			rows, err := openDB(t, s, c.params...).Query("SELECT x")
			if err != nil {
				t.Fatal(err)
			}
			checkSequence(t, scanInts(t, rows), 9)
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			p := prepareOf(t, s, "SELECT x")
			if p.QueryUUID == (codec.HugeInt{}) || !p.HasQueryID || p.InlineRows.Valid != (c.params != nil) {
				t.Fatalf("PREPARE %+v", p)
			}
			if f := fetchesOf(s, p.QueryUUID); len(f) != c.fetches || len(f) > 0 && f[len(f)-1].Response != message.TypeFetchResponse {
				t.Fatalf("FETCHes %+v", f)
			}
		})
	}
}

// TestQueryBatchIndices checks the FETCH sequence without read-ahead:
// indices 1, 2, ... after the inline batches, ack = index-1, a terminal
// response with total_batches, and the same uuid and client_query_id as the
// PREPARE.
func TestQueryBatchIndices(t *testing.T) {
	s := newServer(t)
	// 10 batches of 3 rows; inline_rows=5 inlines the first two (3, then 6 >= 5).
	s.Script("SELECT big", intResult(30, 3))
	db := openDB(t, s, "inline_rows=5", "fetch_ahead=0")
	rows, err := db.Query("SELECT big")
	if err != nil {
		t.Fatal(err)
	}
	checkSequence(t, scanInts(t, rows), 30)
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	p := prepareOf(t, s, "SELECT big")
	if !p.InlineRows.Valid || p.InlineRows.Value != 5 {
		t.Fatalf("inline_rows %+v", p.InlineRows)
	}
	fetches := fetchesOf(s, p.QueryUUID)
	if len(fetches) != 9 { // batches 3..10 are FETCH 1..8, then terminal 9
		t.Fatalf("%d FETCHes, want 9", len(fetches))
	}
	for i, f := range fetches {
		want := uint64(i + 1)
		if f.BatchIndex != want || f.AckIndex != want-1 {
			t.Fatalf("FETCH %d: index %d ack %d", i, f.BatchIndex, f.AckIndex)
		}
		if f.ClientQueryID != p.ClientQueryID || !f.HasQueryID || f.ConnectionID != p.ConnectionID {
			t.Fatalf("FETCH header %+v vs PREPARE %+v", f, p)
		}
		if f.Error != "" {
			t.Fatalf("FETCH %d failed: %s", i, f.Error)
		}
	}
}

// TestQueryReadAhead runs several FETCHes concurrently with random server
// delays; rows must still arrive in order, acks must never cover an index
// still being requested, and the server must see overlapping requests.
func TestQueryReadAhead(t *testing.T) {
	s := newServer(t)
	res := intResult(200, 5)
	var inFlight, maxInFlight atomic.Int32
	res.BeforeFetch = func(ctx context.Context, index uint64) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
	}
	s.Script("SELECT ahead", res)
	db := openDB(t, s, "inline_rows=1", "fetch_ahead=4")
	rows, err := db.Query("SELECT ahead")
	if err != nil {
		t.Fatal(err)
	}
	checkSequence(t, scanInts(t, rows), 200)
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if maxInFlight.Load() < 2 {
		t.Fatalf("read-ahead never overlapped requests (max %d)", maxInFlight.Load())
	}
	p := prepareOf(t, s, "SELECT ahead")
	seen := map[uint64]bool{}
	for _, f := range fetchesOf(s, p.QueryUUID) {
		if f.AckIndex >= f.BatchIndex {
			t.Fatalf("FETCH %d acknowledged %d", f.BatchIndex, f.AckIndex)
		}
		if seen[f.BatchIndex] {
			t.Fatalf("batch %d requested twice", f.BatchIndex)
		}
		seen[f.BatchIndex] = true
		if f.Error != "" {
			t.Fatalf("FETCH %d: %s", f.BatchIndex, f.Error)
		}
	}
	for i := uint64(1); i <= 40; i++ { // 40 batches, the first one inline
		if i < 40 && !seen[i] {
			t.Fatalf("batch %d never requested", i)
		}
	}
	checkNoLeaksAfter(t, db)
}

// checkNoLeaksAfter closes db and checks for leaked goroutines.
func checkNoLeaksAfter(t *testing.T, db interface{ Close() error }) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	checkNoLeaks(t)
}

// TestFetchRetries: a FETCH whose response is lost is retried with the same
// index and ack (the server keeps an unacknowledged batch); once the retries
// are exhausted the error is returned, without ErrBadConn, since
// database/sql would re-run the statement.
func TestFetchRetries(t *testing.T) {
	for _, drops := range []int{2, 10} {
		s := newServer(t)
		res := intResult(12, 3)
		res.DropFetchResponse = map[uint64]int{2: drops}
		s.Script("SELECT flaky", res)
		db := openDB(t, s, "inline_rows=0", "fetch_ahead=0")
		rows, err := db.Query("SELECT flaky")
		if err != nil {
			t.Fatal(err)
		}
		got := scanInts(t, rows)
		recovered := drops <= fetchRetries
		if err := rows.Err(); (err == nil) != recovered || errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("%d drops: rows %v, err %v", drops, got, err)
		}
		if recovered {
			checkSequence(t, got, 12)
		} else {
			checkSequence(t, got, 3) // the batches before the failure
		}
		p := prepareOf(t, s, "SELECT flaky")
		var twos int
		for _, f := range fetchesOf(s, p.QueryUUID) {
			if f.BatchIndex == 2 {
				twos++
				if f.AckIndex != 1 {
					t.Fatalf("retry changed the ack: %d", f.AckIndex)
				}
			}
		}
		if want := min(drops, fetchRetries) + 1; twos != want {
			t.Fatalf("batch 2 requested %d times, want %d", twos, want)
		}
		if recovered {
			rawConn(t, db, func(c *Conn) {
				if !c.IsValid() {
					t.Fatal("a recovered FETCH must not mark the connection bad")
				}
			})
		}
	}
}

func TestFetchProtocolViolations(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*quacktest.Result)
		want string
	}{
		{"wrong batch index", func(r *quacktest.Result) { r.WrongBatchIndex = map[uint64]uint64{2: 3} },
			"FETCH_RESPONSE carries batch 3 but batch 2 was requested"},
		{"short stream", func(r *quacktest.Result) { n := uint64(9); r.TotalOverride = &n },
			"Quack stream ended with 4 of 9 batches received"},
		{"server error", func(r *quacktest.Result) {
			r.FetchErrors = map[uint64]*message.ErrorResponse{3: {Message: "failed to allocate", ExceptionType: "Out of Memory"}}
		}, "Out of Memory Error: failed to allocate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t)
			res := intResult(12, 3)
			c.mod(&res)
			s.Script("SELECT x", res)
			db := openDB(t, s, "inline_rows=0")
			rows, err := db.Query("SELECT x")
			if err != nil {
				t.Fatal(err)
			}
			scanInts(t, rows)
			if err := rows.Err(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

func TestFetcherMarkReceived(t *testing.T) {
	f := (&query{}).newFetcher(0)
	for _, i := range []uint64{2, 4, 1} {
		f.markReceived(i)
	}
	if f.currentAck() != 2 {
		t.Fatalf("ack %d, want 2", f.currentAck())
	}
	f.markReceived(3)
	if f.currentAck() != 4 || len(f.received) != 0 {
		t.Fatalf("ack %d, pending %v", f.currentAck(), f.received)
	}
	f.stop(nil)
	f.stop(nil) // idempotent
}

// TestReadAheadEndKeepsRequests checks that the read-ahead FETCHes issued
// past the end of a result are answered rather than cancelled: cancelling an
// in-flight HTTP request closes its pooled connection, so every statement
// read with read-ahead would dial again.
func TestReadAheadEndKeepsRequests(t *testing.T) {
	s := newServer(t)
	res := intResult(20, 5) // 4 batches: 1 inline, FETCH 1-3, terminal 4
	var pastEnd, cancelled atomic.Int32
	res.BeforeFetch = func(ctx context.Context, index uint64) {
		if index <= 4 {
			return
		}
		pastEnd.Add(1)
		select {
		case <-ctx.Done():
			cancelled.Add(1)
		case <-time.After(50 * time.Millisecond):
		}
	}
	s.Script("SELECT past_end", res)
	db := openDB(t, s, "inline_rows=1", "fetch_ahead=4")
	for range 3 {
		rows, err := db.Query("SELECT past_end")
		if err != nil {
			t.Fatal(err)
		}
		checkSequence(t, scanInts(t, rows), 20)
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("SELECT past_end"); err != nil {
			t.Fatal(err)
		}
	}
	if pastEnd.Load() == 0 {
		t.Fatal("no FETCH was issued past the end; the test does not exercise read-ahead")
	}
	if n := cancelled.Load(); n != 0 {
		t.Fatalf("%d of %d FETCHes past the end were cancelled", n, pastEnd.Load())
	}
	checkNoLeaksAfter(t, db)
}

// TestRowsCloseKeepsRequests closes a result early while read-ahead
// FETCHes are in flight: the statement is cancelled, and the requests are
// left to complete instead of being aborted (which closes connections).
func TestRowsCloseKeepsRequests(t *testing.T) {
	s := newServer(t)
	res := intResult(200, 5)
	var started, cancelled atomic.Int32
	res.BeforeFetch = func(ctx context.Context, index uint64) {
		if index < 2 {
			return
		}
		started.Add(1)
		select {
		case <-ctx.Done():
			cancelled.Add(1)
		case <-time.After(30 * time.Millisecond):
		}
	}
	s.Script("SELECT early_close", res)
	db := openDB(t, s, "inline_rows=0", "fetch_ahead=4")
	for range 3 {
		var v int64
		if err := db.QueryRow("SELECT early_close").Scan(&v); err != nil || v != 0 {
			t.Fatalf("QueryRow: %d, %v", v, err)
		}
	}
	if started.Load() == 0 {
		t.Fatal("no read-ahead FETCH was in flight at Close")
	}
	if n := cancelled.Load(); n != 0 {
		t.Fatalf("%d of %d in-flight FETCHes were aborted by Rows.Close", n, started.Load())
	}
	if s.Count(message.TypeCancelRequest) == 0 {
		t.Fatal("Rows.Close did not cancel the statement")
	}
	checkNoLeaksAfter(t, db)
}
