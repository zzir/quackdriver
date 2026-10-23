// Package transport implements the Quack HTTP/1.1 transport and the DSN
// (URI) parser.
package transport

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzir/quackdriver/internal/value"
)

// DefaultPort is the Quack server default TCP port.
const DefaultPort = 9494

// Endpoint is the HTTP path used by every Quack request.
const Endpoint = "/quack"

// MimeType is the request/response content type of Quack messages. It is
// what the official client sends and what the server answers with.
const MimeType = "application/vnd.duckdb"

// Defaults for the optional DSN parameters.
const (
	// DefaultConnectTimeout bounds the TCP dial and the TLS handshake.
	DefaultConnectTimeout = 10 * time.Second
	// DefaultHeartbeatTimeout is the heartbeat lease requested in the
	// handshake (official setting quack_default_heartbeat_timeout).
	DefaultHeartbeatTimeout = 60 * time.Second
	// DefaultFetchAhead is the FETCH read-ahead depth.
	DefaultFetchAhead = 1
	// DefaultMaxBufferedRows bounds the rows of an earlier result the driver
	// buffers in memory when a new statement starts on the connection.
	DefaultMaxBufferedRows = 1_000_000
	// MaxFetchAhead caps fetch_ahead so a typo cannot spawn an unbounded
	// number of in-flight FETCH requests.
	MaxFetchAhead = 64
	// MinClientIDLength is the minimum length of a non-empty client_id
	// (QuackClient::ValidateClientId).
	MinClientIDLength = 4
	// MaxHeartbeatTimeoutSeconds is the largest lease the server accepts
	// (MAX_HEARTBEAT_TIMEOUT_SECONDS = INT64_MAX / 1000). It exceeds what a
	// time.Duration can hold, so a granted lease must be range-checked
	// before conversion.
	MaxHeartbeatTimeoutSeconds = uint64(math.MaxInt64 / 1000)
)

// ClientIDEnv is the environment variable that supplies the default
// client_id, exactly like the official extension. When it is set (even to
// the empty string, which opts out of sending a client_id) it wins over the
// random per-process id.
const ClientIDEnv = "QUACK_CLIENT_ID"

// URI is the parsed form of a Quack DSN.
type URI struct {
	Host string
	Port int
	IPv6 bool // Host is an IPv6 literal (written in brackets in the DSN)

	// SSL selects HTTPS. Default: plain HTTP for local hosts (see
	// IsLocalHost), HTTPS for every other host.
	SSL bool
	// SSLFingerprint is the pinned SHA-256 fingerprint of the server's DER
	// certificate as 64 uppercase hex digits (see NormalizeFingerprint).
	// Empty means regular CA and hostname verification. A pin implies SSL.
	SSLFingerprint string

	Token string

	// Timeout bounds every HTTP request as a whole; 0 means no limit (the
	// caller's context governs).
	Timeout time.Duration
	// ConnectTimeout bounds the TCP dial and the TLS handshake; 0 means no
	// limit.
	ConnectTimeout time.Duration
	// HeartbeatTimeout is the heartbeat lease requested in the handshake.
	// Always a positive whole number of seconds.
	HeartbeatTimeout time.Duration
	// ClientID is sent in the handshake; empty means none. It is either
	// empty or at least MinClientIDLength bytes long.
	ClientID string
	// InlineRows, when non-nil, is sent as PREPARE inline_rows (the number
	// of rows the server may inline into the PREPARE response). nil leaves
	// the server default.
	InlineRows *uint64
	// FetchAhead is the FETCH read-ahead depth; 0 disables prefetching.
	FetchAhead int
	// MaxBufferedRows bounds the rows of an open result that are buffered
	// in memory when a new statement starts on the connection; 0 means no
	// bound.
	MaxBufferedRows int
	// TimeZone, when non-empty, is set as the session's TimeZone right
	// after the handshake (SET SESSION TimeZone = ...).
	TimeZone string
}

