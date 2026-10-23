package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/sqlfmt"
	"github.com/zzir/quackdriver/internal/transport"
)

const (
	// disconnectTimeout bounds the best-effort DISCONNECT sent by Close
	// (the official client uses 2 s for its teardown request).
	disconnectTimeout = 2 * time.Second
	// cancelRetryDelay is the pause before a CANCEL_REQUEST that reached the
	// server before the PREPARE it cancels is repeated.
	cancelRetryDelay = 20 * time.Millisecond
	// settleTimeout bounds how long a statement waits for the requests of an
	// earlier, cancelled statement (see Conn.waitBackground); the session is
	// given up after that.
	settleTimeout = 10 * time.Second
	// handshakeAbandonTimeout bounds how long a CONNECTION_REQUEST whose
	// caller gave up is still waited for in the background, so that the
	// session the server may have created for it can be disconnected.
	handshakeAbandonTimeout = 30 * time.Second
)

// cancelTimeout bounds how long a synchronous cancel (Rows.Close, a failed
// Appender) waits for the server's answer (see query.cancel). A variable for
// tests.
var cancelTimeout = 2 * time.Second

// Conn is a Quack session. It implements the standard database/sql driver
// interfaces. Like every database/sql connection it runs one statement at a
// time; a heartbeat goroutine keeps the session's lease alive meanwhile.
//
// Quack v3 aborts a statement when the next one is prepared on the same
// session. When a new statement starts while a Rows of this connection still
// has batches on the server, the rest of that result is first read into
// memory, so interleaving (iterating rows inside a transaction while running
// updates) keeps working at the cost of buffering the remaining rows, up to
// the DSN's max_buffered_rows (default 1,000,000): beyond that the new
// statement fails and the Rows stays readable.
type Conn struct {
	transport *transport.Transport
	uri       *transport.URI
	// ownedConnector is set for connections made by Driver.Open; Close
	// closes it.
	ownedConnector *Connector

	connectionID string
	serverInfo   ServerInfo

	closed    atomic.Bool
	bad       atomic.Bool // the server session is gone or unusable
	gone      atomic.Bool // the server said the session no longer exists
	exclusive atomic.Bool // held by an Appender
	inTx      bool
	nextQuery atomic.Uint64
	// lastOK is the UnixNano time of the last answer the server gave this
	// session (any message renews its lease there).
	lastOK atomic.Int64

	mu       sync.Mutex
	active   *Rows // open Rows whose result still has batches on the server
	bgClosed bool  // set by Close: no new background requests
	bgCount  int
	bgIdle   chan struct{} // closed when bgCount drops to 0; nil while 0

	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup // background requests (CANCEL, abandoned PREPARE)

	hbCancel   context.CancelFunc
	hbDone     chan struct{}
	heartbeats atomic.Uint64 // successful heartbeats (tests)
}

// ServerInfo is what the server reported about itself in the handshake.
type ServerInfo struct {
	// DuckDBVersion is the server's DuckDB version, e.g. "v2.0.0".
	DuckDBVersion string
	// Platform is the server's platform string, e.g. "linux_amd64".
	Platform string
	// QuackVersion is the Quack protocol version the session speaks.
	QuackVersion uint64
	// Lease is the heartbeat timeout the server granted.
	Lease time.Duration
}

// ServerInfo returns what the server reported in the handshake, for logging
// or for gating features by DuckDB version. Reach the Conn through
// sql.Conn.Raw.
func (c *Conn) ServerInfo() ServerInfo { return c.serverInfo }

// Standard-library interface assertions.
var (
	_ driver.Conn               = (*Conn)(nil)
	_ driver.ConnBeginTx        = (*Conn)(nil)
	_ driver.ConnPrepareContext = (*Conn)(nil)
	_ driver.ExecerContext      = (*Conn)(nil)
	_ driver.QueryerContext     = (*Conn)(nil)
	_ driver.Pinger             = (*Conn)(nil)
	_ driver.SessionResetter    = (*Conn)(nil)
	_ driver.Validator          = (*Conn)(nil)
)

