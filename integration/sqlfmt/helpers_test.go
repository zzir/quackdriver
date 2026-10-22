//go:build integration

// Package sqlfmt holds end-to-end tests of client-side parameter
// substitution: arguments and placeholder forms go through database/sql to
// a real DuckDB 2.0 server, which must see exactly the intended value.
package sqlfmt

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/value"
)

// openDB returns a pool on this test binary's shared server.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db := livetest.OpenDB(t, livetest.SharedDSN(t))
	db.SetMaxIdleConns(8)
	return db
}

// queryRow runs a query expected to return exactly one row and returns its
// values.
func queryRow(db *sql.DB, query string, args ...any) ([]any, error) {
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		return nil, fmt.Errorf("%q returned no rows: %w", query, rows.Err())
	}
	out := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range out {
		ptrs[i] = &out[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	if rows.Next() {
		return nil, fmt.Errorf("%q returned more than one row", query)
	}
	return out, rows.Err()
}

// mustRow is queryRow failing the test on error.
func mustRow(tb testing.TB, db *sql.DB, query string, args ...any) []any {
	tb.Helper()
	row, err := queryRow(db, query, args...)
	if err != nil {
		tb.Fatalf("%s %v: %v", query, args, err)
	}
	return row
}

// same reports whether a decoded value equals want exactly: floats bit for
// bit (any NaN matches any NaN), times as instants, big integers by value.
func same(got, want any) bool {
	switch w := want.(type) {
	case float64:
		g, ok := got.(float64)
		if math.IsNaN(w) {
			return ok && math.IsNaN(g)
		}
		return ok && math.Float64bits(g) == math.Float64bits(w)
	case time.Time:
		g, ok := got.(time.Time)
		return ok && g.Equal(w)
	case []byte:
		g, ok := got.([]byte)
		return ok && bytes.Equal(g, w)
	case *big.Int:
		g, ok := got.(*big.Int)
		return ok && g.Cmp(w) == 0
	}
	return reflect.DeepEqual(got, want)
}

// placeholderCase runs query with args and expects the single result row.
type placeholderCase struct {
	name  string
	query string
	args  []any
	want  []any
}

func runPlaceholderCases(t *testing.T, db *sql.DB, cases []placeholderCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			row := mustRow(t, db, c.query, c.args...)
			if len(row) != len(c.want) {
				t.Fatalf("%s: got %v, want %v", c.query, row, c.want)
			}
			for i := range row {
				if !same(row[i], c.want[i]) {
					t.Errorf("%s: column %d = %T %#v, want %T %#v", c.query, i, row[i], row[i], c.want[i], c.want[i])
				}
			}
		})
	}
}

func bigInt(s string) *big.Int {
	b, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad big.Int " + s)
	}
	return b
}

func bigRat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("bad big.Rat " + s)
	}
	return r
}

func timeTz(tod time.Duration, offset int32) value.TimeTzValue {
	v, err := value.NewTimeTz(tod, offset)
	if err != nil {
		panic(err)
	}
	return v
}
