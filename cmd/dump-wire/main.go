// dump-wire connects to a running Quack server and captures the request and
// response bytes of a canonical set of round-trips into testdata/golden/.
//
// The captured bytes are immutable fixtures used by the unit tests under
// internal/message — they lock in wire-format compatibility against a known
// server version.
//
// Usage:
//
//	DUCKDB version v1.5.3 server running on quack:127.0.0.1:9494
//	go run ./cmd/dump-wire [-out testdata/golden] [-dsn quack://127.0.0.1:9494?token=super_secret]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/transport"
)

func main() {
	dsn := flag.String("dsn", "quack://127.0.0.1:9494?token=super_secret", "Quack DSN")
	outDir := flag.String("out", "testdata/golden", "directory for captured fixtures")
	flag.Parse()

	uri, err := transport.ParseURI(*dsn)
	if err != nil {
		log.Fatalf("parse dsn: %v", err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}

	c := &capturer{outDir: *outDir, tr: transport.New(uri), token: uri.Token}
	c.tr.Trace = c.onTrace
	if err := c.run(context.Background()); err != nil {
		log.Fatalf("capture: %v", err)
	}
	fmt.Printf("wrote %d fixture file(s) to %s\n", c.count, *outDir)
}

type capturer struct {
	tr       *transport.Transport
	token    string
	outDir   string
	connID   string
	queryID  uint64
	curName  string
	count    int
}

func (c *capturer) onTrace(sent, received []byte) {
	if c.curName == "" {
		return
	}
	c.write(c.curName+".req.bin", sent)
	c.write(c.curName+".resp.bin", received)
}

func (c *capturer) write(name string, data []byte) {
	path := filepath.Join(c.outDir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatalf("write %s: %v", path, err)
	}
	c.count++
}

func (c *capturer) roundTrip(ctx context.Context, name string, body message.Body) (message.Header, message.Body) {
	c.curName = name
	defer func() { c.curName = "" }()

	c.queryID++
	hdr := message.Header{
		ConnectionID:  c.connID,
		HasQueryID:    true,
		ClientQueryID: c.queryID,
	}
	req, err := message.Encode(hdr, body)
	if err != nil {
		log.Fatalf("encode %s: %v", name, err)
	}
	respBytes, err := c.tr.Request(ctx, req)
	if err != nil {
		log.Fatalf("transport %s: %v", name, err)
	}
	h, b, err := message.Decode(respBytes)
	if err != nil {
		log.Fatalf("decode %s: %v", name, err)
	}
	return h, b
}

func (c *capturer) run(ctx context.Context) error {
	// 1. handshake
	h, body := c.roundTrip(ctx, "handshake", message.ConnectionRequest{
		AuthString:               c.token,
		ClientDuckdbVersion:      "v1.5.3",
		ClientPlatform:           "go-" + runtime.GOOS + "-" + runtime.GOARCH,
		MinSupportedQuackVersion: 1,
		MaxSupportedQuackVersion: 1,
	})
	if _, ok := body.(message.ConnectionResponse); !ok {
		return fmt.Errorf("handshake: expected ConnectionResponse, got %T (%+v)", body, body)
	}
	c.connID = h.ConnectionID
	fmt.Printf("connected; connection_id=%q\n", c.connID)

	// 2. canonical SELECT queries — one per primitive family
	cases := []struct {
		name string
		sql  string
	}{
		{"select_int", "SELECT 42::INTEGER AS i"},
		{"select_bigint", "SELECT 9000000000::BIGINT AS b"},
		{"select_double", "SELECT 3.14159::DOUBLE AS d"},
		{"select_bool", "SELECT TRUE AS t, FALSE AS f"},
		{"select_varchar", "SELECT 'hello, 🦆' AS s"},
		{"select_null", "SELECT NULL::INTEGER AS n"},
		{"select_hugeint", "SELECT 170141183460469231731687303715884105727::HUGEINT AS h"},
		{"select_uuid", "SELECT 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::UUID AS u"},
		{"select_date", "SELECT DATE '2026-05-25' AS d"},
		{"select_timestamp", "SELECT TIMESTAMP '2026-05-25 12:34:56.789' AS ts"},
		{"select_decimal", "SELECT 12345.67::DECIMAL(9,2) AS amt"},
		{"select_list", "SELECT [1, 2, 3]::INTEGER[] AS xs"},
		{"select_struct", "SELECT {'a': 1::INTEGER, 'b': 'x'} AS s"},
		{"select_multirow", "SELECT i FROM range(0, 5) AS t(i)"},
	}
	for _, c2 := range cases {
		_, body := c.roundTrip(ctx, c2.name, message.PrepareRequest{SQL: c2.sql})
		if _, ok := body.(message.PrepareResponse); !ok {
			return fmt.Errorf("%s: expected PrepareResponse, got %T", c2.name, body)
		}
	}

	// 3. multi-chunk: needs_more_fetch=true followed by FETCH
	_, body = c.roundTrip(ctx, "select_multichunk", message.PrepareRequest{SQL: "SELECT i FROM range(0, 200000) AS t(i)"})
	prep, ok := body.(message.PrepareResponse)
	if !ok {
		return fmt.Errorf("multichunk: expected PrepareResponse, got %T", body)
	}
	if prep.NeedsMoreFetch {
		_, body := c.roundTrip(ctx, "fetch_more", message.FetchRequest{ResultUUID: prep.ResultUUID})
		if _, ok := body.(message.FetchResponse); !ok {
			return fmt.Errorf("fetch: expected FetchResponse, got %T", body)
		}
	} else {
		fmt.Println("note: 200000-row query did not need a FETCH; chunk size may be larger than expected")
	}

	// 4. error path — bad SQL
	_, body = c.roundTrip(ctx, "error_bad_sql", message.PrepareRequest{SQL: "SELECT * FROM does_not_exist"})
	if _, ok := body.(message.ErrorResponse); !ok {
		return fmt.Errorf("error: expected ErrorResponse, got %T", body)
	}

	// 5. disconnect
	_, body = c.roundTrip(ctx, "disconnect", message.DisconnectMessage{})
	if _, ok := body.(message.SuccessResponse); !ok {
		return fmt.Errorf("disconnect: expected SuccessResponse, got %T", body)
	}
	return nil
}
