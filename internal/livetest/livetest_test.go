//go:build integration

package livetest

import (
	"io"
	"net/http"
	"testing"
)

func TestStartServer(t *testing.T) {
	srv := StartServer(t, ServerOptions{})
	resp, err := http.Get("http://" + srv.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if dsn := SharedDSN(t); dsn == "" {
		t.Fatal("empty shared DSN")
	}
}
