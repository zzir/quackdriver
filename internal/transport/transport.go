package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// DefaultMaxIdleConnsPerHost is the default size of the idle connection pool
// kept to the server.
const DefaultMaxIdleConnsPerHost = 32

// idleConnTimeout closes pooled connections before the server's keep-alive
// timeout (quack_server_keep_alive_timeout, 300 s) would.
const idleConnTimeout = 90 * time.Second

// errorPreviewBytes bounds the body excerpt included in StatusError.
const errorPreviewBytes = 256

// drainBytes bounds how much of a non-2xx response body is discarded to keep
// its connection reusable; a longer body closes the connection instead.
const drainBytes = 64 << 10

// Options customizes a Transport.
type Options struct {
	// HTTPClient, if non-nil, is used as-is: the caller owns TLS, proxies
	// and pooling (and should not follow redirects: set CheckRedirect to
	// return http.ErrUseLastResponse), and TLSConfig / MaxIdleConnsPerHost
	// must not be set.
	// Certificate pinning (ssl_fingerprint) cannot be enforced on a foreign
	// client, so New rejects that combination.
	HTTPClient *http.Client
	// TLSConfig is an optional base TLS configuration (custom RootCAs,
	// client certificates, ...). It is cloned. With ssl_fingerprint the pin
	// replaces CA and hostname verification.
	TLSConfig *tls.Config
	// Header holds extra headers sent with every request (for example
	// authentication for a reverse proxy; the official client's equivalent
	// is the EXTRA_HTTP_HEADERS secret). Content-Type and Accept are always
	// set by the transport.
	Header http.Header
	// MaxIdleConnsPerHost sizes the idle connection pool; <= 0 means
	// DefaultMaxIdleConnsPerHost.
	MaxIdleConnsPerHost int
}

// Transport POSTs Quack messages to one server over HTTP/1.1 and returns the
// binary response bodies. It owns one tuned http.Transport (connection pool)
// and is safe for concurrent use; share one Transport per server/connector.
type Transport struct {
	// URI is a private copy of the URI given to New. Treat it as read-only.
	URI *URI

	client *http.Client
	// own is set when client uses the http.Transport built by New, whose
	// connection tracing makes NotSent exact.
	own    bool
	url    string
	header http.Header
}

// New returns a Transport for u, which must come from ParseURI. Unless
// opts.HTTPClient is given it builds a dedicated http.Transport: HTTP/1.1
// only (the server is cpp-httplib), dial and TLS handshake bounded by
// u.ConnectTimeout, idle connections kept for 90 s, and no response-header
// timeout because PREPARE and FETCH may long-poll. u.Timeout, when non-zero, bounds each Request as a whole.
// Like any Go HTTP client it honors the HTTP_PROXY / HTTPS_PROXY / NO_PROXY
// environment variables (loopback addresses are never proxied); pass
// opts.HTTPClient to control proxying explicitly.
func New(u *URI, opts Options) (*Transport, error) {
	uc := *u
	if uc.InlineRows != nil {
		n := *uc.InlineRows
		uc.InlineRows = &n
	}
	t := &Transport{URI: &uc, url: uc.HTTPURL(), header: opts.Header.Clone()}
	if opts.HTTPClient != nil {
		if opts.TLSConfig != nil || opts.MaxIdleConnsPerHost != 0 {
			return nil, fmt.Errorf("transport: Options.TLSConfig and MaxIdleConnsPerHost cannot be combined with Options.HTTPClient")
		}
		if uc.SSLFingerprint != "" {
			return nil, fmt.Errorf("transport: ssl_fingerprint cannot be enforced on a caller-supplied HTTPClient; pin the certificate in its TLS configuration instead")
		}
		t.client = opts.HTTPClient
		return t, nil
	}

	maxIdle := opts.MaxIdleConnsPerHost
	if maxIdle <= 0 {
		maxIdle = DefaultMaxIdleConnsPerHost
	}
	dialer := &net.Dialer{Timeout: uc.ConnectTimeout, KeepAlive: 30 * time.Second}
	ht := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: uc.ConnectTimeout,
		IdleConnTimeout:     idleConnTimeout,
		MaxIdleConnsPerHost: maxIdle,
		// The server neither compresses nor speaks HTTP/2: no gzip
		// negotiation, and an empty TLSNextProto map disables h2.
		DisableCompression: true,
		TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	if uc.SSL {
		ht.TLSClientConfig = tlsConfig(&uc, opts.TLSConfig)
	}
	t.client = &http.Client{Transport: ht, CheckRedirect: noRedirects}
	t.own = true
	return t, nil
}

