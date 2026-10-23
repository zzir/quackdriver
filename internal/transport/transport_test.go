package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// echoHandler answers every POST /quack with "resp:" + request body and
// records the last request headers.
type echoHandler struct {
	calls  atomic.Int64
	mu     sync.Mutex
	header http.Header
	proto  string
}

func (h *echoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls.Add(1)
	h.mu.Lock()
	h.header = r.Header.Clone()
	h.proto = r.Proto
	h.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/quack" {
		http.Error(w, "bad request line", http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(r.Body)
	_, _ = w.Write(append([]byte("resp:"), body...))
}

func (h *echoHandler) last() (http.Header, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.header, h.proto
}

// uriFor parses a DSN pointing at addr (host:port).
func uriFor(t *testing.T, addr, params string) *URI {
	t.Helper()
	dsn := "quack://" + addr
	if params != "" {
		dsn += "?" + params
	}
	u, err := ParseURI(dsn)
	if err != nil {
		t.Fatalf("%s: %v", dsn, err)
	}
	return u
}

func srvAddr(srv *httptest.Server) string { return srv.Listener.Addr().String() }

func fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func mustNew(t *testing.T, u *URI, opts Options) *Transport {
	t.Helper()
	tr, err := New(u, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

// countingServer starts srv and returns a counter of the TCP connections
// it accepts.
func countingServer(t *testing.T, h http.Handler) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			n.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &n
}

// listener accepts TCP connections on a fresh port and hands each to serve.
func listener(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return ln.Addr().String()
}

// closeAfterRead closes the connection after reading what arrives first,
// like a TLS server receiving plaintext.
func closeAfterRead(c net.Conn) {
	_, _ = c.Read(make([]byte, 512))
	_ = c.Close()
}

// deadAddr returns an address nothing listens on.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRequestPlainHTTP(t *testing.T) {
	h := &echoHandler{}
	srv := httptest.NewServer(h)
	defer srv.Close()

	extra := http.Header{}
	extra.Set("X-Proxy-Auth", "letmein")
	extra.Add("X-Multi", "a")
	extra.Add("X-Multi", "b")
	extra.Set("Content-Type", "text/plain") // must be overridden
	tr := mustNew(t, uriFor(t, srvAddr(srv), "tls=false"), Options{Header: extra})
	// Mutating the caller's header after New must not leak into requests.
	extra.Set("X-Proxy-Auth", "changed")

	got, err := tr.Request(context.Background(), []byte("hello"))
	if err != nil || string(got) != "resp:hello" {
		t.Fatalf("%q %v", got, err)
	}
	hdr, proto := h.last()
	if hdr.Get("Content-Type") != "application/vnd.duckdb" || hdr.Get("Accept") != "application/vnd.duckdb" {
		t.Fatalf("content headers: %v", hdr)
	}
	if hdr.Get("X-Proxy-Auth") != "letmein" {
		t.Fatalf("extra header: %v", hdr)
	}
	if v := hdr.Values("X-Multi"); len(v) != 2 || v[0] != "a" || v[1] != "b" {
		t.Fatalf("multi-valued header: %v", v)
	}
	if hdr.Get("Accept-Encoding") != "" {
		t.Fatalf("compression must be disabled, got Accept-Encoding %q", hdr.Get("Accept-Encoding"))
	}
	if proto != "HTTP/1.1" {
		t.Fatalf("proto %q", proto)
	}

	// Any 2xx is a success.
	accepted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ok"))
	}))
	defer accepted.Close()
	tr = mustNew(t, uriFor(t, srvAddr(accepted), ""), Options{})
	if got, err := tr.Request(context.Background(), nil); err != nil || string(got) != "ok" {
		t.Fatalf("202: %q %v", got, err)
	}
}

// TestRequestStatusError: a non-2xx answer is a *StatusError with a short
// body preview, and the rest of the body is drained so the connection is
// reused.
func TestRequestStatusError(t *testing.T) {
	srv, conns := countingServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, strings.Repeat("e", 4096), http.StatusRequestEntityTooLarge)
	}))
	tr := mustNew(t, uriFor(t, srvAddr(srv), ""), Options{})
	for range 5 {
		_, err := tr.Request(context.Background(), []byte("x"))
		var se *StatusError
		if !errors.As(err, &se) || se.StatusCode != http.StatusRequestEntityTooLarge || se.Status != "413 Request Entity Too Large" {
			t.Fatalf("want a 413 *StatusError, got %v", err)
		}
		if len(se.Body) != errorPreviewBytes || !strings.HasPrefix(err.Error(), "transport: HTTP 413") {
			t.Fatalf("preview %d bytes, error %q", len(se.Body), err)
		}
		if NotSent(err) {
			t.Fatal("a request that got an HTTP answer was sent")
		}
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("5 error responses used %d connections, want 1", n)
	}
}

