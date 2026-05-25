package transport

import (
	"testing"
	"time"
)

func TestParseURI(t *testing.T) {
	cases := []struct {
		dsn  string
		want URI
	}{
		{"quack:localhost", URI{Host: "localhost", Port: DefaultPort, Timeout: 30 * time.Second}},
		{"quack:localhost:7777", URI{Host: "localhost", Port: 7777, Timeout: 30 * time.Second}},
		{"quack://example.com:8080", URI{Host: "example.com", Port: 8080, Timeout: 30 * time.Second}},
		{"quack:[::1]:9494", URI{Host: "::1", Port: 9494, IPv6: true, Timeout: 30 * time.Second}},
		{"quack://[fe80::1]:1234?token=abcd&tls=true&timeout=5s", URI{
			Host: "fe80::1", Port: 1234, IPv6: true, SSL: true, Token: "abcd", Timeout: 5 * time.Second,
		}},
		{"quack://h?token=t", URI{Host: "h", Port: DefaultPort, Token: "t", Timeout: 30 * time.Second}},
	}
	for _, c := range cases {
		got, err := ParseURI(c.dsn)
		if err != nil {
			t.Fatalf("%q: %v", c.dsn, err)
		}
		if *got != c.want {
			t.Fatalf("%q: want %+v, got %+v", c.dsn, c.want, *got)
		}
	}
}

func TestParseURIErrors(t *testing.T) {
	cases := []string{
		"",
		"localhost:9494",
		"http://localhost",
		"quack:",
		"quack:host:abc",
		"quack:host:0",
		"quack:host:99999",
		"quack:[::1",
		"quack://h?tls=maybe",
		"quack://h?timeout=oops",
	}
	for _, dsn := range cases {
		if _, err := ParseURI(dsn); err == nil {
			t.Fatalf("expected error for %q", dsn)
		}
	}
}

func TestHTTPURL(t *testing.T) {
	u, _ := ParseURI("quack://example.com:9494")
	if got, want := u.HTTPURL(), "http://example.com:9494/quack"; got != want {
		t.Fatalf("http url: %q", got)
	}
	u, _ = ParseURI("quack://example.com:9494?tls=true")
	if got, want := u.HTTPURL(), "https://example.com:9494/quack"; got != want {
		t.Fatalf("https url: %q", got)
	}
	u, _ = ParseURI("quack:[::1]:9494")
	if got, want := u.HTTPURL(), "http://[::1]:9494/quack"; got != want {
		t.Fatalf("ipv6 url: %q", got)
	}
}
