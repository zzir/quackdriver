package quackdriver

import "context"

// Tx is a Quack transaction. Commit/Rollback send the corresponding SQL.
type Tx struct {
	conn *Conn
}

var _ interface {
	Commit() error
	Rollback() error
} = (*Tx)(nil)

// Commit issues COMMIT. When it fails without a definite answer from the
// server (the exchange broke after the request was sent), the session may
// still be inside the transaction, so the connection is discarded.
func (t *Tx) Commit() error {
	return t.end("COMMIT")
}

// Rollback issues ROLLBACK. Like Commit, an uncertain outcome discards the
// connection.
func (t *Tx) Rollback() error {
	return t.end("ROLLBACK")
}

func (t *Tx) end(stmt string) error {
	defer func() { t.conn.inTx = false }()
	_, err := t.conn.ExecContext(context.Background(), stmt, nil)
	if err != nil {
		t.conn.uncertainOutcome(err)
	}
	return err
}
