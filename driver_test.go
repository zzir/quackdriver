package quackdriver

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zzir/quackdriver/internal/message"
)

func TestOpenConnector(t *testing.T) {
	db, err := sql.Open("quack", "quack://localhost:9494?token=super_secret&tls=false&timeout=10s")
	if err != nil {
		t.Fatal(err)
	}
	c := db.Driver().(Driver) // through Connector.Driver
	_ = db.Close()
	dc, err := c.OpenConnector("quack://localhost:9494?token=super_secret&tls=false&timeout=10s")
	if err != nil {
		t.Fatal(err)
	}
	conn := dc.(*Connector)
	if conn.uri.Host != "localhost" || conn.uri.Port != 9494 || conn.uri.Token != "super_secret" || conn.transport == nil {
		t.Fatalf("connector %+v", conn.uri)
	}
	if _, err := c.OpenConnector("not-quack://x"); err == nil || !strings.Contains(err.Error(), "quack") {
		t.Fatalf("expected DSN scheme error, got %v", err)
	}
}

func TestConnectorOptions(t *testing.T) {
	const dsn = "quack://localhost:9494?token=abcd"
	for _, opts := range [][]Option{
		{WithHTTPClient(&http.Client{}), WithTLSConfig(&tls.Config{})},
		{WithHTTPClient(&http.Client{}), WithMaxIdleConnsPerHost(8)},
	} {
		if _, err := NewConnector(dsn, opts...); err == nil {
			t.Error("WithHTTPClient must not combine with transport options")
		}
	}
	s := newServer(t)
	c, err := NewConnector(s.DSN(), nil, WithMaxIdleConnsPerHost(200)) // nil options are ignored
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestWithHeaderSentOnEveryRequest(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT 1", intResult(1, 1))
	c, err := NewConnector(s.DSN(), WithHeader("X-Proxy-Auth", "s3cret"), WithHeader("X-Proxy-Auth", "second"))
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	defer db.Close()
	var v int64
	if err := db.QueryRow("SELECT 1").Scan(&v); err != nil {
		t.Fatal(err)
	}
	events := s.Events()
	if len(events) < 2 {
		t.Fatalf("events: %d", len(events))
	}
	for _, e := range events {
		got := e.Header.Values("X-Proxy-Auth")
		if len(got) != 2 || got[0] != "s3cret" || got[1] != "second" {
			t.Fatalf("%s carried X-Proxy-Auth %q", e.Type, got)
		}
		if ct := e.Header.Get("Content-Type"); ct != "application/vnd.duckdb" {
			t.Fatalf("Content-Type %q", ct)
		}
	}
}

type countingTransport struct {
	n    atomic.Int64
	base http.RoundTripper
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.base.RoundTrip(r)
}

func TestWithHTTPClientAndSharedTransport(t *testing.T) {
	s := newServer(t)
	s.Script("SELECT 1", intResult(1, 1))
	ct := &countingTransport{base: http.DefaultTransport}
	c, err := NewConnector(s.DSN(), WithHTTPClient(&http.Client{Transport: ct}))
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	defer db.Close()
	ctx := context.Background()
	c1, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	var t1, t2 any
	_ = c1.Raw(func(dc any) error { t1 = dc.(*Conn).transport; return nil })
	_ = c2.Raw(func(dc any) error { t2 = dc.(*Conn).transport; return nil })
	if t1 != t2 || t1 != any(c.transport) {
		t.Fatal("connections of one Connector must share its transport")
	}
	if ct.n.Load() < 2 {
		t.Fatalf("custom client used for %d requests", ct.n.Load())
	}
}

func TestConnectorClose(t *testing.T) {
	s := newServer(t)
	c, err := NewConnector(s.DSN())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := c.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := c.Connect(context.Background()); !errors.Is(err, errConnectorClosed) {
		t.Fatalf("Connect after Close: %v", err)
	}
}

func TestDriverOpenOwnsConnector(t *testing.T) {
	s := newServer(t)
	dc, err := Driver{}.Open(s.DSN())
	if err != nil {
		t.Fatal(err)
	}
	c := dc.(*Conn)
	if c.ownedConnector == nil {
		t.Fatal("Driver.Open must hand the connection its own connector")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !c.ownedConnector.closed.Load() {
		t.Fatal("closing the connection must close its connector")
	}
	if s.Count(message.TypeDisconnectMessage) != 1 {
		t.Fatalf("DISCONNECT sent %d times", s.Count(message.TypeDisconnectMessage))
	}

	// A failed handshake releases the connector too (no leak).
	if _, err := (Driver{}).Open(s.DSN("client_id=abc")); err == nil {
		t.Fatal("client_id shorter than 4 characters must fail")
	}
	if _, err := (Driver{}).Open(strings.Replace(s.DSN(), testToken, "wrong_token", 1)); err == nil {
		t.Fatal("wrong token must fail")
	}
	checkNoLeaks(t)
}
