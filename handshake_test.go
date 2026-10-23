package quackdriver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/quacktest"
)

func TestHandshakeV3Request(t *testing.T) {
	s := newServer(t)
	c := openConn(t, s, "client_id=my-client", "heartbeat_timeout=7s")
	ev := s.EventsOf(message.TypeConnectionRequest)
	if len(ev) != 1 {
		t.Fatalf("CONNECTION_REQUEST sent %d times", len(ev))
	}
	req := ev[0].Connect
	if req.MinSupportedQuackVersion != 3 || req.MaxSupportedQuackVersion != 3 {
		t.Fatalf("versions %d..%d", req.MinSupportedQuackVersion, req.MaxSupportedQuackVersion)
	}
	if req.ClientDuckdbVersion != "v2.0.0" {
		t.Fatalf("client_duckdb_version %q", req.ClientDuckdbVersion)
	}
	if !strings.HasPrefix(req.ClientPlatform, "quackdriver/0.2.0-dev go") {
		t.Fatalf("client_platform %q", req.ClientPlatform)
	}
	if req.ClientID != "my-client" || req.HeartbeatTimeoutSeconds != 7 || req.AuthString != testToken {
		t.Fatalf("request %+v", req)
	}
	if ev[0].ConnectionID != "" || ev[0].HasQueryID {
		t.Fatalf("handshake header %+v", ev[0])
	}
	if c.connectionID == "" || c.serverInfo.QuackVersion != 3 || c.serverInfo.Lease != 7*time.Second {
		t.Fatalf("session %+v %q", c.serverInfo, c.connectionID)
	}
	if c.serverInfo.DuckDBVersion != "v2.0.0" || c.serverInfo.Platform != "quacktest" {
		t.Fatalf("server info %+v", c.serverInfo)
	}
}

func TestHandshakeDefaults(t *testing.T) {
	t.Setenv("QUACK_CLIENT_ID", "env-client")
	s := newServer(t)
	openConn(t, s)
	req := s.EventsOf(message.TypeConnectionRequest)[0].Connect
	if req.HeartbeatTimeoutSeconds != 60 || req.ClientID != "env-client" {
		t.Fatalf("defaults: %+v", req)
	}
}

func TestHandshakeErrors(t *testing.T) {
	answer := func(raw []byte) func(*quacktest.Server) {
		return func(s *quacktest.Server) {
			s.SetRawHook(func(w http.ResponseWriter, _ message.Header, _ message.Body) bool {
				_, _ = w.Write(raw)
				return true
			})
		}
	}
	encoded := func(b message.Body) []byte {
		raw, err := message.Encode(message.Header{}, b)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, c := range []struct {
		name  string
		setup func(*quacktest.Server)
		want  []string
	}{
		{"unsupported version", func(s *quacktest.Server) {
			s.SetConnectHook(func(message.ConnectionRequest) *message.ErrorResponse {
				return &message.ErrorResponse{Message: "Unsupported Quack version - server only supports version 1 of quack"}
			})
		}, []string{"DuckDB 2.0 or newer", "Unsupported Quack version"}},
		// A 1.5 server answers with a v1 ERROR_RESPONSE (header plus field 1),
		// which the v3 decoder reads as an ERROR_RESPONSE.
		{"v1 server", answer(v1ErrorResponse("Failed to deserialize: field id mismatch, expected: 65535, got: 6")),
			[]string{"DuckDB 2.0 or newer", "field id mismatch"}},
		{"undecodable", answer([]byte{0x01, 0x00, 0x02, 0xff, 0xff, 0x07}), []string{"cannot decode", "DuckDB 2.0 or newer"}},
		{"unexpected message", answer(encoded(message.SuccessResponse{})), []string{"unexpected SUCCESS_RESPONSE"}},
		{"missing connection id", answer(encoded(message.ConnectionResponse{QuackVersion: 3, HeartbeatTimeoutSeconds: 60})),
			[]string{"no connection id"}},
		{"unreachable", func(s *quacktest.Server) { s.Close() }, []string{"handshake"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t)
			c.setup(s)
			_, err := Driver{}.Open(s.DSN())
			for _, want := range c.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("got %v, want %q", err, want)
				}
			}
		})
	}
	checkNoLeaks(t)
}

// TestHandshakeToken: the server's authentication function decides about
// the token; a custom one may accept a short token.
func TestHandshakeToken(t *testing.T) {
	s := quacktest.New("ab1")
	t.Cleanup(s.Close)
	conn, err := Driver{}.Open(s.DSN())
	if err != nil {
		t.Fatalf("short token: %v", err)
	}
	_ = conn.Close()
	if ev := s.EventsOf(message.TypeConnectionRequest); len(ev) != 1 || ev[0].Connect.AuthString != "ab1" {
		t.Fatalf("CONNECTION_REQUEST events %+v", ev)
	}
	_, err = Driver{}.Open(strings.Replace(s.DSN(), "ab1", "ab2", 1))
	var qe *Error
	if !errors.As(err, &qe) || qe.Message != "Authentication failed" || strings.Contains(err.Error(), "DuckDB 2.0") {
		t.Fatalf("wrong token: %v", err)
	}
}

// v1ErrorResponse encodes a Quack v1 ERROR_RESPONSE: the header (type 100)
// and a body with only field 1 (message).
func v1ErrorResponse(msg string) []byte {
	w := codec.NewWriter()
	w.WriteFieldID(1)
	w.WriteULEB128(uint64(message.TypeErrorResponse))
	w.WriteFieldID(3)
	w.WriteULEB128(1)
	w.WriteFieldID(codec.FieldEnd)
	w.WriteFieldID(1)
	w.WriteString(msg)
	w.WriteFieldID(codec.FieldEnd)
	return w.Bytes()
}

func TestConnectContextCancelled(t *testing.T) {
	s := newServer(t)
	c, err := NewConnector(s.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Connect(ctx); err == nil {
		t.Fatal("Connect with a cancelled context must fail")
	}
}
