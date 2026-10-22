package transport

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testClientID = "test-client"

// pin64 is a valid fingerprint in canonical form.
const pin64 = "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"

func u64(v uint64) *uint64 { return &v }

// remote returns the defaults for a non-local host.
func remote(host string, port int) URI {
	return URI{
		Host: host, Port: port, SSL: true,
		ConnectTimeout: DefaultConnectTimeout, HeartbeatTimeout: DefaultHeartbeatTimeout,
		ClientID: testClientID, FetchAhead: DefaultFetchAhead, MaxBufferedRows: DefaultMaxBufferedRows,
	}
}

// local returns the defaults for a local host (plain HTTP).
func local(host string, port int) URI {
	u := remote(host, port)
	u.SSL = false
	return u
}

func with(u URI, f func(*URI)) URI {
	f(&u)
	return u
}

func TestParseURI(t *testing.T) {
	t.Setenv(ClientIDEnv, testClientID)
	cases := []struct {
		dsn  string
		want URI
	}{
		// Host forms and TLS default by locality.
		{"quack:localhost", local("localhost", DefaultPort)},
		{"quack:LocalHost", local("LocalHost", DefaultPort)},
		{"quack:localhost:7777", local("localhost", 7777)},
		{"quack://127.0.0.1:9494", local("127.0.0.1", 9494)},
		{"quack:[::1]:9494", with(local("::1", 9494), func(u *URI) { u.IPv6 = true })},
		{"quack:[::1]", with(local("::1", DefaultPort), func(u *URI) { u.IPv6 = true })},
		{"quack://example.com:8080", remote("example.com", 8080)},
		{"quack:example.com", remote("example.com", DefaultPort)},
		{"quack://example.com:8080/", remote("example.com", 8080)},
		{"  quack:example.com  ", remote("example.com", DefaultPort)},
		{"quack:127.0.0.2", remote("127.0.0.2", DefaultPort)},
		{"quack:[0:0:0:0:0:0:0:1]", with(remote("0:0:0:0:0:0:0:1", DefaultPort), func(u *URI) { u.IPv6 = true })},
		{"quack://[fe80::1]:1234?token=abcd&tls=true&timeout=5s", with(remote("fe80::1", 1234), func(u *URI) {
			u.IPv6, u.Token, u.Timeout = true, "abcd", 5*time.Second
		})},
		{"quack://h?token=t", with(remote("h", DefaultPort), func(u *URI) { u.Token = "t" })},
		{"quack://h?token=a%2Bb%26c", with(remote("h", DefaultPort), func(u *URI) { u.Token = "a+b&c" })},
		{"quack://h?token=", remote("h", DefaultPort)},

		// tls / disable_ssl.
		{"quack:example.com?tls=false", local("example.com", DefaultPort)},
		{"quack:example.com?disable_ssl=true", local("example.com", DefaultPort)},
		{"quack:example.com?disable_ssl=false", remote("example.com", DefaultPort)},
		{"quack:localhost?tls=true", with(local("localhost", DefaultPort), func(u *URI) { u.SSL = true })},
		{"quack:localhost?disable_ssl=false", with(local("localhost", DefaultPort), func(u *URI) { u.SSL = true })},
		{"quack:localhost?tls=1&disable_ssl=0", with(local("localhost", DefaultPort), func(u *URI) { u.SSL = true })},
		{"quack:example.com?tls=false&disable_ssl=true", local("example.com", DefaultPort)},
		{"quack:example.com?TLS=false", local("example.com", DefaultPort)},
		{"quack:example.com?Disable_SSL=TRUE", local("example.com", DefaultPort)},

		// ssl_fingerprint (implies HTTPS, also for local hosts).
		{"quack:example.com?ssl_fingerprint=" + pin64, with(remote("example.com", DefaultPort), func(u *URI) { u.SSLFingerprint = pin64 })},
		{"quack:localhost?ssl_fingerprint=" + strings.ToLower(pin64), with(local("localhost", DefaultPort), func(u *URI) {
			u.SSL, u.SSLFingerprint = true, pin64
		})},
		{"quack:localhost?tls=true&ssl_fingerprint=sha256:" + pin64, with(local("localhost", DefaultPort), func(u *URI) {
			u.SSL, u.SSLFingerprint = true, pin64
		})},

		// Timeouts.
		{"quack:h?timeout=0", remote("h", DefaultPort)},
		{"quack:h?timeout=90", with(remote("h", DefaultPort), func(u *URI) { u.Timeout = 90 * time.Second })},
		{"quack:h?timeout=1m30s", with(remote("h", DefaultPort), func(u *URI) { u.Timeout = 90 * time.Second })},
		{"quack:h?connect_timeout=250ms", with(remote("h", DefaultPort), func(u *URI) { u.ConnectTimeout = 250 * time.Millisecond })},
		{"quack:h?connect_timeout=0", with(remote("h", DefaultPort), func(u *URI) { u.ConnectTimeout = 0 })},
		{"quack:h?heartbeat_timeout=5", with(remote("h", DefaultPort), func(u *URI) { u.HeartbeatTimeout = 5 * time.Second })},
		{"quack:h?heartbeat_timeout=2m", with(remote("h", DefaultPort), func(u *URI) { u.HeartbeatTimeout = 2 * time.Minute })},
		{"quack:h?heartbeat_timeout=1s", with(remote("h", DefaultPort), func(u *URI) { u.HeartbeatTimeout = time.Second })},

		// client_id.
		{"quack:h?client_id=my-app-01", with(remote("h", DefaultPort), func(u *URI) { u.ClientID = "my-app-01" })},
		{"quack:h?client_id=abcd", with(remote("h", DefaultPort), func(u *URI) { u.ClientID = "abcd" })},
		{"quack:h?client_id=", with(remote("h", DefaultPort), func(u *URI) { u.ClientID = "" })},

		// inline_rows / fetch_ahead.
		{"quack:h?inline_rows=0", with(remote("h", DefaultPort), func(u *URI) { u.InlineRows = u64(0) })},
		{"quack:h?inline_rows=24576", with(remote("h", DefaultPort), func(u *URI) { u.InlineRows = u64(24576) })},
		{"quack:h?inline_rows=18446744073709551614", with(remote("h", DefaultPort), func(u *URI) { u.InlineRows = u64(1<<64 - 2) })},
		{"quack:h?fetch_ahead=0", with(remote("h", DefaultPort), func(u *URI) { u.FetchAhead = 0 })},
		{"quack:h?fetch_ahead=8", with(remote("h", DefaultPort), func(u *URI) { u.FetchAhead = 8 })},
		{"quack:h?fetch_ahead=64", with(remote("h", DefaultPort), func(u *URI) { u.FetchAhead = MaxFetchAhead })},
		{"quack:h?max_buffered_rows=0", with(remote("h", DefaultPort), func(u *URI) { u.MaxBufferedRows = 0 })},
		{"quack:h?max_buffered_rows=5000", with(remote("h", DefaultPort), func(u *URI) { u.MaxBufferedRows = 5000 })},
		{"quack:h?timezone=America%2FNew_York", with(remote("h", DefaultPort), func(u *URI) { u.TimeZone = "America/New_York" })},

		// Everything at once.
		{"quack://db.example.com:443?token=s3cret&ssl_fingerprint=" + pin64 + "&timeout=1m&connect_timeout=3s&heartbeat_timeout=30&client_id=svc-a&inline_rows=100&fetch_ahead=4&max_buffered_rows=10",
			URI{
				Host: "db.example.com", Port: 443, SSL: true, SSLFingerprint: pin64, Token: "s3cret",
				Timeout: time.Minute, ConnectTimeout: 3 * time.Second, HeartbeatTimeout: 30 * time.Second,
				ClientID: "svc-a", InlineRows: u64(100), FetchAhead: 4, MaxBufferedRows: 10,
			}},

		// "local" with a port is an ordinary remote host named "local".
		{"quack:local:9494", remote("local", 9494)},
	}
	for _, c := range cases {
		got, err := ParseURI(c.dsn)
		if err != nil {
			t.Errorf("%q: %v", c.dsn, err)
			continue
		}
		if !reflect.DeepEqual(*got, c.want) {
			t.Errorf("%q:\nwant %+v\ngot  %+v", c.dsn, c.want, *got)
		}
	}
}

