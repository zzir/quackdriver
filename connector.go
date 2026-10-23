package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/zzir/quackdriver/internal/sqlfmt"
	"github.com/zzir/quackdriver/internal/transport"
)

// Connector is a parsed DSN plus the HTTP transport shared by every
// connection it opens. Use it with sql.OpenDB:
//
//	c, err := quackdriver.NewConnector("quack://db.example:9494?token=...")
//	if err != nil { ... }
//	db := sql.OpenDB(c) // db.Close also closes the connector
//
// A Connector is safe for concurrent use.
type Connector struct {
	uri       *transport.URI
	transport *transport.Transport

	closed    atomic.Bool
	closeOnce sync.Once
}

var (
	_ driver.Connector = (*Connector)(nil)
	_ io.Closer        = (*Connector)(nil)
)

// NewConnector parses dsn and prepares a Connector.
func NewConnector(dsn string, opts ...Option) (*Connector, error) {
	u, err := transport.ParseURI(dsn)
	if err != nil {
		return nil, err
	}
	var o connectorOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	t, err := transport.New(u, o.transport)
	if err != nil {
		return nil, fmt.Errorf("quackdriver: %w", err)
	}
	return &Connector{uri: t.URI, transport: t}, nil
}

// Close releases the idle HTTP connections of the Connector. Connections
// opened earlier should be closed first. Close is idempotent; database/sql
// calls it from DB.Close.
func (c *Connector) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.transport != nil {
			c.transport.CloseIdleConnections()
		}
	})
	return nil
}

// Connect opens a Quack session (CONNECTION_REQUEST) and starts its
// heartbeat.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// Driver returns the underlying Driver value.
func (c *Connector) Driver() driver.Driver { return Driver{} }

var errConnectorClosed = errors.New("quackdriver: connector is closed")

func (c *Connector) connect(ctx context.Context) (*Conn, error) {
	if c.closed.Load() {
		return nil, errConnectorClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn := newConn(c)
	if err := conn.handshake(ctx); err != nil {
		return nil, err
	}
	if tz := c.uri.TimeZone; tz != "" {
		// time.Time arguments are sent as TIMESTAMP literals of their UTC
		// wall clock, which DuckDB converts to TIMESTAMPTZ in the session's
		// TimeZone: timezone=UTC makes that conversion exact.
		lit, err := sqlfmt.FormatValue(tz)
		if err == nil {
			_, err = conn.ExecContext(ctx, "SET SESSION TimeZone = "+lit, nil)
		}
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("quackdriver: timezone=%s: %w", tz, err)
		}
	}
	conn.startHeartbeat()
	return conn, nil
}