// ParseURI parses a Quack DSN. Accepted forms:
//
//	quack:host
//	quack:host:port
//	quack://host:port
//	quack:[::1]:9494
//	quack://host:port?token=xxx&timeout=30s
//
// Query parameters (names are case-insensitive, each may appear once,
// unknown names are rejected):
//   - token=<auth>: authentication token
//   - tls=true|false: HTTPS on/off
//   - disable_ssl=true|false: the official inverse of tls
//   - ssl_fingerprint=<sha256>: pin the server certificate (implies HTTPS)
//   - timeout=<duration>: overall per-request timeout (default none)
//   - connect_timeout=<duration>: dial + TLS handshake timeout (default 10s)
//   - heartbeat_timeout=<duration>: heartbeat lease (default 60s, >= 1s,
//     whole seconds)
//   - client_id=<id>: client id sent in the handshake (empty or >= 4 chars;
//     default $QUACK_CLIENT_ID, else a random per-process UUID)
//   - inline_rows=<n>: PREPARE inline_rows (default: server setting)
//   - fetch_ahead=<n>: FETCH read-ahead depth (default 1, 0 disables)
//   - max_buffered_rows=<n>: the most rows of an open result buffered in
//     memory when a new statement starts on the connection (default
//     1000000, 0 = unlimited)
//   - timezone=<name>: the session's TimeZone, set after the handshake
//     (default: the server's setting)
//
// Durations use time.ParseDuration syntax; a bare non-negative integer is
// taken as seconds (the unit of the official options). Parameter values are
// URL-decoded, so a literal '+', '&' or '%' must be percent-encoded.
func ParseURI(dsn string) (*URI, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("transport: empty DSN")
	}

	// Strip the scheme prefix without using a real URL parser because the
	// official C++ parser is permissive about non-RFC-3986 forms.
	rest := dsn
	switch {
	case strings.HasPrefix(rest, "quack://"):
		rest = rest[len("quack://"):]
	case strings.HasPrefix(rest, "quack:"):
		rest = rest[len("quack:"):]
	default:
		return nil, fmt.Errorf("transport: DSN must start with quack: or quack://")
	}

	// Split off the query string before consuming the host/port.
	var rawQuery string
	if idx := strings.Index(rest, "?"); idx >= 0 {
		rawQuery = rest[idx+1:]
		rest = rest[:idx]
	}
	if rest == "" {
		return nil, fmt.Errorf("transport: missing hostname")
	}

	out := &URI{
		Port:             DefaultPort,
		ConnectTimeout:   DefaultConnectTimeout,
		HeartbeatTimeout: DefaultHeartbeatTimeout,
		FetchAhead:       DefaultFetchAhead,
		MaxBufferedRows:  DefaultMaxBufferedRows,
	}

	// quack:local[/path] was the driver's former local mode, which started
	// its own DuckDB CLI; refuse it rather than look up a host "local".
	if rest == "local" || strings.HasPrefix(rest, "local/") {
		return nil, fmt.Errorf("transport: local mode (quack:local) is no longer supported; run a server with quack_serve and connect to it")
	}
	if err := parseHostPort(out, rest); err != nil {
		return nil, err
	}

	if err := parseQuery(out, rawQuery); err != nil {
		return nil, err
	}
	return out, nil
}

// parseHostPort fills Host, Port and IPv6 from "host", "host:port",
// "[v6]" or "[v6]:port". A trailing path is ignored.
func parseHostPort(out *URI, rest string) error {
	// Quack DSNs carry no path, but tolerate a trailing slash or path.
	if idx := strings.Index(rest, "/"); idx >= 0 {
		rest = rest[:idx]
	}
	if strings.Contains(rest, "@") {
		// user:password@host or token@host: never echo the part before '@'
		// (it is likely a secret, and errors get logged).
		return fmt.Errorf("transport: credentials in the host part of the DSN are not supported (pass the token as ?token=<token>)")
	}
	portStr := ""
	hasPort := false
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return fmt.Errorf("transport: invalid IPv6 host, missing ']'")
		}
		out.IPv6 = true
		out.Host = rest[1:end]
		if out.Host == "" {
			return fmt.Errorf("transport: missing IPv6 address")
		}
		if strings.ContainsAny(out.Host, "[]") {
			return fmt.Errorf("transport: invalid IPv6 host %q", out.Host)
		}
		tail := rest[end+1:]
		switch {
		case tail == "":
		case strings.HasPrefix(tail, ":"):
			portStr, hasPort = tail[1:], true
		default:
			return fmt.Errorf("transport: unexpected trailing data %q after IPv6 host", tail)
		}
	} else {
		host := rest
		if idx := strings.LastIndex(rest, ":"); idx >= 0 {
			host, portStr, hasPort = rest[:idx], rest[idx+1:], true
		}
		if strings.Contains(host, ":") {
			return fmt.Errorf("transport: IPv6 host must be written in brackets, e.g. quack:[::1]:%d", DefaultPort)
		}
		if strings.ContainsAny(host, "[]") {
			return fmt.Errorf("transport: invalid host %q", host)
		}
		out.Host = host
	}
	if out.Host == "" {
		return fmt.Errorf("transport: missing hostname")
	}
	if hasPort {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return fmt.Errorf("transport: invalid port %q", portStr)
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("transport: port %d out of range", port)
		}
		out.Port = port
	}
	return nil
}

