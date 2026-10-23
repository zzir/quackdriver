package quackdriver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/quacktest"
)

// The Appender's SEND_DATA batching: chunk and batch sizes, the request
// limit, dense batch indices and the terminal message.

// appendEvents returns the SEND_DATA events of one stream.
func sendDataEvents(s *quacktest.Server, streamID string) []quacktest.Event {
	var out []quacktest.Event
	for _, e := range s.EventsOf(message.TypeSendDataRequest) {
		if e.SendData != nil && e.SendData.StreamID == streamID {
			out = append(out, e)
		}
	}
	return out
}

// checkBatches verifies the batch protocol of a finished stream: data
// batches with dense indices 1..N, then exactly one terminal message with
// total_batches = N and no chunks. It returns the chunk row counts per batch.
func checkBatches(t *testing.T, s *quacktest.Server, streamID string) [][]int {
	t.Helper()
	events := sendDataEvents(s, streamID)
	if len(events) == 0 {
		t.Fatal("no SEND_DATA")
	}
	var batches [][]int
	for i, e := range events[:len(events)-1] {
		sd := e.SendData
		if len(sd.Chunks) == 0 || sd.BatchIndex != message.Idx(uint64(i+1)) || sd.TotalBatches.Valid {
			t.Fatalf("SEND_DATA %d: index %+v total %+v chunks %d", i, sd.BatchIndex, sd.TotalBatches, len(sd.Chunks))
		}
		var rows []int
		for _, ch := range sd.Chunks {
			if ch.RowCount < 1 || ch.RowCount > appendChunkRows {
				t.Fatalf("chunk of %d rows", ch.RowCount)
			}
			rows = append(rows, ch.RowCount)
		}
		batches = append(batches, rows)
	}
	term := events[len(events)-1].SendData
	if len(term.Chunks) != 0 || term.BatchIndex.Valid || term.TotalBatches != message.Idx(uint64(len(batches))) {
		t.Fatalf("terminal SEND_DATA %+v after %d batches", term, len(batches))
	}
	return batches
}

var bigintCol = []AppendColumn{{Name: "i", Type: "BIGINT"}}

