// Package quacktest provides an in-process fake Quack v3 server (DuckDB 2.0
// protocol) for driver tests. It speaks the real wire format through
// internal/message and mimics the server-side state machine of duckdb-quack:
// the handshake with its version and heartbeat-lease checks, lease expiry,
// PREPARE with inline batches, FETCH with dense batch indices, ack-driven
// release of served batches and same-index re-serving, CANCEL, HEARTBEAT,
// DISCONNECT, SEND_DATA streams, and statements superseded by a new
// PREPARE. Query results are scripted per SQL text; errors (with exception
// types, extra info and must_invalidate) can be injected at every step.
//
// Every request is recorded as an Event so tests can assert the exact
// protocol the driver spoke.
package quacktest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
)

// DefaultInlineRows is the server's quack_prepare_inline_rows default.
const DefaultInlineRows = 24576

// maxHeartbeatTimeoutSeconds mirrors MAX_HEARTBEAT_TIMEOUT_SECONDS.
const maxHeartbeatTimeoutSeconds = uint64(1<<63-1) / 1000

// MaxChunkRows is the largest chunk the server accepts in SEND_DATA.
const MaxChunkRows = 2048

// Result scripts the answer to one SQL statement.
type Result struct {
	Names []string
	Types []logicaltype.Type
	// Batches are the result's batches in dense order (index 1, 2, ...);
	// each batch holds one or more chunks. PREPARE returns leading batches
	// inline while fewer than inline_rows rows were inlined (default
	// DefaultInlineRows), exactly like the real server; FETCH serves the
	// rest.
	Batches [][]message.DataChunk
	// NeedsMoreFetch forces needs_more_fetch even when PREPARE inlined every
	// batch (the real server reports true when its producer had not
	// finished yet); the first FETCH then gets the terminal response.
	NeedsMoreFetch bool

	// Error makes PREPARE fail with this error.
	Error *message.ErrorResponse
	// FetchErrors makes FETCH of a client-visible batch index fail.
	FetchErrors map[uint64]*message.ErrorResponse

	// PrepareGate, when set, holds PREPARE until it is closed (or the client
	// gives up on the request).
	PrepareGate <-chan struct{}
	// BeforeFetch runs before a FETCH of a client-visible index is served;
	// it may sleep or block (the request context ends when the client gives
	// up). It runs outside the server lock.
	BeforeFetch func(ctx context.Context, index uint64)
	// DropFetchResponse aborts the HTTP connection after serving the given
	// client-visible index this many times (the batch stays retained, as
	// when a response is lost on the way).
	DropFetchResponse map[uint64]int
	// WrongBatchIndex makes the FETCH response for an index echo another
	// index.
	WrongBatchIndex map[uint64]uint64
	// OmitTotal leaves total_batches out of the terminal FETCH response.
	OmitTotal bool
	// TotalOverride, when non-nil, replaces the terminal total_batches.
	TotalOverride *uint64
}

// Rows builds a Result from row-major values, cutting them into chunks of
// rowsPerChunk rows and batches of chunksPerBatch chunks.
func Rows(names []string, types []logicaltype.Type, rows [][]any, rowsPerChunk, chunksPerBatch int) Result {
	if rowsPerChunk <= 0 {
		rowsPerChunk = MaxChunkRows
	}
	if chunksPerBatch <= 0 {
		chunksPerBatch = 1
	}
	var chunks []message.DataChunk
	for start := 0; start < len(rows); start += rowsPerChunk {
		end := min(start+rowsPerChunk, len(rows))
		cols := make([]message.Column, len(types))
		for c := range types {
			vals := make(message.Values, end-start)
			for r := start; r < end; r++ {
				vals[r-start] = rows[r][c]
			}
			cols[c] = vals
		}
		chunks = append(chunks, message.DataChunk{RowCount: end - start, Types: types, Columns: cols})
	}
	res := Result{Names: names, Types: types}
	for start := 0; start < len(chunks); start += chunksPerBatch {
		end := min(start+chunksPerBatch, len(chunks))
		res.Batches = append(res.Batches, chunks[start:end])
	}
	return res
}

