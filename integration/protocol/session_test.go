//go:build integration

package protocol

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/transport"
)

// TestHandshakeClientID: the client_id sent in the handshake shows up as
// its HMAC in quack_active_connections(); equal ids hash equally, and an
// empty id is not sent at all.
func TestHandshakeClientID(t *testing.T) {
	srv, mon := newDB(t)
	ids := map[string]string{}
	for name, param := range map[string]string{
		"a1": "client_id=protocol-client-a", "a2": "client_id=protocol-client-a",
		"b": "client_id=protocol-client-b", "none": "client_id=", "default": "",
	} {
		ids[name] = sessionID(t, livetest.Conn(t, livetest.OpenDB(t, srv.DSN(param))))
	}
	all := sessions(t, mon)
	hash := func(name string) sql.NullString {
		s, ok := all[ids[name]]
		if !ok {
			t.Fatalf("session %s (%s) not listed", name, ids[name])
		}
		return s.ClientHash
	}
	if !hash("a1").Valid || hash("a1") != hash("a2") {
		t.Errorf("same client_id: hashes %v and %v, want equal and set", hash("a1"), hash("a2"))
	}
	if !hash("b").Valid || hash("b") == hash("a1") {
		t.Errorf("different client_ids hash alike: %v", hash("b"))
	}
	if hash("none").Valid {
		t.Errorf("client_id= (empty) still sent a client id: %v", hash("none"))
	}
	// The default is a per-process random UUID (or $QUACK_CLIENT_ID).
	if !hash("default").Valid {
		t.Errorf("default client_id not sent")
	}
}

