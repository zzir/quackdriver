//go:build integration

// Package examples builds every program under examples/ and runs it against
// its own real DuckDB 2.0 server (QUACK_HOST, QUACK_PORT and QUACK_TOKEN
// point at it), checking the output. examples/bun is a separate module; its
// test is skipped when its dependencies cannot be downloaded.
package examples

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/livetest"
)

var repoRoot, _ = filepath.Abs(filepath.Join("..", ".."))

// build compiles examples/<name> (with the race detector) into a test
// directory and returns the executable.
func build(t *testing.T, name string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-race", "-o", bin, ".")
	cmd.Dir = filepath.Join(repoRoot, "examples", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		if name == "bun" && regexp.MustCompile(`module lookup disabled|dial tcp|proxyconnect|GOPROXY|GOFLAGS=-mod=mod`).Match(out) {
			t.Skipf("examples/bun dependencies unavailable: %s", out)
		}
		t.Fatalf("go build ./examples/%s: %v\n%s", name, err, out)
	}
	return bin
}

// run executes bin with env added to the environment (minus the variables
// the examples read) and returns its output, failing on a non-zero exit.
func run(t *testing.T, bin string, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = repoRoot
	for _, kv := range os.Environ() {
		if !regexp.MustCompile(`^QUACK_(DSN|HOST|PORT|TOKEN|SSL_FINGERPRINT|CERT_FILE)=`).MatchString(kv) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	t.Logf("%s output:\n%s", filepath.Base(bin), out.String())
	if err != nil {
		t.Fatalf("%s: %v", filepath.Base(bin), err)
	}
	return out.String()
}

func serverEnv(srv *livetest.Server) []string {
	return []string{"QUACK_HOST=" + srv.Host, fmt.Sprintf("QUACK_PORT=%d", srv.Port), "QUACK_TOKEN=" + srv.Token}
}

// expect fails unless every pattern matches a line of out.
func expect(t *testing.T, out string, patterns ...string) {
	t.Helper()
	for _, p := range patterns {
		if !regexp.MustCompile("(?m)" + p).MatchString(out) {
			t.Errorf("output lacks a line matching %q", p)
		}
	}
}

type exampleCase struct {
	name    string // test name
	example string // directory under examples/ ("" = name)
	token   string // server token ("" = default)
	dsn     bool   // pass QUACK_DSN instead of host/port/token
	args    []string
	want    []string
}

var cases = []exampleCase{
	{name: "basic", want: []string{
		`^inserted 3 row\(s\)$`, `^updated 2 row\(s\)$`, `^user 1: alice active=true$`, `^user 2: bob active=true$`,
		`^user 3: carol active=false$`, `^active users: 2$`,
		`^error type="Catalog" message="Table with name basic_no_such_table does not exist!"$`,
		`^read 200000 rows in order$`, `^closed early; connection still works: 42$`, `^interleaved: read 20000 rows, updated 4$`,
	}},
	// A token that needs URL escaping in the DSN.
	{name: "token-escaping", example: "basic", token: "s3cr&t +=%?'x", want: []string{`^user 3: carol active=false$`}},
	// A full DSN, without FETCH read-ahead.
	{name: "quack-dsn", example: "basic", dsn: true, want: []string{`^read 200000 rows in order$`}},
	{name: "types", want: []string{
		`^all values match the server's VARCHAR rendering$`,
		`^SELECT \[\[1, 2\], \[3, 4\], NULL\]\s+-> \[\]interface \{\} \[\[1 2\] \[3 4\] <nil>\]$`,
		`^SELECT row\(1, 'a', DATE '2026-05-25'\)\s+-> \[\]interface \{\} \[1 a 2026-05-25 00:00:00 \+0000 UTC\]$`,
		`^SELECT MAP \{'k': 'v', 'k2': NULL\}\s+-> \[\]interface \{\} \[map\[key:k value:v\] map\[key:k2 value:<nil>\]\]$`,
		`-> map\[string\]interface \{\} map\[:0 err:<nil> num:42\]$`,
		`^time:\s+12h34m56.789012s$`,
		`^time_tz:\s+13:45:00.5-05:30 \(offset -19800s; on 2026-05-25: 2026-05-25T19:15:00.5Z\)$`,
		`^interval:\s+1 month 2 days 03:04:05.678 \(months=1 days=2 micros=11045678000, ~771h4m5.678s\)$`,
		`^timestamptz: 2026-05-25T03:34:56Z$`, `^BC:\s+-0043-03-15 12:00:00`,
		`^arguments:\s+2026-05-25T14:00:00.123456789\+02:00 -> 2026-05-25T12:00:00.123456789Z \(TIMESTAMP_NS\); TIMETZ "09:30:00\+02", INTERVAL "1 year 2 months -1 day 00:00:01.5"$`,
	}},
	{name: "append", want: []string{
		`^appended 3 row\(s\)$`, `^  huge     -170141183460469231731687303715884105728$`, `^  raw      \\xDE\\xADhi$`,
		`^  spent    1 month 2 days 00:00:03$`, `^  opens    14:00:00\+02$`, `^  seen     2026-05-25 12:00:00\+00$`,
		`^  owner    \{'id': 8, 'name': NULL\}$`, `^  prices   \{eur=56.78, usd=12.34\}$`, `^  mood     ok$`, `^  tags     NULL$`,
		`^ValueError for row 1: quackdriver: row 1 column "mood": ENUM expects a string label or integer index, got bool$`,
		`^server error "Conversion": Could not convert string 'nope' to ENUM`, `^rows in table: 3$`,
		`^skipped: quackdriver: Appender: row 500 column "kind": VARCHAR expects string, got int$`,
		`^Close reported 99999 inserted row\(s\); note defaulted to "none"$`,
		`^query during the stream: quackdriver: connection is busy: an Appender is open on it$`,
		`^constraint violation: type="Constraint"; table holds 99999 row\(s\)$`,
	}},
	{name: "bench-append", args: []string{"-rows", "20000", "-insert-rows", "200"}, want: []string{
		`^INSERT per row\s+200 rows`, `^INSERT 1000 rows/stmt\s+20000 rows`, `^Conn.Append\s+20000 rows`, `^Appender\s+20000 rows`,
	}},
	{name: "bun", want: []string{
		`^  3 carol active=true$`, `^active users:$`, `^updated 1 row\(s\)$`, `^deleted 1 row\(s\) in a transaction$`, `^active count: 3$`,
	}},
}

func TestExamples(t *testing.T) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if c.example == "" {
				c.example = c.name
			}
			bin := build(t, c.example)
			srv := livetest.StartServer(t, livetest.ServerOptions{Token: c.token})
			env := serverEnv(srv)
			if c.dsn {
				env = []string{"QUACK_DSN=" + srv.DSN("fetch_ahead=0"), "QUACK_PORT=1"}
			}
			out := run(t, bin, env, c.args...)
			expect(t, out, c.want...)
			if strings.Contains(out, "MISMATCH") {
				t.Error("output reports a MISMATCH")
			}
		})
	}
}

