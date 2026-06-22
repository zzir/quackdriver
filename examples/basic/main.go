// Basic quackdriver example: connect, run a SELECT, iterate rows.
//
// Usage:
//
//	# In one terminal:
//	bash scripts/start-quack-server.sh
//
//	# In another:
//	QUACK_TOKEN=super_secret go run ./examples/basic
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/zzir/quackdriver"
)

func main() {
	token := os.Getenv("QUACK_TOKEN")
	if token == "" {
		token = "super_secret"
	}
	dsn := fmt.Sprintf("quack://127.0.0.1:9494?token=%s", token)

	db, err := sql.Open("quack", dsn)
	if err != nil {
		log.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping: %v", err)
	}

	// DDL + DML in a transaction.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		log.Fatalf("begin: %v", err)
	}
	mustExec := func(q string, args ...any) {
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			_ = tx.Rollback()
			log.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec("CREATE OR REPLACE TABLE users (id INTEGER, name VARCHAR, active BOOLEAN)")
	mustExec("INSERT INTO users VALUES (?, ?, ?)", 1, "alice", true)
	mustExec("INSERT INTO users VALUES (?, ?, ?)", 2, "bob", false)
	if err := tx.Commit(); err != nil {
		log.Fatalf("commit: %v", err)
	}

	// Query.
	rows, err := db.QueryContext(ctx, "SELECT id, name, active FROM users ORDER BY id")
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int32
		var name string
		var active bool
		if err := rows.Scan(&id, &name, &active); err != nil {
			log.Fatalf("scan: %v", err)
		}
		fmt.Printf("user %d: %s active=%v\n", id, name, active)
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("rows: %v", err)
	}

	// Multi-chunk fetch demo.
	rows2, err := db.QueryContext(ctx, "SELECT * FROM range(5)")
	if err != nil {
		log.Fatalf("range query: %v", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var v int64
		if err := rows2.Scan(&v); err != nil {
			log.Fatalf("scan: %v", err)
		}
		fmt.Println("range:", v)
	}
}