// Count is the result DuckDB returns for DML: one BIGINT row named "Count".
func Count(n int64) Result {
	return Rows([]string{"Count"}, []logicaltype.Type{{ID: logicaltype.IDBigint}}, [][]any{{n}}, 0, 0)
}

// Success is the result of statements that return nothing (BEGIN, CREATE,
// ...): a BOOLEAN "Success" column without rows.
func Success() Result {
	return Result{Names: []string{"Success"}, Types: []logicaltype.Type{{ID: logicaltype.IDBoolean}}}
}

// Event is one request the server received.
type Event struct {
	Type          message.Type
	ConnectionID  string
	HasQueryID    bool
	ClientQueryID uint64
	Header        http.Header
	Time          time.Time
	// Size is the request body's length in bytes.
	Size int

	// CONNECTION_REQUEST
	Connect *message.ConnectionRequest
	// PREPARE_REQUEST
	SQL        string
	QueryUUID  codec.HugeInt // PREPARE, FETCH, CANCEL, ACKNOWLEDGEMENT
	InlineRows message.OptionalIdx
	// FETCH_REQUEST
	BatchIndex uint64
	AckIndex   uint64
	// SEND_DATA_REQUEST
	SendData *message.SendDataRequest

	// Response is the type the server answered with (TypeInvalid when the
	// connection was dropped); Error its message for ERROR_RESPONSE.
	Response message.Type
	Error    string
}

// Stream is the server-side state of one SEND_DATA stream.
type Stream struct {
	ID string
	// Batches holds the chunks received per batch index.
	Batches map[uint64][]message.DataChunk
	// Total is the terminal total_batches, when received.
	Total *uint64
	// Finished is set once the terminal message matched the batches.
	Finished bool
	// Error is the stream's failure, if any.
	Error string
}

