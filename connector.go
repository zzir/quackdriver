package quackdriver

import (
	"context"
	"database/sql/driver"
	"runtime"

	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/transport"
)

// clientVersion is sent in the ConnectionRequest. Bump when a server expects
// a newer string for compatibility checks.
const clientVersion = "v1.5.3"

// Connector is a parsed DSN that can produce new Conn values.
type Connector struct {
	uri *transport.URI
}

func newConnector(dsn string) (*Connector, error) {
	u, err := transport.ParseURI(dsn)
	if err != nil {
		return nil, err
	}
	return &Connector{uri: u}, nil
}

// Connect opens a Quack session by issuing a ConnectionRequest.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	return c.connect(ctx)
}

// Driver returns the underlying Driver value.
func (c *Connector) Driver() driver.Driver { return Driver{} }

func (c *Connector) connect(ctx context.Context) (*Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	t := transport.New(c.uri)
	conn := &Conn{
		transport: t,
		token:     c.uri.Token,
	}
	if err := conn.handshake(ctx); err != nil {
		return nil, err
	}
	return conn, nil
}

func defaultPlatform() string {
	return "go-" + runtime.GOOS + "-" + runtime.GOARCH
}

var _ message.Body = message.ConnectionRequest{}
