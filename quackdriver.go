// Package quackdriver is a pure-Go database/sql driver for DuckDB's Quack
// remote protocol (protocol version 3, DuckDB 2.0 and newer).
//
// Register by importing for side effects, then use sql.Open with the "quack"
// driver name:
//
//	import (
//	    "database/sql"
//	    _ "github.com/zzir/quackdriver"
//	)
//
//	db, _ := sql.Open("quack", "quack://localhost:9494?token=super_secret")
//
// Use NewConnector with sql.OpenDB to pass options such as a custom TLS
// configuration or extra HTTP headers.
package quackdriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
)

// driverName is the registered sql.Driver identifier.
const driverName = "quack"

// Driver implements database/sql/driver.Driver and DriverContext.
type Driver struct{}

var (
	_ driver.Driver        = Driver{}
	_ driver.DriverContext = Driver{}
)

func init() {
	sql.Register(driverName, &Driver{})
}

// Open returns a new connection from a DSN. database/sql uses OpenConnector
// instead; Open exists for callers that use the driver directly. The returned
// connection owns its own Connector and closes it on Close.
func (Driver) Open(dsn string) (driver.Conn, error) {
	c, err := NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := c.connect(context.Background())
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	conn.ownedConnector = c
	return conn, nil
}

// OpenConnector parses a DSN and returns a reusable Connector.
func (Driver) OpenConnector(dsn string) (driver.Connector, error) {
	return NewConnector(dsn)
}
