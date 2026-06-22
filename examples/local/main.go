// Local DuckDB example: the driver auto-starts a DuckDB process — no
// separate server needed. Requires the "duckdb" CLI (v1.5.3+) on PATH.
//
// Usage:
//
//	go run ./examples/local
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/zzir/quackdriver"
	_ "github.com/zzir/quackdriver"
)

func main() {
	dir, err := os.MkdirTemp("", "quack-local-*")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	fmt.Println("using temp dir:", dir)

	dbFile := filepath.Join(dir, "demo.db")
	dsn := fmt.Sprintf("quack://local%s?token=auto", dbFile)

	db, err := sql.Open("quack", dsn)
	if err != nil {
		log.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	defer quackdriver.CloseLocalServers()

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping: %v", err)
	}
	fmt.Println("connected to local DuckDB")

	if _, err := db.ExecContext(ctx, "CREATE TABLE greetings (msg VARCHAR)"); err != nil {
		log.Fatalf("create table: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO greetings VALUES ('hello from local DuckDB')"); err != nil {
		log.Fatalf("insert: %v", err)
	}

	var msg string
	if err := db.QueryRowContext(ctx, "SELECT msg FROM greetings").Scan(&msg); err != nil {
		log.Fatalf("query: %v", err)
	}
	fmt.Println("msg:", msg)
}