func TestTLSPin(t *testing.T) {
	t.Parallel()
	bin := build(t, "tls-pin")
	expect(t, run(t, bin, nil),
		`^No server certificate given: set QUACK_SSL_FINGERPRINT \(or QUACK_CERT_FILE\)\.$`,
		`CALL quack_serve\('quack:127.0.0.1:9494', token => 'super_secret', disable_ssl => false,$`)

	srv := livetest.StartTLSServer(t, "127.0.0.1", "")
	colon := regexp.MustCompile("..").ReplaceAllString(srv.Fingerprint, "$0:")
	colon = colon[:len(colon)-1]
	checks := []string{
		`^pinned connection works: SELECT 40 \+ 2 = 42$`,
		`^server certificate: ` + colon + ` \(matches pin: true\)$`,
		`^wrong pin refused: .*does not match the pinned ssl_fingerprint`,
		`^unpinned HTTPS refused: .*x509`,
		`^pin with tls=false rejected: `,
	}
	for name, extra := range map[string]string{
		"fingerprint":   "QUACK_SSL_FINGERPRINT=" + colon,
		"sha256-prefix": "QUACK_SSL_FINGERPRINT=sha256:" + strings.ToLower(srv.Fingerprint),
		"cert-file":     "QUACK_CERT_FILE=" + filepath.Join(srv.Dir, "server.pem"),
	} {
		t.Run(name, func(t *testing.T) {
			expect(t, run(t, bin, append(serverEnv(srv.Server), extra)), checks...)
		})
	}
}

// TestAllExamplesCovered fails when an example directory has no test here.
func TestAllExamplesCovered(t *testing.T) {
	covered := map[string]bool{"tls-pin": true}
	for _, c := range cases {
		covered[c.name] = true
	}
	entries, err := os.ReadDir(filepath.Join(repoRoot, "examples"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && !covered[e.Name()] {
			t.Errorf("examples/%s has no integration test", e.Name())
		}
	}
}