// TestParseURICredentialsNotEchoed: a DSN written with credentials before
// '@' fails without the secret in the error, which is likely to be logged.
func TestParseURICredentialsNotEchoed(t *testing.T) {
	for _, dsn := range []string{
		"quack://s3cr3t-token@db.example:9494",
		"quack://s3cr3t@db.example",
		"quack://user:s3cr3t@db.example",
		"quack://user:s3cr3t@db.example:9494",
		"quack://user:s3cr3t@[::1]:9494",
		"quack://[::1]@s3cr3t",
	} {
		_, err := ParseURI(dsn)
		if err == nil || strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "?token=<token>") {
			t.Errorf("%q: %v", dsn, err)
		}
	}
}

func TestParseURIErrors(t *testing.T) {
	t.Setenv(ClientIDEnv, testClientID)
	cases := map[string]string{
		"":                                           "empty DSN",
		"   ":                                        "empty DSN",
		"localhost:9494":                             "must start with quack",
		"http://localhost":                           "must start with quack",
		"quacks://localhost":                         "must start with quack",
		"quack:":                                     "missing hostname",
		"quack://":                                   "missing hostname",
		"quack:?token=x":                             "missing hostname",
		"quack::9494":                                "missing hostname",
		"quack:host:":                                "invalid port",
		"quack:host:abc":                             "invalid port",
		"quack:host:0":                               "out of range",
		"quack:host:99999":                           "out of range",
		"quack:host:-1":                              "out of range",
		"quack:[::1":                                 "missing ']'",
		"quack:[]:9494":                              "missing IPv6 address",
		"quack:[::1]x":                               "trailing data",
		"quack:[::1]:":                               "invalid port",
		"quack:::1":                                  "brackets",
		"quack:fe80::1:9494":                         "brackets",
		"quack:user@host":                            "credentials in the host part",
		"quack://h?tls=maybe":                        "invalid tls",
		"quack://h?tls=":                             "invalid tls",
		"quack://h?disable_ssl=":                     "invalid disable_ssl",
		"quack://h?disable_ssl=x":                    "invalid disable_ssl",
		"quack://h?tls=true&disable_ssl=true":        "conflicts",
		"quack://h?tls=false&disable_ssl=false":      "conflicts",
		"quack://h?timeout=oops":                     "invalid timeout",
		"quack://h?timeout=":                         "invalid timeout",
		"quack://h?timeout=-1s":                      "negative",
		"quack://h?timeout=99999999999999999999":     "invalid timeout",
		"quack://h?connect_timeout=x":                "invalid connect_timeout",
		"quack://h?connect_timeout=-5ms":             "negative",
		"quack://h?heartbeat_timeout=0":              "at least 1s",
		"quack://h?heartbeat_timeout=500ms":          "at least 1s",
		"quack://h?heartbeat_timeout=1500ms":         "whole number of seconds",
		"quack://h?heartbeat_timeout=-1":             "invalid heartbeat_timeout",
		"quack://h?heartbeat_timeout=x":              "invalid heartbeat_timeout",
		"quack://h?client_id=abc":                    "at least 4 characters",
		"quack://h?client_id=a":                      "at least 4 characters",
		"quack://h?inline_rows=-1":                   "invalid inline_rows",
		"quack://h?inline_rows=x":                    "invalid inline_rows",
		"quack://h?inline_rows=":                     "invalid inline_rows",
		"quack://h?inline_rows=18446744073709551615": "invalid inline_rows",
		"quack://h?inline_rows=18446744073709551616": "invalid inline_rows",
		"quack://h?fetch_ahead=-1":                   "invalid fetch_ahead",
		"quack://h?fetch_ahead=65":                   "invalid fetch_ahead",
		"quack://h?fetch_ahead=one":                  "invalid fetch_ahead",
		"quack://h?fetch_ahead=":                     "invalid fetch_ahead",
		"quack://h?max_buffered_rows=-1":             "invalid max_buffered_rows",
		"quack://h?max_buffered_rows=":               "invalid max_buffered_rows",
		"quack://h?max_buffered_rows=many":           "invalid max_buffered_rows",
		"quack://h?timezone=":                        "invalid timezone",
		"quack://h?timezone=a%00b":                   "invalid timezone",
		"quack://h?ssl_fingerprint=":                 "64 hex digits",
		"quack:example.com?ssl_fingerprint=":         "64 hex digits",
		"quack://h?ssl_fingerprint=abc":              "64 hex digits",
		"quack://h?ssl_fingerprint=" + pin64 + "00":  "64 hex digits",
		"quack://h?ssl_fingerprint=" + strings.Replace(pin64, "0", "g", 1): "hex digits",
		"quack://h?ssl_fingerprint=" + pin64 + "&tls=false":                "cannot be combined",
		"quack://h?ssl_fingerprint=" + pin64 + "&disable_ssl=true":         "cannot be combined",
		"quack:localhost?ssl_fingerprint=" + pin64 + "&tls=0":              "cannot be combined",
		"quack://h?tokn=x":          "unknown DSN parameter",
		"quack://h?token=a&token=b": "more than once",
		"quack://h?token=a&TOKEN=b": "more than once",
		"quack://h?token=%zz":       "invalid query",
		// The removed local mode.
		"quack:local":                       "local mode",
		"quack://local/tmp/test.db?token=x": "local mode",
		"quack:local/:memory:":              "local mode",
	}
	for dsn, want := range cases {
		u, err := ParseURI(dsn)
		if err == nil {
			t.Errorf("%q: expected error, got %+v", dsn, *u)
			continue
		}
		if !strings.HasPrefix(err.Error(), "transport: ") || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q does not contain %q", dsn, err, want)
		}
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	colon := "01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef"
	ok := []string{
		pin64,
		strings.ToLower(pin64),
		colon,
		strings.ToUpper(colon),
		"sha256:" + pin64,
		"SHA256:" + colon,
		"Sha256:" + strings.ToLower(pin64),
		"  " + colon + "\t\n",
		strings.ReplaceAll(colon, ":", " "),
		"sha256: " + colon,
	}
	for _, in := range ok {
		got, err := NormalizeFingerprint(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != pin64 {
			t.Errorf("%q: got %q", in, got)
		}
	}
	bad := []string{
		"",
		"   ",
		"sha256:",
		pin64[:63],
		pin64 + "0",
		"sha1:" + pin64,
		strings.Replace(pin64, "F", "G", 1),
		strings.Replace(colon, ":", "-", 1),
		"0x" + pin64[2:],
	}
	for _, in := range bad {
		if got, err := NormalizeFingerprint(in); err == nil {
			t.Errorf("%q: expected error, got %q", in, got)
		}
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestDefaultClientID(t *testing.T) {
	t.Setenv(ClientIDEnv, "")
	if got := DefaultClientID(); got != "" {
		t.Fatalf("empty env must opt out, got %q", got)
	}
	u, err := ParseURI("quack:h")
	if err != nil || u.ClientID != "" {
		t.Fatalf("empty env: %v %+v", err, u)
	}

	t.Setenv(ClientIDEnv, "from-env")
	u, err = ParseURI("quack:h")
	if err != nil || u.ClientID != "from-env" {
		t.Fatalf("env: %v %+v", err, u)
	}
	// An explicit parameter wins over the environment.
	u, err = ParseURI("quack:h?client_id=explicit")
	if err != nil || u.ClientID != "explicit" {
		t.Fatalf("explicit: %v %+v", err, u)
	}

	// A too-short environment value is rejected like an explicit one.
	t.Setenv(ClientIDEnv, "abc")
	if _, err := ParseURI("quack:h"); err == nil || !strings.Contains(err.Error(), ClientIDEnv) {
		t.Fatalf("short env id: %v", err)
	}
	if _, err := ParseURI("quack:h?client_id=override"); err != nil {
		t.Fatalf("explicit id must not consult a bad env value: %v", err)
	}

	// Unset: a random UUID, stable for the whole process.
	if err := os.Unsetenv(ClientIDEnv); err != nil {
		t.Fatal(err)
	}
	a := DefaultClientID()
	if !uuidRE.MatchString(a) {
		t.Fatalf("not a v4 UUID: %q", a)
	}
	u1, err := ParseURI("quack:h")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := ParseURI("quack://db.example:9494")
	if err != nil {
		t.Fatal(err)
	}
	if u1.ClientID != a || u2.ClientID != a || DefaultClientID() != a {
		t.Fatalf("process client id not stable: %q %q %q", a, u1.ClientID, u2.ClientID)
	}
	if b := newRandomUUID(); b == a || !uuidRE.MatchString(b) {
		t.Fatalf("newRandomUUID: %q (process id %q)", b, a)
	}
}

func TestHeartbeatTimeoutSeconds(t *testing.T) {
	cases := map[time.Duration]uint64{
		0:                      1,
		time.Second:            1,
		60 * time.Second:       60,
		90*time.Second + 1:     90,
		time.Duration(1 << 62): uint64(time.Duration(1<<62) / time.Second),
	}
	for d, want := range cases {
		u := URI{HeartbeatTimeout: d}
		if got := u.HeartbeatTimeoutSeconds(); got != want {
			t.Errorf("%v: got %d, want %d", d, got, want)
		}
	}
	if MaxHeartbeatTimeoutSeconds != 9223372036854775 {
		t.Errorf("MaxHeartbeatTimeoutSeconds = %d", MaxHeartbeatTimeoutSeconds)
	}
}

func TestHTTPURL(t *testing.T) {
	t.Setenv(ClientIDEnv, testClientID)
	cases := map[string]string{
		"quack://example.com:9494":           "https://example.com:9494/quack",
		"quack://example.com:9494?tls=false": "http://example.com:9494/quack",
		"quack:localhost?tls=true":           "https://localhost:9494/quack",
		"quack:[::1]:9494":                   "http://[::1]:9494/quack",
		"quack:[fe80::1%eth0]:1":             "https://[fe80::1%25eth0]:1/quack",
	}
	for dsn, want := range cases {
		u, err := ParseURI(dsn)
		if err != nil {
			t.Fatalf("%q: %v", dsn, err)
		}
		if got := u.HTTPURL(); got != want {
			t.Errorf("%q: HTTPURL %q, want %q", dsn, got, want)
		}
	}
}
