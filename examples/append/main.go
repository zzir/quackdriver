// APPEND smoke test: bulk-load rows via the Quack APPEND path, then SELECT
// them back to confirm round-trip correctness.
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
	db, err := sql.Open("quack", "quack://127.0.0.1:9494?token="+token)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()

	// Prepare table.
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS append_demo"); err != nil {
		log.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE append_demo (
		id BIGINT,
		name VARCHAR,
		active BOOLEAN,
		score DOUBLE,
		created TIMESTAMP
	)`); err != nil {
		log.Fatal(err)
	}

	// Append directly using the driver's high-level API.
	// We need an actual *Conn to call Append; pull one from the pool.
	conn, err := db.Conn(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	cols := []quackdriver.AppendColumn{
		{Name: "id", Type: "BIGINT"},
		{Name: "name", Type: "VARCHAR"},
		{Name: "active", Type: "BOOLEAN"},
		{Name: "score", Type: "DOUBLE"},
		{Name: "created", Type: "TIMESTAMP"},
	}
	rows := [][]any{
		{int64(1), "alice", true, 99.5, now},
		{int64(2), "bob", false, 73.2, now.Add(time.Hour)},
		{int64(3), nil, true, 0.0, now.Add(2 * time.Hour)},
	}

	if err := conn.Raw(func(driverConn any) error {
		c, ok := driverConn.(*quackdriver.Conn)
		if !ok {
			return fmt.Errorf("driver conn type: %T", driverConn)
		}
		return c.Append(ctx, "append_demo", cols, rows)
	}); err != nil {
		log.Fatalf("append: %v", err)
	}
	fmt.Printf("appended %d row(s)\n", len(rows))

	// Read back.
	r, err := db.QueryContext(ctx, "SELECT id, name, active, score, created FROM append_demo ORDER BY id")
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	for r.Next() {
		var (
			id      int64
			name    sql.NullString
			active  bool
			score   float64
			created time.Time
		)
		if err := r.Scan(&id, &name, &active, &score, &created); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  id=%d name=%-6s active=%v score=%.1f created=%s\n",
			id, displayName(name), active, score, created.Format(time.RFC3339))
	}
}

func displayName(n sql.NullString) string {
	if !n.Valid {
		return "<null>"
	}
	return n.String
}