// Rows returns the stream's rows in batch order.
func (s *Stream) Rows() [][]any {
	idx := make([]uint64, 0, len(s.Batches))
	for i := range s.Batches {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
	var out [][]any
	for _, i := range idx {
		for _, ch := range s.Batches[i] {
			out = append(out, ch.Rows()...)
		}
	}
	return out
}

// Server is a fake Quack v3 server on an httptest.Server.
type Server struct {
	ts *httptest.Server

	mu sync.Mutex
	// configuration (guarded by mu; use the setters)
	token             string
	leaseUnit         time.Duration
	quackVersion      uint64
	heartbeatOverride *uint64
	invalidated       string
	connectHook       func(message.ConnectionRequest) *message.ErrorResponse
	rawHook           func(w http.ResponseWriter, h message.Header, body message.Body) bool
	sendDataHook      func(streamID string, req message.SendDataRequest) *message.ErrorResponse
	beforeSendData    func(ctx context.Context, req message.SendDataRequest)
	scripts           map[string]Result
	scriptFunc        func(sql string) (Result, bool)

	conns   map[string]*connState
	events  []Event
	streams map[string]*Stream
}

type connState struct {
	id        string
	lease     time.Duration
	lastRenew time.Time
	stmt      *stmtState
	// abort: the uuid and error of the last aborted statement
	abortedUUID codec.HugeInt
	abortErr    string
	streams     map[string]*streamState
}

type stmtState struct {
	uuid    codec.HugeInt
	res     Result
	prepare int // dense batches PREPARE consumed
	acked   int // dense batches released by acks
	served  map[int]bool
	drops   map[uint64]int
	// client data
	stream  *streamState
	errMsg  *message.ErrorResponse // the statement failed (stream error)
	done    chan struct{}          // closed when a client-data statement can answer FETCH
	doneSet bool
}

type streamState struct {
	pub  *Stream
	stmt *stmtState
}

var dataStreamPattern = regexp.MustCompile(`scan_data_from_quack_client\('((?:[^']|'')*)'`)

// New starts a fake server requiring token (empty: any token is accepted).
// Close it with Close (or t.Cleanup(s.Close)).
func New(token string) *Server {
	s := &Server{
		token:        token,
		leaseUnit:    time.Second,
		quackVersion: message.QuackVersion,
		scripts:      make(map[string]Result),
		conns:        make(map[string]*connState),
		streams:      make(map[string]*Stream),
	}
	s.ts = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	return s
}

// Close shuts the server down.
func (s *Server) Close() { s.ts.Close() }

// DSN returns a quack:// DSN for this server with the token and extra
// query parameters (e.g. "fetch_ahead=0").
func (s *Server) DSN(params ...string) string {
	s.mu.Lock()
	tok := s.token
	s.mu.Unlock()
	q := []string{}
	if tok != "" {
		q = append(q, "token="+tok)
	}
	q = append(q, params...)
	dsn := "quack://" + strings.TrimPrefix(s.ts.URL, "http://")
	if len(q) > 0 {
		dsn += "?" + strings.Join(q, "&")
	}
	return dsn
}

// Script sets the result for an exact SQL text.
func (s *Server) Script(sql string, r Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts[sql] = r
}

// ScriptFunc sets a fallback consulted for SQL without an exact script.
func (s *Server) ScriptFunc(f func(sql string) (Result, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scriptFunc = f
}

// SetLeaseUnit sets the real duration of one heartbeat-timeout "second"
// (default time.Second), so lease expiry can be tested quickly.
func (s *Server) SetLeaseUnit(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaseUnit = d
}

// SetQuackVersion sets the quack_version the CONNECTION_RESPONSE reports.
func (s *Server) SetQuackVersion(v uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quackVersion = v
}

// SetGrantedHeartbeat makes the CONNECTION_RESPONSE report this lease
// instead of echoing the request.
func (s *Server) SetGrantedHeartbeat(seconds uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeatOverride = &seconds
}

// SetConnectHook lets a test reject CONNECTION_REQUESTs.
func (s *Server) SetConnectHook(f func(message.ConnectionRequest) *message.ErrorResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connectHook = f
}

// SetRawHook installs a hook that sees every decoded request first; when it
// returns true it has written the HTTP response itself (for example garbage
// bytes or a v1-style answer) and normal handling is skipped.
func (s *Server) SetRawHook(f func(w http.ResponseWriter, h message.Header, body message.Body) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rawHook = f
}

// SetSendDataHook lets a test fail SEND_DATA requests.
func (s *Server) SetSendDataHook(f func(streamID string, req message.SendDataRequest) *message.ErrorResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendDataHook = f
}

// SetBeforeSendData installs a hook that runs before each SEND_DATA request
// is handled, outside the server lock, so it may sleep or block (ctx ends
// when the client gives up on the request; the request is then dropped).
func (s *Server) SetBeforeSendData(f func(ctx context.Context, req message.SendDataRequest)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeSendData = f
}

// Invalidate makes the server answer every request like a server whose
// database was invalidated (must_invalidate set).
func (s *Server) Invalidate(original string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated = original
}

// DropConnections forgets every session, as after a server restart: later
// requests get "Invalid connection id".
func (s *Server) DropConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns = make(map[string]*connState)
}

// Connections returns the ids of the live sessions.
func (s *Server) Connections() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.conns))
	for id := range s.conns {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Events returns a copy of the recorded requests.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// EventsOf returns the recorded requests of one type.
func (s *Server) EventsOf(t message.Type) []Event {
	var out []Event
	for _, e := range s.Events() {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// Count returns how many requests of type t were received.
func (s *Server) Count(t message.Type) int { return len(s.EventsOf(t)) }

// ResetEvents clears the recorded requests.
func (s *Server) ResetEvents() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = nil
}

// Stream returns a snapshot of a SEND_DATA stream (nil if unknown).
func (s *Server) Stream(id string) *Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.streams[id]
	if !ok {
		return nil
	}
	cp := *st
	cp.Batches = make(map[uint64][]message.DataChunk, len(st.Batches))
	for k, v := range st.Batches {
		cp.Batches[k] = v
	}
	return &cp
}

func (s *Server) record(e Event) int {
	e.Time = time.Now()
	s.events = append(s.events, e)
	return len(s.events) - 1
}

func errResp(format string, args ...any) *message.ErrorResponse {
	return &message.ErrorResponse{Message: fmt.Sprintf(format, args...), ExceptionType: "Invalid Input"}
}

// serveHTTP decodes one request, dispatches it and writes the answer.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		_, _ = io.WriteString(w, "This is a DuckDB Quack RPC endpoint (quacktest)\n")
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/quack" {
		http.NotFound(w, r)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	h, body, err := message.Decode(raw)
	if err != nil {
		s.write(w, message.Header{}, errResp("Failed to deserialize: %v", err))
		return
	}
	s.mu.Lock()
	hook := s.rawHook
	s.mu.Unlock()
	if hook != nil && hook(w, h, body) {
		return
	}
	connID, resp, drop := s.dispatch(r, h, body, len(raw))
	if drop {
		panic(http.ErrAbortHandler)
	}
	s.write(w, message.Header{ConnectionID: connID}, resp)
}

