package quackdriver

import (
	"crypto/tls"
	"net/http"

	"github.com/zzir/quackdriver/internal/transport"
)

// Option customizes a Connector created by NewConnector.
type Option func(*connectorOptions)

// connectorOptions collects the settings applied by Options.
type connectorOptions struct {
	transport transport.Options
}

// WithHTTPClient makes the Connector send every request through client. The
// caller then owns TLS, proxies and connection pooling: it cannot be combined
// with WithTLSConfig or with an ssl_fingerprint in the DSN (pin the
// certificate in the client's own TLS configuration instead).
//
// The client must allow several concurrent requests per connection: the
// heartbeat and FETCH read-ahead run alongside statements.
func WithHTTPClient(client *http.Client) Option {
	return func(o *connectorOptions) { o.transport.HTTPClient = client }
}

// WithTLSConfig sets the base TLS configuration (custom root CAs, client
// certificates, ...). It is cloned. TLS is used when the DSN says so (tls,
// disable_ssl, ssl_fingerprint, or a non-local host); with ssl_fingerprint
// the pin replaces CA and hostname verification.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *connectorOptions) { o.transport.TLSConfig = cfg }
}

// WithHeader adds an HTTP header sent with every request, for example the
// authentication a reverse proxy in front of the server expects. It may be
// given several times; values for the same key accumulate. Content-Type and
// Accept are always set by the driver.
func WithHeader(key, value string) Option {
	return func(o *connectorOptions) {
		if o.transport.Header == nil {
			o.transport.Header = http.Header{}
		}
		o.transport.Header.Add(key, value)
	}
}

// WithMaxIdleConnsPerHost sizes the pool of idle HTTP connections kept to
// the server (default 32). Every connection of a database/sql pool can have
// 1 + fetch_ahead requests in flight while it reads a result, plus its
// heartbeat, so a pool of n busy connections uses about (2 + fetch_ahead) x n
// HTTP connections; a smaller idle pool closes the extra ones after each
// response, costing a new dial (and TLS handshake) per request. It cannot be
// combined with WithHTTPClient.
func WithMaxIdleConnsPerHost(n int) Option {
	return func(o *connectorOptions) { o.transport.MaxIdleConnsPerHost = n }
}
