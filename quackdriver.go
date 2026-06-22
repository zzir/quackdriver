// Package quackdriver is a pure-Go database/sql driver for DuckDB's Quack
// remote protocol.
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
package quackdriver

import (
	"context"
	"database/sql"
	"database/sql/driver"

	"github.com/zzir/quackdriver/internal/localserver"
)

// driverName is the registered sql.Driver identifier.
const driverName = "quack"

// Driver implements database/sql/driver.Driver and DriverContext.
type Driver struct{}

func init() {
	sql.Register(driverName, &Driver{})
}

// Open returns a new connection from a DSN. Prefer OpenConnector via
// `sql.OpenDB(connector)` for production use; Open is provided for the
// legacy `sql.Open("quack", dsn)` path.
func (Driver) Open(dsn string) (driver.Conn, error) {
	c, err := newConnector(dsn)
	if err != nil {
		return nil, err
	}
	return c.connect(context.TODO())
}

// OpenConnector parses a DSN and returns a reusable Connector.
func (Driver) OpenConnector(dsn string) (driver.Connector, error) {
	return newConnector(dsn)
}

// CloseLocalServers stops all auto-managed local DuckDB server processes.
// Call this during application shutdown if using local-mode DSNs via
// sql.Open (which does not expose the Connector for manual closing).
func CloseLocalServers() {
	localserver.ReleaseAll()
}
