//go:build integration

// Package network holds end-to-end tests of the HTTP(S) transport against
// real DuckDB 2.0 Quack servers: TLS with certificate pinning, the TLS
// defaults for non-local hosts, protocol mismatches, reverse proxies,
// timeouts and connection reuse.
package network

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
)

// nonLoopbackEnv names an optional non-loopback address of this machine for
// the tests that reach a server bound to 0.0.0.0 from outside the loopback
// range. Without it the first non-loopback IPv4 address is used.
const nonLoopbackEnv = "QUACK_TEST_NON_LOOPBACK_ADDR"

func TestMain(m *testing.M) {
	// http.ProxyFromEnvironment reads the environment once per process: test
	// servers on this machine must never be reached through a proxy.
	if addr := nonLoopbackAddr(); addr != "" {
		for _, k := range []string{"NO_PROXY", "no_proxy"} {
			v := addr
			if old := os.Getenv(k); old != "" {
				v = old + "," + addr
			}
			_ = os.Setenv(k, v)
		}
	}
	os.Exit(m.Run())
}

// nonLoopbackAddr returns a non-loopback IPv4 address of this machine, or "".
func nonLoopbackAddr() string {
	if v := os.Getenv(nonLoopbackEnv); v != "" {
		return v
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	return ""
}

// dsn builds a quack:// DSN for host:port with the token and params.
func dsn(host string, port int, token string, params ...string) string {
	return "quack://" + net.JoinHostPort(host, fmt.Sprint(port)) + "?" + strings.Join(append([]string{"token=" + token}, params...), "&")
}

// connect opens one session on a fresh connector, closed when the test ends.
func connect(t *testing.T, dsn string, opts ...quackdriver.Option) (*sql.Conn, error) {
	t.Helper()
	cn, err := quackdriver.NewConnector(dsn, opts...)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(cn)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := db.Conn(ctx)
	if err == nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

// mustConnect is connect failing the test on error.
func mustConnect(t *testing.T, dsn string, opts ...quackdriver.Option) *sql.Conn {
	t.Helper()
	c, err := connect(t, dsn, opts...)
	if err != nil {
		t.Fatalf("%s: %v", dsn, err)
	}
	return c
}

// timed runs f and returns how long it took.
func timed(f func()) time.Duration {
	start := time.Now()
	f()
	return time.Since(start)
}

// counter is a TCP forwarder that counts the connections a client opens and
// the plain-HTTP requests it sends over them.
type counter struct {
	ln       net.Listener
	accepted atomic.Int64 // TCP connections accepted so far
	open     atomic.Int64 // TCP connections currently open
	requests atomic.Int64 // "POST /quack" request lines seen
}

// newCounter forwards a fresh 127.0.0.1 port to target (host:port).
func newCounter(t *testing.T, target string) *counter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &counter{ln: ln}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				_ = in.Close()
				continue
			}
			c.accepted.Add(1)
			c.open.Add(1)
			var once sync.Once
			done := func() {
				once.Do(func() { c.open.Add(-1) })
				_ = in.Close()
				_ = out.Close()
			}
			wg.Add(2)
			go func() { defer wg.Done(); defer done(); c.countRequests(io.TeeReader(in, out)) }()
			go func() { defer wg.Done(); defer done(); _, _ = io.Copy(in, out) }()
		}
	}()
	return c
}

// Port is the port the forwarder listens on.
func (c *counter) Port() int { return c.ln.Addr().(*net.TCPAddr).Port }

// countRequests counts request lines in the client-to-server stream, also
// those split across reads.
func (c *counter) countRequests(r io.Reader) {
	const marker = "POST /quack HTTP/1.1\r\n"
	buf := make([]byte, 32*1024)
	var tail string
	for {
		n, err := r.Read(buf)
		chunk := tail + string(buf[:n])
		c.requests.Add(int64(strings.Count(chunk, marker)))
		tail = chunk[max(0, len(chunk)-len(marker)+1):]
		if err != nil {
			return
		}
	}
}