// noRedirects makes a 3xx answer an error (a StatusError) instead of being
// followed: a Quack server never redirects (its errors arrive as HTTP 200),
// and following one would replay the request body, which carries the token
// in the handshake, and the custom headers to another origin, possibly over
// plain HTTP and past the certificate pin.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// tlsConfig derives the client TLS configuration from the optional base.
func tlsConfig(u *URI, base *tls.Config) *tls.Config {
	var cfg *tls.Config
	if base != nil {
		cfg = base.Clone()
	} else {
		cfg = &tls.Config{}
	}
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	// Offer only HTTP/1.1 via ALPN so the connection can never end up
	// negotiated to a protocol the transport does not speak.
	cfg.NextProtos = []string{"http/1.1"}
	if u.SSLFingerprint == "" {
		return cfg
	}
	// The pin is the whole trust decision (as in PinnedHttpsQuackClient):
	// skip CA and hostname verification, accept exactly the leaf
	// certificate whose DER SHA-256 matches. A base VerifyConnection still
	// runs after the pin check.
	want := u.SSLFingerprint
	addr := net.JoinHostPort(u.Host, strconv.Itoa(u.Port))
	next := cfg.VerifyConnection
	cfg.InsecureSkipVerify = true
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return &PinError{Addr: addr, Want: want}
		}
		sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
		got := strings.ToUpper(hex.EncodeToString(sum[:]))
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			return &PinError{Addr: addr, Want: want, Got: got}
		}
		if next != nil {
			return next(cs)
		}
		return nil
	}
	return cfg
}

// PinError reports that the server presented a certificate other than the
// one pinned with ssl_fingerprint. The TLS handshake is aborted, so the
// request never reaches the server.
type PinError struct {
	Addr string // host:port of the server
	Want string // pinned fingerprint (64 uppercase hex digits)
	Got  string // presented leaf fingerprint; empty if none was presented
}

func (e *PinError) Error() string {
	if e.Got == "" {
		return fmt.Sprintf("transport: %s presented no certificate, but ssl_fingerprint is pinned", e.Addr)
	}
	return fmt.Sprintf("transport: the certificate presented by %s (sha256:%s) does not match the pinned ssl_fingerprint (sha256:%s)", e.Addr, e.Got, e.Want)
}

// StatusError is returned by Request when the server answers with a non-2xx
// HTTP status. Quack protocol errors arrive as HTTP 200 + ERROR_RESPONSE, so
// this indicates an HTTP-level problem (proxy, oversized body, wrong
// endpoint, ...).
type StatusError struct {
	StatusCode int
	Status     string // e.g. "413 Payload Too Large"
	Body       []byte // at most the first 256 bytes of the response body
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("transport: HTTP %s: %q", e.Status, e.Body)
}

// notSentError marks a request failure that provably happened before the
// request reached the server.
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

// NotSent reports whether err (from Request) proves the request never
// reached the server, so it is safe to retry on another connection: the
// connection could not be established (DNS, dial, connect timeout, TLS
// handshake, certificate verification, pin mismatch) or the request was
// cancelled before a connection was obtained. Failures after a connection
// was handed to the request report false, because the server may have
// received and executed it.
func NotSent(err error) bool {
	var ns *notSentError
	return errors.As(err, &ns)
}

// Request POSTs body to the Quack endpoint and returns the response body.
// It is safe for concurrent use. A non-2xx status yields a *StatusError.
// Use NotSent to tell whether a failed request may have reached the server.
func (t *Transport) Request(ctx context.Context, body []byte) ([]byte, error) {
	parent := ctx
	if t.URI.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.URI.Timeout)
		defer cancel()
	}
	// Whether a connection was handed to this request: until then nothing
	// can have been written to the server. The other flags only refine the
	// error message.
	var gotConn, reused, tlsStarted atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			reused.Store(info.Reused)
			gotConn.Store(true)
		},
		TLSHandshakeStart: func() { tlsStarted.Store(true) },
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("transport: build request: %w", err)
	}
	for k, vs := range t.header {
		req.Header[k] = append([]string(nil), vs...)
	}
	req.Header.Set("Content-Type", MimeType)
	req.Header.Set("Accept", MimeType)

	resp, err := t.client.Do(req)
	if err != nil {
		sent := gotConn.Load()
		hint := ""
		switch {
		case ctx.Err() == nil:
			hint = t.hint(err, sent, reused.Load(), tlsStarted.Load())
		case parent.Err() == nil:
			// Only the DSN timeout expired, not the caller's context.
			hint = fmt.Sprintf("the request exceeded the DSN timeout=%s", t.URI.Timeout)
		}
		if !sent && (t.own || connectFailure(err)) {
			err = &notSentError{err: err}
		}
		if hint != "" {
			return nil, fmt.Errorf("transport: request: %w (%s)", err, hint)
		}
		return nil, fmt.Errorf("transport: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, errorPreviewBytes))
		// Drain a bounded rest so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainBytes))
		return nil, &StatusError{StatusCode: resp.StatusCode, Status: resp.Status, Body: preview}
	}
	respBody, err := readBody(resp.Body, resp.ContentLength)
	if err != nil {
		return nil, fmt.Errorf("transport: read response: %w", err)
	}
	return respBody, nil
}

