//go:build integration

package network

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/transport"
)

// TestTLSReverseProxy runs a session (statements, an append and a
// multi-batch result) through a TLS-terminating reverse proxy that requires
// an extra header and checks the Quack content type.
func TestTLSReverseProxy(t *testing.T) {
	t.Parallel()
	const header, secret = "X-Quack-Proxy-Auth", "let me in"
	backend := livetest.StartServer(t, livetest.ServerOptions{})
	target, err := url.Parse("http://" + backend.Addr())
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	var forwarded, bad atomic.Int64
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(header) != secret {
			http.Error(w, "proxy authentication required", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != transport.Endpoint ||
			r.Header.Get("Content-Type") != transport.MimeType || r.Header.Get("Accept") != transport.MimeType {
			bad.Add(1)
		}
		forwarded.Add(1)
		rp.ServeHTTP(w, r)
	}))
	proxy.Config.ErrorLog = log.New(io.Discard, "", 0)
	proxy.StartTLS()
	t.Cleanup(proxy.Close)
	roots := x509.NewCertPool()
	roots.AddCert(proxy.Certificate())
	tlsOpt := quackdriver.WithTLSConfig(&tls.Config{RootCAs: roots})

	// 127.0.0.1 defaults to plain HTTP; the httptest certificate is valid
	// for it, so regular verification against the given roots applies.
	d := strings.Replace(backend.DSN("tls=true", "fetch_ahead=2"), backend.Addr(), proxy.Listener.Addr().String(), 1)
	c := mustConnect(t, d, tlsOpt, quackdriver.WithHeader(header, secret))
	livetest.Exec(t, c, "SET quack_target_batch_bytes = 16384")
	livetest.Exec(t, c, "CREATE TABLE network_proxy_t (id BIGINT)")
	rows := make([][]any, 5000)
	for i := range rows {
		rows[i] = []any{i}
	}
	if err := c.Raw(func(dc any) error {
		return dc.(*quackdriver.Conn).Append(context.Background(), "network_proxy_t", []quackdriver.AppendColumn{{Name: "id", Type: "BIGINT"}}, rows)
	}); err != nil {
		t.Fatalf("Append through the proxy: %v", err)
	}
	if got := livetest.Scalar[int64](t, c, "SELECT sum(id) FROM network_proxy_t"); got != 5000*4999/2 {
		t.Fatal(got)
	}
	if n := readAll(t, c, "SELECT i FROM range(100000) t(i)"); n != 100000 {
		t.Fatalf("read %d rows", n)
	}
	if n := forwarded.Load(); n < 6 || bad.Load() != 0 {
		t.Fatalf("%d requests forwarded, %d not shaped like Quack requests", n, bad.Load())
	}

	// Without the header the proxy answers 403, surfaced as a StatusError.
	_, err = connect(t, d, tlsOpt)
	var se *transport.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusForbidden || !strings.Contains(string(se.Body), "proxy authentication required") {
		t.Fatalf("without the proxy header: %v, want an HTTP 403 StatusError", err)
	}
}
