// Verify TIME columns Scan directly into time.Duration.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/zzir/quackdriver"
)

func main() {
	token := os.Getenv("QUACK_TOKEN")
	if token == "" {
		token = "super_secret"
	}
	db, _ := sql.Open("quack", "quack://127.0.0.1:9494?token="+token)
	defer db.Close()
	ctx := context.Background()

	rows, err := db.QueryContext(ctx, `SELECT TIME '12:34:56.789012' AS t, TIME '00:00:00' AS midnight`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var t, m time.Duration
		if err := rows.Scan(&t, &m); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("noon-ish:   %v (%d ns)\n", t, t)
		fmt.Printf("midnight:   %v\n", m)
	}

	rows2, err := db.QueryContext(ctx, `SELECT INTERVAL '1 month 2 days 03:04:05.678'`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows2.Close()
	if rows2.Next() {
		var iv any
		if err := rows2.Scan(&iv); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("interval:   %v (%T)\n", iv, iv)
	}

	rows3, err := db.QueryContext(ctx, `SELECT '13:45:00-05:00'::TIME WITH TIME ZONE AS tz`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows3.Close()
	if rows3.Next() {
		var tz any
		if err := rows3.Scan(&tz); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("time_tz:    %v\n", tz)
	}
}
