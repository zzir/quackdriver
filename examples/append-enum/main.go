// APPEND with ENUM columns: parser syntax + value conversion + encoder
// dictionary lookup verified end-to-end.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	"github.com/zzir/quackdriver"
)

func main() {
	token := os.Getenv("QUACK_TOKEN")
	if token == "" {
		token = "super_secret"
	}
	db, err := sql.Open("quack", "quack://127.0.0.1:9494?token="+token)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	mustExec(ctx, db, "DROP TYPE IF EXISTS mood")
	mustExec(ctx, db, "CREATE TYPE mood AS ENUM('sad', 'ok', 'happy')")
	mustExec(ctx, db, "DROP TABLE IF EXISTS enum_demo")
	mustExec(ctx, db, "CREATE TABLE enum_demo (id INTEGER, m mood)")

	conn, err := db.Conn(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	cols := []quackdriver.AppendColumn{
		{Name: "id", Type: "INTEGER"},
		{Name: "m", Type: "ENUM('sad', 'ok', 'happy')"},
	}
	rows := [][]any{
		{int32(1), "happy"},
		{int32(2), "sad"},
		{int32(3), uint8(1)}, // integer index → "ok"
		{int32(4), nil},
	}
	if err := conn.Raw(func(d any) error {
		return d.(*quackdriver.Conn).Append(ctx, "enum_demo", cols, rows)
	}); err != nil {
		log.Fatalf("append: %v", err)
	}
	fmt.Printf("appended %d row(s)\n", len(rows))

	r, err := db.QueryContext(ctx, "SELECT id, m FROM enum_demo ORDER BY id")
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	for r.Next() {
		var id any
		var m sql.NullString
		if err := r.Scan(&id, &m); err != nil {
			log.Fatal(err)
		}
		label := "<null>"
		if m.Valid {
			label = m.String
		}
		fmt.Printf("  id=%v m=%s\n", id, label)
	}

	// Reject unknown label.
	if err := conn.Raw(func(d any) error {
		return d.(*quackdriver.Conn).Append(ctx, "enum_demo", cols, [][]any{{int32(99), "nope"}})
	}); err == nil {
		log.Fatal("expected error for unknown label, got nil")
	}
	fmt.Printf("unknown label correctly rejected: %v\n", err)
}

func mustExec(ctx context.Context, db *sql.DB, q string) {
	if _, err := db.ExecContext(ctx, q); err != nil {
		log.Fatalf("%q: %v", q, err)
	}
}
