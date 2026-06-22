// Bench: APPEND vs INSERT for 10,000 rows. Quick-and-dirty wall-clock timing
// to confirm the APPEND path beats parameterized INSERT.
//
//	QUACK_TOKEN=... go run ./examples/bench-append
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/zzir/quackdriver"
)

func main() {
	token := os.Getenv("QUACK_TOKEN")
	if token == "" {
		token = "super_secret"
	}
	db, _ := sql.Open("quack", "quack://127.0.0.1:9494?token="+token)
	defer db.Close()
	ctx := context.Background()

	const N = 10000

	// Setup.
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS bench_t")
	_, _ = db.ExecContext(ctx, "CREATE TABLE bench_t (id BIGINT, name VARCHAR, score DOUBLE)")

	// INSERT path.
	insertStart := time.Now()
	tx, _ := db.BeginTx(ctx, nil)
	for i := range N {
		if _, err := tx.ExecContext(ctx, "INSERT INTO bench_t VALUES (?, ?, ?)", int64(i), "row", float64(i)*1.5); err != nil {
			log.Fatal(err)
		}
	}
	_ = tx.Commit()
	insertDur := time.Since(insertStart)
	_, _ = db.ExecContext(ctx, "DELETE FROM bench_t")

	// APPEND path.
	rows := make([][]any, N)
	for i := range rows {
		rows[i] = []any{int64(i), "row", float64(i) * 1.5}
	}
	appendStart := time.Now()
	conn, _ := db.Conn(ctx)
	_ = conn.Raw(func(d any) error {
		return d.(*quackdriver.Conn).Append(ctx, "bench_t",
			[]quackdriver.AppendColumn{{Name: "id", Type: "BIGINT"}, {Name: "name", Type: "VARCHAR"}, {Name: "score", Type: "DOUBLE"}},
			rows)
	})
	_ = conn.Close()
	appendDur := time.Since(appendStart)

	fmt.Printf("INSERT %d rows: %v (%.0f rows/sec)\n", N, insertDur, float64(N)/insertDur.Seconds())
	fmt.Printf("APPEND %d rows: %v (%.0f rows/sec)\n", N, appendDur, float64(N)/appendDur.Seconds())
	fmt.Printf("speedup: %.1fx\n", float64(insertDur)/float64(appendDur))
}