func (s *Server) write(w http.ResponseWriter, h message.Header, body message.Body) {
	if er, ok := body.(*message.ErrorResponse); ok {
		body = *er
	}
	out, err := message.Encode(h, body)
	if err != nil {
		out, _ = message.Encode(h, message.ErrorResponse{Message: "quacktest: encode: " + err.Error(), ExceptionType: "Internal"})
	}
	w.Header().Set("Content-Type", "application/vnd.duckdb")
	_, _ = w.Write(out)
}

// dispatch handles one decoded request and returns the connection id for
// the response header and the response. drop asks the caller to abort the
// HTTP connection instead of answering.
func (s *Server) dispatch(r *http.Request, h message.Header, body message.Body, size int) (connID string, resp message.Body, drop bool) {
	ev := Event{
		Type:          body.MessageType(),
		ConnectionID:  h.ConnectionID,
		HasQueryID:    h.HasQueryID,
		ClientQueryID: h.ClientQueryID,
		Header:        r.Header.Clone(),
		Size:          size,
	}
	s.mu.Lock()
	idx := s.record(ev)
	connID = h.ConnectionID
	finish := func(b message.Body, dropped bool) (string, message.Body, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		e := &s.events[idx]
		if dropped {
			e.Response = message.TypeInvalid
		} else {
			e.Response = b.MessageType()
			switch er := b.(type) {
			case *message.ErrorResponse:
				e.Error = er.Message
			case message.ErrorResponse:
				e.Error = er.Message
			}
		}
		return connID, b, dropped
	}
	// Annotate the event with the request's fields.
	e := &s.events[idx]
	switch b := body.(type) {
	case message.ConnectionRequest:
		cp := b
		e.Connect = &cp
	case message.PrepareRequest:
		e.SQL, e.QueryUUID, e.InlineRows = b.SQL, b.QueryUUID, b.InlineRows
	case message.FetchRequest:
		e.QueryUUID, e.BatchIndex, e.AckIndex = b.QueryUUID, b.BatchIndex, b.AckIndex
	case message.CancelRequest:
		e.QueryUUID = b.QueryUUID
	case message.Acknowledgement:
		e.QueryUUID = b.QueryUUID
	case message.SendDataRequest:
		cp := b
		e.SendData = &cp
	}

	if original := s.invalidated; original != "" {
		s.mu.Unlock()
		er := errResp("The server database has been invalidated and the server must be restarted before it "+
			"can be used again. Original error: \"%s\"", original)
		er.MustInvalidate = true
		return finish(er, false)
	}

	switch b := body.(type) {
	case message.ConnectionRequest:
		id, out := s.connect(b)
		s.mu.Unlock()
		if id != "" {
			connID = id
		}
		return finish(out, false)
	case message.PrepareRequest, message.FetchRequest, message.SendDataRequest, message.DisconnectMessage,
		message.CancelRequest, message.Acknowledgement, message.HeartbeatRequest:
	default:
		s.mu.Unlock()
		return finish(errResp("Unsupported message type for server"), false)
	}

	conn, er := s.session(h.ConnectionID)
	if er != nil {
		s.mu.Unlock()
		return finish(er, false)
	}
	switch b := body.(type) {
	case message.HeartbeatRequest:
		s.mu.Unlock()
		return finish(message.SuccessResponse{}, false)
	case message.DisconnectMessage:
		delete(s.conns, conn.id)
		s.abortLocked(conn, "connection closed during fetch")
		s.mu.Unlock()
		return finish(message.SuccessResponse{}, false)
	case message.CancelRequest:
		out := s.cancel(conn, b)
		s.mu.Unlock()
		return finish(out, false)
	case message.Acknowledgement:
		s.mu.Unlock()
		return finish(message.SuccessResponse{}, false)
	case message.PrepareRequest:
		return finish(s.prepare(r.Context(), conn, b))
	case message.FetchRequest:
		return finish(s.fetch(r.Context(), conn, b))
	case message.SendDataRequest:
		if hook := s.beforeSendData; hook != nil {
			s.mu.Unlock()
			hook(r.Context(), b)
			if r.Context().Err() != nil {
				return finish(message.SuccessResponse{}, true)
			}
			s.mu.Lock()
		}
		out := s.sendData(conn, b)
		s.mu.Unlock()
		return finish(out, false)
	}
	s.mu.Unlock()
	return finish(errResp("unreachable"), false)
}