// TestRedirectNotFollowed: a 3xx answer (the server never sends one) is an
// error. Following it would replay the body, which carries the token in the
// handshake, and the custom headers to another origin, even from a pinned
// https server to plain http.
func TestRedirectNotFollowed(t *testing.T) {
	stolen := &echoHandler{}
	other := httptest.NewServer(stolen)
	defer other.Close()
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer front.Close()
	u := uriFor(t, srvAddr(front), "ssl_fingerprint="+fingerprint(front.Certificate()))
	tr := mustNew(t, u, Options{Header: http.Header{"X-Proxy-Auth": {"proxy-secret"}}})
	got, err := tr.Request(context.Background(), []byte("AUTH_TOKEN=super_secret"))
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("got %q, %v", got, err)
	}
	if n := stolen.calls.Load(); n != 0 {
		t.Fatalf("the redirect target received %d requests", n)
	}
}

func TestTLS(t *testing.T) {
	h := &echoHandler{}
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true // the client must still speak HTTP/1.1
	srv.StartTLS()
	defer srv.Close()
	addr, fp := srvAddr(srv), fingerprint(srv.Certificate())

	t.Run("pinned", func(t *testing.T) {
		// No RootCAs: only the pin makes the self-signed certificate
		// acceptable; a mismatching ServerName must not matter either.
		tr := mustNew(t, uriFor(t, addr, "ssl_fingerprint="+strings.ToLower(fp)),
			Options{TLSConfig: &tls.Config{ServerName: "not-the-cert-name.invalid"}})
		got, err := tr.Request(context.Background(), []byte("pinned"))
		if err != nil || string(got) != "resp:pinned" {
			t.Fatalf("%q %v", got, err)
		}
		if _, proto := h.last(); proto != "HTTP/1.1" {
			t.Fatalf("proto %q, want HTTP/1.1", proto)
		}
	})

	t.Run("wrong pin", func(t *testing.T) {
		before := h.calls.Load()
		wrong := strings.Repeat("AB", 32)
		tr := mustNew(t, uriFor(t, addr, "ssl_fingerprint="+wrong), Options{})
		_, err := tr.Request(context.Background(), []byte("x"))
		var pe *PinError
		if !errors.As(err, &pe) || pe.Want != wrong || pe.Got != fp {
			t.Fatalf("want *PinError{Want: %s, Got: %s}, got %v", wrong, fp, err)
		}
		if !strings.Contains(err.Error(), "does not match the pinned ssl_fingerprint") || strings.Contains(err.Error(), "self-signed") {
			t.Fatalf("message: %v", err)
		}
		if !NotSent(err) || h.calls.Load() != before {
			t.Fatal("a pin mismatch must abort before anything is sent")
		}
	})

	t.Run("base VerifyConnection after pin", func(t *testing.T) {
		var called atomic.Int64
		base := &tls.Config{
			RootCAs: x509.NewCertPool(), // would reject the server on its own
			VerifyConnection: func(tls.ConnectionState) error {
				called.Add(1)
				return errors.New("base veto")
			},
		}
		tr := mustNew(t, uriFor(t, addr, "ssl_fingerprint="+fp), Options{TLSConfig: base})
		_, err := tr.Request(context.Background(), nil)
		if err == nil || !strings.Contains(err.Error(), "base veto") || called.Load() != 1 {
			t.Fatalf("base VerifyConnection must run after a matching pin: %v (called %d)", err, called.Load())
		}
		if base.InsecureSkipVerify {
			t.Fatal("the caller's TLS config must not be modified")
		}
	})

	t.Run("trusted CA", func(t *testing.T) {
		pool := x509.NewCertPool()
		pool.AddCert(srv.Certificate()) // covers 127.0.0.1
		tr := mustNew(t, uriFor(t, addr, "tls=true"), Options{TLSConfig: &tls.Config{RootCAs: pool}})
		if got, err := tr.Request(context.Background(), []byte("ca")); err != nil || string(got) != "resp:ca" {
			t.Fatalf("%q %v", got, err)
		}
	})

	t.Run("untrusted", func(t *testing.T) {
		tr := mustNew(t, uriFor(t, addr, "tls=true"), Options{})
		_, err := tr.Request(context.Background(), nil)
		var certErr *tls.CertificateVerificationError
		if !errors.As(err, &certErr) || !NotSent(err) || !strings.Contains(err.Error(), "ssl_fingerprint=") {
			t.Fatalf("untrusted certificate: %v", err)
		}
	})
}

