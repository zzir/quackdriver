// Bulk loading with Conn.Append (rows already in memory) and the streaming
// Appender (rows as they are produced). Both run one
// INSERT ... SELECT ... FROM scan_data_from_quack_client(...) statement and
// send the values as text (Quack SEND_DATA) that the server casts to each
// column's type, so the load is atomic and follows DuckDB's own casts.
//
//	go run ./examples/append
//
// The server address comes from QUACK_HOST, QUACK_PORT and QUACK_TOKEN
// (default 127.0.0.1:9494, token super_secret), or from a full QUACK_DSN.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zzir/quackdriver"
)

func main() {
	db, err := sql.Open("quack", dsn())
	check(err)
	defer db.Close()
	ctx := context.Background()
	for _, q := range []string{
		"DROP TABLE IF EXISTS append_demo",
		"DROP TYPE IF EXISTS append_mood",
		"CREATE TYPE append_mood AS ENUM ('sad', 'ok', 'happy')",
		`CREATE TABLE append_demo (id INTEGER, amount DECIMAL(18, 4), huge HUGEINT, user_id UUID, raw BLOB,
			spent INTERVAL, opens TIMETZ, seen TIMESTAMPTZ, tags INTEGER[], owner STRUCT(id INTEGER, name VARCHAR),
			prices MAP(VARCHAR, DECIMAL(9, 2)), mood append_mood)`,
	} {
		_, err := db.ExecContext(ctx, q)
		check(err)
	}

	// Append and the Appender are methods of the driver connection: pin one
	// connection of the pool and reach it through sql.Conn.Raw.
	conn, err := db.Conn(ctx)
	check(err)
	defer conn.Close()
	raw := func(f func(c *quackdriver.Conn) error) error {
		return conn.Raw(func(dc any) error { return f(dc.(*quackdriver.Conn)) })
	}

	// Type is any DuckDB type expression, user-defined types included.
	cols := []quackdriver.AppendColumn{
		{Name: "id", Type: "INTEGER"}, {Name: "amount", Type: "DECIMAL(18,4)"}, {Name: "huge", Type: "HUGEINT"},
		{Name: "user_id", Type: "UUID"}, {Name: "raw", Type: "BLOB"}, {Name: "spent", Type: "INTERVAL"},
		{Name: "opens", Type: "TIMETZ"}, {Name: "seen", Type: "TIMESTAMPTZ"}, {Name: "tags", Type: "INTEGER[]"},
		{Name: "owner", Type: "STRUCT(id INTEGER, name VARCHAR)"}, {Name: "prices", Type: "MAP(VARCHAR, DECIMAL(9,2))"},
		{Name: "mood", Type: "append_mood"},
	}
	huge, _ := new(big.Int).SetString("-170141183460469231731687303715884105728", 10)
	opens, err := quackdriver.NewTimeTz(9*time.Hour, -5*3600)
	check(err)
	seen := time.Date(2026, 5, 25, 14, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	rows := [][]any{
		{1, "1234.5678", huge, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", []byte{0xde, 0xad, 'h', 'i'},
			quackdriver.IntervalValue{Months: 1, Days: 2, Micros: 3_000_000}, opens, seen, []any{1, 2, nil},
			map[string]any{"id": 7, "name": "alice"}, map[string]any{"usd": "12.34", "eur": 56.78}, "happy"},
		{2, -0.0001, int64(-1), "00000000-0000-0000-0000-000000000001", "text as blob", 90 * time.Minute,
			seen /* a time.Time gives its time of day and offset */, seen.UTC(), []any{},
			map[string]any{"id": 8}, []any{map[string]any{"key": "zzz", "value": nil}}, 1 /* an ENUM index */},
		{3, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil},
	}
	check(raw(func(c *quackdriver.Conn) error { return c.Append(ctx, "append_demo", cols, rows) }))
	fmt.Printf("appended %d row(s)\n", len(rows))
	printTable(ctx, conn, "append_demo", cols)

	// A Go value a column does not accept is a *ValueError naming the row
	// and column; a value the server cannot cast fails the statement. Either
	// way Append inserts nothing.
	for _, bad := range []any{true, "nope"} {
		err := raw(func(c *quackdriver.Conn) error {
			return c.Append(ctx, "append_demo", cols[11:], [][]any{{"ok"}, {bad}})
		})
		var ve *quackdriver.ValueError
		var qe *quackdriver.Error
		switch {
		case errors.As(err, &ve):
			fmt.Printf("ValueError for row %d: %v\n", ve.Row, err)
		case errors.As(err, &qe):
			fmt.Printf("server error %q: %s\n", qe.Type, strings.SplitN(qe.Message, "\n", 2)[0])
		default:
			log.Fatalf("%v: %v", bad, err)
		}
	}
	fmt.Printf("rows in table: %d\n", scalar(ctx, conn, "SELECT count(*) FROM append_demo"))

	streaming(ctx, raw, conn)
}

// streaming loads rows with an Appender, which buffers them into batches and
// returns the number of inserted rows from Close. It holds its connection
// until Close or Abort; the stream is one statement, so an aborted or failed
// stream inserts nothing.
func streaming(ctx context.Context, raw func(func(*quackdriver.Conn) error) error, conn *sql.Conn) {
	_, err := conn.ExecContext(ctx, `CREATE OR REPLACE TABLE append_events (id BIGINT NOT NULL, kind VARCHAR,
		ts TIMESTAMP, note VARCHAR DEFAULT 'none')`)
	check(err)
	// Named columns: the INSERT lists them, so "note" takes its default.
	cols := []quackdriver.AppendColumn{{Name: "id", Type: "BIGINT"}, {Name: "kind", Type: "VARCHAR"}, {Name: "ts", Type: "TIMESTAMP"}}
	start := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	const total = 100_000

	var inserted int64
	check(raw(func(c *quackdriver.Conn) error {
		a, err := c.NewAppender(ctx, "", "append_events", cols, quackdriver.WithBatchBytes(1<<20))
		if err != nil {
			return err
		}
		defer func() { _ = a.Abort() }() // a no-op after a successful Close
		for i := range total {
			var kind any = []string{"click", "view", "buy"}[i%3]
			if i == 500 {
				kind = 12345 // not a string: the row is reported and skipped
			}
			if err := a.AppendRow(int64(i), kind, start.Add(time.Duration(i)*time.Second)); err != nil {
				if !errors.As(err, new(*quackdriver.ValueError)) {
					return err // any other error ends the stream
				}
				fmt.Printf("skipped: %v\n", err)
			}
		}
		inserted, err = a.Close()
		return err
	}))
	fmt.Printf("Close reported %d inserted row(s); note defaulted to %q\n", inserted,
		scalar(ctx, conn, "SELECT string_agg(DISTINCT note) FROM append_events"))

	// While an Appender is open its connection is busy; Abort cancels the
	// INSERT.
	check(raw(func(c *quackdriver.Conn) error {
		a, err := c.NewAppender(ctx, "main", "append_events", cols, quackdriver.WithUnordered())
		if err != nil {
			return err
		}
		if err := a.AppendRow(int64(-1), "aborted", start); err != nil {
			return err
		}
		if _, err := c.QueryContext(ctx, "SELECT 1", nil); errors.Is(err, quackdriver.ErrConnBusy) {
			fmt.Println("query during the stream:", err)
		}
		return a.Abort()
	}))

	// A server-side failure (the NOT NULL constraint) fails Close.
	err = raw(func(c *quackdriver.Conn) error {
		a, err := c.NewAppender(ctx, "", "append_events", cols)
		if err != nil {
			return err
		}
		defer func() { _ = a.Abort() }()
		for _, id := range []any{int64(-2), nil} {
			if err := a.AppendRow(id, "bad", start); err != nil {
				return err
			}
		}
		_, err = a.Close()
		return err
	})
	var qe *quackdriver.Error
	if !errors.As(err, &qe) {
		log.Fatalf("expected a server error, got %v", err)
	}
	fmt.Printf("constraint violation: type=%q; table holds %d row(s)\n", qe.Type,
		scalar(ctx, conn, "SELECT count(*) FROM append_events"))
}

// printTable prints the rows of table through the server's VARCHAR rendering,
// so the output shows exactly what DuckDB stored.
func printTable(ctx context.Context, conn *sql.Conn, table string, cols []quackdriver.AppendColumn) {
	exprs := make([]string, len(cols))
	for i, c := range cols {
		exprs[i] = c.Name + "::VARCHAR"
	}
	r, err := conn.QueryContext(ctx, "SELECT "+strings.Join(exprs, ", ")+" FROM "+table+" ORDER BY id")
	check(err)
	defer r.Close()
	for r.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		check(r.Scan(ptrs...))
		fmt.Println("row:")
		for i, v := range vals {
			if !v.Valid {
				v.String = "NULL"
			}
			fmt.Printf("  %-8s %s\n", cols[i].Name, v.String)
		}
	}
	check(r.Err())
}

func scalar(ctx context.Context, conn *sql.Conn, q string) any {
	var v any
	check(conn.QueryRowContext(ctx, q).Scan(&v))
	return v
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// dsn returns QUACK_DSN when set, else a DSN built from QUACK_HOST,
// QUACK_PORT and QUACK_TOKEN (default 127.0.0.1:9494, token super_secret).
func dsn() string {
	if d := os.Getenv("QUACK_DSN"); d != "" {
		return d
	}
	return "quack://" + net.JoinHostPort(env("QUACK_HOST", "127.0.0.1"), env("QUACK_PORT", "9494")) +
		"?token=" + url.QueryEscape(env("QUACK_TOKEN", "super_secret"))
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