// connect handles CONNECTION_REQUEST (s.mu held). It returns the new
// connection id ("" on failure) and the response.
func (s *Server) connect(b message.ConnectionRequest) (string, message.Body) {
	if b.MinSupportedQuackVersion > message.QuackVersion || b.MaxSupportedQuackVersion < message.QuackVersion {
		return "", errResp("Unsupported Quack version - server only supports version %d of quack", message.QuackVersion)
	}
	if b.HeartbeatTimeoutSeconds == 0 || b.HeartbeatTimeoutSeconds > maxHeartbeatTimeoutSeconds {
		return "", errResp("heartbeat_timeout out of range - must be between 1 and %d seconds", maxHeartbeatTimeoutSeconds)
	}
	if s.connectHook != nil {
		if er := s.connectHook(b); er != nil {
			return "", er
		}
	}
	if s.token != "" && b.AuthString != s.token {
		return "", errResp("Authentication failed")
	}
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	granted := b.HeartbeatTimeoutSeconds
	if s.heartbeatOverride != nil {
		granted = *s.heartbeatOverride
	}
	s.conns[id] = &connState{
		id:        id,
		lease:     time.Duration(b.HeartbeatTimeoutSeconds) * s.leaseUnit,
		lastRenew: time.Now(),
		streams:   make(map[string]*streamState),
	}
	return id, message.ConnectionResponse{
		ServerDuckdbVersion:     "v2.0.0",
		ServerPlatform:          "quacktest",
		QuackVersion:            s.quackVersion,
		HeartbeatTimeoutSeconds: granted,
	}
}

// session looks up and renews a session (s.mu held), like
// QuackServer::DispatchMessage + RenewConnectionLease.
func (s *Server) session(id string) (*connState, *message.ErrorResponse) {
	c := s.conns[id]
	if c == nil {
		return nil, errResp("Invalid connection id")
	}
	now := time.Now()
	if now.Sub(c.lastRenew) >= c.lease {
		delete(s.conns, id)
		s.abortLocked(c, "connection heartbeat lease expired")
		return nil, errResp("Connection heartbeat lease expired")
	}
	c.lastRenew = now
	return c, nil
}

// abortLocked aborts the session's statement (s.mu held).
func (s *Server) abortLocked(c *connState, reason string) {
	if c.stmt == nil {
		return
	}
	c.abortedUUID = c.stmt.uuid
	c.abortErr = reason
	if c.stmt.stream != nil {
		c.stmt.stream.pub.Error = reason
		delete(c.streams, c.stmt.stream.pub.ID)
	}
	c.stmt.finishClientData()
	c.stmt = nil
}