// TestPingAndDisconnect: Ping works on a live session, a connection back in
// the pool keeps its session, and closing the pool ends it (DISCONNECT).
func TestPingAndDisconnect(t *testing.T) {
	srv, mon := newDB(t)
	db, err := sql.Open("quack", srv.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PingContext(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	id := sessionID(t, c)
	_ = c.Close()
	if _, ok := sessions(t, mon)[id]; !ok {
		t.Fatalf("session %s gone after returning the conn to the pool", id)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := sessions(t, mon)[id]; ok {
		t.Fatalf("session %s still listed after DB.Close (no DISCONNECT?)", id)
	}
}

// TestPoolReuse: database/sql reuses one session for sequential statements.
func TestPoolReuse(t *testing.T) {
	srv, mon := newDB(t)
	db := livetest.OpenDB(t, srv.DSN("client_id=protocol-pool"))
	db.SetMaxOpenConns(1)
	first := sessionID(t, db)
	for i := range 30 {
		if id := sessionID(t, db); id != first {
			t.Fatalf("statement %d ran on session %s, want %s", i, id, first)
		}
		livetest.Exec(t, db, "SELECT 1")
	}
	all := sessions(t, mon)
	mine := 0
	for _, s := range all {
		if s.ClientHash == all[first].ClientHash {
			mine++
		}
	}
	if mine != 1 {
		t.Fatalf("%d sessions with the pool's client id, want 1", mine)
	}
}

// TestLeaseExpiresWithoutHeartbeat is the control for the heartbeat tests:
// the real server drops a session that sends nothing for longer than its
// lease, so the driver's heartbeats are what keeps sessions alive.
func TestLeaseExpiresWithoutHeartbeat(t *testing.T) {
	srv := livetest.StartServer(t, livetest.ServerOptions{})
	u, err := transport.ParseURI(srv.DSN())
	if err != nil {
		t.Fatal(err)
	}
	tr, err := transport.New(u, transport.Options{})
	if err != nil {
		t.Fatal(err)
	}
	req := func(h message.Header, b message.Body) (message.Header, message.Body) {
		payload, err := message.Encode(h, b)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := tr.Request(context.Background(), payload)
		if err != nil {
			t.Fatal(err)
		}
		rh, rb, err := message.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		return rh, rb
	}
	h, b := req(message.Header{}, message.ConnectionRequest{
		AuthString: srv.Token, MinSupportedQuackVersion: 3, MaxSupportedQuackVersion: 3, HeartbeatTimeoutSeconds: 1,
	})
	if resp, ok := b.(message.ConnectionResponse); !ok || resp.HeartbeatTimeoutSeconds != 1 || resp.QuackVersion != 3 {
		t.Fatalf("handshake: %#v", b)
	}
	hb := message.Header{ConnectionID: h.ConnectionID}
	if _, b := req(hb, message.HeartbeatRequest{}); b.MessageType() != message.TypeSuccessResponse {
		t.Fatalf("heartbeat: %#v", b)
	}
	time.Sleep(2500 * time.Millisecond)
	for _, want := range []string{"Connection heartbeat lease expired", "Invalid connection id"} {
		if _, b := req(hb, message.HeartbeatRequest{}); !isError(b, want) {
			t.Fatalf("after the lease: %#v, want %q", b, want)
		}
	}
}

func isError(b message.Body, msg string) bool {
	e, ok := b.(message.ErrorResponse)
	return ok && e.Message == msg
}

// TestCancelledHandshakesLeaveNoSessions: Connect calls whose context ends
// while the CONNECTION_REQUEST is in flight do not leave sessions behind.
func TestCancelledHandshakesLeaveNoSessions(t *testing.T) {
	srv, mon := newDB(t)
	mon.SetMaxOpenConns(1)
	others := func() int64 {
		return scalar(t, mon, "SELECT count(*) FROM quack_active_connections() WHERE query IS NULL OR query NOT LIKE '%quack_active_connections%'")
	}
	if n := others(); n != 0 {
		t.Fatalf("%d sessions before", n)
	}
	cn, err := quackdriver.NewConnector(srv.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	for i := range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(200+i*100)*time.Microsecond)
		if c, err := cn.Connect(ctx); err == nil {
			_ = c.Close()
		}
		cancel()
	}
	livetest.Eventually(t, 10*time.Second, "abandoned sessions to be disconnected", func() bool { return others() == 0 })
}

// TestTimeZoneParameter: with timezone=UTC a time.Time argument meets
// TIMESTAMPTZ as the right instant even when the server's TimeZone is not
// UTC; without it, the documented shift happens.
func TestTimeZoneParameter(t *testing.T) {
	srv := livetest.StartServer(t, livetest.ServerOptions{InitSQL: "SET GLOBAL TimeZone = 'America/New_York';"})
	in := time.Date(2020, 6, 1, 12, 0, 0, 0, time.UTC)
	utc := livetest.OpenDB(t, srv.DSN("timezone=UTC"))
	if tz := livetest.Scalar[string](t, utc, "SELECT current_setting('TimeZone')"); tz != "UTC" {
		t.Fatalf("session TimeZone %q", tz)
	}
	livetest.Exec(t, utc, "CREATE TABLE pz (t TIMESTAMPTZ)")
	if out := livetest.Scalar[time.Time](t, utc, "INSERT INTO pz VALUES (?) RETURNING t", in); !out.Equal(in) {
		t.Fatalf("timezone=UTC: stored %v, want %v", out, in)
	}
	plain := livetest.OpenDB(t, srv.DSN())
	if out := livetest.Scalar[time.Time](t, plain, "SELECT ?::TIMESTAMPTZ", in); out.Equal(in) {
		t.Fatalf("without timezone the server zone applies, got %v", out)
	}
	bad := livetest.OpenDB(t, srv.DSN("timezone=Not/AZone"))
	if err := bad.Ping(); err == nil || !strings.Contains(err.Error(), "timezone=Not/AZone") {
		t.Fatalf("unknown zone: %v", err)
	}
}

// TestAcknowledgeReleasesRetainedResult: with quack_enable_reconnects the
// server retains every result until it is acknowledged; the driver does so
// after reading a FETCH stream to its end.
func TestAcknowledgeReleasesRetainedResult(t *testing.T) {
	srv := livetest.StartServer(t, livetest.ServerOptions{InitSQL: "SET GLOBAL quack_enable_reconnects = true;"})
	db := livetest.OpenDB(t, srv.DSN())
	rows, err := livetest.Conn(t, db).QueryContext(context.Background(), "SELECT i FROM range(60000) t(i)")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Close(); err != nil || n != 60000 {
		t.Fatalf("read %d rows: %v", n, err)
	}
	livetest.Eventually(t, 5*time.Second, "the retained result to be released", func() bool {
		return scalar(t, db, "SELECT coalesce(max(cached_rows), 0) FROM quack_active_connections()") == 0
	})
}
