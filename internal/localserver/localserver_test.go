package localserver

import (
	"encoding/hex"
	"testing"
)

func TestPickFreePort(t *testing.T) {
	port, err := pickFreePort()
	if err != nil {
		t.Fatal(err)
	}
	if port < 1 || port > 65535 {
		t.Fatalf("port %d out of range", port)
	}
}

func TestPickFreePortUnique(t *testing.T) {
	ports := map[int]bool{}
	for i := 0; i < 10; i++ {
		port, err := pickFreePort()
		if err != nil {
			t.Fatal(err)
		}
		ports[port] = true
	}
	if len(ports) < 5 {
		t.Fatalf("expected mostly unique ports, got %d distinct out of 10", len(ports))
	}
}

func TestGenerateToken(t *testing.T) {
	token := generateToken()
	if len(token) != 32 {
		t.Fatalf("expected 32-char hex token, got %d chars: %q", len(token), token)
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Fatalf("token is not valid hex: %q", token)
	}
}

func TestGenerateTokenUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok := generateToken()
		if seen[tok] {
			t.Fatalf("duplicate token after %d iterations", i)
		}
		seen[tok] = true
	}
}

func TestCanonicalize(t *testing.T) {
	got, err := canonicalize("")
	if err != nil || got != "" {
		t.Fatalf("empty: got %q, err %v", got, err)
	}

	got, err = canonicalize(":memory:")
	if err != nil || got != ":memory:" {
		t.Fatalf(":memory:: got %q, err %v", got, err)
	}

	got, err = canonicalize("/tmp/test.db")
	if err != nil || got != "/tmp/test.db" {
		t.Fatalf("abs path: got %q, err %v", got, err)
	}
}