func newConn(c *Connector) *Conn {
	conn := &Conn{transport: c.transport, uri: c.uri}
	conn.bgCtx, conn.bgCancel = context.WithCancel(context.Background())
	return conn
}

// maxLeaseSeconds is the longest heartbeat lease the driver accepts from the
// server: the server's own bound, further limited to what a time.Duration
// can hold.
var maxLeaseSeconds = min(transport.MaxHeartbeatTimeoutSeconds, uint64(math.MaxInt64/int64(time.Second)))

// handshake opens the session with a CONNECTION_REQUEST.
func (c *Conn) handshake(ctx context.Context) error {
	req := message.ConnectionRequest{
		AuthString:               c.uri.Token,
		ClientDuckdbVersion:      clientDuckDBVersion,
		ClientPlatform:           clientPlatform(),
		MinSupportedQuackVersion: message.QuackVersion,
		MaxSupportedQuackVersion: message.QuackVersion,
		ClientID:                 c.uri.ClientID,
		HeartbeatTimeoutSeconds:  c.uri.HeartbeatTimeoutSeconds(),
	}
	payload, err := message.Encode(message.Header{}, req)
	if err != nil {
		return fmt.Errorf("quackdriver: handshake: encode: %w", err)
	}
	raw, err := c.handshakeRequest(ctx, payload)
	if err != nil {
		return fmt.Errorf("quackdriver: handshake: %w", err)
	}
	return c.acceptHandshake(raw)
}

// handshakeResult is the outcome of a CONNECTION_REQUEST.
type handshakeResult struct {
	raw []byte
	err error
}

// handshakeRequest posts the CONNECTION_REQUEST. The HTTP request is not
// bound to ctx: the server completes it anyway and creates a session that
// it reclaims only when a message for it arrives. When ctx ends first, the
// request is left to finish in the background and the session it created,
// if any, is disconnected.
func (c *Conn) handshakeRequest(ctx context.Context, payload []byte) ([]byte, error) {
	if ctx.Done() == nil {
		return c.transport.Request(ctx, payload)
	}
	if err := ctx.Err(); err != nil {
		return nil, err // nothing was sent
	}
	reqCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan handshakeResult, 1)
	go func() {
		raw, err := c.transport.Request(reqCtx, payload)
		done <- handshakeResult{raw: raw, err: err}
	}()
	if r, ok := await(ctx, done); ok {
		cancel()
		return r.raw, r.err
	}
	go c.disconnectAbandoned(done, cancel)
	return nil, ctx.Err()
}

// await receives from done unless ctx ends first; a result that is ready
// when ctx ends still wins.
func await[T any](ctx context.Context, done <-chan T) (T, bool) {
	select {
	case r := <-done:
		return r, true
	case <-ctx.Done():
	}
	select {
	case r := <-done:
		return r, true
	default:
		var zero T
		return zero, false
	}
}

// disconnectAbandoned waits for a CONNECTION_REQUEST whose caller gave up
// and disconnects the session the server created for it.
func (c *Conn) disconnectAbandoned(done <-chan handshakeResult, cancel context.CancelFunc) {
	defer cancel()
	t := time.NewTimer(handshakeAbandonTimeout)
	defer t.Stop()
	var r handshakeResult
	select {
	case r = <-done:
	case <-t.C:
		cancel()
		r = <-done
	}
	if r.err != nil {
		return
	}
	h, body, err := message.Decode(r.raw)
	if err != nil || h.ConnectionID == "" {
		return
	}
	if _, ok := body.(message.ConnectionResponse); ok {
		c.disconnectSession(h.ConnectionID)
	}
}

// disconnectSession sends a best-effort DISCONNECT for a session id
// (bounded by disconnectTimeout).
func (c *Conn) disconnectSession(id string) {
	payload, err := message.Encode(message.Header{ConnectionID: id}, message.DisconnectMessage{})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), disconnectTimeout)
	defer cancel()
	_, _ = c.transport.Request(ctx, payload)
}