// TestTLSClientToNonTLSPeer: a TLS handshake against a plain HTTP server,
// a peer that closes the connection or one that never answers fails before
// anything is sent, with an explanation (net/http's own for an HTTP answer,
// otherwise a hint about tls=false).
func TestTLSClientToNonTLSPeer(t *testing.T) {
	plain := httptest.NewServer(&echoHandler{})
	defer plain.Close()
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	silent := listener(t, func(c net.Conn) {
		mu.Lock()
		held = append(held, c)
		mu.Unlock()
	})
	for _, c := range []struct{ name, addr, want string }{
		{"plain HTTP", srvAddr(plain), "server gave HTTP response to HTTPS client"},
		{"closes", listener(t, closeAfterRead), "use tls=false"},
		{"silent", silent, "use tls=false"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := mustNew(t, uriFor(t, c.addr, "tls=true&connect_timeout=200ms"), Options{})
			start := time.Now()
			_, err := tr.Request(context.Background(), []byte("x"))
			if err == nil || !NotSent(err) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v (NotSent=%v)", err, NotSent(err))
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Fatalf("connect_timeout not applied: %v", d)
			}
		})
	}
}

// TestPlainClientToTLSServer: a plain request the server closes without an
// answer may have been read, and gets a hint about tls=true; a Go TLS
// server's HTTP 400 needs no hint.
func TestPlainClientToTLSServer(t *testing.T) {
	tr := mustNew(t, uriFor(t, listener(t, closeAfterRead), ""), Options{})
	_, err := tr.Request(context.Background(), []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "use tls=true") || !errors.Is(err, io.EOF) || NotSent(err) {
		t.Fatalf("closed without answer: %v (NotSent=%v)", err, NotSent(err))
	}

	srv := httptest.NewTLSServer(&echoHandler{})
	defer srv.Close()
	tr = mustNew(t, uriFor(t, srvAddr(srv), "tls=false"), Options{})
	_, err = tr.Request(context.Background(), nil)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain HTTP to a Go TLS server: %v", err)
	}
}

func TestTimeouts(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	// The DSN timeout is named; the request may have been executed.
	tr := mustNew(t, uriFor(t, srvAddr(srv), "timeout=100ms"), Options{})
	_, err := tr.Request(context.Background(), nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.HasSuffix(err.Error(), "(the request exceeded the DSN timeout=100ms)") || NotSent(err) {
		t.Fatalf("DSN timeout: %v", err)
	}
	// A caller deadline is not.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	tr = mustNew(t, uriFor(t, srvAddr(srv), "timeout=1h"), Options{})
	if _, err = tr.Request(ctx, nil); !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "(") {
		t.Fatalf("caller deadline: %v", err)
	}
}

