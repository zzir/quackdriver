package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/quacktest"
)

var (
	streamSQLPattern = regexp.MustCompile(`scan_data_from_quack_client\('([^']*)', NULL::STRUCT\((.*)\), ordered := (true|false)\)$`)
)

// prototypeTypes extracts the scan prototype of an Appender INSERT, checks
// that its members are "c0" VARCHAR, "c1" VARCHAR, ..., and returns their
// types.
func prototypeTypes(t *testing.T, sql string) (streamID string, types []logicaltype.Type) {
	t.Helper()
	m := streamSQLPattern.FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("no scan prototype in %q", sql)
	}
	for i, member := range strings.Split(m[2], ", ") {
		if member != fmt.Sprintf(`"c%d" VARCHAR`, i) {
			t.Fatalf("prototype member %d is %q", i, member)
		}
		types = append(types, typVarchar)
	}
	return m[1], types
}

// typeChecker verifies, on the server side, that every SEND_DATA chunk
// decodes to exactly the types of the scan prototype in its INSERT (the
// real server raises an INTERNAL error on a mismatch).
type typeChecker struct {
	t        *testing.T
	mu       sync.Mutex
	expected map[string][]logicaltype.Type
	problems []string
}

func checkChunkTypes(t *testing.T, s *quacktest.Server) {
	t.Helper()
	tc := &typeChecker{t: t, expected: make(map[string][]logicaltype.Type)}
	s.SetRawHook(func(_ http.ResponseWriter, _ message.Header, body message.Body) bool {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		switch b := body.(type) {
		case message.PrepareRequest:
			if streamSQLPattern.MatchString(b.SQL) {
				id, types := prototypeTypes(t, b.SQL)
				tc.expected[id] = types
			}
		case message.SendDataRequest:
			want, ok := tc.expected[b.StreamID]
			if !ok {
				tc.problems = append(tc.problems, "SEND_DATA for unknown stream "+b.StreamID)
			}
			for i, ch := range b.Chunks {
				if !reflect.DeepEqual(ch.Types, want) {
					tc.problems = append(tc.problems, fmt.Sprintf("stream %s chunk %d types %v, prototype %v", b.StreamID, i, ch.Types, want))
				}
				if ch.RowCount > appendChunkRows {
					tc.problems = append(tc.problems, fmt.Sprintf("chunk of %d rows", ch.RowCount))
				}
			}
		}
		return false
	})
	t.Cleanup(func() {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		for _, p := range tc.problems {
			t.Error(p)
		}
	})
}

func TestAppendSQLText(t *testing.T) {
	types := []logicaltype.Type{typInteger, structOf(logicaltype.IDStruct, child("a b", typVarchar)), decimalOf(18, 4)}
	cols := []AppendColumn{{Name: "id", Type: "INTEGER"}, {Name: `we"ird`, Type: `STRUCT("a b" VARCHAR)`}, {Name: "Amt", Type: "DECIMAL(18,4)"}}
	got := appendSQL("my schema", "t", cols, types, "0f8c5f0e-8d6b-4c1e-9d0a-3b8f6a1e2c4d", true)
	want := `INSERT INTO "my schema"."t" ("id", "we""ird", "Amt") SELECT "c0"::INTEGER, "c1"::STRUCT("a b" VARCHAR), "c2"::DECIMAL(18,4) ` +
		`FROM scan_data_from_quack_client('0f8c5f0e-8d6b-4c1e-9d0a-3b8f6a1e2c4d', NULL::STRUCT("c0" VARCHAR, "c1" VARCHAR, "c2" VARCHAR), ordered := true)`
	if got != want {
		t.Fatalf("SQL\n got %s\nwant %s", got, want)
	}
	// No schema; an unnamed column drops the column list; unordered.
	cols[1].Name = ""
	got = appendSQL("", `ta"ble`, cols, types[:1], "s", false)
	want = `INSERT INTO "ta""ble" SELECT "c0"::INTEGER FROM scan_data_from_quack_client('s', NULL::STRUCT("c0" VARCHAR), ordered := false)`
	if got != want {
		t.Fatalf("SQL\n got %s\nwant %s", got, want)
	}
}

func TestNewStreamID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := make(map[string]bool)
	for range 100 {
		id, err := newStreamID()
		if err != nil || !re.MatchString(id) || seen[id] {
			t.Fatalf("stream id %q %v", id, err)
		}
		seen[id] = true
	}
}

