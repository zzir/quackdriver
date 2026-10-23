// Type example: scan every scalar DuckDB type into `any` and check the Go
// value against the server's own VARCHAR rendering, then nested types,
// temporal types and time.Time arguments.
//
//	go run ./examples/types
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
	"strconv"
	"strings"
	"time"

	"github.com/zzir/quackdriver"
)

func main() {
	db, err := sql.Open("quack", dsn())
	check(err)
	defer db.Close()
	ctx := context.Background()

	// The server renders TIMESTAMPTZ in its session TimeZone, which defaults
	// to the server host's zone; the comparison below expects UTC. A
	// sql.Conn keeps the setting for every query of this example.
	conn, err := db.Conn(ctx)
	check(err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, "SET TimeZone = 'UTC'")
	check(err)

	// Scalars: SELECT <expr>, (<expr>)::VARCHAR, and the Go value rendered
	// the way DuckDB casts it to VARCHAR.
	failed := 0
	for _, expr := range []string{
		"TRUE", "-42::SMALLINT", "'-9223372036854775808'::BIGINT", "18446744073709551615::UBIGINT",
		"170141183460469231731687303715884105727::HUGEINT", "3.14::FLOAT", "1e20::DOUBLE",
		"12345.67::DECIMAL(9,2)", "-12.5::DECIMAL(38,10)", "'hello, 世界'", "'{\"a\": [1, 2]}'::JSON",
		"'\\xAA\\x00ab'::BLOB", "'0110'::BIT", "-123456789012345678901234567890::BIGNUM",
		"'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::UUID", "DATE '2026-05-25'", "TIME '12:34:56.789012'",
		"TIME_NS '12:34:56.123456789'", "'13:45:00.25-05:30'::TIMETZ", "TIMESTAMP '2026-05-25 12:34:56.5'",
		"TIMESTAMP_NS '2026-05-25 12:34:56.123456789'", "TIMESTAMPTZ '2026-05-25 12:34:56.5+02'",
		"INTERVAL '1 year 2 months 3 days 04:05:06.7'", "'b'::ENUM('a', 'b', 'c')", "NULL::INTEGER",
	} {
		rows, err := conn.QueryContext(ctx, fmt.Sprintf("SELECT %s, (%s)::VARCHAR", expr, expr))
		check(err)
		cols, err := rows.ColumnTypes()
		check(err)
		var got any
		var server sql.NullString
		if !rows.Next() {
			log.Fatalf("%s: no row (%v)", expr, rows.Err())
		}
		check(rows.Scan(&got, &server))
		_ = rows.Close()
		typeName := cols[0].DatabaseTypeName()
		mine, status := render(got, typeName), "ok"
		if server.Valid != (got != nil) || server.Valid && mine != server.String {
			status = fmt.Sprintf("MISMATCH (server says %q)", server.String)
			failed++
		}
		fmt.Printf("%-48s %-26s %-22T %-34s %s\n", expr, typeName, got, mine, status)
	}
	if failed > 0 {
		log.Fatalf("%d mismatch(es)", failed)
	}
	fmt.Println("all values match the server's VARCHAR rendering")

	// Nested types scan into []any and map[string]any.
	for _, q := range []string{
		"SELECT [[1, 2], [3, 4], NULL]",
		"SELECT array_value(10::INTEGER, 20::INTEGER)",
		"SELECT {'name': 'duck', 'tags': ['a', 'b']}",
		"SELECT row(1, 'a', DATE '2026-05-25')",
		"SELECT MAP {'k': 'v', 'k2': NULL}",
		"SELECT union_value(num := 42)::UNION(num INTEGER, err VARCHAR)",
	} {
		var v any
		check(conn.QueryRowContext(ctx, q).Scan(&v))
		fmt.Printf("%-64s -> %T %v\n", q, v, v)
	}

	// Temporal types: TIME scans into time.Duration, TIME WITH TIME ZONE into
	// TimeTzValue, INTERVAL into IntervalValue (months, days and micros stay
	// separate), DATE and every TIMESTAMP into a UTC time.Time.
	var (
		tod time.Duration
		tz  quackdriver.TimeTzValue
		iv  quackdriver.IntervalValue
		tst time.Time
		bc  time.Time
	)
	check(conn.QueryRowContext(ctx, `SELECT TIME '12:34:56.789012', '13:45:00.5-05:30'::TIMETZ,
		INTERVAL '1 month 2 days 03:04:05.678', TIMESTAMPTZ '2026-05-25 12:34:56+09',
		TIMESTAMP '0044-03-15 (BC) 12:00:00'`).Scan(&tod, &tz, &iv, &tst, &bc))
	fmt.Printf("time:        %v\n", tod)
	at := time.Date(2026, 5, 25, 0, 0, 0, 0, tz.Location()).Add(tz.TimeOfDay())
	fmt.Printf("time_tz:     %v (offset %ds; on 2026-05-25: %s)\n", tz, tz.OffsetSeconds(), at.UTC().Format(time.RFC3339Nano))
	fmt.Printf("interval:    %v (months=%d days=%d micros=%d, ~%v)\n", iv, iv.Months, iv.Days, iv.Micros, iv.ApproxDuration())
	fmt.Printf("timestamptz: %s\n", tst.Format(time.RFC3339))
	fmt.Printf("BC:          %s (Go's year 0 is 1 BC)\n", bc.Format("2006-01-02 15:04:05"))

	// A time.Time argument is sent as a TIMESTAMP literal of its UTC wall
	// clock (TIMESTAMP_NS with sub-microsecond digits); TimeTzValue and
	// IntervalValue arguments keep their own types.
	in := time.Date(2026, 5, 25, 14, 0, 0, 123456789, time.FixedZone("CEST", 2*3600))
	arg, err := quackdriver.NewTimeTz(9*time.Hour+30*time.Minute, 2*3600)
	check(err)
	var back time.Time
	var typ, tzText, ivText string
	check(conn.QueryRowContext(ctx, "SELECT ?, typeof(?), ?::VARCHAR, ?::VARCHAR", in, in, arg,
		quackdriver.IntervalValue{Months: 14, Days: -1, Micros: 1_500_000}).Scan(&back, &typ, &tzText, &ivText))
	fmt.Printf("arguments:   %s -> %s (%s); TIMETZ %q, INTERVAL %q\n",
		in.Format(time.RFC3339Nano), back.Format(time.RFC3339Nano), typ, tzText, ivText)
}