func TestCanceledBeforeSendNotSent(t *testing.T) {
	h := &echoHandler{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	tr := mustNew(t, uriFor(t, srvAddr(srv), ""), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tr.Request(ctx, nil)
	if !errors.Is(err, context.Canceled) || !NotSent(err) || h.calls.Load() != 0 {
		t.Fatalf("canceled before connect: %v (NotSent=%v)", err, NotSent(err))
	}
}

func TestCustomHTTPClient(t *testing.T) {
	h := &echoHandler{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	var used atomic.Bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used.Store(true)
		return http.DefaultTransport.RoundTrip(r)
	})}
	tr := mustNew(t, uriFor(t, srvAddr(srv), ""), Options{HTTPClient: client, Header: http.Header{"X-A": {"1"}}})
	got, err := tr.Request(context.Background(), []byte("c"))
	if err != nil || string(got) != "resp:c" || !used.Load() {
		t.Fatalf("%q %v used=%v", got, err, used.Load())
	}
	if hdr, _ := h.last(); hdr.Get("X-A") != "1" || hdr.Get("Content-Type") != MimeType {
		t.Fatalf("headers %v", hdr)
	}

	// Connection failures are NotSent with the own and a foreign client.
	dead := uriFor(t, deadAddr(t), "")
	for _, opts := range []Options{{}, {HTTPClient: client}} {
		_, err := mustNew(t, dead, opts).Request(context.Background(), nil)
		if !NotSent(err) || !strings.HasPrefix(err.Error(), "transport: request: ") {
			t.Fatalf("refused (custom client %v): %v", opts.HTTPClient != nil, err)
		}
	}

	// A foreign client failing for other reasons is not provably unsent.
	failing := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})}
	tr = mustNew(t, uriFor(t, srvAddr(srv), ""), Options{HTTPClient: failing})
	if _, err := tr.Request(context.Background(), nil); err == nil || NotSent(err) {
		t.Fatalf("unexpected EOF: %v", err)
	}
}

func TestNewErrors(t *testing.T) {
	t.Setenv(ClientIDEnv, testClientID)
	pinned, err := ParseURI("quack:example.com?ssl_fingerprint=" + pin64)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := ParseURI("quack:localhost")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		u    *URI
		opts Options
		want string
	}{
		{pinned, Options{HTTPClient: &http.Client{}}, "cannot be enforced"},
		{plain, Options{HTTPClient: &http.Client{}, TLSConfig: &tls.Config{}}, "cannot be combined"},
		{plain, Options{HTTPClient: &http.Client{}, MaxIdleConnsPerHost: 4}, "cannot be combined"},
	} {
		if _, err := New(c.u, c.opts); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: error %v, want %q", c.opts, err, c.want)
		}
	}
}

func TestNewCopiesURIAndTunesTransport(t *testing.T) {
	t.Setenv(ClientIDEnv, testClientID)
	u, err := ParseURI("quack:example.com:1234?ssl_fingerprint=" + strings.ToLower(pin64) + "&inline_rows=7&connect_timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := New(u, Options{TLSConfig: &tls.Config{NextProtos: []string{"h2"}}})
	if err != nil {
		t.Fatal(err)
	}
	u.Port = 1
	*u.InlineRows = 99
	if tr.URI.Port != 1234 || *tr.URI.InlineRows != 7 || tr.URI.SSLFingerprint != pin64 {
		t.Fatalf("URI not copied: %+v", tr.URI)
	}
	if tr.url != "https://example.com:1234/quack" || !tr.own {
		t.Fatalf("url %q own %v", tr.url, tr.own)
	}
	ht := tr.client.Transport.(*http.Transport)
	if tr.client.Timeout != 0 || ht.ResponseHeaderTimeout != 0 {
		t.Fatal("no overall or response-header timeout expected")
	}
	if ht.TLSHandshakeTimeout != 3*time.Second || ht.IdleConnTimeout != 90*time.Second ||
		ht.MaxIdleConnsPerHost != DefaultMaxIdleConnsPerHost || ht.ForceAttemptHTTP2 ||
		ht.TLSNextProto == nil || len(ht.TLSNextProto) != 0 || !ht.DisableCompression {
		t.Fatalf("transport tuning: %+v", ht)
	}
	cfg := ht.TLSClientConfig
	if cfg == nil || !cfg.InsecureSkipVerify || cfg.VerifyConnection == nil ||
		len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" || cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("tls config: %+v", cfg)
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{}); err == nil || !strings.Contains(err.Error(), "presented no certificate") {
		t.Fatalf("no peer certificate: %v", err)
	}

	// Plain HTTP builds no TLS configuration at all.
	tr = mustNew(t, uriFor(t, "localhost", ""), Options{MaxIdleConnsPerHost: 5})
	ht = tr.client.Transport.(*http.Transport)
	if ht.TLSClientConfig != nil || ht.MaxIdleConnsPerHost != 5 || ht.TLSHandshakeTimeout != DefaultConnectTimeout {
		t.Fatalf("plain tuning: %+v", ht)
	}
}

