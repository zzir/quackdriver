// Bench: load rows with per-row INSERT, multi-row INSERT, Conn.Append and a
// streaming Appender, and compare the wall-clock throughput.
//
// Every INSERT is one Quack round trip with the values rendered as SQL
// literals; Append and the Appender stream the values as text (SEND_DATA)
// into a single INSERT ... SELECT statement that casts them.
//
//	go run ./examples/bench-append [-rows 200000] [-insert-rows 1000]
//
// The server address comes from QUACK_HOST, QUACK_PORT and QUACK_TOKEN
// (default 127.0.0.1:9494, token super_secret), or from a full QUACK_DSN.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zzir/quackdriver"
)

func main() {
	n := flag.Int("rows", 200_000, "rows for the multi-row INSERT, Append and Appender runs")
	nInsert := flag.Int("insert-rows", 1_000, "rows for the per-row INSERT run (one round trip each)")
	flag.Parse()

	db, err := sql.Open("quack", dsn())
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	conn, err := db.Conn(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	reset := func() {
		if _, err := conn.ExecContext(ctx,
			"CREATE OR REPLACE TABLE bench_append_t (id BIGINT, name VARCHAR, score DOUBLE)"); err != nil {
			log.Fatal(err)
		}
	}
	check := func(label string, want int, d time.Duration) {
		var got int
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM bench_append_t").Scan(&got); err != nil {
			log.Fatal(err)
		}
		if got != want {
			log.Fatalf("%s: table holds %d rows, want %d", label, got, want)
		}
		fmt.Printf("%-24s %8d rows %12v %12.0f rows/s\n", label, want, d.Round(time.Microsecond), float64(want)/d.Seconds())
	}
	row := func(i int) []any { return []any{int64(i), fmt.Sprintf("row-%d", i), float64(i) * 1.5} }
	cols := []quackdriver.AppendColumn{
		{Name: "id", Type: "BIGINT"},
		{Name: "name", Type: "VARCHAR"},
		{Name: "score", Type: "DOUBLE"},
	}

	// Per-row INSERT inside one transaction.
	reset()
	start := time.Now()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	for i := range *nInsert {
		if _, err := tx.ExecContext(ctx, "INSERT INTO bench_append_t VALUES (?, ?, ?)", row(i)...); err != nil {
			log.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		log.Fatal(err)
	}
	check("INSERT per row", *nInsert, time.Since(start))

	// Multi-row INSERT: 1000 rows per statement.
	reset()
	start = time.Now()
	const perStmt = 1000
	for lo := 0; lo < *n; lo += perStmt {
		hi := min(lo+perStmt, *n)
		args := make([]any, 0, 3*(hi-lo))
		for i := lo; i < hi; i++ {
			args = append(args, row(i)...)
		}
		q := "INSERT INTO bench_append_t VALUES " + strings.TrimSuffix(strings.Repeat("(?, ?, ?),", hi-lo), ",")
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			log.Fatal(err)
		}
	}
	check("INSERT 1000 rows/stmt", *n, time.Since(start))

	// Conn.Append with every row in memory.
	reset()
	rows := make([][]any, *n)
	for i := range rows {
		rows[i] = row(i)
	}
	start = time.Now()
	if err := conn.Raw(func(d any) error {
		return d.(*quackdriver.Conn).Append(ctx, "bench_append_t", cols, rows)
	}); err != nil {
		log.Fatal(err)
	}
	check("Conn.Append", *n, time.Since(start))

	// Streaming Appender, producing rows on the fly.
	reset()
	start = time.Now()
	var inserted int64
	if err := conn.Raw(func(d any) error {
		a, err := d.(*quackdriver.Conn).NewAppender(ctx, "", "bench_append_t", cols)
		if err != nil {
			return err
		}
		defer func() { _ = a.Abort() }()
		for i := range *n {
			if err := a.AppendRow(row(i)...); err != nil {
				return err
			}
		}
		inserted, err = a.Close()
		return err
	}); err != nil {
		log.Fatal(err)
	}
	if inserted != int64(*n) {
		log.Fatalf("Appender.Close reported %d rows, want %d", inserted, *n)
	}
	check("Appender", *n, time.Since(start))

	if _, err := conn.ExecContext(ctx, "DROP TABLE bench_append_t"); err != nil {
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
