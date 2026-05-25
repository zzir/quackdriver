// APPEND smoke test for the rich types: DECIMAL, HUGEINT, UUID, INTERVAL,
// LIST<INTEGER>, INTEGER[3].
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

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

	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS rich_append"); err != nil {
		log.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE rich_append (
		amt DECIMAL(18, 4),
		bignum HUGEINT,
		user_id UUID,
		spent INTERVAL,
		tags INTEGER[],
		triple INTEGER[3]
	)`); err != nil {
		log.Fatal(err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	cols := []quackdriver.AppendColumn{
		{Name: "amt", Type: "DECIMAL(18,4)"},
		{Name: "bignum", Type: "HUGEINT"},
		{Name: "user_id", Type: "UUID"},
		{Name: "spent", Type: "INTERVAL"},
		{Name: "tags", Type: "INTEGER[]"},
		{Name: "triple", Type: "INTEGER[3]"},
	}
	bignum, _ := new(big.Int).SetString("12345678901234567890", 10)
	rows := [][]any{
		{
			"1234.5678",
			bignum,
			"a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
			time.Hour + 30*time.Minute,
			[]any{int32(1), int32(2), int32(3)},
			[]any{int32(10), int32(20), int32(30)},
		},
		{
			"-0.0001",
			big.NewInt(-1),
			"00000000-0000-0000-0000-000000000001",
			3 * time.Second,
			[]any{int32(99)},
			[]any{int32(0), int32(0), int32(0)},
		},
		{nil, nil, nil, nil, nil, nil},
	}

	if err := conn.Raw(func(d any) error {
		return d.(*quackdriver.Conn).Append(ctx, "rich_append", cols, rows)
	}); err != nil {
		log.Fatalf("append: %v", err)
	}
	fmt.Printf("appended %d row(s)\n", len(rows))

	r, err := db.QueryContext(ctx,
		`SELECT amt::VARCHAR, bignum::VARCHAR, user_id::VARCHAR, spent, tags, triple FROM rich_append ORDER BY user_id NULLS LAST`)
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	for r.Next() {
		var (
			amt    sql.NullString
			bn     sql.NullString
			uid    sql.NullString
			spent  any
			tags   any
			triple any
		)
		if err := r.Scan(&amt, &bn, &uid, &spent, &tags, &triple); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  amt=%v bignum=%v uuid=%v spent=%v tags=%v triple=%v\n",
			amt, bn, uid, spent, tags, triple)
	}
}
