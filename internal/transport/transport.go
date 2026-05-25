package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Transport sends Quack request bodies over HTTP/2 and returns the binary
// response body. It is concurrency-safe.
type Transport struct {
	URI    *URI
	Client *http.Client
	url    string

	// Trace, if non-nil, is invoked with the request and response bodies of
	// every Request call. It runs synchronously and must not retain the byte
	// slices — copy them first if you need to. Used by cmd/dump-wire to
	// capture golden fixtures.
	Trace func(sent, received []byte)
}

// New returns a Transport configured for the given URI.
func New(u *URI) *Transport {
	client := &http.Client{
		Timeout: u.Timeout,
	}
	return &Transport{
		URI:    u,
		Client: client,
		url:    u.HTTPURL(),
	}
}

// Request POSTs the given body and returns the response bytes. On non-2xx
// responses, the body is included in the returned error.
func (t *Transport) Request(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("transport: build request: %w", err)
	}
	req.Header.Set("Content-Type", MimeType)
	req.Header.Set("Accept", MimeType)

	resp, err := t.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transport: request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("transport: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview := respBody
		if len(preview) > 256 {
			preview = preview[:256]
		}
		return nil, fmt.Errorf("transport: HTTP %d: %q", resp.StatusCode, preview)
	}
	if t.Trace != nil {
		t.Trace(body, respBody)
	}
	return respBody, nil
}

// SetTimeout updates the underlying client's request timeout.
func (t *Transport) SetTimeout(d time.Duration) { t.Client.Timeout = d }