func TestNotSentClassification(t *testing.T) {
	if NotSent(nil) || NotSent(errors.New("x")) || NotSent(context.Canceled) {
		t.Fatal("plain errors are not NotSent")
	}
	if !NotSent(fmt.Errorf("x: %w", &notSentError{err: io.EOF})) || !errors.Is(&notSentError{err: io.EOF}, io.EOF) {
		t.Fatal("notSentError must be found when wrapped and must unwrap")
	}
	for _, err := range []error{
		&net.DNSError{Err: "no such host", Name: "x"},
		&net.OpError{Op: "dial", Err: errors.New("refused")},
		&tls.CertificateVerificationError{Err: errors.New("bad")},
	} {
		if !connectFailure(fmt.Errorf("w: %w", err)) {
			t.Errorf("%T should be a connect failure", err)
		}
	}
	for _, err := range []error{io.EOF, &net.OpError{Op: "read", Err: errors.New("reset")}, context.DeadlineExceeded} {
		if connectFailure(err) {
			t.Errorf("%v should not be a connect failure", err)
		}
	}
}

// TestConnectionPooling: concurrent requests share pooled connections
// without cross-talk, and CloseIdleConnections drops them.
func TestConnectionPooling(t *testing.T) {
	srv, conns := countingServer(t, &echoHandler{})
	const workers, perWorker = 16, 25
	tr := mustNew(t, uriFor(t, srvAddr(srv), ""), Options{MaxIdleConnsPerHost: workers})
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				body := []byte("w" + strconv.Itoa(w) + "-" + strconv.Itoa(i))
				got, err := tr.Request(context.Background(), body)
				if err != nil {
					t.Error(err)
					return
				}
				if !bytes.Equal(got, append([]byte("resp:"), body...)) {
					t.Errorf("cross-talk: sent %q got %q", body, got)
				}
			}
		}()
	}
	wg.Wait()
	// The transport may race an extra dial against a connection being
	// returned to the pool, hence the slack.
	n := conns.Load()
	if n > 2*workers {
		t.Fatalf("%d connections opened for %d concurrent workers", n, workers)
	}
	tr.CloseIdleConnections()
	if _, err := tr.Request(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if conns.Load() != n+1 {
		t.Fatal("CloseIdleConnections did not drop the pooled connections")
	}
}

// TestRequestBodySizes reads bodies with and without Content-Length (the
// first is read into one buffer of exactly that size) and checks that the
// connection is reused either way.
func TestRequestBodySizes(t *testing.T) {
	srv, conns := countingServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.Header.Get("X-Size"))
		chunked := r.Header.Get("X-Chunked") != ""
		if !chunked {
			w.Header().Set("Content-Length", strconv.Itoa(n))
		}
		body := bytes.Repeat([]byte{'q'}, n)
		for len(body) > 0 {
			k := min(len(body), 1000)
			_, _ = w.Write(body[:k])
			body = body[k:]
			if chunked {
				w.(http.Flusher).Flush()
			}
		}
	}))
	for _, chunked := range []bool{false, true} {
		for _, n := range []int{0, 1, 4095, 1 << 20} {
			h := http.Header{"X-Size": {strconv.Itoa(n)}}
			if chunked {
				h.Set("X-Chunked", "1")
			}
			tr := mustNew(t, uriFor(t, srvAddr(srv), ""), Options{Header: h})
			for range 3 {
				b, err := tr.Request(context.Background(), nil)
				if err != nil || b == nil || len(b) != n || (n > 0 && b[n-1] != 'q') {
					t.Fatalf("chunked=%v size %d: got %d bytes, %v", chunked, n, len(b), err)
				}
			}
			tr.CloseIdleConnections()
		}
	}
	// One connection per Transport (8), each reused for its 3 requests.
	if c := conns.Load(); c != 8 {
		t.Fatalf("24 requests on 8 transports opened %d connections, want 8", c)
	}
}

// TestRequestShortBody reports a body shorter than its Content-Length.
func TestRequestShortBody(t *testing.T) {
	addr := listener(t, func(c net.Conn) {
		defer c.Close()
		_, _ = c.Read(make([]byte, 4096))
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort")
	})
	tr := mustNew(t, uriFor(t, addr, ""), Options{})
	if _, err := tr.Request(context.Background(), nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short body: %v, want io.ErrUnexpectedEOF", err)
	}
}