func (s *Server) cancel(c *connState, b message.CancelRequest) message.Body {
	wildcard := b.QueryUUID == (codec.HugeInt{})
	var active codec.HugeInt
	if c.stmt != nil {
		active = c.stmt.uuid
	} else {
		active = c.abortedUUID
	}
	if !wildcard && active != b.QueryUUID {
		return errResp("Attempted to cancel a different query with id '%v' instead of '%v'", b.QueryUUID, active)
	}
	s.abortLocked(c, "Interrupted: query was cancelled")
	return message.SuccessResponse{}
}

func (s *Server) lookup(sql string) (Result, bool) {
	if r, ok := s.scripts[sql]; ok {
		return r, true
	}
	if s.scriptFunc != nil {
		if r, ok := s.scriptFunc(sql); ok {
			return r, true
		}
	}
	switch strings.ToUpper(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";"))) {
	case "BEGIN", "BEGIN TRANSACTION", "COMMIT", "ROLLBACK", "END":
		return Success(), true
	}
	return appendTypes(sql)
}

// appendTypesPattern matches the statement with which an Appender resolves
// its column types.
var appendTypesPattern = regexp.MustCompile(`^SELECT (NULL::[A-Za-z]+(?:, NULL::[A-Za-z]+)*) LIMIT 0$`)

// scalarTypes are the type names appendTypes knows.
var scalarTypes = map[string]logicaltype.ID{
	"BOOLEAN": logicaltype.IDBoolean, "TINYINT": logicaltype.IDTinyint, "SMALLINT": logicaltype.IDSmallint,
	"INTEGER": logicaltype.IDInteger, "BIGINT": logicaltype.IDBigint, "HUGEINT": logicaltype.IDHugeint,
	"UTINYINT": logicaltype.IDUTinyint, "USMALLINT": logicaltype.IDUSmallint, "UINTEGER": logicaltype.IDUInteger,
	"UBIGINT": logicaltype.IDUBigint, "FLOAT": logicaltype.IDFloat, "DOUBLE": logicaltype.IDDouble,
	"VARCHAR": logicaltype.IDVarchar, "BLOB": logicaltype.IDBlob, "UUID": logicaltype.IDUUID, "DATE": logicaltype.IDDate,
	"TIME": logicaltype.IDTime, "TIMESTAMP": logicaltype.IDTimestamp, "TIMESTAMPTZ": logicaltype.IDTimestampTz,
	"INTERVAL": logicaltype.IDInterval,
}

// appendTypes answers an Appender's type statement for scalar type names,
// as the real server does: an empty result of those types.
func appendTypes(sql string) (Result, bool) {
	m := appendTypesPattern.FindStringSubmatch(sql)
	if m == nil {
		return Result{}, false
	}
	var names []string
	var types []logicaltype.Type
	for i, part := range strings.Split(m[1], ", ") {
		id, ok := scalarTypes[strings.ToUpper(strings.TrimPrefix(part, "NULL::"))]
		if !ok {
			return Result{}, false
		}
		names = append(names, fmt.Sprintf("c%d", i))
		types = append(types, logicaltype.Type{ID: id})
	}
	return Rows(names, types, nil, 1, 1), true
}