// maxPreallocBytes caps the buffer allocated up front from a response's
// Content-Length, so a bogus header cannot make the client allocate more
// than it receives; longer bodies grow beyond it as they arrive.
const maxPreallocBytes = 256 << 20

// readBody reads a response body of n bytes (n < 0 when unknown). With a
// known length the buffer is allocated once instead of being grown and
// copied repeatedly, which dominates client CPU for large FETCH batches.
func readBody(r io.Reader, n int64) ([]byte, error) {
	if n < 0 {
		return io.ReadAll(r)
	}
	if n <= maxPreallocBytes {
		// net/http enforces Content-Length: a shorter body is an
		// io.ErrUnexpectedEOF, a longer one is cut off.
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		// Make sure the end of the body has been seen (net/http usually
		// reports it with the last bytes): a body closed before its EOF
		// closes the connection instead of returning it to the pool.
		var one [1]byte
		_, _ = r.Read(one[:])
		return buf, nil
	}
	var b bytes.Buffer
	b.Grow(maxPreallocBytes)
	if _, err := b.ReadFrom(r); err != nil {
		return nil, err
	}
	if int64(b.Len()) != n {
		return nil, io.ErrUnexpectedEOF
	}
	return b.Bytes(), nil
}

// hint explains the likely cause of a failed request when the error alone
// is cryptic: an HTTP/HTTPS mismatch between DSN and server (the server
// either closes the connection or never completes the handshake) or a
// certificate that needs pinning. gotConn and reused describe the
// connection the request got, tlsStarted whether this request started a TLS
// handshake. It returns "" when there is nothing useful to add.
func (t *Transport) hint(err error, gotConn, reused, tlsStarted bool) string {
	u := t.URI
	if !u.SSL {
		// A TLS server closes a plaintext connection without an answer
		// (or with a TLS alert, which net/http reports as a malformed
		// response). A reused connection may simply have gone stale.
		if gotConn && !reused && (closedWithoutAnswer(err) || strings.Contains(err.Error(), `malformed HTTP response "\x15\x03`)) {
			return "the server closed the connection without answering; if it serves HTTPS, which is the default for " +
				"servers listening on a non-local address, use tls=true or ssl_fingerprint=<SHA-256 of its certificate>"
		}
		return ""
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) && u.SSLFingerprint == "" {
		return "a Quack server uses a self-signed certificate unless configured otherwise: pin it with " +
			"ssl_fingerprint=<SHA-256 of the certificate, as reported by quack_generate_keys()> or trust its CA in the TLS configuration"
	}
	var recErr tls.RecordHeaderError
	if !gotConn && tlsStarted && (errors.As(err, &recErr) || closedWithoutAnswer(err) || isTimeout(err)) {
		return "the server did not complete the TLS handshake; if it serves plain HTTP, which is the default for " +
			"servers listening on localhost, use tls=false or disable_ssl=true"
	}
	return ""
}

// closedWithoutAnswer reports whether err means the peer closed or reset
// the connection.
func closedWithoutAnswer(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// isTimeout reports whether err is a network timeout (as the TLS handshake
// timeout of net/http is).
func isTimeout(err error) bool {
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

// connectFailure classifies errors from a caller-supplied HTTP client whose
// round tripper may not report connection tracing: only failures that occur
// while establishing the connection count. (A PinError cannot occur there:
// New refuses ssl_fingerprint with a caller-supplied client.)
func connectFailure(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var certErr *tls.CertificateVerificationError
	return errors.As(err, &certErr)
}

// CloseIdleConnections closes pooled connections that are not in use.
func (t *Transport) CloseIdleConnections() {
	t.client.CloseIdleConnections()
}
