//go:build integration

package network

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver"
	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/transport"
)

// TestPinnedTLS: the pin computed from the certificate file equals what
// quack_generate_keys reports, and both spellings switch a local DSN (plain
// HTTP by default) to HTTPS.
func TestPinnedTLS(t *testing.T) {
	t.Parallel()
	srv := livetest.StartTLSServer(t, "127.0.0.1", "")
	c := mustConnect(t, srv.DSN("ssl_fingerprint=sha256:"+strings.ToLower(srv.Fingerprint)))
	reported := livetest.Scalar[string](t, c, "SELECT fingerprint FROM quack_generate_keys(directory := "+livetest.SQLString(srv.Dir)+")")
	if norm, err := transport.NormalizeFingerprint(reported); err != nil || norm != srv.Fingerprint {
		t.Fatalf("server reports fingerprint %s (%v), the certificate file hashes to %s", reported, err, srv.Fingerprint)
	}
	c = mustConnect(t, srv.DSN("ssl_fingerprint="+strings.ReplaceAll(reported, ":", "%3A")))
	if got := livetest.Scalar[int64](t, c, "SELECT 40 + 2"); got != 42 {
		t.Fatal(got)
	}
}

// TestWrongPin: a mismatching pin aborts the TLS handshake before anything
// is sent; the error names both fingerprints.
func TestWrongPin(t *testing.T) {
	t.Parallel()
	srv := livetest.StartTLSServer(t, "127.0.0.1", "")
	wrong := strings.Repeat("AB", 32)
	_, err := connect(t, srv.DSN("ssl_fingerprint="+wrong))
	var pe *transport.PinError
	if !errors.As(err, &pe) || pe.Got != srv.Fingerprint || pe.Want != wrong || !transport.NotSent(err) {
		t.Fatalf("wrong pin: %v, want a NotSent *transport.PinError", err)
	}
}

// TestTLSWithoutPin: the generated certificate is self-signed for CN
// localhost without SANs, so regular verification fails even with the
// certificate as the only root (Go ignores the Common Name); a pin
// overrides the failing verification of a base config.
func TestTLSWithoutPin(t *testing.T) {
	t.Parallel()
	srv := livetest.StartTLSServer(t, "127.0.0.1", "")
	for _, opts := range [][]quackdriver.Option{nil,
		{quackdriver.WithTLSConfig(&tls.Config{RootCAs: srv.Pool})},
		{quackdriver.WithTLSConfig(&tls.Config{RootCAs: srv.Pool, ServerName: "localhost"})},
	} {
		_, err := connect(t, srv.DSN("tls=true"), opts...)
		var cve *tls.CertificateVerificationError
		if !errors.As(err, &cve) || !transport.NotSent(err) {
			t.Fatalf("tls=true without a pin: %v, want a NotSent certificate error", err)
		}
	}
	c := mustConnect(t, srv.DSN("ssl_fingerprint="+srv.Fingerprint), quackdriver.WithTLSConfig(&tls.Config{RootCAs: x509.NewCertPool()}))
	livetest.Scalar[int64](t, c, "SELECT 1")
}