// prepare handles PREPARE_REQUEST; it is entered with s.mu held and
// releases it.
func (s *Server) prepare(ctx context.Context, c *connState, b message.PrepareRequest) (message.Body, bool) {
	// A new statement supersedes the previous one.
	s.abortLocked(c, "superseded by a new query")

	if m := dataStreamPattern.FindStringSubmatch(b.SQL); m != nil {
		defer s.mu.Unlock()
		return s.prepareClientData(c, b, strings.ReplaceAll(m[1], "''", "'")), false
	}

	res, ok := s.lookup(b.SQL)
	if !ok {
		s.mu.Unlock()
		return &message.ErrorResponse{
			Message:       fmt.Sprintf("quacktest: no script for query %q", b.SQL),
			ExceptionType: "Parser",
		}, false
	}
	gate := res.PrepareGate
	var waiting *stmtState
	if gate != nil {
		// Like the real server, the statement is installed before it
		// produces anything, so a CANCEL for it applies while it waits.
		waiting = &stmtState{uuid: b.QueryUUID, served: make(map[int]bool), done: make(chan struct{})}
		c.stmt = waiting
	}
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-waiting.done:
		case <-ctx.Done():
			return message.SuccessResponse{}, true
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if waiting != nil {
		if c.stmt != waiting {
			// cancelled or superseded while waiting
			return &message.ErrorResponse{Message: c.abortErr, ExceptionType: "INTERRUPT"}, false
		}
		c.stmt = nil
	}
	if res.Error != nil {
		return res.Error, false
	}
	maxInline := uint64(DefaultInlineRows)
	if b.InlineRows.Valid {
		maxInline = b.InlineRows.Value
	}
	var inline []message.DataChunk
	consumed := 0
	var rows uint64
	for consumed < len(res.Batches) && rows < maxInline {
		for _, ch := range res.Batches[consumed] {
			inline = append(inline, ch)
			rows += uint64(ch.RowCount)
		}
		consumed++
	}
	st := &stmtState{
		uuid:    b.QueryUUID,
		res:     res,
		prepare: consumed,
		served:  make(map[int]bool),
		drops:   make(map[uint64]int),
	}
	for k, v := range res.DropFetchResponse {
		st.drops[k] = v
	}
	c.stmt = st
	return message.PrepareResponse{
		ResultTypes:    res.Types,
		ResultNames:    res.Names,
		NeedsMoreFetch: consumed < len(res.Batches) || res.NeedsMoreFetch,
		Results:        inline,
		QueryUUID:      b.QueryUUID,
	}, false
}

// prepareClientData starts a statement reading a SEND_DATA stream (s.mu
// held). It answers at once, without types, as the real server does when a
// scan waits for client data.
func (s *Server) prepareClientData(c *connState, b message.PrepareRequest, streamID string) message.Body {
	if !b.InlineRows.Valid || b.InlineRows.Value != 0 {
		return errResp("quacktest: a PREPARE that reads client data must use inline_rows = 0 (the real server would wait for data that cannot arrive)")
	}
	pub := &Stream{ID: streamID, Batches: make(map[uint64][]message.DataChunk)}
	st := &stmtState{
		uuid:    b.QueryUUID,
		served:  make(map[int]bool),
		drops:   make(map[uint64]int),
		done:    make(chan struct{}),
		prepare: 0,
	}
	ss := &streamState{pub: pub, stmt: st}
	st.stream = ss
	c.streams[streamID] = ss
	s.streams[streamID] = pub
	c.stmt = st
	return message.PrepareResponse{NeedsMoreFetch: true, QueryUUID: b.QueryUUID}
}

func (s *Server) sendData(c *connState, b message.SendDataRequest) message.Body {
	ss := c.streams[b.StreamID]
	if ss == nil {
		return errResp("No active data stream '%s'", b.StreamID)
	}
	st := ss.stmt
	if st.errMsg != nil {
		return st.errMsg
	}
	if s.sendDataHook != nil {
		if er := s.sendDataHook(b.StreamID, b); er != nil {
			st.errMsg = er
			ss.pub.Error = er.Message
			st.finishClientData()
			return er
		}
	}
	if len(b.Chunks) > 0 {
		if !b.BatchIndex.Valid {
			return errResp("send_data_request is missing its batch index")
		}
		for _, ch := range b.Chunks {
			if ch.RowCount > MaxChunkRows {
				er := &message.ErrorResponse{
					Message:       fmt.Sprintf("quacktest: chunk with %d rows exceeds %d", ch.RowCount, MaxChunkRows),
					ExceptionType: "Internal",
				}
				st.errMsg = er
				return er
			}
		}
		if _, dup := ss.pub.Batches[b.BatchIndex.Value]; !dup {
			ss.pub.Batches[b.BatchIndex.Value] = b.Chunks
		}
	}
	if !b.TotalBatches.Valid {
		return message.SendDataResponse{}
	}
	total := b.TotalBatches.Value
	ss.pub.Total = &total
	if uint64(len(ss.pub.Batches)) != total {
		er := &message.ErrorResponse{
			Message:       fmt.Sprintf("Quack stream ended with %d of %d batches received", len(ss.pub.Batches), total),
			ExceptionType: "IO",
		}
		st.errMsg = er
		ss.pub.Error = er.Message
		st.finishClientData()
		return er
	}
	ss.pub.Finished = true
	rows := 0
	for _, chunks := range ss.pub.Batches {
		for _, ch := range chunks {
			rows += ch.RowCount
		}
	}
	cnt := Count(int64(rows))
	st.res = cnt
	st.finishClientData()
	return message.SendDataResponse{}
}