// render formats a scanned value the way DuckDB casts the column's type to
// VARCHAR.
func render(v any, typeName string) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case []byte:
		var b strings.Builder
		for _, c := range x {
			if c >= 0x20 && c < 0x7f && c != '\'' && c != '\\' {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "\\x%02X", c)
			}
		}
		return b.String()
	case time.Time:
		switch typeName {
		case "DATE":
			return x.Format("2006-01-02")
		case "TIMESTAMP_NS":
			return x.Format("2006-01-02 15:04:05.999999999")
		case "TIMESTAMP WITH TIME ZONE":
			return x.Format("2006-01-02 15:04:05.999999") + "+00" // the session is in UTC
		}
		return x.Format("2006-01-02 15:04:05.999999")
	case time.Duration:
		s := fmt.Sprintf("%02d:%02d:%02d", int64(x/time.Hour), int64(x/time.Minute%60), int64(x/time.Second%60))
		if frac := int64(x % time.Second); frac != 0 {
			digits := 6
			if typeName == "TIME_NS" {
				digits = 9
			}
			s += "." + strings.TrimRight(fmt.Sprintf("%09d", frac)[:digits], "0")
		}
		return s
	case fmt.Stringer: // *big.Int, TimeTzValue, IntervalValue
		return x.String()
	}
	return fmt.Sprint(v)
}

func check(err error) {
	if err != nil {
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