// knownParams lists the DSN parameters (see ParseURI).
var knownParams = map[string]bool{
	"token": true, "tls": true, "disable_ssl": true, "ssl_fingerprint": true,
	"timeout": true, "connect_timeout": true, "heartbeat_timeout": true,
	"client_id": true, "inline_rows": true, "fetch_ahead": true, "max_buffered_rows": true, "timezone": true,
}

func parseQuery(out *URI, rawQuery string) error {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return fmt.Errorf("transport: invalid query string: %w", err)
	}
	params := make(map[string]string, len(q))
	for key, values := range q {
		name := strings.ToLower(key)
		if !knownParams[name] {
			return fmt.Errorf("transport: unknown DSN parameter %q", key)
		}
		if _, dup := params[name]; dup || len(values) != 1 {
			return fmt.Errorf("transport: DSN parameter %q given more than once", name)
		}
		params[name] = values[0]
	}

	out.Token = params["token"]

	// TLS selection: explicit tls / disable_ssl (which must agree), else by
	// host locality, mirroring the official ATTACH (no ssl on local hosts by
	// default).
	explicitSSL, hasExplicitSSL, err := parseSSLParams(params)
	if err != nil {
		return err
	}
	if hasExplicitSSL {
		out.SSL = explicitSSL
	} else {
		out.SSL = !IsLocalHost(out.Host)
	}
	// An empty value is an error like every other malformed value: a pin
	// that silently falls back to CA verification (or plain HTTP) would hide
	// a misconfiguration.
	if v, ok := params["ssl_fingerprint"]; ok {
		fp, err := NormalizeFingerprint(v)
		if err != nil {
			return err
		}
		if hasExplicitSSL && !explicitSSL {
			return fmt.Errorf("transport: ssl_fingerprint pins the server's TLS certificate and cannot be combined with tls=false/disable_ssl=true")
		}
		out.SSLFingerprint = fp
		out.SSL = true
	}

	if v, ok := params["timeout"]; ok {
		if out.Timeout, err = parseDuration("timeout", v); err != nil {
			return err
		}
	}
	if v, ok := params["connect_timeout"]; ok {
		if out.ConnectTimeout, err = parseDuration("connect_timeout", v); err != nil {
			return err
		}
	}
	if v, ok := params["heartbeat_timeout"]; ok {
		d, err := parseDuration("heartbeat_timeout", v)
		if err != nil {
			return err
		}
		if d < time.Second {
			return fmt.Errorf("transport: heartbeat_timeout=%q must be at least 1s", v)
		}
		if d%time.Second != 0 {
			return fmt.Errorf("transport: heartbeat_timeout=%q must be a whole number of seconds", v)
		}
		out.HeartbeatTimeout = d
	}

	if v, ok := params["client_id"]; ok {
		out.ClientID = v
	} else {
		out.ClientID = DefaultClientID()
	}
	if err := ValidateClientID(out.ClientID); err != nil {
		if _, ok := params["client_id"]; !ok {
			return fmt.Errorf("transport: $%s: %w", ClientIDEnv, err)
		}
		return err
	}

	if v, ok := params["inline_rows"]; ok {
		n, err := strconv.ParseUint(v, 10, 64)
		// MaxUint64 is the optional_idx "unset" sentinel on the wire.
		if err != nil || n == math.MaxUint64 {
			return fmt.Errorf("transport: invalid inline_rows=%q (want a non-negative integer)", v)
		}
		out.InlineRows = &n
	}
	if v, ok := params["fetch_ahead"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > MaxFetchAhead {
			return fmt.Errorf("transport: invalid fetch_ahead=%q (want an integer between 0 and %d)", v, MaxFetchAhead)
		}
		out.FetchAhead = n
	}
	if v, ok := params["max_buffered_rows"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("transport: invalid max_buffered_rows=%q (want a non-negative integer, 0 = unlimited)", v)
		}
		out.MaxBufferedRows = n
	}
	if v, ok := params["timezone"]; ok {
		if v == "" || strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("transport: invalid timezone=%q (want a time zone name such as UTC)", v)
		}
		out.TimeZone = v
	}
	return nil
}

// parseSSLParams resolves tls= and disable_ssl=. ok reports whether either
// was given; conflicting values are an error.
func parseSSLParams(params map[string]string) (ssl, ok bool, err error) {
	tlsVal, hasTLS := params["tls"]
	disVal, hasDis := params["disable_ssl"]
	if hasTLS {
		if ssl, err = strconv.ParseBool(tlsVal); err != nil {
			return false, false, fmt.Errorf("transport: invalid tls=%q", tlsVal)
		}
	}
	if hasDis {
		disable, err := strconv.ParseBool(disVal)
		if err != nil {
			return false, false, fmt.Errorf("transport: invalid disable_ssl=%q", disVal)
		}
		if hasTLS && ssl == disable {
			return false, false, fmt.Errorf("transport: tls=%s conflicts with disable_ssl=%s", tlsVal, disVal)
		}
		ssl = !disable
	}
	return ssl, hasTLS || hasDis, nil
}

