//go:build integration

package appendit

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/message"
)

// rowString is the VARCHAR value of row i; the server recomputes it to check
// every row.
func rowString(i int) string { return fmt.Sprintf("row-%d-%s", i, strings.Repeat("x", i%20)) }

// checkRows verifies that tbl (id BIGINT, s VARCHAR) holds rows 0..n-1 and,
// when ordered, in append order.
func checkRows(t *testing.T, e *env, tbl string, n int64, ordered bool) {
	t.Helper()
	bad := e.queryInt(t, fmt.Sprintf(`SELECT count(*) FILTER (WHERE s IS DISTINCT FROM 'row-' || id || '-' || repeat('x', (id %% 20)::INTEGER))
		+ abs(count(*) - %[2]d) + abs(count(DISTINCT id) - %[2]d) + abs(max(id) - %[2]d + 1) FROM %[1]s`, tbl, n))
	if bad != 0 {
		t.Fatalf("%s: %d wrong rows", tbl, bad)
	}
	if ordered {
		if off := e.queryInt(t, "SELECT count(*) FROM (SELECT id, row_number() OVER () - 1 AS rn FROM "+tbl+") WHERE id <> rn"); off != 0 {
			t.Fatalf("%s: %d rows out of append order", tbl, off)
		}
	}
}

// checkBatches verifies the SEND_DATA requests of one stream: dense batch
// indexes 1..N, chunks of at most 2048 rows, and a terminal message
// announcing N. It returns N.
func checkBatches(t *testing.T, e *env, mark int, wantRows int) int {
	t.Helper()
	sends := sent[message.SendDataRequest](t, e, mark)
	if len(sends) == 0 {
		t.Fatal("no SEND_DATA recorded")
	}
	rows, batches := 0, 0
	for i, s := range sends {
		if s.StreamID != sends[0].StreamID {
			t.Fatalf("SEND_DATA %d belongs to stream %q, want %q", i, s.StreamID, sends[0].StreamID)
		}
		if i == len(sends)-1 {
			if s.BatchIndex.Valid || len(s.Chunks) != 0 || !s.TotalBatches.Valid || int(s.TotalBatches.Value) != batches {
				t.Fatalf("terminal SEND_DATA %+v, want total_batches=%d", s, batches)
			}
			continue
		}
		batches++
		if !s.BatchIndex.Valid || int(s.BatchIndex.Value) != batches || s.TotalBatches.Valid {
			t.Fatalf("SEND_DATA %d: batch_index=%+v total=%+v", i, s.BatchIndex, s.TotalBatches)
		}
		for _, ch := range s.Chunks {
			if ch.RowCount < 1 || ch.RowCount > 2048 {
				t.Fatalf("chunk of %d rows", ch.RowCount)
			}
			rows += ch.RowCount
		}
	}
	if rows != wantRows {
		t.Fatalf("sent %d rows, want %d", rows, wantRows)
	}
	return batches
}

// TestAppendMillionRows streams a million rows through Conn.Append and
// through an Appender with 1 MiB batches.
func TestAppendMillionRows(t *testing.T) {
	const n = 1_000_000
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_million_a (id BIGINT, s VARCHAR)", "CREATE TABLE append_million_b (id BIGINT, s VARCHAR)")
	cols := []quackdriver.AppendColumn{{Name: "id", Type: "BIGINT"}, {Name: "s", Type: "VARCHAR"}}
	sc := e.conn(t)

	rows := make([][]any, n)
	for i := range rows {
		rows[i] = []any{i, rowString(i)}
	}
	mark := e.rec.Mark()
	if err := appendRows(sc, "append_million_a", cols, rows); err != nil {
		t.Fatal(err)
	}
	checkBatches(t, e, mark, n)
	checkRows(t, e, "append_million_a", n, true)

	mark = e.rec.Mark()
	count, err := appender(sc, "main", "append_million_b", cols, n, func(i int) []any { return []any{int64(i), rowString(i)} },
		quackdriver.WithBatchBytes(1<<20))
	if err != nil || count != n {
		t.Fatalf("Appender: %d rows, %v", count, err)
	}
	if batches := checkBatches(t, e, mark, n); batches < 10 {
		t.Fatalf("only %d batches with 1 MiB batches", batches)
	}
	for _, x := range e.rec.Since(mark) {
		if len(x.Req) > 2<<20 {
			t.Fatalf("request of %d bytes with WithBatchBytes(1 MiB)", len(x.Req))
		}
	}
	checkRows(t, e, "append_million_b", n, true)
}

