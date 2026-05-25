// Verify APPEND of STRUCT and MAP rows end-to-end.
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

	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS struct_demo"); err != nil {
		log.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE struct_demo (
		row_id INTEGER,
		owner STRUCT(id INTEGER, name VARCHAR),
		amounts MAP(VARCHAR, DECIMAL(9, 2)),
		matrix INTEGER[3][2]
	)`); err != nil {
		log.Fatalf("create: %v", err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	cols := []quackdriver.AppendColumn{
		{Name: "row_id", Type: "INTEGER"},
		{Name: "owner", Type: "STRUCT(id INTEGER, name VARCHAR)"},
		{Name: "amounts", Type: "MAP(VARCHAR, DECIMAL(9, 2))"},
		{Name: "matrix", Type: "INTEGER[3][2]"},
	}
	rows := [][]any{
		{
			int32(1),
			map[string]any{"id": int32(7), "name": "alice"},
			map[string]any{"usd": "12.34", "eur": "56.78"},
			[]any{
				[]any{int32(1), int32(2), int32(3)},
				[]any{int32(4), int32(5), int32(6)},
			},
		},
		{
			int32(2),
			map[string]any{"id": int32(8), "name": nil},
			map[string]any{},
			[]any{
				[]any{int32(0), int32(0), int32(0)},
				[]any{int32(0), int32(0), int32(0)},
			},
		},
		{int32(3), nil, nil, nil},
	}

	if err := conn.Raw(func(d any) error {
		return d.(*quackdriver.Conn).Append(ctx, "struct_demo", cols, rows)
	}); err != nil {
		log.Fatalf("append: %v", err)
	}
	fmt.Printf("appended %d row(s)\n", len(rows))

	r, err := db.QueryContext(ctx, `SELECT row_id, owner, amounts, matrix FROM struct_demo ORDER BY row_id`)
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	for r.Next() {
		var id any
		var owner, amts, mtx any
		if err := r.Scan(&id, &owner, &amts, &mtx); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  id=%v  owner=%v  amounts=%v  matrix=%v\n", id, owner, amts, mtx)
	}
}
