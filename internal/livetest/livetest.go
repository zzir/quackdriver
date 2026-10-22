// Package livetest starts real DuckDB Quack servers for integration tests.
//
// Tests that use it are compiled only with the "integration" build tag and
// need a DuckDB 2.0+ CLI with the quack extension:
//
//	DUCKDB_BIN            CLI to run (default: "duckdb" on PATH)
//	QUACK_TEST_INIT_SQL   statements run before quack_serve
//	                      (default: "INSTALL quack; LOAD quack;")
//	QUACK_DSN             if set, SharedDSN returns it instead of starting
//	                      a server
//
// The servers run with TZ=UTC: their default TimeZone, in which DuckDB
// renders TIMESTAMPTZ and casts zone-less timestamps to it, is UTC whatever
// the zone of the machine running the tests (as on CI). A test that needs
// another zone sets TimeZone itself.
package livetest

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/logicaltype"
)

// DefaultToken is the token used by servers started without an explicit one.
const DefaultToken = "super_secret"

// ServerOptions configures StartServer.
type ServerOptions struct {
	// Host to bind; default 127.0.0.1.
	Host string
	// Port to bind; 0 picks a free port.
	Port int
	// Token for quack_serve; default DefaultToken.
	Token string
	// ServeArgs are extra named arguments appended to quack_serve, e.g.
	// "disable_ssl => false, allow_other_hostname => true".
	ServeArgs string
	// InitSQL runs after QUACK_TEST_INIT_SQL and before quack_serve.
	InitSQL string
	// TLS makes the readiness probe use HTTPS (without verification).
	TLS bool
}

// Server is a running DuckDB CLI serving Quack.
type Server struct {
	Host  string
	Port  int
	Token string

	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   lockedBuffer
	done  chan struct{}
}

// lockedBuffer collects the CLI's output.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Addr returns host:port in DSN form (IPv6 hosts bracketed).
func (s *Server) Addr() string { return net.JoinHostPort(s.Host, fmt.Sprint(s.Port)) }

// DSN returns a quack:// DSN for the server with the given extra query
// parameters (without the leading '?' or '&').
func (s *Server) DSN(params ...string) string {
	q := append([]string{"token=" + s.Token}, params...)
	return "quack://" + s.Addr() + "?" + strings.Join(q, "&")
}

// Output returns everything the CLI printed so far.
func (s *Server) Output() string {
	s.out.mu.Lock()
	defer s.out.mu.Unlock()
	return s.out.buf.String()
}

// Stop terminates the server and waits for it to exit.
func (s *Server) Stop() {
	_ = s.stdin.Close()
	select {
	case <-s.done:
		return
	case <-time.After(3 * time.Second):
	}
	_ = s.cmd.Process.Kill()
	<-s.done
}

// DuckDBBin returns the CLI to use, skipping the test when none is available.
func DuckDBBin(tb testing.TB) string {
	tb.Helper()
	bin := os.Getenv("DUCKDB_BIN")
	if bin == "" {
		bin = "duckdb"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		tb.Skipf("livetest: DuckDB CLI %q not found (set DUCKDB_BIN): %v", bin, err)
	}
	return path
}

// FreePort returns a currently unused TCP port on 127.0.0.1.
func FreePort(tb testing.TB) int {
	tb.Helper()
	port, err := freePort()
	if err != nil {
		tb.Fatalf("livetest: pick port: %v", err)
	}
	return port
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func sqlString(s string) string { return logicaltype.QuoteString(s) }

// StartServer launches a DuckDB CLI running quack_serve and registers its
// shutdown with t.Cleanup. It fails the test if the server is not ready
// within 30 seconds.
func StartServer(tb testing.TB, opts ServerOptions) *Server {
	tb.Helper()
	s, err := start(DuckDBBin(tb), opts)
	if s != nil {
		tb.Cleanup(s.Stop)
	}
	if err != nil {
		tb.Fatalf("livetest: %v", err)
	}
	return s
}

// start launches the server; on a failure after the process started it
// returns the Server too, so that the caller can stop it.
func start(bin string, opts ServerOptions) (*Server, error) {
	if opts.Host == "" {
		opts.Host = "127.0.0.1"
	}
	if opts.Port == 0 {
		port, err := freePort()
		if err != nil {
			return nil, fmt.Errorf("pick port: %w", err)
		}
		opts.Port = port
	}
	if opts.Token == "" {
		opts.Token = DefaultToken
	}
	init := os.Getenv("QUACK_TEST_INIT_SQL")
	if init == "" {
		init = "INSTALL quack; LOAD quack;"
	}
	s := &Server{Host: opts.Host, Port: opts.Port, Token: opts.Token, done: make(chan struct{})}
	serve := fmt.Sprintf("CALL quack_serve(%s, token => %s", sqlString("quack:"+s.Addr()), sqlString(opts.Token))
	if opts.ServeArgs != "" {
		serve += ", " + opts.ServeArgs
	}
	script := strings.TrimSpace(init + " " + opts.InitSQL + " " + serve + ");")

	s.cmd = exec.Command(bin, "-no-agent", "-init", os.DevNull, "-cmd", script)
	s.cmd.Env = append(os.Environ(), "TZ=UTC") // see the package comment
	s.cmd.Stdout = &s.out
	s.cmd.Stderr = &s.out
	stdin, err := s.cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin: %w", err)
	}
	s.stdin = stdin
	if err := s.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	go func() {
		_ = s.cmd.Wait()
		close(s.done)
	}()

	scheme := "http"
	if opts.TLS {
		scheme = "https"
	}
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // readiness probe only
	}
	url := fmt.Sprintf("%s://%s/", scheme, s.Addr())
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		select {
		case <-s.done:
			return s, fmt.Errorf("server exited before becoming ready:\n%s", s.Output())
		default:
		}
		if resp, err := client.Get(url); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return s, nil
			}
		}
	}
	return s, fmt.Errorf("server on %s not ready after 30s:\n%s", s.Addr(), s.Output())
}

var (
	sharedOnce sync.Once
	sharedDSN  string
	sharedErr  error
)

// SharedDSN returns QUACK_DSN when set; otherwise it starts one in-memory
// server per test binary (kept alive until the process exits) and returns
// its DSN with the given extra parameters.
func SharedDSN(tb testing.TB, params ...string) string {
	tb.Helper()
	dsn := os.Getenv("QUACK_DSN")
	if dsn == "" {
		bin := DuckDBBin(tb)
		sharedOnce.Do(func() {
			srv, err := start(bin, ServerOptions{})
			switch {
			case err == nil:
				sharedDSN = srv.DSN()
			case srv != nil:
				srv.Stop()
			}
			sharedErr = err
		})
		if sharedErr != nil {
			tb.Fatalf("livetest: shared server: %v", sharedErr)
		}
		dsn = sharedDSN
	}
	if len(params) == 0 {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + strings.Join(params, "&")
}
