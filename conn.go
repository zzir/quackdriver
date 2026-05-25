package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/sqlfmt"
	"github.com/zzir/quackdriver/internal/transport"
)

// Conn is a Quack session. It implements the standard database/sql driver
// interfaces; concurrent use of one Conn for queries is not supported (the
// connection pool serializes), but `closed` is read by the pool from a
// different goroutine than Close runs in, so it must be atomic.
type Conn struct {
	transport    *transport.Transport
	token        string
	connectionID string

	closed     atomic.Bool
	inTx       bool
	nextQuery  atomic.Uint64
	serverInfo serverInfo
}

type serverInfo struct {
	DuckDBVersion string
	Platform      string
	QuackVersion  uint64
}

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

func (c *Conn) handshake(ctx context.Context) error {
	if c.token != "" && len(c.token) < 4 {
		return fmt.Errorf("quackdriver: token must be at least 4 characters")
	}
	req := message.ConnectionRequest{
		AuthString:               c.token,
		ClientDuckdbVersion:      clientVersion,
		ClientPlatform:           defaultPlatform(),
		MinSupportedQuackVersion: 1,
		MaxSupportedQuackVersion: 1,
	}
	respHeader, respBody, err := c.roundTrip(ctx, message.Header{}, req)
	if err != nil {
		return fmt.Errorf("quackdriver: handshake: %w", err)
	}
	if errResp, ok := respBody.(message.ErrorResponse); ok {
		return fmt.Errorf("quackdriver: handshake rejected: %s", errResp.Message)
	}
	cresp, ok := respBody.(message.ConnectionResponse)
	if !ok {
		return fmt.Errorf("quackdriver: handshake: unexpected response type %T", respBody)
	}
	c.connectionID = respHeader.ConnectionID
	c.serverInfo = serverInfo{
		DuckDBVersion: cresp.ServerDuckdbVersion,
		Platform:      cresp.ServerPlatform,
		QuackVersion:  cresp.QuackVersion,
	}
	return nil
}

// roundTrip issues one request and waits for a single response. The header's
// Type is overwritten with body.MessageType() and ClientQueryID is auto-assigned.
func (c *Conn) roundTrip(ctx context.Context, h message.Header, body message.Body) (message.Header, message.Body, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	h.ConnectionID = c.connectionID
	if !h.HasQueryID {
		h.HasQueryID = true
		h.ClientQueryID = c.nextQuery.Add(1)
	}
	req, err := message.Encode(h, body)
	if err != nil {
		return message.Header{}, nil, fmt.Errorf("quackdriver: encode: %w", err)
	}
	respBytes, err := c.transport.Request(ctx, req)
	if err != nil {
		return message.Header{}, nil, err
	}
	rh, rb, err := message.Decode(respBytes)
	if err != nil {
		return message.Header{}, nil, fmt.Errorf("quackdriver: decode response: %w", err)
	}
	if e, ok := rb.(message.ErrorResponse); ok {
		return rh, rb, fmt.Errorf("quackdriver: server error: %s", e.Message)
	}
	return rh, rb, nil
}

// Prepare returns a Stmt that caches the SQL string. Quack has no server-side
// bind, so this is purely a placeholder for database/sql ergonomics.
func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext is the context-aware variant of Prepare.
func (c *Conn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	return &Stmt{conn: c, query: query}, nil
}

// Close releases the session by sending a DisconnectMessage. It is safe to
// call from any goroutine and idempotent — only the first call performs the
// network round-trip; subsequent calls are no-ops.
//
// Errors from the disconnect request (other than context.Canceled, which
// happens during graceful shutdowns) are returned to the caller. Pool-driven
// closures in database/sql ignore the return value, but callers using Conn
// directly may want to log it.
func (c *Conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.transport.URI.Timeout)
	defer cancel()
	_, _, err := c.roundTrip(ctx, message.Header{}, message.DisconnectMessage{})
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// Begin is the legacy transaction entry point.
func (c *Conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx starts a transaction by sending `BEGIN`.
//
// Per the database/sql contract, isolation levels other than
// LevelDefault and the ReadOnly flag are not silently dropped — Quack
// does not negotiate them on the wire, so they are rejected here. Use
// the default isolation and apply read-only intent at the application
// layer if you need it.
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
		return nil, err
	}
	c.inTx = true
	return &Tx{conn: c}, nil
}

// ExecContext runs a statement without returning rows. If the statement
// happens to produce results (e.g. a SELECT misrouted through Exec), they
// are drained server-side so the result UUID isn't left dangling.
func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	sql, err := sqlfmt.FormatQuery(query, args)
	if err != nil {
		return nil, err
	}
	_, body, err := c.roundTrip(ctx, message.Header{}, message.PrepareRequest{SQL: sql})
	if err != nil {
		return nil, err
	}
	if prep, ok := body.(message.PrepareResponse); ok && prep.NeedsMoreFetch {
		if err := c.drainResult(ctx, prep.ResultUUID); err != nil {
			return nil, err
		}
	}
	return execResult{}, nil
}

// drainResult pulls FETCH responses until the server returns an empty result
// set, signalling exhaustion. Used by ExecContext when callers run a query
// through Exec; rows.go uses an interleaved iterator for QueryContext.
func (c *Conn) drainResult(ctx context.Context, uuid codec.HugeInt) error {
	for {
		_, body, err := c.roundTrip(ctx, message.Header{}, message.FetchRequest{ResultUUID: uuid})
		if err != nil {
			return err
		}
		resp, ok := body.(message.FetchResponse)
		if !ok {
			return fmt.Errorf("quackdriver: unexpected response %T while draining result", body)
		}
		if len(resp.Results) == 0 {
			return nil
		}
	}
}

// QueryContext runs a statement and returns a Rows.
func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	sql, err := sqlfmt.FormatQuery(query, args)
	if err != nil {
		return nil, err
	}
	_, body, err := c.roundTrip(ctx, message.Header{}, message.PrepareRequest{SQL: sql})
	if err != nil {
		return nil, err
	}
	prep, ok := body.(message.PrepareResponse)
	if !ok {
		return nil, fmt.Errorf("quackdriver: unexpected response %T to PREPARE", body)
	}
	return newRows(ctx, c, prep), nil
}

// Ping issues a no-op SELECT 1 to verify liveness.
func (c *Conn) Ping(ctx context.Context) error {
	_, err := c.ExecContext(ctx, "SELECT 1", nil)
	return err
}

// ResetSession is called by database/sql before reusing a pooled connection.
func (c *Conn) ResetSession(_ context.Context) error {
	if c.closed.Load() {
		return driver.ErrBadConn
	}
	return nil
}

// IsValid reports whether the connection is still usable.
func (c *Conn) IsValid() bool { return !c.closed.Load() }

// execResult satisfies driver.Result. Quack v1.5.3 does not expose
// last-insert-id or row-count from PREPARE, so both are zero.
type execResult struct{}

func (execResult) LastInsertId() (int64, error) {
	return 0, errors.New("quackdriver: LastInsertId is not supported")
}

func (execResult) RowsAffected() (int64, error) { return 0, nil }
