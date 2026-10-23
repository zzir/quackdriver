package quacktest

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
)

var bigint = logicaltype.Type{ID: logicaltype.IDBigint}

// client is a minimal Quack client for exercising the server.
type client struct {
	t    *testing.T
	s    *Server
	conn string
}

func (c *client) post(body message.Body) (message.Header, message.Body, error) {
	c.t.Helper()
	req, err := message.Encode(message.Header{ConnectionID: c.conn, HasQueryID: true, ClientQueryID: 1}, body)
	if err != nil {
		c.t.Fatal(err)
	}
	resp, err := http.Post(c.s.ts.URL+"/quack", "application/vnd.duckdb", bytes.NewReader(req))
	if err != nil {
		return message.Header{}, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return message.Header{}, nil, err
	}
	h, b, err := message.Decode(raw)
	if err != nil {
		c.t.Fatalf("decode response: %v", err)
	}
	return h, b, nil
}

func (c *client) do(body message.Body) message.Body {
	c.t.Helper()
	_, b, err := c.post(body)
	if err != nil {
		c.t.Fatal(err)
	}
	return b
}

func (c *client) wantError(body message.Body, substr string) message.ErrorResponse {
	c.t.Helper()
	b := c.do(body)
	er, ok := b.(message.ErrorResponse)
	if !ok || !strings.Contains(er.Message, substr) {
		c.t.Fatalf("%s: got %#v, want error containing %q", body.MessageType(), b, substr)
	}
	return er
}