func TestAppendUnordered(t *testing.T) {
	const n = 100_000
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_unordered (id BIGINT, s VARCHAR)")
	mark := e.rec.Mark()
	count, err := appender(e.conn(t), "", "append_unordered", []quackdriver.AppendColumn{{Type: "BIGINT"}, {Type: "VARCHAR"}}, n,
		func(i int) []any { return []any{i, rowString(i)} }, quackdriver.WithUnordered(), quackdriver.WithBatchBytes(256<<10))
	if err != nil || count != n {
		t.Fatalf("Appender: %d rows, %v", count, err)
	}
	p := appenderPrepare(t, e, mark)
	if !strings.HasSuffix(p.SQL, "ordered := false)") || !p.InlineRows.Valid || p.InlineRows.Value != 0 {
		t.Fatalf("unordered INSERT %s with inline_rows %+v", p.SQL, p.InlineRows)
	}
	checkBatches(t, e, mark, n)
	checkRows(t, e, "append_unordered", n, false)
}

// TestAppendFlushSemantics covers Flush, Close and the ended Appender, and
// appends of zero rows.
func TestAppendFlushSemantics(t *testing.T) {
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_flush (id INTEGER)")
	ctx := context.Background()
	sc := e.conn(t)
	cols := []quackdriver.AppendColumn{{Name: "id", Type: "INTEGER"}}
	sendData := func(mark int) []message.SendDataRequest { return sent[message.SendDataRequest](t, e, mark) }
	err := raw(sc, func(c *quackdriver.Conn) error {
		if err := c.Append(ctx, "append_flush", cols, nil); err != nil {
			return fmt.Errorf("Append(nil): %w", err)
		}
		a, err := c.NewAppender(ctx, "", "append_flush", cols)
		if err != nil {
			return err
		}
		defer func() { _ = a.Abort() }()

		// Flush with nothing buffered sends nothing; buffered rows wait for
		// Flush.
		mark := e.rec.Mark()
		if err := a.Flush(); err != nil {
			return err
		}
		for i := range 10 {
			if err := a.AppendRow(i); err != nil {
				return err
			}
		}
		if s := sendData(mark); len(s) != 0 {
			t.Fatalf("sent %d SEND_DATA before Flush", len(s))
		}
		if err := a.Flush(); err != nil {
			return err
		}
		if s := sendData(mark); len(s) != 1 || len(s[0].Chunks) != 1 || s[0].Chunks[0].RowCount != 10 || s[0].BatchIndex.Value != 1 {
			t.Fatalf("Flush sent %+v", s)
		}
		// The INSERT is one statement: other sessions see nothing yet.
		if n := e.queryInt(t, "SELECT count(*) FROM append_flush"); n != 0 {
			t.Fatalf("other connection sees %d rows before Close", n)
		}
		for i := 10; i <= 3000; i++ {
			if err := a.AppendRow(i); err != nil {
				return err
			}
		}
		if n, err := a.Close(); err != nil || n != 3001 {
			return fmt.Errorf("Close: %d, %w", n, err)
		}
		// Close is idempotent; the ended Appender rejects further use.
		if n, err := a.Close(); n != 3001 || err != nil {
			t.Fatalf("second Close: %d %v", n, err)
		}
		if a.AppendRow(1) == nil || a.Flush() == nil || a.Abort() != nil {
			t.Fatal("the closed Appender is still usable")
		}

		// A stream of zero rows.
		a, err = c.NewAppender(ctx, "", "append_flush", cols)
		if err != nil {
			return err
		}
		if n, err := a.Close(); err != nil || n != 0 {
			return fmt.Errorf("empty stream: %d, %w", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.queryInt(t, "SELECT count(*) * 10000 + sum(id) FROM append_flush"); n != 3001*10000+3000*3001/2 {
		t.Fatalf("table checksum %d", n)
	}
	connUsable(t, sc)
}

// TestAppendLargeValues sends rows larger than the batch size: a batch holds
// at least one row.
func TestAppendLargeValues(t *testing.T) {
	const size = 3 << 20
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_large (id INTEGER, b BLOB)")
	mark := e.rec.Mark()
	n, err := appender(e.conn(t), "", "append_large", []quackdriver.AppendColumn{{Name: "id", Type: "INTEGER"}, {Name: "b", Type: "BLOB"}}, 3,
		func(i int) []any { return []any{i, []byte(strings.Repeat(string(rune('a'+i)), size))} }, quackdriver.WithBatchBytes(1<<20))
	if err != nil || n != 3 {
		t.Fatalf("Appender: %d rows, %v", n, err)
	}
	checkBatches(t, e, mark, 3)
	if ok := e.queryInt(t, fmt.Sprintf("SELECT count(*) FROM append_large WHERE b = repeat(chr(97 + id)::BLOB, %d)", size)); ok != 3 {
		t.Fatalf("%d of 3 values stored intact", ok)
	}
}

// TestAppendRequestSplit sends rows whose encoded size the batching estimate
// underrates (wide all-NULL STRUCTs): requests above the server's body limit
// are split.
func TestAppendRequestSplit(t *testing.T) {
	if testing.Short() {
		t.Skip("sends about 200 MB")
	}
	const n = 80_000
	e := newEnv(t)
	var fields []string
	for i := range 200 {
		fields = append(fields, fmt.Sprintf("f%d BIGINT", i))
	}
	typ := "STRUCT(" + strings.Join(fields, ", ") + ")"
	e.exec(t, "CREATE TABLE append_split (id INTEGER, s "+typ+")")
	mark := e.rec.Mark()
	empty := map[string]any{}
	count, err := appender(e.conn(t), "", "append_split", []quackdriver.AppendColumn{{Type: "INTEGER"}, {Type: typ}}, n,
		func(i int) []any { return []any{i, empty} })
	if err != nil || count != n {
		t.Fatalf("Appender: %d rows, %v", count, err)
	}
	for _, x := range e.rec.Since(mark) {
		if len(x.Req) > quackdriver.MaxAppendRequestBytes {
			t.Fatalf("request of %d bytes", len(x.Req))
		}
	}
	checkBatches(t, e, mark, n)
	if got := e.queryInt(t, "SELECT count(*) FROM append_split WHERE s IS NOT NULL AND s.f199 IS NULL"); got != n {
		t.Fatalf("got %d rows", got)
	}
}

// TestAppendConcurrentAppendRow appends and flushes from several goroutines;
// the Appender serializes them.
func TestAppendConcurrentAppendRow(t *testing.T) {
	const workers, per = 4, 20000
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_conc (g INTEGER, i INTEGER, s VARCHAR)")
	err := raw(e.conn(t), func(c *quackdriver.Conn) error {
		a, err := c.NewAppender(context.Background(), "", "append_conc",
			[]quackdriver.AppendColumn{{Type: "INTEGER"}, {Type: "INTEGER"}, {Type: "VARCHAR"}}, quackdriver.WithBatchBytes(64<<10))
		if err != nil {
			return err
		}
		defer func() { _ = a.Abort() }()
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for g := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range per {
					err := a.AppendRow(g, i, fmt.Sprint(g, "-", i))
					if err == nil && i%5000 == 0 {
						err = a.Flush()
					}
					if err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		if err := <-errs; err != nil {
			return err
		}
		if n, err := a.Close(); err != nil || n != workers*per {
			return fmt.Errorf("Close: %d, %w", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if bad := e.queryInt(t, "SELECT count(*) FILTER (WHERE s <> g || '-' || i) + abs(count(DISTINCT (g, i)) - 80000) FROM append_conc"); bad != 0 {
		t.Fatalf("bad=%d", bad)
	}
}

// TestAppendHeartbeatWhileIdle keeps an Appender open longer than the
// session lease without sending rows: the heartbeat keeps the session alive.
func TestAppendHeartbeatWhileIdle(t *testing.T) {
	e := newEnv(t, "heartbeat_timeout=1s")
	e.exec(t, "CREATE TABLE append_idle (id INTEGER)")
	sc := e.conn(t)
	n, err := appender(sc, "", "append_idle", []quackdriver.AppendColumn{{Type: "INTEGER"}}, 2, func(i int) []any {
		if i == 1 {
			time.Sleep(3 * time.Second)
		}
		return []any{i}
	})
	if err != nil || n != 2 {
		t.Fatalf("Appender: %d rows, %v", n, err)
	}
	connUsable(t, sc)
}

// TestAppendWhileRowsOpen appends on a connection whose previous result is
// still being read: the result is buffered first and stays complete.
func TestAppendWhileRowsOpen(t *testing.T) {
	e := newEnv(t, "inline_rows=100")
	e.exec(t, "CREATE TABLE append_rowsopen (id INTEGER)")
	sc := e.conn(t)
	rows, err := sc.QueryContext(context.Background(), "SELECT range FROM range(300000)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal(rows.Err())
	}
	if err := appendRows(sc, "append_rowsopen", []quackdriver.AppendColumn{{Type: "INTEGER"}}, [][]any{{1}, {2}}); err != nil {
		t.Fatal(err)
	}
	n, sum := 1, int64(0)
	for ; rows.Next(); n++ {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		sum += v
	}
	if err := rows.Err(); err != nil || n != 300000 || sum != 300000*299999/2 {
		t.Fatalf("read %d rows (sum %d) after the append: %v", n, sum, err)
	}
	if got := e.queryInt(t, "SELECT sum(id) FROM append_rowsopen"); got != 3 {
		t.Fatalf("sum %d", got)
	}
}