// parseDuration parses a non-negative duration parameter. A bare integer is
// a number of seconds.
func parseDuration(name, v string) (time.Duration, error) {
	if v != "" && strings.Trim(v, "0123456789") == "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n > int64(math.MaxInt64/time.Second) {
			return 0, fmt.Errorf("transport: invalid %s=%q", name, v)
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("transport: invalid %s=%q", name, v)
	}
	if d < 0 {
		return 0, fmt.Errorf("transport: %s=%q must not be negative", name, v)
	}
	return d, nil
}

// IsLocalHost reports whether host is one of the hosts the official client
// treats as local (QuackUri::IsLocal): "localhost" (any case), "127.0.0.1"
// or "::1". Local hosts default to plain HTTP, all others to HTTPS.
func IsLocalHost(host string) bool {
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
}

// NormalizeFingerprint returns the canonical form of a SHA-256 certificate
// fingerprint: 64 uppercase hex digits without separators. Like
// QuackUri::NormalizeFingerprint it trims surrounding whitespace, accepts an
// optional case-insensitive "sha256:" prefix, ignores ':' and ' ' separators
// (the form `openssl x509 -fingerprint -sha256` prints) and accepts hex
// digits in any case.
func NormalizeFingerprint(fingerprint string) (string, error) {
	s := strings.TrimSpace(fingerprint)
	if len(s) >= len("sha256:") && strings.EqualFold(s[:len("sha256:")], "sha256:") {
		s = s[len("sha256:"):]
	}
	var b strings.Builder
	b.Grow(64)
	for i := range len(s) {
		c := s[i]
		switch {
		case c == ':' || c == ' ':
			continue
		case c >= '0' && c <= '9', c >= 'A' && c <= 'F':
			b.WriteByte(c)
		case c >= 'a' && c <= 'f':
			b.WriteByte(c - 'a' + 'A')
		default:
			return "", fmt.Errorf("transport: invalid ssl_fingerprint %q: expected the SHA-256 fingerprint of the server certificate as hex digits, optionally separated by colons", fingerprint)
		}
	}
	if b.Len() != 64 {
		return "", fmt.Errorf("transport: invalid ssl_fingerprint %q: a SHA-256 fingerprint has 64 hex digits, got %d", fingerprint, b.Len())
	}
	return b.String(), nil
}

// ValidateClientID mirrors QuackClient::ValidateClientId: an empty id means
// "no client_id"; a non-empty one must be at least MinClientIDLength bytes.
func ValidateClientID(id string) error {
	if id != "" && len(id) < MinClientIDLength {
		return fmt.Errorf("transport: client_id must be at least %d characters long", MinClientIDLength)
	}
	return nil
}

// processClientID is the random client_id shared by every connection of this
// process that has no explicit or environment-provided id.
var processClientID = sync.OnceValue(newRandomUUID)

// DefaultClientID returns the client_id used when the DSN has none: the
// value of $QUACK_CLIENT_ID when that variable is set (possibly empty, which
// opts out), otherwise a random UUID generated once per process. The
// environment is consulted on every call.
func DefaultClientID() string {
	if v, ok := os.LookupEnv(ClientIDEnv); ok {
		return v
	}
	return processClientID()
}

// newRandomUUID returns a random (version 4) UUID in canonical lowercase
// form, or "" (no client_id) if the system random source fails.
func newRandomUUID() string {
	u, err := value.RandomUUID()
	if err != nil {
		return ""
	}
	return value.FormatUUID(u)
}

// HeartbeatTimeoutSeconds returns the heartbeat lease in whole seconds as
// sent in the CONNECTION_REQUEST (at least 1).
func (u *URI) HeartbeatTimeoutSeconds() uint64 {
	s := u.HeartbeatTimeout / time.Second
	if s < 1 {
		return 1
	}
	return uint64(s)
}

// HTTPURL returns the http(s)://host:port/quack URL used by the HTTP client.
func (u *URI) HTTPURL() string {
	scheme := "http"
	if u.SSL {
		scheme = "https"
	}
	host := u.Host
	if u.IPv6 {
		// An IPv6 zone ("fe80::1%eth0") must be percent-encoded in a URL.
		host = "[" + strings.ReplaceAll(host, "%", "%25") + "]"
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, host, u.Port, Endpoint)
}
