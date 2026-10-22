// bun ORM on top of quackdriver (database/sql), using bun's SQLite dialect,
// whose SQL DuckDB accepts.
//
//	# In one terminal (DuckDB 2.0+ with the quack extension):
//	bash ../../scripts/start-quack-server.sh
//
//	# In examples/bun (its own module, so bun stays out of the driver's
//	# dependencies):
//	go run .
//
// The server address comes from QUACK_HOST, QUACK_PORT and QUACK_TOKEN
// (default 127.0.0.1:9494, token super_secret), or from a full QUACK_DSN.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	_ "github.com/zzir/quackdriver"
)

// User is a tiny model for the smoke test.
//
// We explicitly tag ID as BIGINT because bun's sqlitedialect maps int64 to
// "INTEGER" — but DuckDB's INTEGER is 32-bit, so the scan back into int64
// would fail. BIGINT is 64-bit in DuckDB and matches the Go type exactly.
type User struct {
	bun.BaseModel `bun:"table:bun_users"`

	ID     int64  `bun:"id,pk,type:BIGINT"`
	Name   string `bun:"name,notnull"`
	Active bool   `bun:"active"`
}

func main() {
	sqldb, err := sql.Open("quack", dsn())
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer sqldb.Close()

	db := bun.NewDB(sqldb, sqlitedialect.New())
	ctx := context.Background()

	// Drop + create.
	if _, err := db.NewDropTable().Model((*User)(nil)).IfExists().Exec(ctx); err != nil {
		log.Fatalf("drop: %v", err)
	}
	if _, err := db.NewCreateTable().Model((*User)(nil)).Exec(ctx); err != nil {
		log.Fatalf("create: %v", err)
	}

	// Insert.
	users := []User{
		{ID: 1, Name: "alice", Active: true},
		{ID: 2, Name: "bob", Active: false},
		{ID: 3, Name: "carol", Active: true},
	}
	if _, err := db.NewInsert().Model(&users).Exec(ctx); err != nil {
		log.Fatalf("insert: %v", err)
	}

	// Select all.
	var got []User
	if err := db.NewSelect().Model(&got).Order("id ASC").Scan(ctx); err != nil {
		log.Fatalf("select: %v", err)
	}
	fmt.Println("all users:")
	for _, u := range got {
		fmt.Printf("  %d %s active=%v\n", u.ID, u.Name, u.Active)
	}

	// Where filter.
	var actives []User
	if err := db.NewSelect().Model(&actives).Where("active = ?", true).Order("id ASC").Scan(ctx); err != nil {
		log.Fatalf("filter: %v", err)
	}
	fmt.Println("active users:")
	for _, u := range actives {
		fmt.Printf("  %d %s\n", u.ID, u.Name)
	}

	// Update: bun reads RowsAffected from the driver.
	res, err := db.NewUpdate().Model(&User{ID: 2, Name: "bob", Active: true}).WherePK().Exec(ctx)
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	n, _ := res.RowsAffected()
	fmt.Printf("updated %d row(s)\n", n)

	// Transaction: delete and insert atomically.
	err = db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewDelete().Model((*User)(nil)).Where("name = ?", "carol").Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		fmt.Printf("deleted %d row(s) in a transaction\n", n)
		_, err = tx.NewInsert().Model(&User{ID: 4, Name: "dave", Active: true}).Exec(ctx)
		return err
	})
	if err != nil {
		log.Fatalf("tx: %v", err)
	}

	// Count.
	cnt, err := db.NewSelect().Model((*User)(nil)).Where("active = ?", true).Count(ctx)
	if err != nil {
		log.Fatalf("count: %v", err)
	}
	fmt.Printf("active count: %d\n", cnt)
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
