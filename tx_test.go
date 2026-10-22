package quackdriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/quacktest"
)

func TestTxCommitRollback(t *testing.T) {
	s := newServer(t)
	db := openDB(t, s)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var sqls []string
	for _, e := range s.EventsOf(message.TypePrepareRequest) {
		sqls = append(sqls, e.SQL)
	}
	if strings.Join(sqls, ",") != "BEGIN,COMMIT,BEGIN,ROLLBACK" {
		t.Fatalf("statements %v", sqls)
	}
	rawConn(t, db, func(c *Conn) {
		// Options Quack cannot honour are rejected, not dropped.
		for _, opts := range []driver.TxOptions{{Isolation: driver.IsolationLevel(sql.LevelSerializable)}, {ReadOnly: true}} {
			if _, err := c.BeginTx(context.Background(), opts); err == nil {
				t.Fatalf("BeginTx(%+v) must fail", opts)
			}
		}
		if _, err := c.BeginTx(context.Background(), driver.TxOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.BeginTx(context.Background(), driver.TxOptions{}); err == nil {
			t.Fatal("nested BeginTx must fail")
		}
		if _, err := c.ExecContext(context.Background(), "ROLLBACK", nil); err != nil {
			t.Fatal(err)
		}
		c.inTx = false
	})
}

// TestBeginTxCancelledDiscardsConn: when ctx ends while BEGIN is in flight,
// the server may still run it, leaving the session inside a transaction.
// Such a connection must not go back to the pool, where every later
// autocommit statement would silently run inside that transaction.
func TestBeginTxCancelledDiscardsConn(t *testing.T) {
	s := newServer(t)
	gate := make(chan struct{})
	defer close(gate)
	begin := quacktest.Success()
	begin.PrepareGate = gate
	s.Script("BEGIN", begin)
	s.Script("INSERT INTO t VALUES (1)", quacktest.Count(1))
	db := openDB(t, s)
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := db.BeginTx(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BeginTx: %s", errString(err))
	}
	if _, err := db.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	insert := prepareOf(t, s, "INSERT INTO t VALUES (1)")
	beginEv := prepareOf(t, s, "BEGIN")
	if insert.ConnectionID == beginEv.ConnectionID || s.Count(message.TypeConnectionRequest) != 2 {
		t.Fatal("the INSERT ran on the session whose BEGIN had an unknown outcome")
	}
	eventually(t, "DISCONNECT of the discarded session", func() bool {
		for _, e := range s.EventsOf(message.TypeDisconnectMessage) {
			if e.ConnectionID == beginEv.ConnectionID {
				return true
			}
		}
		return false
	})
}

// TestTxOutcomes: a server error answering BEGIN proves it did not run and
// keeps the connection; a COMMIT whose response is lost may or may not have
// run, so the connection is discarded.
func TestTxOutcomes(t *testing.T) {
	s := newServer(t)
	s.Script("BEGIN", quacktest.Result{Error: &message.ErrorResponse{Message: "no", ExceptionType: "TransactionContext"}})
	c := openConn(t, s)
	if _, err := c.BeginTx(context.Background(), driver.TxOptions{}); err == nil || !c.IsValid() {
		t.Fatalf("BEGIN refused by the server: %v, valid=%v", err, c.IsValid())
	}

	s = newServer(t)
	s.SetRawHook(func(_ http.ResponseWriter, _ message.Header, body message.Body) bool {
		if p, ok := body.(message.PrepareRequest); ok && p.SQL == "COMMIT" {
			panic(http.ErrAbortHandler)
		}
		return false
	})
	c = openConn(t, s)
	tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil || c.IsValid() || c.inTx {
		t.Fatalf("COMMIT with a lost response: %v, valid=%v inTx=%v", err, c.IsValid(), c.inTx)
	}
}