// acceptHandshake checks the server's answer to CONNECTION_REQUEST. A
// session the server created but the driver rejects is disconnected.
func (c *Conn) acceptHandshake(raw []byte) error {
	h, body, err := message.Decode(raw)
	if err != nil {
		return fmt.Errorf("quackdriver: handshake: cannot decode the server's response; "+
			"the server must be DuckDB 2.0 or newer (Quack protocol v%d): %w", message.QuackVersion, err)
	}
	var resp message.ConnectionResponse
	switch b := body.(type) {
	case message.ConnectionResponse:
		resp = b
	case message.ErrorResponse:
		se := newServerError(b)
		if isVersionRejection(se.Message) {
			return fmt.Errorf("quackdriver: handshake rejected: the server must be DuckDB 2.0 or newer "+
				"(Quack protocol v%d): %w", message.QuackVersion, se)
		}
		return fmt.Errorf("quackdriver: handshake rejected: %w", se)
	default:
		return fmt.Errorf("quackdriver: handshake: unexpected %s in response to CONNECTION_REQUEST", body.MessageType())
	}
	if h.ConnectionID == "" {
		return fmt.Errorf("quackdriver: handshake: the server sent no connection id")
	}
	reject := func(err error) error {
		c.disconnectSession(h.ConnectionID)
		return err
	}
	if resp.QuackVersion != message.QuackVersion {
		return reject(fmt.Errorf("quackdriver: handshake: the server speaks Quack protocol v%d, but quackdriver %s "+
			"requires v%d (DuckDB 2.0 or newer)", resp.QuackVersion, Version, message.QuackVersion))
	}
	if resp.HeartbeatTimeoutSeconds == 0 || resp.HeartbeatTimeoutSeconds > maxLeaseSeconds {
		return reject(fmt.Errorf("quackdriver: handshake: the server granted an invalid heartbeat timeout of %d seconds",
			resp.HeartbeatTimeoutSeconds))
	}
	c.connectionID = h.ConnectionID
	c.serverInfo = ServerInfo{
		DuckDBVersion: resp.ServerDuckdbVersion,
		Platform:      resp.ServerPlatform,
		QuackVersion:  resp.QuackVersion,
		Lease:         time.Duration(resp.HeartbeatTimeoutSeconds) * time.Second,
	}
	c.lastOK.Store(time.Now().UnixNano())
	return nil
}

// isVersionRejection recognizes a handshake error caused by a protocol
// version mismatch: "Unsupported Quack version - server only supports
// version N of quack" from v3 servers, or a v1 server failing to read the
// v3 request.
func isVersionRejection(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "quack version") || strings.Contains(m, "deserializ")
}

// transportError marks an error of the HTTP exchange itself (as opposed to
// a server ERROR_RESPONSE or an undecodable answer).
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// send encodes body under this session's header and posts it. queryID is the
// client_query_id for log correlation (0 = none). An ERROR_RESPONSE becomes a
// *Error. Errors are not classified (see classify).
func (c *Conn) send(ctx context.Context, queryID uint64, body message.Body) (message.Body, error) {
	h := message.Header{ConnectionID: c.connectionID, HasQueryID: queryID != 0, ClientQueryID: queryID}
	payload, err := message.Encode(h, body)
	if err != nil {
		return nil, fmt.Errorf("quackdriver: encode %s: %w", body.MessageType(), err)
	}
	return c.post(ctx, payload, body.MessageType())
}

// post sends an encoded request and decodes the answer.
func (c *Conn) post(ctx context.Context, payload []byte, reqType message.Type) (message.Body, error) {
	raw, err := c.transport.Request(ctx, payload)
	if err != nil {
		return nil, &transportError{err: fmt.Errorf("quackdriver: %s: %w", reqType, err)}
	}
	_, body, err := message.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("quackdriver: decode response to %s: %w", reqType, err)
	}
	c.lastOK.Store(time.Now().UnixNano())
	if e, ok := body.(message.ErrorResponse); ok {
		return nil, newServerError(e)
	}
	return body, nil
}

