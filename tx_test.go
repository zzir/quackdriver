package quackdriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
)

// Regression: BeginTx must reject non-default isolation levels and the
// read-only flag instead of silently dropping them. Per the database/sql
// contract, drivers signal lack of support via an error so the caller
// learns the requested semantics are not honoured.
func TestBeginTxRejectsUnsupportedOptions(t *testing.T) {
	// Build a Conn without performing a real handshake — BeginTx checks
	// opts before doing any network I/O.
	c := &Conn{}

	if _, err := c.BeginTx(context.Background(), driver.TxOptions{
		Isolation: driver.IsolationLevel(sql.LevelSerializable),
	}); err == nil || !strings.Contains(err.Error(), "isolation") {
		t.Fatalf("Serializable should be rejected; got %v", err)
	}

	if _, err := c.BeginTx(context.Background(), driver.TxOptions{ReadOnly: true}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("ReadOnly should be rejected; got %v", err)
	}
}
