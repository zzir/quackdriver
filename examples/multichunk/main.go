// Integration test for the multi-chunk fetch loop and Exec drain paths.
//
// SEQUENCE-based queries (range(), generate_series) collapse to a single
// chunk regardless of row count, so they don't exercise the FETCH loop.
// To force real multi-chunk results we APPEND a wide enough table of
// VARCHAR rows that the server must split it across multiple DataChunks
// and require follow-up FETCH_REQUESTs.
//
// This verifies two regressions:
//
//   1. Rows.Next must keep fetching until the server returns an empty
//      result list. The earlier (buggy) code used HasBatchIndex as the
//      terminator, which could exit early and silently drop rows.
//
//   2. Conn.Exec must drain the result set when NeedsMoreFetch is true,
//      so subsequent operations on the same connection don't see
//      leftover server-side state.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	"github.com/zzir/quackdriver"
)

// Empirically the server packs ~20k 80-byte VARCHAR rows into a single
// chunk; ~200k splits into ~10 FETCH round-trips. The exact threshold
// depends on response-size caps inside DuckDB, so we pick a count well
// above where we observed splits during regression testing.
//
// To verify locally that this still drives a multi-FETCH loop, run
// `go run ./cmd/probe-fetch` — it reports the round-trip count.
const rows = 200000

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

	mustExec(ctx, db, "DROP TABLE IF EXISTS multichunk_demo")
	mustExec(ctx, db, "CREATE TABLE multichunk_demo (id BIGINT, payload VARCHAR)")

	conn, err := db.Conn(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	// Build a payload string long enough to spill across chunks.
	const payload = "the quick brown duck jumps over the lazy SQL parser " +
		"and lands on row "

	batch := make([][]any, rows)
	for i := range batch {
		batch[i] = []any{int64(i), fmt.Sprintf("%s%07d", payload, i)}
	}
	cols := []quackdriver.AppendColumn{
		{Name: "id", Type: "BIGINT"},
		{Name: "payload", Type: "VARCHAR"},
	}
	if err := conn.Raw(func(d any) error {
		return d.(*quackdriver.Conn).Append(ctx, "multichunk_demo", cols, batch)
	}); err != nil {
		log.Fatalf("append: %v", err)
	}
	fmt.Printf("appended %d rows\n", rows)

	// --- #5 regression: Query must return all rows.
	count, lastID := scanAll(ctx, db)
	if count != rows {
		log.Fatalf("multichunk Query missed rows: got %d want %d (last id=%d)", count, rows, lastID)
	}
	if lastID != int64(rows-1) {
		log.Fatalf("multichunk Query out of order: last id=%d want %d", lastID, rows-1)
	}
	fmt.Printf("Query saw all %d rows; last id=%d (#5 ok)\n", count, lastID)

	// --- #6 regression: Exec on a multi-chunk SELECT must drain the result
	// server-side, then leave the connection healthy for the next request.
	if _, err := db.ExecContext(ctx, "SELECT * FROM multichunk_demo"); err != nil {
		log.Fatalf("Exec multichunk SELECT: %v", err)
	}
	// If the previous Exec leaked the result UUID, a follow-up query on the
	// same connection may receive out-of-band responses, hang, or fail.
	var sanity int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM multichunk_demo").Scan(&sanity); err != nil {
		log.Fatalf("post-Exec sanity SELECT: %v", err)
	}
	if sanity != int64(rows) {
		log.Fatalf("post-Exec sanity: count=%d want %d", sanity, rows)
	}
	fmt.Printf("Exec drained cleanly; sanity count=%d (#6 ok)\n", sanity)
}

func mustExec(ctx context.Context, db *sql.DB, q string) {
	if _, err := db.ExecContext(ctx, q); err != nil {
		log.Fatalf("%q: %v", q, err)
	}
}

func scanAll(ctx context.Context, db *sql.DB) (count int, lastID int64) {
	r, err := db.QueryContext(ctx, "SELECT id, payload FROM multichunk_demo ORDER BY id")
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	for r.Next() {
		var id int64
		var p string
		if err := r.Scan(&id, &p); err != nil {
			log.Fatal(err)
		}
		count++
		lastID = id
	}
	if err := r.Err(); err != nil {
		log.Fatalf("rows err: %v", err)
	}
	return
}