// interrupted reports whether err is the result of ctx ending (rather than an
// answer the server sent before the deadline).
func interrupted(ctx context.Context, err error) bool {
	if ctx.Err() == nil {
		return false
	}
	var te *transportError
	return errors.As(err, &te) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// markBad makes the connection unusable; database/sql then discards it.
// Close still sends DISCONNECT, since the server session may exist.
func (c *Conn) markBad() { c.bad.Store(true) }

// markGone records that the server no longer has the session (and makes the
// connection unusable): Close sends no DISCONNECT.
func (c *Conn) markGone() {
	c.gone.Store(true)
	c.bad.Store(true)
}

// checkUsable rejects statements on a closed or broken connection and, unless
// holder is set (the Appender's own requests), while an Appender holds it.
func (c *Conn) checkUsable(holder bool) error {
	if c.closed.Load() || c.bad.Load() {
		return driver.ErrBadConn
	}
	if !holder && c.exclusive.Load() {
		return ErrConnBusy
	}
	return nil
}

// acquireExclusive reserves the connection for an Appender: until
// releaseExclusive, statements issued through the database/sql interfaces
// fail with ErrConnBusy. An open Rows is buffered first, because the
// Appender's PREPARE would abort its statement.
func (c *Conn) acquireExclusive(ctx context.Context) error {
	if err := c.checkUsable(false); err != nil {
		return err
	}
	if !c.exclusive.CompareAndSwap(false, true) {
		return ErrConnBusy
	}
	if err := c.settleActive(ctx); err != nil {
		c.exclusive.Store(false)
		return err
	}
	return nil
}

// releaseExclusive ends an acquireExclusive reservation.
func (c *Conn) releaseExclusive() { c.exclusive.Store(false) }

// settleActive reads the rest of the open Rows (if any) into memory before a
// new statement replaces it on the server. When that would buffer more than
// the DSN's max_buffered_rows, the Rows keeps its stream and the new
// statement fails with that error.
func (c *Conn) settleActive(ctx context.Context) error {
	c.mu.Lock()
	r := c.active
	c.active = nil
	c.mu.Unlock()
	if r == nil {
		return nil
	}
	if err := r.buffer(ctx, c.uri.MaxBufferedRows); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.bad.Load() {
		return &badConnError{cause: errors.New("quackdriver: the session was lost while reading the previous result")}
	}
	return nil
}

// query is a statement the server has prepared (and started executing).
type query struct {
	conn *Conn
	uuid codec.HugeInt
	id   uint64 // client_query_id
	info stmtInfo
	prep message.PrepareResponse
}

// inlineRows is the PREPARE inline_rows from the DSN (unset = server default).
func (c *Conn) inlineRows() message.OptionalIdx {
	if c.uri.InlineRows != nil {
		return message.Idx(*c.uri.InlineRows)
	}
	return message.OptionalIdx{}
}

// prepare sends PREPARE_REQUEST for sql. holder is set for requests of the
// Appender holding the connection. A session-gone answer or a request that
// never left the client yields an error satisfying errors.Is(err,
// driver.ErrBadConn): the statement did not run, so database/sql may retry it
// elsewhere. When ctx ends the statement is cancelled in the background and
// ctx.Err() is returned.
func (c *Conn) prepare(ctx context.Context, sql string, inlineRows message.OptionalIdx, holder bool) (*query, error) {
	if err := c.checkUsable(holder); err != nil {
		return nil, err
	}
	if err := c.settleActive(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err // nothing was sent, nothing to cancel
	}
	if err := c.waitBackground(ctx); err != nil {
		return nil, err
	}
	q := &query{
		conn: c,
		uuid: message.NewQueryUUID(),
		id:   c.nextQuery.Add(1),
		info: classifyStatement(sql),
	}
	body, err := q.start(ctx, message.PrepareRequest{SQL: sql, QueryUUID: q.uuid, InlineRows: inlineRows})
	if err != nil {
		if errors.Is(err, errAbandoned) {
			return nil, ctx.Err()
		}
		if interrupted(ctx, err) {
			q.cancelAsync()
			return nil, ctx.Err()
		}
		if errors.Is(err, message.ErrVariant) {
			// Inline rows held VARIANT values; the statement may still have
			// batches to send (a cancel of a finished one is refused, harmlessly).
			q.cancelAsync()
			return nil, errVariant
		}
		return nil, c.classify(err, true)
	}
	prep, ok := body.(message.PrepareResponse)
	if !ok {
		return nil, fmt.Errorf("quackdriver: unexpected %s in response to PREPARE_REQUEST", body.MessageType())
	}
	if prep.QueryUUID != q.uuid {
		return nil, fmt.Errorf("quackdriver: PREPARE_RESPONSE belongs to another query")
	}
	// Fail at PREPARE time rather than with a bounds error during a later
	// row decode.
	if len(prep.ResultNames) != len(prep.ResultTypes) {
		return nil, fmt.Errorf("quackdriver: PREPARE_RESPONSE has %d names but %d types", len(prep.ResultNames), len(prep.ResultTypes))
	}
	if slices.ContainsFunc(prep.ResultTypes, func(t logicaltype.Type) bool { return t.Contains(logicaltype.IDVariant) }) {
		if prep.NeedsMoreFetch {
			q.cancelAsync()
		}
		return nil, errVariant
	}
	q.prep = prep
	if !prep.NeedsMoreFetch && rowCount(prep.Results) >= ackMinRows {
		// The whole result arrived inline: release a retained copy.
		q.acknowledge()
	}
	return q, nil
}

// ackMinRows is the smallest result PREPARE returns whole that the driver
// acknowledges (see query.acknowledge): smaller ones are not worth an extra
// request per statement, and the session's next statement releases them.
// Results read through FETCH are always acknowledged.
const ackMinRows = 2048

func rowCount(chunks []message.DataChunk) int {
	n := 0
	for _, ch := range chunks {
		n += ch.RowCount
	}
	return n
}

// errAbandoned is returned by query.start when ctx ended first.
var errAbandoned = errors.New("quackdriver: request abandoned")

type sendResult struct {
	body message.Body
	err  error
}

// start sends the PREPARE_REQUEST of q. The HTTP request is not bound to
// ctx: the server still runs a request whose connection the client closed,
// and a late one would supersede the session's next statement. When ctx
// ends first, the request is left to finish in the background while the
// statement is cancelled, and errAbandoned is returned; the session's next
// statement waits for both (waitBackground).
func (q *query) start(ctx context.Context, body message.Body) (message.Body, error) {
	c := q.conn
	if ctx.Done() == nil {
		return c.send(ctx, q.id, body)
	}
	done := make(chan sendResult, 1)
	if !c.goBackground(true, func(bg context.Context) {
		b, err := c.send(bg, q.id, body)
		done <- sendResult{body: b, err: err}
	}) {
		return c.send(ctx, q.id, body)
	}
	if r, ok := await(ctx, done); ok {
		return r.body, r.err
	}
	q.cancelPending(done)
	return nil, errAbandoned
}

// cancelPending cancels q while its PREPARE_REQUEST is still on its way
// (done delivers the answer). A CANCEL that reaches the server before that
// PREPARE finds another statement and is refused, so it is repeated until
// it applies or the PREPARE has been answered.
func (q *query) cancelPending(done <-chan sendResult) {
	c := q.conn
	c.goBackground(true, func(bg context.Context) {
		deadline := time.Now().Add(settleTimeout)
		for {
			var se *Error
			if err := q.cancelWith(bg); !errors.As(err, &se) || !isCancelMismatch(se) {
				return
			}
			t := time.NewTimer(cancelRetryDelay)
			select {
			case r := <-done:
				t.Stop()
				// The refused CANCEL came before the PREPARE installed the
				// statement. If it still runs now, cancel it again: this
				// CANCEL applies.
				if prep, ok := r.body.(message.PrepareResponse); ok && r.err == nil && prep.NeedsMoreFetch {
					_ = q.cancelWith(bg)
				}
				return
			case <-bg.Done():
				t.Stop()
				return
			case <-t.C:
			}
			if time.Now().After(deadline) {
				return
			}
		}
	})
}

// isCancelMismatch recognizes the server's refusal of a CANCEL_REQUEST whose
// query is not (yet) the session's statement.
func isCancelMismatch(e *Error) bool {
	return strings.HasPrefix(e.Message, "Attempted to cancel a different query")
}

// goBackground runs f in a goroutine that Close interrupts (through the
// context passed to f) and waits for. With settle set, the session's next
// statement waits for it too (waitBackground). It reports false, without
// running f, once Close has started.
func (c *Conn) goBackground(settle bool, f func(ctx context.Context)) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bgClosed {
		return false
	}
	if settle {
		if c.bgCount == 0 {
			c.bgIdle = make(chan struct{})
		}
		c.bgCount++
	}
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		if settle {
			defer c.endBackground()
		}
		f(c.bgCtx)
	}()
	return true
}

