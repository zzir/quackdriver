package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/transport"
)

// Fixed client identity written into the handshake fixture. They are
// constants (not runtime.GOOS etc.) so that a re-capture produces the same
// request bytes on every machine.
const (
	clientDuckdbVersion = "v2.0.0"
	clientPlatform      = "linux_amd64"
	clientID            = "quackdriver-golden"
	heartbeatSeconds    = 60
)

// queryUUID returns the fixed query UUID of the n-th statement of a capture:
// a version-4 UUID 51c0ffee-0000-4000-8000-<n as 12 hex digits>, laid out
// the way message.NewQueryUUID lays out random ones.
func queryUUID(n uint64) codec.HugeInt {
	return codec.HugeInt{Upper: 0x51c0ffee_0000_4000, Lower: 0x8000_0000_0000_0000 | n}
}

// capturer drives one Quack session and writes every exchange as
// <name>.req.bin / <name>.resp.bin.
type capturer struct {
	tr      *transport.Transport
	token   string
	outDir  string
	verbose bool

	connID        string
	queryID       uint64 // last client_query_id used
	uuids         uint64 // last fixed query UUID handed out
	count         int
	serverVersion string
	written       map[string]bool
}

// nextUUID hands out the next fixed query UUID.
func (c *capturer) nextUUID() codec.HugeInt {
	c.uuids++
	return queryUUID(c.uuids)
}

func (c *capturer) write(name string, data []byte) error {
	if c.written[name] {
		return fmt.Errorf("fixture %s written twice", name)
	}
	c.written[name] = true
	path := filepath.Join(c.outDir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	c.count++
	return nil
}

// header builds the request header. withQueryID assigns the next sequential
// client_query_id; without it the field carries the unset optional_idx.
func (c *capturer) header(withQueryID bool) message.Header {
	h := message.Header{ConnectionID: c.connID}
	if withQueryID {
		c.queryID++
		h.HasQueryID, h.ClientQueryID = true, c.queryID
	}
	return h
}

// exchange sends body with header h, records both directions under name
// (unless name is empty) and returns the decoded response.
func (c *capturer) exchange(ctx context.Context, name string, h message.Header, body message.Body) (message.Header, message.Body, error) {
	req, err := message.Encode(h, body)
	if err != nil {
		return message.Header{}, nil, fmt.Errorf("%s: encode: %w", name, err)
	}
	resp, err := c.tr.Request(ctx, req)
	if err != nil {
		return message.Header{}, nil, fmt.Errorf("%s: %w", name, err)
	}
	// A response with VARIANT columns is captured undecoded (rb is nil).
	rh, rb, err := message.Decode(resp)
	if err != nil && !errors.Is(err, message.ErrVariant) {
		return rh, rb, fmt.Errorf("%s: decode response: %w", name, err)
	}
	if name != "" {
		if err := c.write(name+".req.bin", req); err != nil {
			return rh, rb, err
		}
		if err := c.write(name+".resp.bin", resp); err != nil {
			return rh, rb, err
		}
	}
	if c.verbose {
		label := name
		if label == "" {
			label = "(not captured)"
		}
		fmt.Printf("== %s: %s -> %s (%d/%d bytes)\n%s", label, body.MessageType(), rh.Type, len(req), len(resp), describe(rb))
	}
	return rh, rb, nil
}

// expect asserts the response type and returns it.
func expect[T message.Body](name string, b message.Body) (T, error) {
	v, ok := b.(T)
	if !ok {
		var zero T
		if e, isErr := b.(message.ErrorResponse); isErr {
			return zero, fmt.Errorf("%s: server error (%s): %s", name, e.ExceptionType, e.Message)
		}
		return zero, fmt.Errorf("%s: got %T, want %T", name, b, zero)
	}
	return v, nil
}

// do runs one exchange that must answer with a T.
func do[T message.Body](ctx context.Context, c *capturer, name string, withQueryID bool, body message.Body) (T, error) {
	_, b, err := c.exchange(ctx, name, c.header(withQueryID), body)
	if err != nil {
		var zero T
		return zero, err
	}
	return expect[T](name, b)
}

// describe renders a decoded response for -v.
func describe(b message.Body) string {
	var sb strings.Builder
	switch m := b.(type) {
	case nil:
		sb.WriteString("   (VARIANT columns, not decoded)\n")
	case message.PrepareResponse:
		fmt.Fprintf(&sb, "   names=%q needs_more_fetch=%v chunks=%d\n", m.ResultNames, m.NeedsMoreFetch, len(m.Results))
		for i, t := range m.ResultTypes {
			fmt.Fprintf(&sb, "   type[%d]=%s\n", i, t.SQL())
		}
		describeChunks(&sb, m.Results)
	case message.FetchResponse:
		fmt.Fprintf(&sb, "   batch_index=%v total_batches=%v chunks=%d\n", m.BatchIndex, m.TotalBatches, len(m.Results))
		describeChunks(&sb, m.Results)
	default:
		fmt.Fprintf(&sb, "   %+v\n", m)
	}
	return sb.String()
}

func describeChunks(sb *strings.Builder, chunks []message.DataChunk) {
	const maxRows = 6
	for i, ch := range chunks {
		kinds := make([]string, len(ch.Columns))
		for k, col := range ch.Columns {
			kinds[k] = fmt.Sprintf("%T", col)
		}
		fmt.Fprintf(sb, "   chunk %d: %d rows, columns %v\n", i, ch.RowCount, kinds)
		for r := 0; r < ch.RowCount && r < maxRows; r++ {
			row := make([]string, len(ch.Columns))
			for k, col := range ch.Columns {
				row[k] = fmt.Sprintf("%#v", col.Value(r))
			}
			fmt.Fprintf(sb, "     %s\n", strings.Join(row, " | "))
		}
	}
}
