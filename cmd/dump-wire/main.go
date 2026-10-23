// dump-wire connects to a running Quack server (DuckDB 2.0, Quack protocol
// v3) and captures the request and response bytes of a canonical set of
// round trips into testdata/golden/.
//
// The captured bytes are immutable fixtures used by the unit tests under
// internal/message (golden_test.go): they lock in wire-format compatibility
// against a known server build. Every client request is deterministic (fixed
// query UUIDs, stream id, client id and platform strings, sequential
// client_query_id); only the connection_id the server assigns varies between
// captures. See testdata/golden/README.md for the fixture catalog and their
// provenance.
//
// Usage:
//
//	make dump-wire                            (starts a UTC server when none runs)
//	go run ./cmd/dump-wire [-out testdata/golden] [-dsn quack://127.0.0.1:9494?token=super_secret] [-v]
//
// Without -dsn the server address comes from QUACK_HOST, QUACK_PORT and
// QUACK_TOKEN (default 127.0.0.1:9494, token super_secret), or from a full
// QUACK_DSN, like the examples. The tool creates and drops tables named
// golden_* on the server. The server must run in UTC (start it with TZ=UTC):
// the fixtures hold its TIMESTAMPTZ renderings, and a capture from a server
// in another time zone fails, leaving the fixtures untouched.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/zzir/quackdriver/internal/transport"
)

func main() {
	dsn := flag.String("dsn", envDSN(), "Quack DSN of a DuckDB 2.0 server")
	outDir := flag.String("out", "testdata/golden", "directory for captured fixtures")
	verbose := flag.Bool("v", false, "print a summary of every captured exchange")
	timeout := flag.Duration("timeout", 2*time.Minute, "overall capture timeout")
	flag.Parse()
	if err := capture(*dsn, *outDir, *verbose, *timeout); err != nil {
		log.Fatalf("dump-wire: %v", err)
	}
}

// envDSN returns QUACK_DSN when set, else a DSN built from QUACK_HOST,
// QUACK_PORT and QUACK_TOKEN (default 127.0.0.1:9494, token super_secret).
func envDSN() string {
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

func capture(dsn, outDir string, verbose bool, timeout time.Duration) error {
	uri, err := transport.ParseURI(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	tr, err := transport.New(uri, transport.Options{})
	if err != nil {
		return err
	}
	defer tr.CloseIdleConnections()
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	// Capture into a scratch directory next to the output and replace the
	// old fixture set only once the whole capture succeeded, so that a
	// failed run neither leaves a half-updated set nor stale files.
	tmp, err := os.MkdirTemp(filepath.Dir(filepath.Clean(outDir)), ".dump-wire-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := &capturer{tr: tr, token: uri.Token, outDir: tmp, verbose: verbose, written: map[string]bool{}}
	if err := c.run(ctx); err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	if err := replaceFixtures(tmp, outDir); err != nil {
		return fmt.Errorf("install fixtures: %w", err)
	}
	fmt.Printf("wrote %d fixture file(s) to %s (server %s)\n", c.count, outDir, c.serverVersion)
	return nil
}

// replaceFixtures removes every *.req.bin / *.resp.bin in dst and moves the
// freshly captured ones from src there. Other files (README.md) are kept.
func replaceFixtures(src, dst string) error {
	for _, pattern := range []string{"*.req.bin", "*.resp.bin"} {
		old, err := filepath.Glob(filepath.Join(dst, pattern))
		if err != nil {
			return err
		}
		for _, f := range old {
			if err := os.Remove(f); err != nil {
				return err
			}
		}
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