func (c *Conn) endBackground() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bgCount--
	if c.bgCount == 0 {
		close(c.bgIdle)
		c.bgIdle = nil
	}
}

// waitBackground waits until the background requests of earlier statements
// (CANCEL, an abandoned PREPARE) are answered. The server handles each
// request on its own thread, so a new PREPARE sent meanwhile could be
// interrupted by a late CANCEL or superseded by a late PREPARE. If they take
// longer than settleTimeout the session is given up: the error satisfies
// errors.Is(err, driver.ErrBadConn), nothing was sent.
func (c *Conn) waitBackground(ctx context.Context) error {
	c.mu.Lock()
	idle := c.bgIdle
	c.mu.Unlock()
	if idle == nil {
		return nil
	}
	t := time.NewTimer(settleTimeout)
	defer t.Stop()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		c.markBad()
		return &badConnError{cause: errors.New("quackdriver: a cancelled statement did not finish on the server")}
	}
}

// acknowledge tells the server, best effort and in the background, that
// the whole result of q has arrived, so a server that retains results for
// reconnects (quack_enable_reconnects) can release it. The answer is
// ignored: a late ACKNOWLEDGEMENT is refused harmlessly, so the next
// statement need not wait for it.
func (q *query) acknowledge() {
	c := q.conn
	if c.closed.Load() || c.bad.Load() {
		return
	}
	c.goBackground(false, func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, disconnectTimeout)
		defer cancel()
		_, _ = c.send(ctx, q.id, message.Acknowledgement{QueryUUID: q.uuid})
	})
}

