// bun integration smoke test.
//
//	# In one terminal:
//	bash ../../scripts/start-quack-server.sh
//
//	# In examples/bun:
//	go run .
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
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
	token := os.Getenv("QUACK_TOKEN")
	if token == "" {
		token = "super_secret"
	}
	sqldb, err := sql.Open("quack", "quack://127.0.0.1:9494?token="+token)
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

	// Update.
	if _, err := db.NewUpdate().Model(&User{ID: 2, Name: "bob", Active: true}).WherePK().Exec(ctx); err != nil {
		log.Fatalf("update: %v", err)
	}

	// Count.
	cnt, err := db.NewSelect().Model((*User)(nil)).Where("active = ?", true).Count(ctx)
	if err != nil {
		log.Fatalf("count: %v", err)
	}
	fmt.Printf("active count: %d\n", cnt)
}
