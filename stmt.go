package quackdriver

import (
	"context"
	"database/sql/driver"
)

// Stmt holds a SQL string. Quack has no server-side prepare; parameters are
// substituted client-side at Exec/Query time.
type Stmt struct {
	conn  *Conn
	query string
}

// Standard-library interface assertions.
var (
	_ driver.Stmt             = (*Stmt)(nil)
	_ driver.StmtExecContext  = (*Stmt)(nil)
	_ driver.StmtQueryContext = (*Stmt)(nil)
)

// Close is a no-op — there is no server-side state to release.
func (s *Stmt) Close() error { return nil }

// NumInput tells database/sql to skip its own placeholder validation
// (Quack accepts arbitrary positional `?` and we substitute them client-side).
func (s *Stmt) NumInput() int { return -1 }

// Exec is the legacy entry point.
func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), toNamedValues(args))
}

// Query is the legacy entry point.
func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), toNamedValues(args))
}

// ExecContext substitutes args then runs the statement.
func (s *Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}

// QueryContext substitutes args then runs the query.
func (s *Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
}

func toNamedValues(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}