// cancelAsync sends CANCEL_REQUEST for q in the background. Close waits for
// (and interrupts) background cancels; the session's next statement waits
// until the server answered.
func (q *query) cancelAsync() {
	q.conn.goBackground(true, func(ctx context.Context) { _ = q.cancelWith(ctx) })
}

// cancel sends CANCEL_REQUEST for q in the background and waits at most
// cancelTimeout for the answer. An unanswered CANCEL stays tracked, because
// the server interrupts whatever runs on the session when it handles it:
// the session's next statement waits for it (waitBackground).
func (q *query) cancel() {
	done := make(chan struct{})
	if !q.conn.goBackground(true, func(ctx context.Context) {
		defer close(done)
		_ = q.cancelWith(ctx)
	}) {
		return
	}
	t := time.NewTimer(cancelTimeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// cancelWith sends CANCEL_REQUEST for q and returns the server's refusal,
// if any; otherwise a failed cancel needs no handling, since the next
// PREPARE supersedes the statement anyway.
func (q *query) cancelWith(ctx context.Context) error {
	c := q.conn
	if c.closed.Load() || c.bad.Load() {
		return nil
	}
	_, err := c.send(ctx, q.id, message.CancelRequest{QueryUUID: q.uuid})
	if err != nil && !interrupted(ctx, err) {
		return c.classify(err, false)
	}
	return nil
}

// Prepare returns a Stmt that caches the SQL string. Quack has no server-side
// bind, so this is purely a placeholder for database/sql ergonomics.
func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext is the context-aware variant of Prepare.
func (c *Conn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	if c.closed.Load() || c.bad.Load() {
		return nil, driver.ErrBadConn
	}
	return &Stmt{conn: c, query: query}, nil
}

// Close ends the session: it stops the heartbeat and any FETCH read-ahead,
// then sends a best-effort DISCONNECT (bounded to 2 s) unless the server
// reported the session gone. It is idempotent and safe to call from any
// goroutine. For a connection made by Driver.Open it also closes the
// connection's own Connector and returns that error.
func (c *Conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.stopHeartbeat()
	c.mu.Lock()
	var f *fetcher
	if r := c.active; r != nil {
		f = r.fetcher // written under c.mu (see Rows.takeFetcher)
	}
	c.active = nil
	c.bgClosed = true
	c.mu.Unlock()
	if f != nil {
		f.stop(errConnClosed)
	}
	c.bgCancel()
	c.bg.Wait()
	// The server reclaims a session only when a message for it arrives, so
	// DISCONNECT is sent even for a connection that is merely unusable.
	if c.connectionID != "" && !c.gone.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), disconnectTimeout)
		_, _ = c.send(ctx, 0, message.DisconnectMessage{})
		cancel()
	}
	if c.ownedConnector != nil {
		return c.ownedConnector.Close()
	}
	return nil
}

