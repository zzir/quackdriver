// Nested-type smoke test: LIST, STRUCT, ARRAY, MAP, DECIMAL, INTERVAL.
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
	db, err := sql.Open("quack", "quack://127.0.0.1:9494?token="+token)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	queries := []string{
		"SELECT [1, 2, 3]::INTEGER[]",
		"SELECT {'a': 1, 'b': 'x'}",
		"SELECT [[1,2],[3,4]]",
		"SELECT MAP {'k':'v', 'k2':'v2'}",
		"SELECT array_value(10::INTEGER, 20::INTEGER, 30::INTEGER)",
		"SELECT 1.23::DECIMAL(9,2)",
		"SELECT INTERVAL '1 day 2 hours'",
	}
	for _, q := range queries {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			log.Fatalf("%s: %v", q, err)
		}
		if !rows.Next() {
			rows.Close()
			log.Fatalf("%s: no rows", q)
		}
		var v any
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			log.Fatalf("%s scan: %v", q, err)
		}
		fmt.Printf("%-50s -> %T %v\n", q, v, v)
		rows.Close()
	}
}