func connect(t *testing.T, s *Server) *client {
	t.Helper()
	c := &client{t: t, s: s}
	h, b, err := c.post(message.ConnectionRequest{
		AuthString: "tok_1234", MinSupportedQuackVersion: 3, MaxSupportedQuackVersion: 3, HeartbeatTimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := b.(message.ConnectionResponse)
	if !ok || h.ConnectionID == "" || resp.QuackVersion != 3 || resp.HeartbeatTimeoutSeconds != 60 {
		t.Fatalf("handshake: %+v %#v", h, b)
	}
	c.conn = h.ConnectionID
	return c
}

func newServer(t *testing.T) *Server {
	t.Helper()
	s := New("tok_1234")
	t.Cleanup(s.Close)
	return s
}

func TestHandshake(t *testing.T) {
	s := newServer(t)
	c := &client{t: t, s: s}
	c.wantError(message.ConnectionRequest{AuthString: "tok_1234", MinSupportedQuackVersion: 1, MaxSupportedQuackVersion: 1, HeartbeatTimeoutSeconds: 60},
		"Unsupported Quack version - server only supports version 3 of quack")
	c.wantError(message.ConnectionRequest{AuthString: "tok_1234", MinSupportedQuackVersion: 3, MaxSupportedQuackVersion: 3},
		"heartbeat_timeout out of range")
	c.wantError(message.ConnectionRequest{AuthString: "nope", MinSupportedQuackVersion: 3, MaxSupportedQuackVersion: 3, HeartbeatTimeoutSeconds: 1},
		"Authentication failed")
	cc := connect(t, s)
	if len(s.Connections()) != 1 || s.Connections()[0] != cc.conn {
		t.Fatal("session not registered")
	}
	if !strings.HasPrefix(s.DSN("fetch_ahead=0"), "quack://127.0.0.1:") || !strings.HasSuffix(s.DSN("fetch_ahead=0"), "?token=tok_1234&fetch_ahead=0") {
		t.Fatalf("DSN %q", s.DSN("fetch_ahead=0"))
	}
	ev := s.EventsOf(message.TypeConnectionRequest)
	if len(ev) != 4 || ev[3].Connect.AuthString != "tok_1234" || ev[0].Response != message.TypeErrorResponse {
		t.Fatalf("events %+v", ev)
	}
	// GET / banner, wrong path
	resp, err := http.Get(s.ts.URL + "/")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	resp, _ = http.Post(s.ts.URL+"/other", "x", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /other: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSessionLifecycle(t *testing.T) {
	s := newServer(t)
	c := connect(t, s)
	s.ExpireLeases() // the handshake renewed the session during this lease
	if _, ok := c.do(message.HeartbeatRequest{}).(message.SuccessResponse); !ok {
		t.Fatal("heartbeat")
	}
	s.ExpireLeases() // the heartbeat renewed it during this one
	s.ExpireLeases() // nothing did during this one
	c.wantError(message.HeartbeatRequest{}, "Connection heartbeat lease expired")
	c.wantError(message.HeartbeatRequest{}, "Invalid connection id")

	// The same on the clock; the sleep only bounds the lease from below.
	s.SetLeaseUnit(time.Millisecond)
	c = connect(t, s) // lease 60 units = 60ms
	time.Sleep(70 * time.Millisecond)
	c.wantError(message.HeartbeatRequest{}, "Connection heartbeat lease expired")

	s.SetLeaseUnit(time.Second)
	c = connect(t, s)
	if _, ok := c.do(message.DisconnectMessage{}).(message.SuccessResponse); !ok {
		t.Fatal("disconnect")
	}
	c.wantError(message.DisconnectMessage{}, "Invalid connection id")

	c = connect(t, s)
	s.DropConnections()
	c.wantError(message.PrepareRequest{SQL: "SELECT 1"}, "Invalid connection id")

	c = connect(t, s)
	s.Invalidate("boom")
	er := c.wantError(message.HeartbeatRequest{}, "The server database has been invalidated")
	if !er.MustInvalidate || !strings.Contains(er.Message, `Original error: "boom"`) {
		t.Fatalf("%+v", er)
	}
	c.wantError(message.SuccessResponse{}, "invalidated")
}

func TestUnsupportedMessage(t *testing.T) {
	s := newServer(t)
	c := connect(t, s)
	c.wantError(message.SuccessResponse{}, "Unsupported message type for server")
	c.wantError(message.PrepareRequest{SQL: "SELECT unknown", QueryUUID: codec.HugeInt{Lower: 1}}, "no script")
	// BEGIN/COMMIT/ROLLBACK are scripted by default.
	for _, q := range []string{"BEGIN", "commit;", "ROLLBACK"} {
		p, ok := c.do(message.PrepareRequest{SQL: q, QueryUUID: codec.HugeInt{Lower: 2}}).(message.PrepareResponse)
		if !ok || len(p.ResultNames) != 1 || p.ResultNames[0] != "Success" {
			t.Fatalf("%s: %+v", q, p)
		}
	}
	// garbage request
	resp, err := http.Post(s.ts.URL+"/quack", "application/vnd.duckdb", strings.NewReader("garbage"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if _, b, err := message.Decode(raw); err != nil || !strings.Contains(b.(message.ErrorResponse).Message, "deserialize") {
		t.Fatalf("garbage: %v %#v", err, b)
	}
}

func TestPrepareInlineAndFetch(t *testing.T) {
	s := newServer(t)
	rows := make([][]any, 10)
	for i := range rows {
		rows[i] = []any{int64(i)}
	}
	res := Rows([]string{"i"}, []logicaltype.Type{bigint}, rows, 2, 1) // 5 batches of 2
	if len(res.Batches) != 5 {
		t.Fatalf("%d batches", len(res.Batches))
	}
	s.Script("SELECT i", res)
	c := connect(t, s)
	uuid := codec.HugeInt{Upper: 1, Lower: 2}
	p := c.do(message.PrepareRequest{SQL: "SELECT i", QueryUUID: uuid, InlineRows: message.Idx(3)}).(message.PrepareResponse)
	// 2 rows < 3 -> second batch also inline (4 rows), then stop
	if len(p.Results) != 2 || !p.NeedsMoreFetch || p.QueryUUID != uuid {
		t.Fatalf("PREPARE %+v", p)
	}
	c.wantError(message.FetchRequest{QueryUUID: uuid}, "FETCH_REQUEST is missing its batch index")
	c.wantError(message.FetchRequest{QueryUUID: codec.HugeInt{Lower: 9}, BatchIndex: 1}, "Result has been closed")

	f := c.do(message.FetchRequest{QueryUUID: uuid, BatchIndex: 1}).(message.FetchResponse)
	if len(f.Results) != 1 || f.BatchIndex != message.Idx(1) || f.Results[0].Rows()[0][0] != int64(4) {
		t.Fatalf("FETCH 1 %+v", f)
	}
	// re-serving the same index before an ack returns the same batch
	f = c.do(message.FetchRequest{QueryUUID: uuid, BatchIndex: 1}).(message.FetchResponse)
	if f.Results[0].Rows()[0][0] != int64(4) {
		t.Fatalf("re-serve %+v", f)
	}
	f = c.do(message.FetchRequest{QueryUUID: uuid, BatchIndex: 2, AckIndex: 1}).(message.FetchResponse)
	if f.Results[0].Rows()[0][0] != int64(6) {
		t.Fatalf("FETCH 2 %+v", f)
	}
	// asking for an acknowledged batch is a protocol violation
	c.wantError(message.FetchRequest{QueryUUID: uuid, BatchIndex: 1, AckIndex: 2}, "after an ack released it")
	f = c.do(message.FetchRequest{QueryUUID: uuid, BatchIndex: 3, AckIndex: 2}).(message.FetchResponse)
	if f.Results[0].Rows()[0][0] != int64(8) {
		t.Fatalf("FETCH 3 %+v", f)
	}
	f = c.do(message.FetchRequest{QueryUUID: uuid, BatchIndex: 4, AckIndex: 3}).(message.FetchResponse)
	if len(f.Results) != 0 || f.TotalBatches != message.Idx(3) {
		t.Fatalf("terminal %+v", f)
	}
	// default inline (24576 rows) inlines everything
	p = c.do(message.PrepareRequest{SQL: "SELECT i", QueryUUID: uuid}).(message.PrepareResponse)
	if len(p.Results) != 5 || p.NeedsMoreFetch {
		t.Fatalf("default inline %+v", p)
	}
}

func TestFetchInjections(t *testing.T) {
	s := newServer(t)
	rows := [][]any{{int64(1)}, {int64(2)}, {int64(3)}}
	res := Rows([]string{"i"}, []logicaltype.Type{bigint}, rows, 1, 1)
	res.WrongBatchIndex = map[uint64]uint64{1: 7}
	res.FetchErrors = map[uint64]*message.ErrorResponse{2: {Message: "bad batch", ExceptionType: "IO"}}
	res.DropFetchResponse = map[uint64]int{3: 1}
	over := uint64(42)
	res.TotalOverride = &over
	s.Script("SELECT x", res)
	c := connect(t, s)
	uuid := codec.HugeInt{Lower: 5}
	c.do(message.PrepareRequest{SQL: "SELECT x", QueryUUID: uuid, InlineRows: message.Idx(0)})
	if f := c.do(message.FetchRequest{QueryUUID: uuid, BatchIndex: 1}).(message.FetchResponse); f.BatchIndex != message.Idx(7) {
		t.Fatalf("wrong index %+v", f)
	}
	er := c.wantError(message.FetchRequest{QueryUUID: uuid, BatchIndex: 2}, "bad batch")
	if er.ExceptionType != "IO" {
		t.Fatalf("%+v", er)
	}
	if _, _, err := c.post(message.FetchRequest{QueryUUID: uuid, BatchIndex: 3}); err == nil {
		t.Fatal("expected a dropped connection")
	}
	if f := c.do(message.FetchRequest{QueryUUID: uuid, BatchIndex: 3}).(message.FetchResponse); len(f.Results) != 1 {
		t.Fatalf("after drop %+v", f)
	}
	if f := c.do(message.FetchRequest{QueryUUID: uuid, BatchIndex: 4}).(message.FetchResponse); f.TotalBatches != message.Idx(42) {
		t.Fatalf("total %+v", f)
	}
	var dropped bool
	for _, e := range s.EventsOf(message.TypeFetchRequest) {
		if e.BatchIndex == 3 && e.Response == message.TypeInvalid {
			dropped = true
		}
	}
	if !dropped {
		t.Fatal("dropped response not recorded")
	}
}

func TestCancelAndSupersede(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT a", Rows([]string{"i"}, []logicaltype.Type{bigint}, [][]any{{int64(1)}, {int64(2)}}, 1, 1))
	c := connect(t, s)
	u1 := codec.HugeInt{Lower: 1}
	u2 := codec.HugeInt{Lower: 2}
	c.do(message.PrepareRequest{SQL: "SELECT a", QueryUUID: u1, InlineRows: message.Idx(0)})
	c.wantError(message.CancelRequest{QueryUUID: u2}, "Attempted to cancel a different query")
	if _, ok := c.do(message.CancelRequest{QueryUUID: u1}).(message.SuccessResponse); !ok {
		t.Fatal("cancel")
	}
	c.wantError(message.FetchRequest{QueryUUID: u1, BatchIndex: 1}, "Interrupted: query was cancelled")
	// cancelling again (same uuid) is accepted
	if _, ok := c.do(message.CancelRequest{QueryUUID: u1}).(message.SuccessResponse); !ok {
		t.Fatal("second cancel")
	}

	c.do(message.PrepareRequest{SQL: "SELECT a", QueryUUID: u1, InlineRows: message.Idx(0)})
	c.do(message.PrepareRequest{SQL: "SELECT a", QueryUUID: u2, InlineRows: message.Idx(0)})
	// like the real server: the statement slot now names u2
	c.wantError(message.FetchRequest{QueryUUID: u1, BatchIndex: 1}, "Result has been closed")
	if f := c.do(message.FetchRequest{QueryUUID: u2, BatchIndex: 1}).(message.FetchResponse); len(f.Results) != 1 {
		t.Fatalf("%+v", f)
	}
	// wildcard cancel
	if _, ok := c.do(message.CancelRequest{}).(message.SuccessResponse); !ok {
		t.Fatal("wildcard cancel")
	}
	c.wantError(message.FetchRequest{QueryUUID: u2, BatchIndex: 2}, "Interrupted")
	if _, ok := c.do(message.Acknowledgement{QueryUUID: u2}).(message.SuccessResponse); !ok {
		t.Fatal("ack")
	}
}

func TestPrepareGateAndError(t *testing.T) {
	s := newServer(t)
	gate := make(chan struct{})
	s.Script("SELECT gated", Result{Names: []string{"x"}, Types: []logicaltype.Type{bigint}, PrepareGate: gate})
	s.Script("SELECT err", Result{Error: &message.ErrorResponse{Message: "nope", ExceptionType: "Binder", MustInvalidate: true}})
	c := connect(t, s)
	er := c.wantError(message.PrepareRequest{SQL: "SELECT err"}, "nope")
	if er.ExceptionType != "Binder" || !er.MustInvalidate {
		t.Fatalf("%+v", er)
	}
	done := make(chan message.Body, 1)
	go func() { done <- c.do(message.PrepareRequest{SQL: "SELECT gated"}) }()
	select {
	case <-done:
		t.Fatal("PREPARE did not wait for its gate")
	case <-time.After(30 * time.Millisecond):
	}
	// heartbeats are served meanwhile
	if _, ok := c.do(message.HeartbeatRequest{}).(message.SuccessResponse); !ok {
		t.Fatal("heartbeat during PREPARE")
	}
	close(gate)
	if _, ok := (<-done).(message.PrepareResponse); !ok {
		t.Fatal("gated PREPARE")
	}
}

func TestSendDataStream(t *testing.T) {
	s := newServer(t)
	var hookCalls int
	s.SetSendDataHook(func(id string, req message.SendDataRequest) *message.ErrorResponse {
		hookCalls++
		if id == "failing" && req.BatchIndex == message.Idx(2) {
			return &message.ErrorResponse{Message: "constraint violated", ExceptionType: "Constraint"}
		}
		return nil
	})
	c := connect(t, s)
	uuid := codec.HugeInt{Lower: 77}
	sql := "INSERT INTO t SELECT * FROM scan_data_from_quack_client('it''s', NULL::STRUCT(\"a\" BIGINT), ordered := true)"
	c.wantError(message.PrepareRequest{SQL: sql, QueryUUID: uuid}, "inline_rows = 0")
	p := c.do(message.PrepareRequest{SQL: sql, QueryUUID: uuid, InlineRows: message.Idx(0)}).(message.PrepareResponse)
	if !p.NeedsMoreFetch || len(p.ResultTypes) != 0 {
		t.Fatalf("%+v", p)
	}
	ch := func(n int) message.DataChunk {
		vals := make(message.Values, n)
		for i := range vals {
			vals[i] = int64(i)
		}
		return message.DataChunk{RowCount: n, Types: []logicaltype.Type{bigint}, Columns: []message.Column{vals}}
	}
	c.wantError(message.SendDataRequest{StreamID: "other"}, "No active data stream 'other'")
	c.wantError(message.SendDataRequest{StreamID: "it's", Chunks: []message.DataChunk{ch(1)}}, "missing its batch index")

	// FETCH long-polls until the stream ends.
	result := make(chan message.Body, 1)
	go func() { result <- c.do(message.FetchRequest{QueryUUID: uuid, BatchIndex: 1}) }()
	for i, n := range []int{3, 2} {
		if _, ok := c.do(message.SendDataRequest{StreamID: "it's", Chunks: []message.DataChunk{ch(n)}, BatchIndex: message.Idx(uint64(i + 1))}).(message.SendDataResponse); !ok {
			t.Fatal("send")
		}
	}
	// duplicate index is dropped
	c.do(message.SendDataRequest{StreamID: "it's", Chunks: []message.DataChunk{ch(9)}, BatchIndex: message.Idx(1)})
	select {
	case b := <-result:
		t.Fatalf("FETCH answered before the stream ended: %#v", b)
	case <-time.After(20 * time.Millisecond):
	}
	if _, ok := c.do(message.SendDataRequest{StreamID: "it's", TotalBatches: message.Idx(2)}).(message.SendDataResponse); !ok {
		t.Fatal("terminal")
	}
	f := (<-result).(message.FetchResponse)
	if len(f.Results) != 1 || f.Results[0].Rows()[0][0] != int64(5) {
		t.Fatalf("count %+v", f)
	}
	if f := c.do(message.FetchRequest{QueryUUID: uuid, BatchIndex: 2, AckIndex: 1}).(message.FetchResponse); len(f.Results) != 0 || f.TotalBatches != message.Idx(1) {
		t.Fatalf("terminal FETCH %+v", f)
	}
	st := s.Stream("it's")
	if st == nil || !st.Finished || *st.Total != 2 || len(st.Rows()) != 5 || st.Error != "" {
		t.Fatalf("stream %+v", st)
	}
	if s.Stream("missing") != nil {
		t.Fatal("unknown stream")
	}

	// a stream with a missing batch fails on the terminal message
	sql2 := "INSERT INTO t SELECT * FROM scan_data_from_quack_client('short', NULL::STRUCT(\"a\" BIGINT))"
	c.do(message.PrepareRequest{SQL: sql2, QueryUUID: uuid, InlineRows: message.Idx(0)})
	c.do(message.SendDataRequest{StreamID: "short", Chunks: []message.DataChunk{ch(1)}, BatchIndex: message.Idx(2)})
	c.wantError(message.SendDataRequest{StreamID: "short", TotalBatches: message.Idx(2)}, "Quack stream ended with 1 of 2 batches received")
	c.wantError(message.FetchRequest{QueryUUID: uuid, BatchIndex: 1}, "1 of 2 batches")

	// oversized chunk
	sql3 := "INSERT INTO t SELECT * FROM scan_data_from_quack_client('big', NULL::STRUCT(\"a\" BIGINT))"
	c.do(message.PrepareRequest{SQL: sql3, QueryUUID: uuid, InlineRows: message.Idx(0)})
	c.wantError(message.SendDataRequest{StreamID: "big", Chunks: []message.DataChunk{ch(MaxChunkRows + 1)}, BatchIndex: message.Idx(1)}, "exceeds 2048")

	// hook-injected failure, and a new PREPARE drops the old stream
	sql4 := "INSERT INTO t SELECT * FROM scan_data_from_quack_client('failing', NULL::STRUCT(\"a\" BIGINT))"
	c.do(message.PrepareRequest{SQL: sql4, QueryUUID: uuid, InlineRows: message.Idx(0)})
	c.do(message.SendDataRequest{StreamID: "failing", Chunks: []message.DataChunk{ch(1)}, BatchIndex: message.Idx(1)})
	c.wantError(message.SendDataRequest{StreamID: "failing", Chunks: []message.DataChunk{ch(1)}, BatchIndex: message.Idx(2)}, "constraint violated")
	c.wantError(message.SendDataRequest{StreamID: "failing", TotalBatches: message.Idx(2)}, "constraint violated")
	c.do(message.PrepareRequest{SQL: "BEGIN", QueryUUID: codec.HugeInt{Lower: 1}})
	c.wantError(message.SendDataRequest{StreamID: "failing", TotalBatches: message.Idx(2)}, "No active data stream")
	if hookCalls == 0 {
		t.Fatal("hook not called")
	}
	if len(s.EventsOf(message.TypeSendDataRequest)) == 0 || s.EventsOf(message.TypeSendDataRequest)[0].SendData == nil {
		t.Fatal("SEND_DATA not recorded")
	}
}