// Begin is the legacy transaction entry point.
func (c *Conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx starts a transaction by sending BEGIN. Quack cannot negotiate
// isolation levels or read-only transactions, so options other than the
// defaults are rejected rather than silently dropped.
func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.inTx {
		return nil, fmt.Errorf("quackdriver: nested transactions are not supported")
	}
	// driver.IsolationLevel is an alias for sql.IsolationLevel; 0 == LevelDefault.
	if opts.Isolation != 0 {
		return nil, fmt.Errorf("quackdriver: isolation level %d is not supported (Quack uses the server default)", opts.Isolation)
	}
	if opts.ReadOnly {
		return nil, fmt.Errorf("quackdriver: read-only transactions are not supported")
	}
	if _, err := c.ExecContext(ctx, "BEGIN", nil); err != nil {
		c.uncertainOutcome(err)
		return nil, err
	}
	c.inTx = true
	return &Tx{conn: c}, nil
}

// uncertainOutcome marks the connection bad after BEGIN, COMMIT or ROLLBACK
// failed without a definite answer (the server may have run it): pooled
// inside an open transaction, the session would silently lose every later
// autocommit write. A server ERROR_RESPONSE, a bad-connection error and
// ErrConnBusy are definite.
func (c *Conn) uncertainOutcome(err error) {
	var se *Error
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, ErrConnBusy) || errors.As(err, &se) {
		return
	}
	c.markBad()
}

// ExecContext runs a statement and reads its whole result. RowsAffected is
// the statement's row count for INSERT, UPDATE, DELETE, MERGE and COPY (also
// behind a WITH clause) and CREATE TABLE ... AS, the number of returned rows
// for INSERT, UPDATE, DELETE and MERGE with a RETURNING clause; 0 otherwise.
// A query string with several statements runs them all, but the server
// returns one result (the first statement's that returns rows, else the
// last one's), so RowsAffected describes that result only.
func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sql, err := sqlfmt.FormatQuery(query, args)
	if err != nil {
		return nil, err
	}
	q, err := c.prepare(ctx, sql, c.inlineRows(), false)
	if err != nil {
		return nil, err
	}
	return q.exec(ctx)
}

// exec reads the whole result of q, counting rows for RowsAffected.
func (q *query) exec(ctx context.Context) (driver.Result, error) {
	counter := newRowCounter(q)
	if err := counter.add(q.prep.Results); err != nil {
		return nil, err
	}
	if q.prep.NeedsMoreFetch {
		f := q.newFetcher(q.conn.uri.FetchAhead)
		defer f.stop(nil)
		for {
			chunks, end, err := f.next(ctx)
			if end {
				break
			}
			if err != nil {
				if interrupted(ctx, err) {
					q.cancelAsync()
					return nil, ctx.Err()
				}
				return nil, err
			}
			if err := counter.add(chunks); err != nil {
				return nil, err
			}
		}
	}
	return execResult{rowsAffected: counter.result()}, nil
}

// QueryContext runs a statement and returns its Rows.
func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sql, err := sqlfmt.FormatQuery(query, args)
	if err != nil {
		return nil, err
	}
	q, err := c.prepare(ctx, sql, c.inlineRows(), false)
	if err != nil {
		return nil, err
	}
	r := newRows(ctx, q)
	if r.fetcher != nil {
		c.mu.Lock()
		c.active = r
		c.mu.Unlock()
	}
	return r, nil
}

