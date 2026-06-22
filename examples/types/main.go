// Type-coverage smoke test: exercise primitive types end-to-end against a
// running Quack server.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

	_ "github.com/zzir/quackdriver"
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

	type tc struct {
		sql  string
		want any
	}
	cases := []tc{
		{"SELECT TRUE", true},
		{"SELECT 42::TINYINT", int8(42)},
		{"SELECT 42::SMALLINT", int16(42)},
		{"SELECT 42::INTEGER", int32(42)},
		{"SELECT 42::BIGINT", int64(42)},
		{"SELECT 42::UTINYINT", uint8(42)},
		{"SELECT 42::USMALLINT", uint16(42)},
		{"SELECT 42::UINTEGER", uint32(42)},
		{"SELECT 42::UBIGINT", uint64(42)},
		{"SELECT 3.14::FLOAT", float32(3.14)},
		{"SELECT 3.14::DOUBLE", float64(3.14)},
		{"SELECT 'hello'", "hello"},
		{"SELECT NULL::INTEGER", nil},
	}

	for _, c := range cases {
		rows, err := db.QueryContext(ctx, c.sql)
		if err != nil {
			log.Fatalf("%s: query: %v", c.sql, err)
		}
		if !rows.Next() {
			_ = rows.Close()
			log.Fatalf("%s: no rows", c.sql)
		}
		var got any
		if err := rows.Scan(&got); err != nil {
			_ = rows.Close()
			log.Fatalf("%s: scan: %v", c.sql, err)
		}
		_ = rows.Close()
		fmt.Printf("%-40s  want=%-25T got=%-25T value=%v\n", c.sql, c.want, got, got)
	}

	// HUGEINT — driver returns *big.Int.
	var huge *big.Int
	if err := db.QueryRowContext(ctx, "SELECT 170141183460469231731687303715884105727::HUGEINT").Scan(&huge); err != nil {
		log.Fatalf("hugeint scan: %v", err)
	}
	fmt.Printf("HUGEINT max  -> %v\n", huge)

	// UUID stays as string; DATE / TIMESTAMP scan into time.Time; DECIMAL scans into string.
	rows, err := db.QueryContext(ctx,
		`SELECT 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::UUID,
		        '2026-05-25'::DATE,
		        TIMESTAMP '2026-05-25 12:34:56',
		        12345.67::DECIMAL(9,2)`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var (
			uuid string
			date time.Time
			ts   time.Time
			dec  string
		)
		if err := rows.Scan(&uuid, &date, &ts, &dec); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("UUID=%s DATE=%s TIMESTAMP=%s DECIMAL=%s\n",
			uuid, date.Format("2006-01-02"), ts.Format(time.RFC3339Nano), dec)
	}

	// Multi-chunk: range(5000) crosses chunk boundaries (default 2048 / chunk).
	r, err := db.QueryContext(ctx, "SELECT count(*) FROM range(5000)")
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	if r.Next() {
		var n int64
		_ = r.Scan(&n)
		fmt.Printf("range(5000) count = %d\n", n)
	}
}
