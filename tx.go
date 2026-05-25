package quackdriver

import "context"

// Tx is a Quack transaction. Commit/Rollback send the corresponding SQL.
type Tx struct {
	conn *Conn
}

// Commit issues COMMIT.
func (t *Tx) Commit() error {
	defer func() { t.conn.inTx = false }()
	_, err := t.conn.ExecContext(context.Background(), "COMMIT", nil)
	return err
}

// Rollback issues ROLLBACK.
func (t *Tx) Rollback() error {
	defer func() { t.conn.inTx = false }()
	_, err := t.conn.ExecContext(context.Background(), "ROLLBACK", nil)
	return err
}