// Ping sends a HEARTBEAT_REQUEST. A gone session reports driver.ErrBadConn.
func (c *Conn) Ping(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.checkUsable(true); err != nil {
		return err
	}
	body, err := c.send(ctx, 0, message.HeartbeatRequest{})
	if err != nil {
		if interrupted(ctx, err) {
			return ctx.Err()
		}
		return c.classify(err, true)
	}
	if _, ok := body.(message.SuccessResponse); !ok {
		return fmt.Errorf("quackdriver: unexpected %s in response to HEARTBEAT_REQUEST", body.MessageType())
	}
	return nil
}

// ResetSession is called by database/sql before reusing a pooled connection.
// A connection still held by an Appender that was never closed is not
// reusable: it is discarded, which disconnects the session and so aborts the
// Appender's INSERT.
func (c *Conn) ResetSession(_ context.Context) error {
	if !c.IsValid() {
		return driver.ErrBadConn
	}
	return nil
}

// IsValid reports whether the connection is still usable. database/sql
// discards connections that report false, including one returned to the
// pool while an Appender still holds it.
func (c *Conn) IsValid() bool {
	return !c.closed.Load() && !c.bad.Load() && !c.exclusive.Load()
}

// rowCounter derives RowsAffected from a statement's result, the way DuckDB
// reports changed rows: the single row of a BIGINT column named "Count"
// (followed by "Files" for COPY ... RETURN_FILES) for DML statements and
// CREATE TABLE ... AS; the number of result rows for INSERT, UPDATE, DELETE
// and MERGE with RETURNING (even when the first returned column happens to
// be a BIGINT named "Count"). A statement that reads client data (SEND_DATA)
// is answered before it is bound, so its PREPARE carries no schema; the
// chunk types are checked instead.
type rowCounter struct {
	eligible  bool
	noSchema  bool
	returning bool // count result rows
	rows      int
	count     int64
}

func newRowCounter(q *query) *rowCounter {
	p := q.prep
	if !q.info.dml && !q.info.create {
		return &rowCounter{}
	}
	if q.info.hasReturning {
		return &rowCounter{eligible: true, returning: true}
	}
	if len(p.ResultNames) == 0 && len(p.ResultTypes) == 0 {
		return &rowCounter{eligible: q.info.dml, noSchema: true}
	}
	if len(p.ResultNames) >= 1 && len(p.ResultTypes) >= 1 &&
		p.ResultNames[0] == "Count" && p.ResultTypes[0].ID == logicaltype.IDBigint {
		return &rowCounter{eligible: true}
	}
	return &rowCounter{eligible: q.info.returning, returning: true}
}

func (rc *rowCounter) add(chunks []message.DataChunk) error {
	if !rc.eligible {
		return nil
	}
	for _, ch := range chunks {
		if ch.RowCount == 0 {
			continue
		}
		if rc.returning {
			rc.rows += ch.RowCount
			continue
		}
		if rc.noSchema && (len(ch.Types) != 1 || ch.Types[0].ID != logicaltype.IDBigint) {
			rc.eligible = false
			return nil
		}
		if len(ch.Columns) < 1 || ch.Columns[0] == nil || ch.Columns[0].Len() < ch.RowCount {
			return fmt.Errorf("quackdriver: malformed Count result chunk")
		}
		if rc.rows == 0 {
			if v, ok := ch.Columns[0].Value(0).(int64); ok {
				rc.count = v
			}
		}
		rc.rows += ch.RowCount
	}
	return nil
}

func (rc *rowCounter) result() int64 {
	switch {
	case !rc.eligible:
		return 0
	case rc.returning:
		return int64(rc.rows)
	case rc.rows != 1:
		return 0
	}
	return rc.count
}

// execResult satisfies driver.Result.
type execResult struct{ rowsAffected int64 }

// LastInsertId is not supported: DuckDB has no implicit row ids to report.
// Use INSERT ... RETURNING instead.
func (execResult) LastInsertId() (int64, error) {
	return 0, errors.New("quackdriver: LastInsertId is not supported; use INSERT ... RETURNING")
}

// RowsAffected returns the row count of an INSERT/UPDATE/DELETE/MERGE/COPY
// or CREATE TABLE ... AS (see Conn.ExecContext), 0 for other statements.
func (r execResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }
