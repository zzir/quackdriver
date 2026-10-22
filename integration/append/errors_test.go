//go:build integration

package appendit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/message"
)

func TestAppendDefaultsAndImplicitCasts(t *testing.T) {
	e := newEnv(t)
	e.exec(t,
		"CREATE TYPE append_mood AS ENUM ('happy', 'sad', 'ok')",
		"CREATE SEQUENCE append_seq START 100",
		`CREATE TABLE append_casts (id BIGINT, seq INTEGER DEFAULT nextval('append_seq'), tag VARCHAR DEFAULT 'dflt',
			j JSON, mood append_mood, d DECIMAL(10,2), ts TIMESTAMPTZ, l BIGINT[], created DATE DEFAULT DATE '2000-01-01')`)
	cols := []quackdriver.AppendColumn{
		{Name: "id", Type: "INTEGER"},             // INTEGER -> BIGINT
		{Name: "j", Type: "VARCHAR"},              // VARCHAR -> JSON
		{Name: "mood", Type: "ENUM('ok', 'sad')"}, // anonymous ENUM -> named ENUM (other order)
		{Name: "d", Type: "DOUBLE"},               // DOUBLE -> DECIMAL
		{Name: "ts", Type: "TIMESTAMP"},           // TIMESTAMP -> TIMESTAMPTZ
		{Name: "l", Type: "INTEGER[]"},            // INTEGER[] -> BIGINT[]
	}
	sc := e.conn(t)
	if err := appendRows(sc, "append_casts", cols, [][]any{
		{1, `{"a": [1, 2]}`, "ok", 1.25, time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC), []any{1, 2}},
		{2, nil, "sad", nil, nil, nil},
		{3, `[true]`, 1, 2.5, time.Unix(0, 0), []any{}},
	}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(e.queryStrings(t, `SELECT concat_ws('|', id, seq, tag, j, mood, d, ts, l::VARCHAR, created, typeof(j),
		typeof(mood) = typeof(NULL::append_mood)) FROM append_casts ORDER BY id`), "\n")
	if want := `1|100|dflt|{"a": [1, 2]}|ok|1.25|2024-05-06 07:08:09+00|[1, 2]|2000-01-01|JSON|true
2|101|dflt|sad|2000-01-01|JSON|true
3|102|dflt|[true]|sad|2.50|1970-01-01 00:00:00+00|[]|2000-01-01|JSON|true`; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}

	// An implicit cast that fails on the server (invalid JSON) is a server
	// error; nothing is inserted and the connection stays usable.
	if err := appendRows(sc, "append_casts", cols[:2], [][]any{{4, `{"ok": 1}`}, {5, "not json"}}); serverError(err) == nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if n := e.queryInt(t, "SELECT count(*) FROM append_casts"); n != 3 {
		t.Fatalf("failed append left %d rows", n)
	}
	// Without a column list the values must cover the table's columns.
	if qe := serverError(appendRows(sc, "append_casts", []quackdriver.AppendColumn{{Type: "INTEGER"}}, [][]any{{1}})); qe == nil || qe.Type != "Binder" {
		t.Fatalf("too few columns: %v", qe)
	}
	connUsable(t, sc)
}

func TestAppendConstraintViolation(t *testing.T) {
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_pk (id INTEGER PRIMARY KEY, s VARCHAR NOT NULL)", "INSERT INTO append_pk VALUES (1, 'one')")
	cols := []quackdriver.AppendColumn{{Name: "id", Type: "INTEGER"}, {Name: "s", Type: "VARCHAR"}}
	sc := e.conn(t)
	for name, rows := range map[string][][]any{
		"duplicate of existing row":   {{2, "two"}, {1, "dup"}},
		"duplicate within the stream": {{10, "a"}, {11, "b"}, {10, "c"}},
		"NOT NULL":                    {{20, "a"}, {21, nil}},
	} {
		if qe := serverError(appendRows(sc, "append_pk", cols, rows)); qe == nil || qe.Type != "Constraint" {
			t.Fatalf("%s: got %v, want a Constraint *Error", name, qe)
		}
		if n := e.queryInt(t, "SELECT count(*) FROM append_pk"); n != 1 {
			t.Fatalf("%s: table has %d rows after the failed append", name, n)
		}
		connUsable(t, sc)
	}

	if err := appendRows(sc, "append_pk", cols, [][]any{{2, "two"}}); err != nil {
		t.Fatalf("append after the violation: %v", err)
	}
}

// TestAppendServerFailureMidStream makes the server fail while it consumes
// the stream (a cast error in a late row): the failure surfaces as a
// *Error, the INSERT is cancelled, nothing is inserted, the Appender keeps
// reporting the failure and the connection is usable.
func TestAppendServerFailureMidStream(t *testing.T) {
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_midfail (id INTEGER, n INTEGER)")
	sc := e.conn(t)
	mark := e.rec.Mark()
	err := raw(sc, func(c *quackdriver.Conn) error {
		a, err := c.NewAppender(context.Background(), "", "append_midfail",
			[]quackdriver.AppendColumn{{Name: "id", Type: "INTEGER"}, {Name: "n", Type: "VARCHAR"}}, quackdriver.WithBatchBytes(32<<10))
		if err != nil {
			return err
		}
		defer func() { _ = a.Abort() }()
		for i := range 200_000 {
			v := "7"
			if i == 10_000 {
				v = "not a number"
			}
			if err = a.AppendRow(i, v); err != nil {
				break
			}
		}
		if err == nil {
			_, err = a.Close()
		}
		if _, err2 := a.Close(); err2 == nil {
			t.Error("Close after a failure succeeded")
		}
		return err
	})
	if qe := serverError(err); qe == nil || qe.Type != "Conversion" {
		t.Fatalf("got %v, want a Conversion *Error", err)
	}
	if n := e.queryInt(t, "SELECT count(*) FROM append_midfail"); n != 0 {
		t.Fatalf("table has %d rows", n)
	}
	if cancelsFor(t, e, mark) == 0 {
		t.Error("no CANCEL_REQUEST after the server failure")
	}
	connUsable(t, sc)
}

func TestAppendAbortSendsCancel(t *testing.T) {
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_abort (id INTEGER)")
	sc := e.conn(t)
	cols := []quackdriver.AppendColumn{{Name: "id", Type: "INTEGER"}}
	mark := e.rec.Mark()
	if err := raw(sc, func(c *quackdriver.Conn) error {
		a, err := c.NewAppender(context.Background(), "", "append_abort", cols)
		if err != nil {
			return err
		}
		for i := range 5000 {
			if err := a.AppendRow(i); err != nil {
				return err
			}
		}
		if err := a.Flush(); err != nil {
			return err
		}
		if err := a.Abort(); err != nil {
			return err
		}
		if err := a.Abort(); err != nil {
			t.Errorf("second Abort: %v", err)
		}
		if _, err := a.Close(); err == nil || a.AppendRow(1) == nil {
			t.Error("the aborted Appender is still usable")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := cancelsFor(t, e, mark); n != 1 {
		t.Fatalf("Abort sent %d CANCEL_REQUESTs for the INSERT, want 1", n)
	}
	for _, s := range sent[message.SendDataRequest](t, e, mark) {
		if s.TotalBatches.Valid {
			t.Fatal("Abort ended the stream with a terminal SEND_DATA")
		}
	}
	if n := e.queryInt(t, "SELECT count(*) FROM append_abort"); n != 0 {
		t.Fatalf("aborted stream inserted %d rows", n)
	}
	if err := appendRows(sc, "append_abort", cols, [][]any{{1}, {2}}); err != nil {
		t.Fatalf("append after Abort: %v", err)
	}
}

// TestAppendContextCancel: a cancelled context fails Close, cancels the
// INSERT on the server and leaves the connection usable at once (the
// Appender waits for its CANCEL, so the next statement is not interrupted).
func TestAppendContextCancel(t *testing.T) {
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_ctx (id INTEGER)")
	sc := e.conn(t)
	mark := e.rec.Mark()
	for range 10 {
		ctx, cancel := context.WithCancel(context.Background())
		err := raw(sc, func(c *quackdriver.Conn) error {
			a, err := c.NewAppender(ctx, "", "append_ctx", []quackdriver.AppendColumn{{Type: "INTEGER"}})
			if err != nil {
				return err
			}
			defer func() { _ = a.Abort() }()
			for i := range 3000 {
				if err := a.AppendRow(i); err != nil {
					return err
				}
			}
			if err := a.Flush(); err != nil {
				return err
			}
			cancel()
			_, err = a.Close()
			return err
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Close after cancel: %v", err)
		}
		connUsable(t, sc)
	}
	if n := e.queryInt(t, "SELECT count(*) FROM append_ctx"); n != 0 {
		t.Fatalf("cancelled streams inserted %d rows", n)
	}
	if cancelsFor(t, e, mark) == 0 {
		t.Fatal("no CANCEL_REQUEST after the context was cancelled")
	}
}

// TestAppendConnBusy: until an Appender ends, statements on its connection
// fail with ErrConnBusy (also when it outlives the Raw callback); a
// connection whose Appender is never closed is discarded by the pool.
func TestAppendConnBusy(t *testing.T) {
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_busy (id INTEGER)")
	ctx := context.Background()
	sc := e.conn(t)
	cols := []quackdriver.AppendColumn{{Name: "id", Type: "INTEGER"}}
	var a *quackdriver.Appender
	if err := raw(sc, func(c *quackdriver.Conn) error {
		var err error
		a, err = c.NewAppender(ctx, "", "append_busy", cols)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.AppendRow(1); err != nil {
		t.Fatal(err)
	}
	_, execErr := sc.ExecContext(ctx, "SELECT 1")
	_, queryErr := sc.QueryContext(ctx, "SELECT 1")
	_, txErr := sc.BeginTx(ctx, nil)
	_, appenderErr := appender(sc, "", "append_busy", cols, 0, nil)
	for _, err := range []error{execErr, queryErr, txErr, appenderErr, appendRows(sc, "append_busy", cols, [][]any{{9}})} {
		if !errors.Is(err, quackdriver.ErrConnBusy) {
			t.Errorf("statement while an Appender is open: %v, want ErrConnBusy", err)
		}
	}
	if n := e.queryInt(t, "SELECT count(*) FROM append_busy"); n != 0 {
		t.Fatalf("pool sees %d rows", n)
	}
	if err := a.AppendRow(2); err != nil {
		t.Fatalf("AppendRow after the busy statements: %v", err)
	}
	if n, err := a.Close(); err != nil || n != 2 {
		t.Fatalf("Close: %d %v", n, err)
	}
	connUsable(t, sc)

	// Neither Close nor Abort: the connection goes back to the pool, which
	// discards it (DISCONNECT aborts the INSERT).
	e.db.SetMaxOpenConns(1)
	_ = sc.Close()
	leak := e.conn(t)
	if err := raw(leak, func(c *quackdriver.Conn) error {
		a, err := c.NewAppender(ctx, "", "append_busy", cols)
		if err != nil {
			return err
		}
		return a.AppendRow(3)
	}); err != nil {
		t.Fatal(err)
	}
	_ = leak.Close()
	for range 3 {
		if n := e.queryInt(t, "SELECT count(*) FROM append_busy"); n != 2 {
			t.Fatalf("table has %d rows", n)
		}
	}
}

func TestAppendInTransaction(t *testing.T) {
	e := newEnv(t)
	e.exec(t, "CREATE TABLE append_tx (id INTEGER PRIMARY KEY)")
	sc := e.conn(t)
	add := func(ids ...int) error {
		rows := make([][]any, len(ids))
		for i, v := range ids {
			rows[i] = []any{v}
		}
		return appendRows(sc, "append_tx", []quackdriver.AppendColumn{{Name: "id", Type: "INTEGER"}}, rows)
	}
	count := func() int64 { return e.queryInt(t, "SELECT count(*) FROM append_tx") }

	livetest.Exec(t, sc, "BEGIN")
	if err := add(1, 2, 3); err != nil {
		t.Fatal(err)
	}
	if n := livetest.Scalar[int64](t, sc, "SELECT count(*) FROM append_tx"); n != 3 || count() != 0 {
		t.Fatalf("inside the transaction %d rows, outside %d", n, count())
	}
	livetest.Exec(t, sc, "ROLLBACK")
	if n := count(); n != 0 {
		t.Fatalf("rolled back rows visible: %d", n)
	}

	livetest.Exec(t, sc, "BEGIN")
	if err := add(1, 2); err != nil {
		t.Fatal(err)
	}
	if err := add(3); err != nil {
		t.Fatal(err)
	}
	livetest.Exec(t, sc, "COMMIT")
	if n := count(); n != 3 {
		t.Fatalf("committed rows: %d", n)
	}

	// A failed append aborts the transaction, like any failed statement.
	livetest.Exec(t, sc, "BEGIN")
	if err := add(10); err != nil {
		t.Fatal(err)
	}
	if err := add(1); serverError(err) == nil {
		t.Fatalf("duplicate in transaction: %v", err)
	}
	if _, err := sc.ExecContext(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("statement in an aborted transaction succeeded")
	}
	livetest.Exec(t, sc, "ROLLBACK")
	if n := count(); n != 3 {
		t.Fatalf("rows after the aborted transaction: %d", n)
	}
}

// TestAppendConversionErrors: a Go value a column does not accept is a
// *ValueError numbered by input row (AppendRow call): Conn.Append aborts the
// statement, the Appender skips the row and stays usable. A value the
// server cannot cast fails the statement; either way nothing is inserted.
func TestAppendConversionErrors(t *testing.T) {
	e := newEnv(t)
	e.exec(t, `CREATE TABLE append_conv (i INTEGER, "a b" VARCHAR)`)
	sc := e.conn(t)
	cols := []quackdriver.AppendColumn{{Name: "i", Type: "INTEGER"}, {Name: "a b", Type: "VARCHAR"}}
	valueError := func(err error, row int64, col int) bool {
		var ve *quackdriver.ValueError
		return errors.As(err, &ve) && ve.Row == row && ve.Column == col
	}

	for _, c := range []struct {
		rows     [][]any
		row, col int
	}{
		{[][]any{{1, "a"}, {2, "b"}, {3, 3}}, 2, 1},
		{[][]any{{1, "\xff"}}, 0, 1},
		{[][]any{{1}}, 0, -1},
	} {
		if err := appendRows(sc, "append_conv", cols, c.rows); !valueError(err, int64(c.row), c.col) {
			t.Fatalf("rows %v: %v, want a *ValueError for row %d column %d", c.rows, err, c.row, c.col)
		}
	}
	if qe := serverError(appendRows(sc, "append_conv", cols, [][]any{{1, "a"}, {1 << 40, "b"}})); qe == nil || qe.Type != "Conversion" {
		t.Fatalf("out-of-range INTEGER: %v, want a Conversion error", qe)
	}
	if n := e.queryInt(t, "SELECT count(*) FROM append_conv"); n != 0 {
		t.Fatalf("failed appends inserted %d rows", n)
	}

	if err := raw(sc, func(c *quackdriver.Conn) error {
		a, err := c.NewAppender(context.Background(), "", "append_conv", cols)
		if err != nil {
			return err
		}
		defer func() { _ = a.Abort() }()
		for call := range 5 {
			var v any = fmt.Sprint(call)
			if call%2 == 1 {
				v = call // wrong Go type for VARCHAR
			}
			err := a.AppendRow(call, v)
			if call%2 == 0 && err != nil {
				return fmt.Errorf("call %d: %w", call, err)
			}
			if call%2 == 1 && (!valueError(err, int64(call), 1) || !strings.Contains(err.Error(), fmt.Sprintf(`row %d column "a b"`, call))) {
				return fmt.Errorf("call %d: got %w, want a *ValueError for row %d column 1", call, err, call)
			}
		}
		if err := a.AppendRow(1); !valueError(err, 5, -1) {
			return fmt.Errorf("value count: got %w, want a *ValueError for row 5", err)
		}
		if n, err := a.Close(); err != nil || n != 3 {
			return fmt.Errorf("Close: %d, %w", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.queryStrings(t, `SELECT i || '=' || "a b" FROM append_conv ORDER BY i`), ","); got != "0=0,2=2,4=4" {
		t.Fatalf("table holds %q", got)
	}
}

// TestAppendTargets covers quoted schema, table and column names, temporary
// tables, a table of another attached database (named by its catalog), a
// wide table and missing targets.
func TestAppendTargets(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := e.conn(t)
	const width = 300
	var defs []string
	var wide []quackdriver.AppendColumn
	row := make([]any, width)
	for i := range width {
		defs = append(defs, fmt.Sprintf("c%d INTEGER", i))
		wide = append(wide, quackdriver.AppendColumn{Name: fmt.Sprintf("c%d", i), Type: "INTEGER"})
		row[i] = i
	}
	e.exec(t,
		`CREATE SCHEMA "append ""schema"" ü"`,
		`CREATE TABLE "append ""schema"" ü"."Tab""le 表" ("Col A" INTEGER, "col""b" VARCHAR, "select" BOOLEAN, "ü 🦆" DOUBLE DEFAULT 1.5)`,
		`CREATE TABLE main."append MixedCase" ("ID" INTEGER)`,
		"ATTACH ':memory:' AS append_other", "CREATE TABLE append_other.append_t (id INTEGER)",
		"CREATE TABLE append_wide ("+strings.Join(defs, ", ")+")")
	livetest.Exec(t, sc, "CREATE TEMP TABLE append_temp (id INTEGER)")
	one := []quackdriver.AppendColumn{{Name: "id", Type: "INTEGER"}}
	if err := raw(sc, func(c *quackdriver.Conn) error {
		return errors.Join(
			c.AppendWithSchema(ctx, `append "schema" ü`, `Tab"le 表`, []quackdriver.AppendColumn{
				{Name: "Col A", Type: "INTEGER"}, {Name: `col"b`, Type: "VARCHAR"}, {Name: "select", Type: "BOOLEAN"},
			}, [][]any{{1, "x", true}, {2, nil, false}}),
			c.Append(ctx, "append_temp", one, [][]any{{1}}),
			c.AppendWithSchema(ctx, "temp", "append_temp", one, [][]any{{2}}),
			c.AppendWithSchema(ctx, "append_other", "append_t", one, [][]any{{3}}),
			c.Append(ctx, "append_wide", wide, [][]any{row, row}))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := appender(sc, "main", "append MixedCase", one, 1, func(int) []any { return []any{7} }); err != nil {
		t.Fatal(err)
	}
	got := e.queryStrings(t, `SELECT concat_ws('|', "Col A", "col""b", "select", "ü 🦆") FROM "append ""schema"" ü"."Tab""le 表" ORDER BY 1`)
	if strings.Join(got, ",") != "1|x|true|1.5,2|false|1.5" {
		t.Fatalf("got %q", got)
	}
	if n := livetest.Scalar[int64](t, sc, `SELECT (SELECT "ID" FROM "append MixedCase") * 1000000 + (SELECT sum(id) FROM append_temp) * 10000
		+ (SELECT sum(id) FROM append_other.append_t) * 1000 + (SELECT sum(c299) + sum(c0) + count(*) FROM append_wide)`); n != 7_033_600 {
		t.Fatalf("checksum %d", n)
	}
	// Missing tables and schemas are Catalog errors and free the connection.
	for _, target := range [][2]string{{"", "append_missing"}, {"append_missing_schema", "t"}} {
		_, err := appender(sc, target[0], target[1], []quackdriver.AppendColumn{{Type: "INTEGER"}}, 0, nil)
		if qe := serverError(err); qe == nil || qe.Type != "Catalog" {
			t.Fatalf("%v: %v", target, err)
		}
		connUsable(t, sc)
	}
}
