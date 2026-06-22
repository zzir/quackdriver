// Package transport implements the Quack HTTP/2 transport and URI parser.
package transport

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is the Quack server default TCP port.
const DefaultPort = 9494

// Endpoint is the HTTP path used by every Quack request.
const Endpoint = "/quack"

// MimeType is the request/response content type negotiated with the server.
const MimeType = "application/duckdb"

// URI is the parsed form of a Quack DSN.
type URI struct {
	Host    string
	Port    int
	IPv6    bool
	SSL     bool
	Token   string
	Timeout time.Duration

	IsLocal bool   // auto-manage a local DuckDB process
	DBPath  string // database file path; empty or ":memory:" for in-memory
}

// ParseURI parses a Quack DSN. Accepted forms:
//
//	quack:host
//	quack:host:port
//	quack://host:port
//	quack:[::1]:9494
//	quack://host:port?token=xxx&tls=true&timeout=30s
//
// Query parameters:
//   - token=<auth>: authentication token
//   - tls=true|false: enable HTTPS (default false)
//   - timeout=<duration>: per-request HTTP timeout
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
	if rest == "" {
		return nil, fmt.Errorf("transport: missing hostname")
	}

	// Split off the query string before consuming the host/port.
	var rawQuery string
	if idx := strings.Index(rest, "?"); idx >= 0 {
		rawQuery = rest[idx+1:]
		rest = rest[:idx]
	}

	// Detect local mode: "local", "local/path", "local/:memory:" etc.
	isLocal := rest == "local" || strings.HasPrefix(rest, "local/")
	if isLocal {
		dbPath := ""
		if strings.HasPrefix(rest, "local/") {
			dbPath = rest[len("local"):]
		}
		out := &URI{
			Host:    "127.0.0.1",
			Port:    0,
			IsLocal: true,
			DBPath:  dbPath,
			Timeout: 30 * time.Second,
		}
		if err := parseQuery(out, rawQuery); err != nil {
			return nil, err
		}
		if out.SSL {
			return nil, fmt.Errorf("transport: tls is not supported for local mode")
		}
		return out, nil
	}

	// Drop any path component (Quack DSNs don't use them, but some users may
	// add a trailing slash).
	if idx := strings.Index(rest, "/"); idx >= 0 {
		rest = rest[:idx]
	}

	out := &URI{Port: DefaultPort, Timeout: 30 * time.Second}

	// Detect IPv6 in [::1]:port form.
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return nil, fmt.Errorf("transport: missing ']' in IPv6 host")
		}
		out.IPv6 = true
		out.Host = rest[1:end]
		if out.Host == "" {
			return nil, fmt.Errorf("transport: missing IPv6 address")
		}
		rest = rest[end+1:]
		if strings.HasPrefix(rest, ":") {
			port, err := strconv.Atoi(rest[1:])
			if err != nil {
				return nil, fmt.Errorf("transport: invalid port %q", rest[1:])
			}
			out.Port = port
		} else if rest != "" {
			return nil, fmt.Errorf("transport: unexpected trailing data %q after IPv6 host", rest)
		}
	} else if idx := strings.LastIndex(rest, ":"); idx >= 0 {
		out.Host = rest[:idx]
		port, err := strconv.Atoi(rest[idx+1:])
		if err != nil {
			return nil, fmt.Errorf("transport: invalid port %q", rest[idx+1:])
		}
		out.Port = port
	} else {
		out.Host = rest
	}
	if out.Host == "" {
		return nil, fmt.Errorf("transport: missing host")
	}
	if out.Port < 1 || out.Port > 65535 {
		return nil, fmt.Errorf("transport: port %d out of range", out.Port)
	}

	if err := parseQuery(out, rawQuery); err != nil {
		return nil, err
	}
	return out, nil
}

func parseQuery(out *URI, rawQuery string) error {
	if rawQuery == "" {
		return nil
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return fmt.Errorf("transport: invalid query string: %w", err)
	}
	if v := q.Get("token"); v != "" {
		out.Token = v
	}
	if v := q.Get("tls"); v != "" {
		ssl, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("transport: invalid tls=%q", v)
		}
		out.SSL = ssl
	}
	if v := q.Get("timeout"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("transport: invalid timeout=%q", v)
		}
		out.Timeout = d
	}
	return nil
}

// HTTPURL returns the http(s)://host:port URL used by the HTTP client.
func (u *URI) HTTPURL() string {
	scheme := "http"
	if u.SSL {
		scheme = "https"
	}
	host := u.Host
	if u.IPv6 {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, host, u.Port, Endpoint)
}
