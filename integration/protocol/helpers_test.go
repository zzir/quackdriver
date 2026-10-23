//go:build integration

// Package protocol holds end-to-end tests of the session lifecycle and
// statement execution against real DuckDB 2.0 Quack servers.
package protocol

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/livetest"
)

// newDB starts a private server and opens a pool on it.
func newDB(t *testing.T, params ...string) (*livetest.Server, *sql.DB) {
	t.Helper()
	srv := livetest.StartServer(t, livetest.ServerOptions{})
	return srv, livetest.OpenDB(t, srv.DSN(params...))
}

// recordingDB opens a pool whose requests are recorded, to make sure a test
// really exercises FETCH round trips.
func recordingDB(t *testing.T, dsn string) (*sql.DB, *livetest.Recorder) {
	t.Helper()
	rec := livetest.NewRecorder()
	return livetest.OpenDB(t, dsn, quackdriver.WithHTTPClient(&http.Client{Transport: rec})), rec
}

func scalar(t *testing.T, q livetest.Queryer, query string, args ...any) int64 {
	t.Helper()
	return livetest.Scalar[int64](t, q, query, args...)
}

// session is a row of quack_active_connections().
type session struct {
	ID, Query, State string
	ClientHash       sql.NullString
}

// sessions lists the server's active Quack sessions.
func sessions(t *testing.T, q livetest.Queryer) map[string]session {
	t.Helper()
	rows, err := q.QueryContext(context.Background(),
		"SELECT connection_id, coalesce(query, ''), state, client_id_hash FROM quack_active_connections()")
	if err != nil {
		t.Fatalf("quack_active_connections: %v", err)
	}
	defer rows.Close()
	out := map[string]session{}
	for rows.Next() {
		var s session
		if err := rows.Scan(&s.ID, &s.Query, &s.State, &s.ClientHash); err != nil {
			t.Fatal(err)
		}
		out[s.ID] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// sessionID returns the server-side connection id of q: the session whose
// current statement carries a unique marker.
func sessionID(t *testing.T, q livetest.Queryer) string {
	t.Helper()
	marker := fmt.Sprintf("marker_%d", time.Now().UnixNano())
	return livetest.Scalar[string](t, q, "SELECT connection_id FROM quack_active_connections() WHERE query LIKE '%"+marker+"%'")
}

// waitCancelled waits until the server reports session id as cancelled.
func waitCancelled(t *testing.T, mon *sql.DB, id string) {
	t.Helper()
	livetest.Eventually(t, 3*time.Second, "the statement to be cancelled", func() bool {
		return sessions(t, mon)[id].State == "cancelled"
	})
}

// within fails the test when f takes longer than d.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	start := time.Now()
	f()
	if el := time.Since(start); el > d {
		t.Fatalf("%s took %v, want at most %v", what, el, d)
	}
}
