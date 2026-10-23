// Basic quackdriver example: connect, run statements in a transaction with
// placeholders, iterate rows, inspect a typed server error, and read a large
// result that arrives in many FETCH batches.
//
//	go run ./examples/basic
//
// The server address comes from QUACK_HOST, QUACK_PORT and QUACK_TOKEN
// (default 127.0.0.1:9494, token super_secret), or from a full QUACK_DSN.
// scripts/start-quack-server.sh starts a server (the DuckDB 2.0+ CLI with
// the quack extension).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/zzir/quackdriver"
)

func main() {
	db, err := sql.Open("quack", dsn())
	check(err)
	defer db.Close()
	ctx := context.Background()

	// DDL and DML in a transaction. Arguments are rendered client-side as
	// SQL literals: Quack carries only SQL text.
	tx, err := db.BeginTx(ctx, nil)
	check(err)
	_, err = tx.ExecContext(ctx, "CREATE OR REPLACE TABLE basic_users (id INTEGER, name VARCHAR, active BOOLEAN)")
	check(err)
	res, err := tx.ExecContext(ctx, "INSERT INTO basic_users VALUES (?, ?, ?), (?, ?, ?), (?, ?, ?)",
		1, "alice", true, 2, "bob", false, 3, "carol", true)
	check(err)
	n, _ := res.RowsAffected()
	fmt.Printf("inserted %d row(s)\n", n)
	res, err = tx.ExecContext(ctx, "UPDATE basic_users SET active = NOT active WHERE id >= $1", 2)
	check(err)
	n, _ = res.RowsAffected()
	fmt.Printf("updated %d row(s)\n", n)
	check(tx.Commit())

	rows, err := db.QueryContext(ctx, "SELECT id, name, active FROM basic_users ORDER BY id")
	check(err)
	for rows.Next() {
		var id int32
		var name string
		var active bool
		check(rows.Scan(&id, &name, &active))
		fmt.Printf("user %d: %s active=%v\n", id, name, active)
	}
	check(rows.Err())
	var count int64
	check(db.QueryRowContext(ctx, "SELECT count(*) FROM basic_users WHERE active = $active",
		sql.Named("active", true)).Scan(&count))
	fmt.Printf("active users: %d\n", count)

	// Server errors are *quackdriver.Error values carrying DuckDB's exception
	// type; the pool stays usable.
	_, err = db.ExecContext(ctx, "SELECT * FROM basic_no_such_table")
	var qe *quackdriver.Error
	if !errors.As(err, &qe) {
		log.Fatalf("expected a *quackdriver.Error, got %v", err)
	}
	msg, _, _ := strings.Cut(qe.Message, "\n")
	fmt.Printf("error type=%q message=%q\n", qe.Type, msg)

	largeResult(ctx, db)
}

// largeResult reads a result that arrives in many FETCH batches: PREPARE
// returns the first rows inline, and the driver reads the rest ahead in the
// background (DSN parameter fetch_ahead, default 1). Server settings are per
// session, so one pinned connection makes the batches small.
func largeResult(ctx context.Context, db *sql.DB) {
	c, err := db.Conn(ctx)
	check(err)
	defer c.Close()
	for _, q := range []string{
		"SET quack_target_batch_bytes = 65536",
		"CREATE OR REPLACE TABLE basic_big AS SELECT i AS id, 'row ' || i AS payload FROM range(200000) t(i)",
	} {
		_, err := c.ExecContext(ctx, q)
		check(err)
	}

	r, err := c.QueryContext(ctx, "SELECT id FROM basic_big ORDER BY id")
	check(err)
	read, last := 0, int64(-1)
	for r.Next() {
		var id int64
		check(r.Scan(&id))
		if id != last+1 {
			log.Fatalf("row %d after %d", id, last)
		}
		read, last = read+1, id
	}
	check(r.Err())
	fmt.Printf("read %d rows in order\n", read)

	// Closing Rows early cancels the statement; the connection keeps working.
	r, err = c.QueryContext(ctx, "SELECT id FROM basic_big ORDER BY id")
	check(err)
	for range 10 {
		r.Next()
	}
	check(r.Close())
	var v int
	check(c.QueryRowContext(ctx, "SELECT 42").Scan(&v))
	fmt.Printf("closed early; connection still works: %d\n", v)

	// A statement started while Rows still has batches on the server first
	// buffers the rest of that result (Quack aborts a running statement when
	// the next one is prepared), so reads and writes can interleave.
	tx, err := c.BeginTx(ctx, nil)
	check(err)
	r, err = tx.QueryContext(ctx, "SELECT id FROM basic_big WHERE id % 10 = 0 ORDER BY id")
	check(err)
	seen, updated := 0, int64(0)
	for r.Next() {
		var id int64
		check(r.Scan(&id))
		seen++
		if id%50_000 == 0 {
			res, err := tx.ExecContext(ctx, "UPDATE basic_big SET payload = 'touched' WHERE id = ?", id)
			check(err)
			n, _ := res.RowsAffected()
			updated += n
		}
	}
	check(r.Err())
	check(tx.Commit())
	fmt.Printf("interleaved: read %d rows, updated %d\n", seen, updated)
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
