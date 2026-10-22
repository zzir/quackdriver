//go:build integration

package protocol

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/livetest"
)

// smallBatches opens a dedicated connection whose results span many small
// FETCH batches.
func smallBatches(t *testing.T, db *sql.DB) *sql.Conn {
	t.Helper()
	c := livetest.Conn(t, db)
	livetest.Exec(t, c, "SET quack_target_batch_bytes = 16384")
	return c
}

// TestRowsCloseReadOnlyCancels closes a SELECT before its end: the driver
// cancels the statement instead of reading the rest.
func TestRowsCloseReadOnlyCancels(t *testing.T) {
	srv, mon := newDB(t)
	c := smallBatches(t, livetest.OpenDB(t, srv.DSN("inline_rows=10")))
	id := sessionID(t, c)
	rows, err := c.QueryContext(context.Background(), "SELECT i FROM range(100000000) t(i)")
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		rows.Next()
	}
	within(t, 2*time.Second, "Rows.Close", func() {
		if err := rows.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	if s := sessions(t, mon)[id]; s.State != "cancelled" {
		t.Fatalf("state after Close: %+v, want cancelled", s)
	}
	scalar(t, c, "SELECT 7")
}

// TestRowsCloseDMLDrains closes an INSERT ... RETURNING before its end: the
// statement must complete, so every row is inserted.
func TestRowsCloseDMLDrains(t *testing.T) {
	_, db := newDB(t, "inline_rows=10")
	c := smallBatches(t, db)
	livetest.Exec(t, c, "CREATE TABLE protocol_ret (i BIGINT)")
	for _, q := range []string{
		"INSERT INTO protocol_ret SELECT i FROM range(300000) t(i) RETURNING i",
		"WITH s AS (SELECT i FROM range(300000, 600000) t(i)) INSERT INTO protocol_ret SELECT i FROM s RETURNING i",
	} {
		rows, err := c.QueryContext(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		for range 5 {
			rows.Next()
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	if n := scalar(t, c, "SELECT count(DISTINCT i) FROM protocol_ret"); n != 600000 {
		t.Fatalf("%d rows inserted, want 600000", n)
	}
}

// TestOpenRowsThenStatement runs statements on the connection of an open
// Rows (directly and inside a transaction): the rest of the result is
// buffered, so both complete correctly.
func TestOpenRowsThenStatement(t *testing.T) {
	_, db := newDB(t, "inline_rows=10", "fetch_ahead=2")
	c := smallBatches(t, db)
	livetest.Exec(t, c, "CREATE TABLE protocol_interleave (i BIGINT)")
	const total = 100000
	check := func(q livetest.Queryer) {
		rows, err := q.QueryContext(context.Background(), fmt.Sprintf("SELECT i FROM range(%d) t(i)", total))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := int64(0)
		for ; rows.Next(); n++ {
			var i int64
			if err := rows.Scan(&i); err != nil || i != n {
				t.Fatalf("row %d = %d, %v", n, i, err)
			}
			if n%25000 == 3 {
				livetest.Exec(t, q, "INSERT INTO protocol_interleave VALUES (?)", n)
				if scalar(t, q, "SELECT count(*) FROM protocol_interleave") == 0 {
					t.Fatal("inserted row not visible")
				}
			}
		}
		if err := rows.Err(); err != nil || n != total {
			t.Fatalf("%d rows: %v", n, err)
		}
	}
	check(c)
	tx, err := c.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	check(tx)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, c, "SELECT count(*) FROM protocol_interleave"); n != 8 {
		t.Fatalf("%d interleaved inserts, want 8", n)
	}
}

// TestLargeResults reads a large result of flat and nested columns with
// different read-ahead depths, inline row counts and batch sizes, checking
// every value and the order.
func TestLargeResults(t *testing.T) {
	srv := livetest.StartServer(t, livetest.ServerOptions{})
	const total = 400000
	query := fmt.Sprintf(`SELECT i, i::VARCHAR AS s, i * 0.5 AS d,
		CASE WHEN i %% 7 = 0 THEN NULL ELSE (i %% 100000)::INTEGER END AS n,
		DATE '2000-01-01' + (i %% 10000)::INTEGER AS dt,
		CASE WHEN i %% 2 = 0 THEN [i, i + 1] END AS l,
		{'d': (i %% 100000)::DECIMAL(18,3)} AS st,
		(i %% 7)::VARCHAR::ENUM('0', '1', '2', '3', '4', '5', '6') AS e
		FROM range(%d) t(i)`, total)
	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, params := range []string{"fetch_ahead=0", "fetch_ahead=1&inline_rows=0", "fetch_ahead=8&inline_rows=1", "fetch_ahead=4&inline_rows=2048"} {
		t.Run(params, func(t *testing.T) {
			t.Parallel()
			db, rec := recordingDB(t, srv.DSN(params))
			c := livetest.Conn(t, db)
			livetest.Exec(t, c, "SET quack_target_batch_bytes = 1048576")
			rows, err := c.QueryContext(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var n int64
			for ; rows.Next(); n++ {
				var (
					i     int64
					s, e  string
					d     float64
					nn    sql.NullInt32
					dt    time.Time
					l, st any
				)
				if err := rows.Scan(&i, &s, &d, &nn, &dt, &l, &st, &e); err != nil {
					t.Fatal(err)
				}
				var wantL any
				if n%2 == 0 {
					wantL = []any{n, n + 1}
				}
				if i != n || s != fmt.Sprint(n) || d != float64(n)*0.5 ||
					nn.Valid != (n%7 != 0) || (nn.Valid && nn.Int32 != int32(n%100000)) ||
					!dt.Equal(base.AddDate(0, 0, int(n%10000))) || !reflect.DeepEqual(l, wantL) ||
					!reflect.DeepEqual(st, map[string]any{"d": fmt.Sprintf("%d.000", n%100000)}) || e != fmt.Sprint(n%7) {
					t.Fatalf("row %d: %v %q %v %v %v %v %v %q", n, i, s, d, nn, dt, l, st, e)
				}
			}
			if err := rows.Err(); err != nil || n != total {
				t.Fatalf("%d rows: %v", n, err)
			}
			if reqs := rec.Mark(); reqs < 20 {
				t.Fatalf("%d requests: the result did not span batches", reqs)
			}
		})
	}
}

// TestInlineRows varies the PREPARE inline_rows across result sizes around
// the chunk and inline boundaries, including empty and single-row results.
func TestInlineRows(t *testing.T) {
	srv := livetest.StartServer(t, livetest.ServerOptions{})
	for _, ir := range []string{"inline_rows=0", "inline_rows=1", "inline_rows=2048", "inline_rows=100000", "inline_rows=10000000"} {
		t.Run(ir, func(t *testing.T) {
			c := livetest.Conn(t, livetest.OpenDB(t, srv.DSN(ir)))
			livetest.Exec(t, c, "SET quack_target_batch_bytes = 65536")
			for _, total := range []int64{0, 1, 2047, 2049, 150000} {
				rows, err := c.QueryContext(context.Background(), fmt.Sprintf("SELECT i FROM range(%d) t(i)", total))
				if err != nil {
					t.Fatal(err)
				}
				n := int64(0)
				for ; rows.Next(); n++ {
					var i int64
					if err := rows.Scan(&i); err != nil || i != n {
						t.Fatalf("total %d: row %d = %d, %v", total, n, i, err)
					}
				}
				if err := rows.Err(); err != nil || n != total {
					t.Fatalf("total %d: got %d rows: %v", total, n, err)
				}
				_ = rows.Close()
			}
		})
	}
}

// TestColumnTypes checks the column metadata of every type against the
// server's typeof() and the Go type of the scanned value.
func TestColumnTypes(t *testing.T) {
	_, db := newDB(t)
	c := livetest.Conn(t, db)
	decimal := regexp.MustCompile(`^DECIMAL\((\d+),(\d+)\)$`)
	for _, expr := range []string{
		"true", "1::TINYINT", "1::SMALLINT", "1::INTEGER", "1::BIGINT", "1::UTINYINT", "1::USMALLINT", "1::UINTEGER",
		"1::UBIGINT", "1::HUGEINT", "1::UHUGEINT", "1.5::FLOAT", "1.5::DOUBLE", "1.5::DECIMAL(4,1)", "1.5::DECIMAL(9,2)",
		"1.5::DECIMAL(18,3)", "1.5::DECIMAL(38,10)", "'x'::VARCHAR", "'\\x01'::BLOB", "'0101'::BIT",
		"'1234567890123456789012345678901234567890'::BIGNUM", "'00000000-0000-0000-0000-000000000001'::UUID",
		"DATE '2024-01-02'", "TIME '01:02:03'", "'01:02:03'::TIME_NS", "TIMETZ '01:02:03+01'",
		"TIMESTAMP '2024-01-02 03:04:05'", "'2024-01-02 03:04:05'::TIMESTAMP_S", "'2024-01-02 03:04:05'::TIMESTAMP_MS",
		"'2024-01-02 03:04:05'::TIMESTAMP_NS", "TIMESTAMPTZ '2024-01-02 03:04:05+00'",
		"'2024-01-02 03:04:05.123456789+00'::TIMESTAMPTZ_NS", "'POINT (1 2)'::GEOMETRY", "INTERVAL 1 DAY",
		"[1, 2]", "[1, 2]::INTEGER[2]", "{'a': 1, 'b': 'x'}", "row(1, 'x')", "MAP {'k': 1}",
		"union_value(n := 1)::UNION(n INTEGER, s VARCHAR)", "'a'::ENUM('a', 'b')", "'{\"a\":1}'::JSON", "NULL",
	} {
		typeName := livetest.Scalar[string](t, c, "SELECT typeof("+expr+")")
		rows, err := c.QueryContext(context.Background(), "SELECT "+expr+" AS v")
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		cts, err := rows.ColumnTypes()
		if err != nil || !rows.Next() {
			t.Fatalf("%s: %v %v", expr, err, rows.Err())
		}
		var v any
		if err := rows.Scan(&v); err != nil {
			t.Errorf("%s: scan: %v", expr, err)
		}
		_ = rows.Close()
		ct := cts[0]
		dbName, scanType := ct.DatabaseTypeName(), ct.ScanType()
		prec, scale, hasDec := ct.DecimalSize()
		if nullable, ok := ct.Nullable(); !ok || !nullable {
			t.Errorf("%s: Nullable = %v, %v", expr, nullable, ok)
		}
		// NULL scans as any.
		if v != nil && scanType.Kind() != reflect.Interface && reflect.TypeOf(v) != scanType {
			t.Errorf("%s: ScanType %v but the value is %T", expr, scanType, v)
		}
		if m := decimal.FindStringSubmatch(typeName); m != nil {
			if !hasDec || fmt.Sprint(prec) != m[1] || fmt.Sprint(scale) != m[2] || dbName != "DECIMAL" {
				t.Errorf("%s: DecimalSize = %d, %d, %v, DatabaseTypeName %q; server type %s", expr, prec, scale, hasDec, dbName, typeName)
			}
		} else if hasDec {
			t.Errorf("%s: DecimalSize reported for %s", expr, typeName)
		}
		// Scalar types: the database type name is the server's type name.
		if !strings.ContainsAny(typeName, "([{\"") && !strings.HasSuffix(typeName, "]") && dbName != typeName {
			t.Errorf("%s: DatabaseTypeName %q, server says %q", expr, dbName, typeName)
		}
	}
}
