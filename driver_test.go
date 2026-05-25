package quackdriver

import (
	"database/sql"
	"strings"
	"testing"
)

func TestDriverRegistered(t *testing.T) {
	for _, name := range sql.Drivers() {
		if name == driverName {
			return
		}
	}
	t.Fatalf("driver %q not registered: %v", driverName, sql.Drivers())
}

func TestOpenConnectorParsesDSN(t *testing.T) {
	d := Driver{}
	c, err := d.OpenConnector("quack://localhost:9494?token=super_secret&tls=false&timeout=10s")
	if err != nil {
		t.Fatalf("open connector: %v", err)
	}
	conn := c.(*Connector)
	if conn.uri.Host != "localhost" || conn.uri.Port != 9494 || conn.uri.Token != "super_secret" {
		t.Fatalf("uri mismatch: %+v", conn.uri)
	}
}

func TestOpenConnectorRejectsBadDSN(t *testing.T) {
	d := Driver{}
	_, err := d.OpenConnector("not-quack://x")
	if err == nil || !strings.Contains(err.Error(), "quack") {
		t.Fatalf("expected DSN scheme error, got %v", err)
	}
}