// finishClientData lets a client-data statement answer FETCH.
func (st *stmtState) finishClientData() {
	if st.done != nil && !st.doneSet {
		st.doneSet = true
		close(st.done)
	}
}

// fetch handles FETCH_REQUEST; it is entered with s.mu held and releases it.
func (s *Server) fetch(ctx context.Context, c *connState, b message.FetchRequest) (message.Body, bool) {
	st := c.stmt
	if st == nil || st.uuid != b.QueryUUID {
		defer s.mu.Unlock()
		if st != nil {
			return errResp("Result has been closed"), false
		}
		return closedResult(c, b.QueryUUID), false
	}
	if b.BatchIndex == 0 {
		s.mu.Unlock()
		return errResp("FETCH_REQUEST is missing its batch index"), false
	}
	if done := st.done; done != nil {
		// a client-data statement answers once its stream ended
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return message.SuccessResponse{}, true
		}
		s.mu.Lock()
		if c.stmt != st {
			defer s.mu.Unlock()
			return closedResult(c, b.QueryUUID), false
		}
		if st.errMsg != nil {
			defer s.mu.Unlock()
			return st.errMsg, false
		}
	}
	res := st.res
	hook := res.BeforeFetch
	s.mu.Unlock()
	if hook != nil {
		hook(ctx, b.BatchIndex)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.stmt != st {
		return closedResult(c, b.QueryUUID), false
	}
	dense := int(b.BatchIndex) + st.prepare
	denseAck := int(b.AckIndex) + st.prepare
	for i := range st.served {
		if i <= denseAck {
			delete(st.served, i)
		}
	}
	if denseAck > st.acked {
		st.acked = denseAck
	}
	if er, ok := res.FetchErrors[b.BatchIndex]; ok {
		return er, false
	}
	if dense <= len(res.Batches) {
		if dense <= st.acked {
			return &message.ErrorResponse{
				Message:       fmt.Sprintf("quacktest: batch %d was requested after an ack released it", b.BatchIndex),
				ExceptionType: "Internal",
			}, false
		}
		st.served[dense] = true
		echo := b.BatchIndex
		if w, ok := res.WrongBatchIndex[b.BatchIndex]; ok {
			echo = w
		}
		out := message.FetchResponse{Results: res.Batches[dense-1], BatchIndex: message.Idx(echo)}
		if st.drops[b.BatchIndex] > 0 {
			st.drops[b.BatchIndex]--
			return out, true
		}
		return out, false
	}
	out := message.FetchResponse{}
	if !res.OmitTotal {
		total := uint64(len(res.Batches) - st.prepare)
		if res.TotalOverride != nil {
			total = *res.TotalOverride
		}
		out.TotalBatches = message.Idx(total)
	}
	return out, false
}

// closedResult answers a FETCH for a statement that is no longer the
// session's: its abort reason, or "Result has been closed".
func closedResult(c *connState, uuid codec.HugeInt) *message.ErrorResponse {
	if c.abortedUUID == uuid && c.abortErr != "" {
		return errResp("%s", c.abortErr)
	}
	return errResp("Result has been closed")
}

// RawResponse encodes a message for SetRawHook handlers.
func RawResponse(w http.ResponseWriter, h message.Header, body message.Body) {
	out, err := message.Encode(h, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.duckdb")
	_, _ = w.Write(out)
}