func newAppender(t *testing.T, c *Conn, cols []AppendColumn, opts ...AppenderOption) *Appender {
	t.Helper()
	a, err := c.NewAppender(context.Background(), "", "t", cols, opts...)
	if err != nil {
		t.Fatalf("NewAppender: %v", err)
	}
	return a
}

func TestAppenderConversionErrorsKeepStream(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	a := newAppender(t, c, []AppendColumn{{Name: "a", Type: "INTEGER"}, {Name: "b", Type: "VARCHAR"}})
	if err := a.AppendRow(1, "x"); err != nil {
		t.Fatal(err)
	}
	err := a.AppendRow(2, 3)
	if err == nil || !strings.Contains(err.Error(), `row 1 column "b"`) || !strings.Contains(err.Error(), "VARCHAR expects string") {
		t.Fatalf("conversion error: %v", err)
	}
	var ve *ValueError
	if !errors.As(err, &ve) || ve.Row != 1 || ve.Column != 1 || ve.Err == nil {
		t.Fatalf("conversion error is not a *ValueError for row 1 column 1: %#v", err)
	}
	// Rows are numbered by AppendRow call, skipped rows included.
	if err := a.AppendRow(1); err == nil || !strings.Contains(err.Error(), "row 2 has 1 values, want 2") {
		t.Fatalf("value count: %v", err)
	} else if !errors.As(err, &ve) || ve.Row != 2 || ve.Column != -1 {
		t.Fatalf("value count error: %#v", err)
	}
	if err := a.AppendRow([]any{1, "x"}); err == nil {
		t.Fatal("a row passed as one slice argument has the wrong value count")
	}
	if err := a.AppendRow(1.5, "y"); err == nil || !strings.Contains(err.Error(), `row 4 column "a"`) {
		t.Fatalf("Go type error: %v", err)
	}
	if err := a.AppendRow(nil, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := a.Close(); n != 2 || err != nil {
		t.Fatalf("Close %d %v", n, err)
	}
	if rows := s.Stream(a.streamID).Rows(); !reflect.DeepEqual(rows, [][]any{{"1", "x"}, {nil, nil}}) {
		t.Fatalf("rows %v", rows)
	}

	// Unnamed columns are reported by position.
	a = newAppender(t, c, []AppendColumn{{Type: "INTEGER"}})
	if err := a.AppendRow("x"); err == nil || !strings.Contains(err.Error(), "row 0 column 0:") {
		t.Fatalf("unnamed column: %v", err)
	}
	_ = a.Abort()
}

func TestAppenderServerErrorMidStream(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	s.SetSendDataHook(func(_ string, req message.SendDataRequest) *message.ErrorResponse {
		if req.BatchIndex == message.Idx(2) {
			return &message.ErrorResponse{Message: "NOT NULL constraint failed: t.i", ExceptionType: "Constraint",
				ExtraInfo: map[string]string{"error_subtype": "NOT_NULL"}}
		}
		return nil
	})
	a := newAppender(t, c, bigintCol, WithBatchBytes(2048*8))
	var err error
	for i := 0; i < 5000 && err == nil; i++ {
		err = a.AppendRow(i)
	}
	var qe *Error
	if !errors.As(err, &qe) || qe.Type != "Constraint" || qe.ExtraInfo["error_subtype"] != "NOT_NULL" {
		t.Fatalf("mid-stream error: %s", errString(err))
	}
	if msg := err.Error(); !regexp.MustCompile(`^quackdriver: Appender: SEND_DATA batch 2 \(rows [1-9]\d*-\d+\): Constraint Error: NOT NULL constraint failed: t\.i$`).MatchString(msg) {
		t.Fatalf("message %q", msg)
	}
	if !cancelled(s, a.q.uuid) {
		t.Fatal("a failed stream must be cancelled")
	}
	if err2 := a.AppendRow(1); !errors.Is(err2, err) {
		t.Fatalf("AppendRow after failure: %v", err2)
	}
	if n, err2 := a.Close(); n != 0 || !errors.Is(err2, err) {
		t.Fatalf("Close after failure: %d %v", n, err2)
	}
	if c.exclusive.Load() || !c.IsValid() {
		t.Fatal("a statement error must free the connection and keep it valid")
	}
	if _, err := c.ExecContext(context.Background(), "BEGIN", nil); err != nil {
		t.Fatalf("connection unusable after a failed stream: %v", err)
	}
}

// TestAppenderEndErrors: failures at the end of the stream (the terminal
// message, the INSERT reported by FETCH, a missing or mistyped count)
// cancel the INSERT and free the connection.
func TestAppenderEndErrors(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	s.SetSendDataHook(func(_ string, req message.SendDataRequest) *message.ErrorResponse {
		if req.TotalBatches.Valid {
			return &message.ErrorResponse{Message: "boom", ExceptionType: "IO"}
		}
		return nil
	})
	a := newAppender(t, c, bigintCol)
	_ = a.AppendRow(1)
	_, err := a.Close()
	var qe *Error
	if !errors.As(err, &qe) || qe.Message != "boom" || !strings.Contains(err.Error(), "end of the stream (1 batches, 1 rows)") {
		t.Fatalf("terminal error: %s", errString(err))
	}
	if !cancelled(s, a.q.uuid) || c.exclusive.Load() {
		t.Fatal("terminal failure must cancel and free")
	}
	s.SetSendDataHook(nil)

	// The FETCH that reads the count answers with body.
	for _, c2 := range []struct {
		body   message.Body
		want   string
		cancel bool // the statement may still run
	}{
		{message.ErrorResponse{Message: `Duplicate key "i: 1" violates primary key constraint`, ExceptionType: "Constraint"},
			"quackdriver: Appender: INSERT: Constraint Error: Duplicate key", true},
		{message.FetchResponse{TotalBatches: message.Idx(0)}, "the INSERT finished without a row count", false},
		{message.FetchResponse{BatchIndex: message.Idx(1), Results: []message.DataChunk{{RowCount: 1,
			Types: []logicaltype.Type{typVarchar}, Columns: []message.Column{message.Values{"5"}}}}}, "instead of a BIGINT row count", true},
	} {
		s.SetRawHook(func(w http.ResponseWriter, h message.Header, body message.Body) bool {
			if _, ok := body.(message.FetchRequest); !ok {
				return false
			}
			quacktest.RawResponse(w, message.Header{ConnectionID: h.ConnectionID}, c2.body)
			return true
		})
		a := newAppender(t, c, bigintCol)
		_ = a.AppendRow(1)
		if _, err := a.Close(); err == nil || !strings.Contains(err.Error(), c2.want) {
			t.Fatalf("got %v, want %q", err, c2.want)
		}
		if c.exclusive.Load() || !c.IsValid() || cancelled(s, a.q.uuid) != c2.cancel {
			t.Fatalf("%q: held=%v valid=%v cancelled=%v", c2.want, c.exclusive.Load(), c.IsValid(), cancelled(s, a.q.uuid))
		}
	}
}

func TestAppenderHoldsConnection(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT 1", intResult(1, 1))
	c := openConn(t, s)
	ctx := context.Background()
	a := newAppender(t, c, bigintCol)

	if _, err := c.ExecContext(ctx, "SELECT 1", nil); !errors.Is(err, ErrConnBusy) {
		t.Fatalf("Exec: %v", err)
	}
	if _, err := c.QueryContext(ctx, "SELECT 1", nil); !errors.Is(err, ErrConnBusy) {
		t.Fatalf("Query: %v", err)
	}
	if _, err := c.BeginTx(ctx, driver.TxOptions{}); !errors.Is(err, ErrConnBusy) {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := c.NewAppender(ctx, "", "t", bigintCol); !errors.Is(err, ErrConnBusy) {
		t.Fatalf("second NewAppender: %v", err)
	}
	if err := c.Append(ctx, "t", bigintCol, [][]any{{1}}); !errors.Is(err, ErrConnBusy) {
		t.Fatalf("Append: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping while an Appender is open: %v", err)
	}
	if err := a.AppendRow(1); err != nil {
		t.Fatal(err)
	}
	if n, err := a.Close(); n != 1 || err != nil {
		t.Fatalf("Close %d %v", n, err)
	}
	if _, err := c.ExecContext(ctx, "SELECT 1", nil); err != nil {
		t.Fatalf("Exec after Close: %v", err)
	}
}

func TestAppenderAbort(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	a := newAppender(t, c, bigintCol)
	for i := range 10 {
		_ = a.AppendRow(i)
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = a.AppendRow(10) // buffered, never sent
	if err := a.Abort(); err != nil {
		t.Fatal(err)
	}
	if !cancelled(s, a.q.uuid) {
		t.Fatal("Abort must send CANCEL")
	}
	st := s.Stream(a.streamID)
	if st.Finished || !strings.Contains(st.Error, "Interrupted") {
		t.Fatalf("stream %+v", st)
	}
	if err := a.Abort(); err != nil {
		t.Fatal("second Abort")
	}
	if err := a.AppendRow(1); !errors.Is(err, errAppenderAborted) {
		t.Fatalf("AppendRow after Abort: %v", err)
	}
	if n, err := a.Close(); n != 0 || !errors.Is(err, errAppenderAborted) {
		t.Fatalf("Close after Abort: %d %v", n, err)
	}
	if s.Count(message.TypeSendDataRequest) != 1 || s.Count(message.TypeCancelRequest) != 1 {
		t.Fatalf("SEND_DATA %d CANCEL %d", s.Count(message.TypeSendDataRequest), s.Count(message.TypeCancelRequest))
	}
	if c.exclusive.Load() || !c.IsValid() {
		t.Fatal("Abort must free the connection")
	}
}

func TestAppenderContextCancel(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	block := make(chan struct{})
	s.SetBeforeSendData(func(ctx context.Context, req message.SendDataRequest) {
		select {
		case <-block:
		case <-ctx.Done():
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	a, err := c.NewAppender(ctx, "", "t", bigintCol)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.AppendRow(1)
	done := make(chan error, 1)
	go func() { done <- a.Flush() }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Flush did not return after cancel")
	}
	close(block)
	if !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "quackdriver: Appender: SEND_DATA batch 1 (row 0): ") {
		t.Fatalf("Flush: %s", errString(err))
	}
	eventually(t, "background CANCEL", func() bool { return cancelled(s, a.q.uuid) })
	if c.exclusive.Load() || !c.IsValid() {
		t.Fatal("a cancelled stream must free the connection and keep it valid")
	}
	if _, err := a.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close after cancel: %v", err)
	}

	// A context that is already done fails NewAppender without holding the conn.
	if _, err := c.NewAppender(ctx, "", "t", bigintCol); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewAppender with a done ctx: %v", err)
	}
	if c.exclusive.Load() {
		t.Fatal("conn held after a failed NewAppender")
	}
	s.SetBeforeSendData(nil)
	if err := c.Append(context.Background(), "t", bigintCol, [][]any{{1}}); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	checkNoLeaks(t)
}

func TestNewAppenderErrors(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	ctx := context.Background()
	if _, err := c.NewAppender(ctx, "", "t", nil); err == nil {
		t.Fatal("no columns")
	}
	if _, err := c.NewAppender(ctx, "", "", bigintCol); err == nil {
		t.Fatal("no table")
	}
	if _, err := c.NewAppender(ctx, "", "t", []AppendColumn{{Name: "v", Type: "INTEGER; DROP TABLE t"}}); err == nil ||
		!strings.Contains(err.Error(), `column "v"`) {
		t.Fatalf("invalid type: %v", err)
	}
	if s.Count(message.TypePrepareRequest) != 0 {
		t.Fatal("validation errors must not reach the server")
	}

	// A type the server resolves but the driver cannot send.
	union := structOf(logicaltype.IDUnion, child("", tp(logicaltype.IDUTinyint)), child("a", typInteger))
	s.Script("SELECT NULL::UNION(a INTEGER) LIMIT 0", quacktest.Rows([]string{"c0"}, []logicaltype.Type{union}, nil, 1, 1))
	if _, err := c.NewAppender(ctx, "", "t", []AppendColumn{{Name: "u", Type: "UNION(a INTEGER)"}}); err == nil ||
		!strings.Contains(err.Error(), `column "u": UNION is not supported`) || c.exclusive.Load() {
		t.Fatalf("unsupported type: %v", err)
	}

	// The server rejects the INSERT (unknown table).
	s.SetRawHook(func(w http.ResponseWriter, h message.Header, body message.Body) bool {
		p, ok := body.(message.PrepareRequest)
		if !ok || !strings.Contains(p.SQL, `"missing"`) {
			return false
		}
		quacktest.RawResponse(w, message.Header{ConnectionID: h.ConnectionID}, message.ErrorResponse{
			Message: "Table with name missing does not exist!", ExceptionType: "Catalog"})
		return true
	})
	_, err := c.NewAppender(ctx, "main", "missing", bigintCol)
	var qe *Error
	if !errors.As(err, &qe) || qe.Type != "Catalog" {
		t.Fatalf("PREPARE error: %s", errString(err))
	}
	if c.exclusive.Load() || !c.IsValid() {
		t.Fatal("conn must be free and valid")
	}
	if err := c.Append(ctx, "missing", bigintCol, [][]any{{1}}); !errors.As(err, &qe) {
		t.Fatalf("Append PREPARE error: %v", err)
	}

	// A closed connection.
	_ = c.Close()
	if _, err := c.NewAppender(ctx, "", "t", bigintCol); !errors.Is(err, errConnClosed) {
		t.Fatalf("closed conn: %v", err)
	}
	if err := c.Append(ctx, "t", bigintCol, nil); !errors.Is(err, errConnClosed) {
		t.Fatalf("closed conn Append: %v", err)
	}
}

func TestAppenderConnClosedMidStream(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	a := newAppender(t, c, bigintCol)
	_ = a.AppendRow(1)
	_ = c.Close()
	if err := a.Flush(); !errors.Is(err, errConnClosed) {
		t.Fatalf("Flush on a closed conn: %v", err)
	}
	if err := a.Abort(); err != nil {
		t.Fatal(err)
	}

	// A session lost mid-stream marks the conn bad; the Appender fails.
	c = openConn(t, s)
	a = newAppender(t, c, bigintCol)
	_ = a.AppendRow(1)
	s.DropConnections()
	err := a.Flush()
	var qe *Error
	if !errors.As(err, &qe) || !strings.Contains(qe.Message, "Invalid connection id") || c.IsValid() {
		t.Fatalf("lost session: %s valid=%v", errString(err), c.IsValid())
	}
	if errors.Is(err, driver.ErrBadConn) {
		t.Fatal("a mid-stream failure must not ask database/sql to retry")
	}
	if err2 := a.AppendRow(2); !errors.Is(err2, err) {
		t.Fatalf("AppendRow after a lost session: %v", err2)
	}
	if _, err := a.Close(); err == nil {
		t.Fatal("Close after a lost session must fail")
	}
}

// A failure while encoding (a value that bypassed conversion) is reported
// with its rows and cancels the INSERT.
func TestAppenderEncodeError(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s)
	a := newAppender(t, c, bigintCol)
	a.mu.Lock()
	err := a.add([]any{"1"})
	if err == nil {
		err = a.add([]any{int64(2)}) // not text
	}
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Close()
	if err == nil || !strings.HasPrefix(err.Error(), "quackdriver: Appender: encode rows 0-1: message:") {
		t.Fatalf("encode error: %v", err)
	}
	if !cancelled(s, a.q.uuid) || c.exclusive.Load() {
		t.Fatal("encode failure must cancel and free")
	}
}

// TestConnAppend: Conn.Append streams the rows as text; a row it cannot
// convert, or one with the wrong number of values, aborts the statement (a
// CANCEL), so nothing is inserted.
func TestConnAppend(t *testing.T) {
	s := newServer(t)
	checkChunkTypes(t, s)
	db := openDB(t, s)
	cols := []AppendColumn{{Name: "id", Type: "INTEGER"}, {Name: "name", Type: "VARCHAR"}}
	ctx := context.Background()
	rawConn(t, db, func(c *Conn) {
		if err := c.Append(ctx, "people", cols, [][]any{{1, "alice"}, {int8(2), nil}}); err != nil {
			t.Fatal(err)
		}
		err := c.Append(ctx, "people", cols, [][]any{{3, "bob"}, {4, 5}})
		if ve := (*ValueError)(nil); !errors.As(err, &ve) || ve.Row != 1 || ve.Column != 1 ||
			err.Error() != `quackdriver: row 1 column "name": VARCHAR expects string, got int` {
			t.Fatalf("conversion error: %v", err)
		}
		if err := c.Append(ctx, "people", cols, [][]any{{1}}); err == nil || err.Error() != "quackdriver: row 0 has 1 values, want 2" {
			t.Fatalf("value count: %v", err)
		}
		if err := c.AppendWithSchema(ctx, "main", "people", cols[:1], [][]any{{5}}); err != nil {
			t.Fatal(err)
		}
	})
	var inserts []quacktest.Event
	for _, e := range s.EventsOf(message.TypePrepareRequest) {
		if strings.HasPrefix(e.SQL, "INSERT") {
			inserts = append(inserts, e)
		}
	}
	if len(inserts) != 4 || !strings.HasPrefix(inserts[3].SQL, `INSERT INTO "main"."people" ("id") SELECT "c0"::INTEGER FROM`) {
		t.Fatalf("INSERTs %v", inserts)
	}
	id, _ := prototypeTypes(t, inserts[0].SQL)
	if got := s.Stream(id).Rows(); !reflect.DeepEqual(got, [][]any{{"1", "alice"}, {"2", nil}}) {
		t.Fatalf("rows %#v", got)
	}
	for _, e := range inserts[1:3] {
		if !cancelled(s, e.QueryUUID) {
			t.Fatalf("the aborted INSERT was not cancelled: %s", e.SQL)
		}
	}
}