// TestNonLocalBind relies on quack_serve's default of HTTPS for a non-local
// listen address (0.0.0.0) and reaches it through addresses the DSN treats
// as non-local, where TLS is the default too.
func TestNonLocalBind(t *testing.T) {
	t.Parallel()
	var hosts []string
	// Linux routes all of 127.0.0.0/8 to the loopback interface; macOS
	// assigns only 127.0.0.1 (dialling 127.0.0.2 times out) unless an alias
	// is added.
	if l, err := net.Listen("tcp", "127.0.0.2:0"); err == nil {
		_ = l.Close()
		hosts = append(hosts, "127.0.0.2")
	} else {
		t.Logf("127.0.0.2 is not an address of this machine (%v); on macOS: sudo ifconfig lo0 alias 127.0.0.2", err)
	}
	if a := nonLoopbackAddr(); a != "" {
		hosts = append(hosts, a)
	} else {
		t.Logf("no non-loopback address found; set %s to test one", nonLoopbackEnv)
	}
	if len(hosts) == 0 {
		t.Skip("no address of this machine other than 127.0.0.1 to reach the server through")
	}
	keys := livetest.StartTLSServer(t, "127.0.0.1", "") // only for its key pair
	srv := livetest.StartServer(t, livetest.ServerOptions{
		Host: "0.0.0.0",
		ServeArgs: fmt.Sprintf("allow_other_hostname => true, ssl_cert_file => %s, ssl_key_file => %s",
			livetest.SQLString(keys.Dir+"/server.pem"), livetest.SQLString(keys.Dir+"/private_key.pem")),
		TLS: true,
	})
	for _, host := range hosts {
		u, err := transport.ParseURI(dsn(host, srv.Port, srv.Token))
		if err != nil || !u.SSL {
			t.Fatalf("%s: SSL=%v %v, want HTTPS by default", host, u.SSL, err)
		}
		c := mustConnect(t, dsn(host, srv.Port, srv.Token, "ssl_fingerprint="+keys.Fingerprint))
		if got := livetest.Scalar[string](t, c, "SELECT 'hello ' || 'tls'"); got != "hello tls" {
			t.Fatal(got)
		}
		// Default verification fails on the self-signed certificate, with a
		// hint at pinning.
		_, err = connect(t, dsn(host, srv.Port, srv.Token, "connect_timeout=5s"))
		var cve *tls.CertificateVerificationError
		if !errors.As(err, &cve) || !transport.NotSent(err) || !strings.Contains(err.Error(), "ssl_fingerprint=") {
			t.Fatalf("%s: default TLS to a self-signed server: %v", host, err)
		}
	}
}

// TestProtocolMismatch: plain HTTP to an HTTPS server (which closes the
// connection without an answer) and HTTPS to a plain server (which never
// answers a ClientHello) fail clearly within connect_timeout.
func TestProtocolMismatch(t *testing.T) {
	t.Parallel()
	tlsSrv := livetest.StartTLSServer(t, "127.0.0.1", "")
	plain := livetest.StartServer(t, livetest.ServerOptions{})
	for _, c := range []struct{ dsn, hint string }{
		{tlsSrv.DSN("connect_timeout=2s"), "use tls=true"},
		{plain.DSN("tls=true", "connect_timeout=2s"), "use tls=false"},
	} {
		var err error
		if el := timed(func() { _, err = connect(t, c.dsn) }); el > 2500*time.Millisecond {
			t.Errorf("%s: failing took %v", c.dsn, el)
		}
		if err == nil || !strings.Contains(err.Error(), c.hint) {
			t.Errorf("%s: %v, want a hint to %s", c.dsn, err, c.hint)
		}
	}
	livetest.Scalar[int64](t, mustConnect(t, plain.DSN()), "SELECT 1") // the server is unharmed
}

// TestIPv6 serves on [::1], which is local: both DSN spellings default to
// plain HTTP, and a pin switches to HTTPS.
func TestIPv6(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	_ = ln.Close()
	srv := livetest.StartServer(t, livetest.ServerOptions{Host: "::1"})
	for _, d := range []string{srv.DSN(), strings.Replace(srv.DSN(), "quack://", "quack:", 1)} {
		if u, err := transport.ParseURI(d); err != nil || u.SSL || !u.IPv6 || u.Host != "::1" {
			t.Fatalf("%s: %+v %v", d, u, err)
		}
		if got := livetest.Scalar[string](t, mustConnect(t, d), "SELECT 'v6'"); got != "v6" {
			t.Fatal(got)
		}
	}
	tlsSrv := livetest.StartTLSServer(t, "::1", "")
	livetest.Scalar[int64](t, mustConnect(t, tlsSrv.DSN("ssl_fingerprint="+tlsSrv.Fingerprint)), "SELECT 6")
}