func TestAppenderChunksAndCount(t *testing.T) {
	s := newServer(t)
	checkChunkTypes(t, s)
	c := openConn(t, s)
	a := newAppender(t, c, bigintCol)
	for i := range 5000 {
		if err := a.AppendRow(i); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.Count(message.TypeSendDataRequest); n != 0 {
		t.Fatalf("%d SEND_DATA before the batch target was reached", n)
	}
	n, err := a.Close()
	if err != nil || n != 5000 {
		t.Fatalf("Close: %d %v", n, err)
	}
	if got := checkBatches(t, s, a.streamID); !reflect.DeepEqual(got, [][]int{{2048, 2048, 904}}) {
		t.Fatalf("batches %v", got)
	}
	st := s.Stream(a.streamID)
	if !st.Finished || len(st.Rows()) != 5000 {
		t.Fatalf("stream %+v", st)
	}
	for i, r := range st.Rows() {
		if r[0] != strconv.Itoa(i) {
			t.Fatalf("row %d = %v", i, r[0])
		}
	}

	// The PREPARE: inline_rows = 0 and the exact SQL; every request of the
	// statement carries its client_query_id; the count is read with FETCH.
	p := prepareOf(t, s, a.sql)
	if p.InlineRows != message.Idx(0) || p.QueryUUID != a.q.uuid {
		t.Fatalf("PREPARE %+v", p)
	}
	if !strings.HasPrefix(p.SQL, `INSERT INTO "t" ("i") SELECT "c0"::BIGINT FROM scan_data_from_quack_client('`) ||
		!strings.HasSuffix(p.SQL, `', NULL::STRUCT("c0" VARCHAR), ordered := true)`) {
		t.Fatalf("SQL %s", p.SQL)
	}
	for _, e := range append(sendDataEvents(s, a.streamID), fetchesOf(s, a.q.uuid)...) {
		if !e.HasQueryID || e.ClientQueryID != p.ClientQueryID || e.ConnectionID != c.connectionID {
			t.Fatalf("event %+v does not carry the statement's ids", e)
		}
	}
	if f := fetchesOf(s, a.q.uuid); len(f) == 0 || f[0].BatchIndex != 1 || f[0].AckIndex != 0 {
		t.Fatalf("FETCH %+v", f)
	}
	if s.Count(message.TypeCancelRequest) != 0 {
		t.Fatal("a successful stream must not be cancelled")
	}
	if c.exclusive.Load() {
		t.Fatal("Close must free the connection")
	}
	// Close is idempotent; other calls report the closed Appender.
	if n, err := a.Close(); n != 5000 || err != nil {
		t.Fatalf("second Close: %d %v", n, err)
	}
	if err := a.AppendRow(1); !errors.Is(err, errAppenderClosed) {
		t.Fatalf("AppendRow after Close: %v", err)
	}
	if err := a.Flush(); !errors.Is(err, errAppenderClosed) {
		t.Fatalf("Flush after Close: %v", err)
	}
	if err := a.Abort(); err != nil {
		t.Fatalf("Abort after Close: %v", err)
	}
}

func TestAppenderBatchBytes(t *testing.T) {
	s := newServer(t)
	checkChunkTypes(t, s)
	c := openConn(t, s)

	// A four-digit value counts 9 bytes (1 + its text + 4): a 2048-row
	// chunk reaches 2048*9 bytes, so every chunk is a batch of its own and
	// is sent as soon as it is full.
	a := newAppender(t, c, bigintCol, WithBatchBytes(2048*9))
	for i := range 5000 {
		if err := a.AppendRow(int64(1000 + i)); err != nil {
			t.Fatal(err)
		}
		if i == 2047 && s.Count(message.TypeSendDataRequest) != 1 {
			t.Fatal("a full batch must be sent at once")
		}
	}
	if n, err := a.Close(); n != 5000 || err != nil {
		t.Fatalf("Close %d %v", n, err)
	}
	if got := checkBatches(t, s, a.streamID); !reflect.DeepEqual(got, [][]int{{2048}, {2048}, {904}}) {
		t.Fatalf("batches %v", got)
	}

	// A smaller target cuts chunks before 2048 rows.
	a = newAppender(t, c, bigintCol, WithBatchBytes(1000))
	for i := range 300 {
		if err := a.AppendRow(int64(1000 + i)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := a.Close(); n != 300 || err != nil {
		t.Fatalf("Close %d %v", n, err)
	}
	if got := checkBatches(t, s, a.streamID); !reflect.DeepEqual(got, [][]int{{112}, {112}, {76}}) {
		t.Fatalf("batches %v", got)
	}
	rows := s.Stream(a.streamID).Rows()
	for i, r := range rows {
		if r[0] != strconv.Itoa(1000+i) {
			t.Fatalf("row %d = %v", i, r[0])
		}
	}

	// Options: the default, n <= 0, and the cap at the request limit.
	for _, tc := range []struct {
		opts []AppenderOption
		want int
	}{
		{nil, DefaultAppendBatchBytes},
		{[]AppenderOption{WithBatchBytes(0)}, DefaultAppendBatchBytes},
		{[]AppenderOption{WithBatchBytes(-5)}, DefaultAppendBatchBytes},
		{[]AppenderOption{WithBatchBytes(1 << 40)}, MaxAppendRequestBytes},
		{[]AppenderOption{nil, WithBatchBytes(4096)}, 4096},
	} {
		a := newAppender(t, c, bigintCol, tc.opts...)
		if a.batchBytes != tc.want || a.maxRequest != MaxAppendRequestBytes {
			t.Fatalf("batchBytes %d, want %d", a.batchBytes, tc.want)
		}
		if err := a.Abort(); err != nil {
			t.Fatal(err)
		}
	}
}

// Requests never exceed the limit: an oversized batch is split by chunks,
// then by rows; a single oversized row is an error that cancels the INSERT.
func TestAppenderRequestLimitSplits(t *testing.T) {
	s := newServer(t)
	checkChunkTypes(t, s)
	c := openConn(t, s)
	cols := []AppendColumn{{Name: "s", Type: "VARCHAR"}, {Name: "i", Type: "INTEGER"}}
	a := newAppender(t, c, cols)
	a.batchBytes = 1 << 30 // estimate never triggers a send
	a.maxRequest = 8 << 10
	const rows = 6000
	for i := range rows {
		if err := a.AppendRow(fmt.Sprintf("value-%06d", i), i); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := a.Close(); n != rows || err != nil {
		t.Fatalf("Close %d %v", n, err)
	}
	batches := checkBatches(t, s, a.streamID)
	if len(batches) < 10 {
		t.Fatalf("expected the batch to be split, got %v", batches)
	}
	for _, e := range sendDataEvents(s, a.streamID) {
		if e.Size > 8<<10 {
			t.Fatalf("request of %d bytes exceeds the limit", e.Size)
		}
	}
	got := s.Stream(a.streamID).Rows()
	if len(got) != rows {
		t.Fatalf("%d rows", len(got))
	}
	for i, r := range got {
		if r[0] != fmt.Sprintf("value-%06d", i) || r[1] != strconv.Itoa(i) {
			t.Fatalf("row %d = %v", i, r)
		}
	}

	// A single row larger than the limit.
	s.ResetEvents()
	a = newAppender(t, c, cols)
	a.maxRequest = 8 << 10
	if err := a.AppendRow("small", 1); err != nil {
		t.Fatal(err)
	}
	if err := a.AppendRow(strings.Repeat("x", 9<<10), 2); err != nil {
		t.Fatal(err)
	}
	err := a.Flush()
	if err == nil || !strings.Contains(err.Error(), "row 1:") || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized row: %v", err)
	}
	if n, err2 := a.Close(); n != 0 || !errors.Is(err2, err) {
		t.Fatalf("Close after failure: %d %v", n, err2)
	}
	if !cancelled(s, a.q.uuid) || c.exclusive.Load() || !c.IsValid() {
		t.Fatal("the INSERT must be cancelled and the connection freed")
	}
	// row 0 went out in its own batch before the oversized row failed
	if ev := sendDataEvents(s, a.streamID); len(ev) != 1 || ev[0].SendData.Chunks[0].RowCount != 1 {
		t.Fatalf("SEND_DATA %+v", ev)
	}
}

func TestAppenderZeroRows(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	a := newAppender(t, c, bigintCol)
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	n, err := a.Close()
	if err != nil || n != 0 {
		t.Fatalf("Close %d %v", n, err)
	}
	if got := checkBatches(t, s, a.streamID); len(got) != 0 {
		t.Fatalf("batches %v", got)
	}
	if st := s.Stream(a.streamID); !st.Finished || *st.Total != 0 {
		t.Fatalf("stream %+v", st)
	}
	// Conn.Append with no rows runs the (empty) INSERT too; each INSERT
	// follows the statement resolving its column types.
	if err := c.Append(context.Background(), "t", bigintCol, nil); err != nil {
		t.Fatal(err)
	}
	if s.Count(message.TypePrepareRequest) != 4 {
		t.Fatalf("%d PREPAREs", s.Count(message.TypePrepareRequest))
	}
}

func TestAppenderFlushAndOrdering(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	a := newAppender(t, c, bigintCol, WithUnordered())
	if !strings.HasSuffix(a.sql, "ordered := false)") {
		t.Fatalf("SQL %s", a.sql)
	}
	for i := range 3 {
		if err := a.AppendRow(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := a.Flush(); err != nil { // nothing buffered: no request
		t.Fatal(err)
	}
	if err := a.AppendRow(3); err != nil {
		t.Fatal(err)
	}
	if n, err := a.Close(); n != 4 || err != nil {
		t.Fatalf("Close %d %v", n, err)
	}
	if got := checkBatches(t, s, a.streamID); !reflect.DeepEqual(got, [][]int{{3}, {1}}) {
		t.Fatalf("batches %v", got)
	}
}

// Concurrent use is serialized; Close from several goroutines yields one
// stream end.
func TestAppenderConcurrentUse(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	a := newAppender(t, c, bigintCol, WithBatchBytes(512), WithUnordered())
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 250 {
				if err := a.AppendRow(g*1000 + i); err != nil {
					t.Error(err)
					return
				}
				if i%50 == 0 {
					if err := a.Flush(); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	results := make(chan int64, 4)
	for range 4 {
		go func() {
			n, err := a.Close()
			if err != nil {
				t.Error(err)
			}
			results <- n
		}()
	}
	for range 4 {
		if n := <-results; n != 2000 {
			t.Fatalf("Close %d", n)
		}
	}
	checkBatches(t, s, a.streamID)
	if len(s.Stream(a.streamID).Rows()) != 2000 {
		t.Fatal("rows lost")
	}
}

// A sealed chunk carries the Appender's size estimate for its rows (1 per
// value, the text plus 4 per string), which sizes the request buffer; a
// split shares the estimate between the halves.
func TestPendingChunkBytes(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	a := newAppender(t, c, bigintCol)
	for i := range 10 {
		if err := a.AppendRow(int64(1000 + i)); err != nil { // "1000".."1009": 9 bytes each
			t.Fatal(err)
		}
	}
	a.mu.Lock()
	a.sealChunk()
	pending := a.pending
	a.mu.Unlock()
	if len(pending) != 1 || pending[0].bytes != 90 || pending[0].chunk.RowCount != 10 {
		t.Fatalf("pending %+v", pending)
	}
	left, right := splitChunk(pending[0])
	if left.bytes != 45 || right.bytes != 45 || left.chunk.RowCount != 5 || right.chunk.RowCount != 5 {
		t.Fatalf("split %d/%d bytes, %d/%d rows", left.bytes, right.bytes, left.chunk.RowCount, right.chunk.RowCount)
	}
	odd := pendingChunk{chunk: pending[0].chunk, bytes: 91}
	if l, r := splitChunk(odd); l.bytes+r.bytes != 91 {
		t.Fatalf("odd split %d+%d", l.bytes, r.bytes)
	}
	if err := a.Abort(); err != nil {
		t.Fatal(err)
	}
}
