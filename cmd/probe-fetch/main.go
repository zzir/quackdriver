// probe-fetch drives the Quack v3 FETCH protocol by hand against a running
// server and reports the round trips a large result takes: how many rows
// PREPARE inlined, every FETCH request (batch_index / ack_index, rows,
// bytes, latency), the terminal response's total_batches, and how many
// requests the read-ahead window kept in flight.
//
// The read-ahead depth comes from the DSN's fetch_ahead parameter (or
// -fetch-ahead): with depth k the probe keeps up to k+1 FETCH requests in
// flight, like the driver does; 0 fetches one batch at a time. A small
// -batch-bytes sets quack_target_batch_bytes for the session so that a
// moderate result is split into many batches.
//
//	scripts/with-server.sh go run ./cmd/probe-fetch -rows 1000000 -dsn 'quack://127.0.0.1:9494?token=super_secret&fetch_ahead=4'
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/transport"
)

func main() {
	dsn := flag.String("dsn", "quack://127.0.0.1:9494?token=super_secret", "Quack DSN (fetch_ahead and inline_rows are honoured)")
	rows := flag.Int64("rows", 1_000_000, "rows to select")
	width := flag.Int("width", 64, "approximate payload bytes per row")
	batchBytes := flag.Uint64("batch-bytes", 1<<20, "quack_target_batch_bytes for the session (0 keeps the server setting)")
	fetchAhead := flag.Int("fetch-ahead", -1, "read-ahead depth; -1 uses the DSN's fetch_ahead")
	verbose := flag.Bool("v", false, "print every FETCH round trip")
	flag.Parse()

	uri, err := transport.ParseURI(*dsn)
	if err != nil {
		log.Fatalf("parse dsn: %v", err)
	}
	if *fetchAhead >= 0 {
		uri.FetchAhead = *fetchAhead
	}
	tr, err := transport.New(uri, transport.Options{})
	if err != nil {
		log.Fatalf("transport: %v", err)
	}
	defer tr.CloseIdleConnections()

	p := &prober{tr: tr, uri: uri, verbose: *verbose}
	rep, err := p.run(context.Background(), *rows, *width, *batchBytes)
	if err != nil {
		log.Fatalf("probe-fetch: %v", err)
	}
	fmt.Print(rep.String())
	if err := rep.check(*rows); err != nil {
		log.Fatalf("probe-fetch: %v", err)
	}
}

// prober runs one Quack session.
type prober struct {
	tr      *transport.Transport
	uri     *transport.URI
	verbose bool

	connID  string
	queryID atomic.Uint64
}

// roundTrip sends one message on the session and decodes the response. An
// ERROR_RESPONSE is returned as an error.
func (p *prober) roundTrip(ctx context.Context, body message.Body) (message.Body, int, error) {
	h := message.Header{ConnectionID: p.connID, HasQueryID: true, ClientQueryID: p.queryID.Add(1)}
	req, err := message.Encode(h, body)
	if err != nil {
		return nil, 0, err
	}
	resp, err := p.tr.Request(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	rh, b, err := message.Decode(resp)
	if err != nil {
		return nil, len(resp), err
	}
	if p.connID == "" {
		p.connID = rh.ConnectionID
	}
	if e, ok := b.(message.ErrorResponse); ok {
		return nil, len(resp), fmt.Errorf("server error (%s): %s", e.ExceptionType, e.Message)
	}
	return b, len(resp), nil
}

// fetchTrip records one FETCH round trip.
type fetchTrip struct {
	batch, ack uint64
	rows       int
	chunks     int
	bytes      int
	latency    time.Duration
	terminal   bool
	total      message.OptionalIdx
	inFlight   int // requests in flight when this one was sent (itself included)
	firstID    int64
	lastID     int64
}

// report is the outcome of a probe.
type report struct {
	server        string
	fetchAhead    int
	prepareRows   int
	prepareChunks int
	prepareBytes  int
	prepareTime   time.Duration
	trips         []fetchTrip
	rows          int64
	dataBatches   int
	totalBatches  message.OptionalIdx
	speculative   int // FETCHes for indices past the end of the stream
	maxInFlight   int
	elapsed       time.Duration
	outOfOrder    int64 // rows whose id did not follow the previous row's
}

func (p *prober) run(ctx context.Context, rows int64, width int, batchBytes uint64) (*report, error) {
	b, _, err := p.roundTrip(ctx, message.ConnectionRequest{
		AuthString:               p.uri.Token,
		ClientDuckdbVersion:      "quackdriver-probe-fetch",
		ClientPlatform:           "go",
		MinSupportedQuackVersion: message.QuackVersion,
		MaxSupportedQuackVersion: message.QuackVersion,
		ClientID:                 p.uri.ClientID,
		HeartbeatTimeoutSeconds:  p.uri.HeartbeatTimeoutSeconds(),
	})
	if err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	hs, ok := b.(message.ConnectionResponse)
	if !ok {
		return nil, fmt.Errorf("handshake: got %T", b)
	}
	rep := &report{server: hs.ServerDuckdbVersion + " " + hs.ServerPlatform, fetchAhead: p.uri.FetchAhead}
	defer func() { _, _, _ = p.roundTrip(context.WithoutCancel(ctx), message.DisconnectMessage{}) }()

	// Keep the lease alive while batches are long-polled.
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go p.heartbeats(hbCtx, time.Duration(hs.HeartbeatTimeoutSeconds)*time.Second/3)

	if batchBytes > 0 {
		if err := p.exec(ctx, "SET quack_target_batch_bytes = "+strconv.FormatUint(batchBytes, 10)); err != nil {
			return nil, err
		}
	}

	pad := max(width-8, 1)
	sql := fmt.Sprintf("SELECT i, repeat('x', %d) || i AS payload FROM range(0, %d) AS t(i)", pad, rows)
	req := message.PrepareRequest{SQL: sql, QueryUUID: message.NewQueryUUID()}
	if p.uri.InlineRows != nil {
		req.InlineRows = message.Idx(*p.uri.InlineRows)
	}
	start := time.Now()
	b, n, err := p.roundTrip(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("prepare: %w", err)
	}
	prep, ok := b.(message.PrepareResponse)
	if !ok {
		return nil, fmt.Errorf("prepare: got %T", b)
	}
	rep.prepareTime = time.Since(start)
	rep.prepareBytes = n
	rep.prepareChunks = len(prep.Results)
	next := int64(0)
	for _, ch := range prep.Results {
		rep.prepareRows += ch.RowCount
		rep.outOfOrder += checkOrder(ch, &next)
	}
	rep.rows = int64(rep.prepareRows)
	if prep.NeedsMoreFetch {
		if err := p.fetchAll(ctx, req.QueryUUID, rep, &next); err != nil {
			return rep, err
		}
	}
	rep.elapsed = time.Since(start)
	// Release the result (a no-op unless the server caches results).
	if _, _, err := p.roundTrip(ctx, message.Acknowledgement{QueryUUID: req.QueryUUID}); err != nil {
		return rep, fmt.Errorf("acknowledgement: %w", err)
	}
	return rep, nil
}

// exec runs a statement and drains its result.
func (p *prober) exec(ctx context.Context, sql string) error {
	uuid := message.NewQueryUUID()
	b, _, err := p.roundTrip(ctx, message.PrepareRequest{SQL: sql, QueryUUID: uuid})
	if err != nil {
		return fmt.Errorf("%q: %w", sql, err)
	}
	prep, ok := b.(message.PrepareResponse)
	if !ok {
		return fmt.Errorf("%q: got %T", sql, b)
	}
	for i := uint64(1); prep.NeedsMoreFetch; i++ {
		b, _, err := p.roundTrip(ctx, message.FetchRequest{QueryUUID: uuid, BatchIndex: i, AckIndex: i - 1})
		if err != nil {
			return fmt.Errorf("%q: %w", sql, err)
		}
		if f, ok := b.(message.FetchResponse); !ok || len(f.Results) == 0 {
			break
		}
	}
	return nil
}

func (p *prober) heartbeats(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _, _ = p.roundTrip(ctx, message.HeartbeatRequest{})
		}
	}
}

// fetchResult is the outcome of one in-flight FETCH.
type fetchResult struct {
	trip fetchTrip
	resp message.FetchResponse
	err  error
}

// fetchAll reads batches 1, 2, ... with up to fetchAhead+1 requests in
// flight. Each request acknowledges every batch already consumed in order
// (ack_index = highest contiguous batch), which is always below its own
// batch_index. Batches are consumed strictly in order.
func (p *prober) fetchAll(ctx context.Context, uuid codec.HugeInt, rep *report, next *int64) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	window := p.uri.FetchAhead + 1
	pending := map[uint64]chan fetchResult{}

	var sent, consumed uint64 // highest batch requested / consumed in order
	ended := false
	issue := func() {
		for !ended && sent < consumed+uint64(window) {
			sent++
			idx, ack := sent, consumed
			ch := make(chan fetchResult, 1)
			pending[idx] = ch
			inFlight := len(pending)
			rep.maxInFlight = max(rep.maxInFlight, inFlight)
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				b, n, err := p.roundTrip(ctx, message.FetchRequest{QueryUUID: uuid, BatchIndex: idx, AckIndex: ack})
				res := fetchResult{trip: fetchTrip{batch: idx, ack: ack, bytes: n, latency: time.Since(start), inFlight: inFlight}, err: err}
				if err == nil {
					f, ok := b.(message.FetchResponse)
					if !ok {
						res.err = fmt.Errorf("FETCH %d: got %T", idx, b)
					}
					res.resp = f
				}
				ch <- res
			}()
		}
	}

	issue()
	for !ended {
		res := <-pending[consumed+1]
		delete(pending, consumed+1)
		if res.err != nil {
			return fmt.Errorf("FETCH batch %d: %w", consumed+1, res.err)
		}
		f := res.resp
		t := res.trip
		t.chunks = len(f.Results)
		t.total = f.TotalBatches
		if len(f.Results) == 0 {
			t.terminal = true
			ended = true
			rep.totalBatches = f.TotalBatches
		} else {
			if !f.BatchIndex.Valid || f.BatchIndex.Value != t.batch {
				return fmt.Errorf("FETCH batch %d answered with batch_index %v", t.batch, f.BatchIndex)
			}
			t.firstID = *next
			for _, ch := range f.Results {
				t.rows += ch.RowCount
				rep.outOfOrder += checkOrder(ch, next)
			}
			t.lastID = *next - 1
			rep.rows += int64(t.rows)
			rep.dataBatches++
		}
		rep.trips = append(rep.trips, t)
		if p.verbose {
			fmt.Println(t.String())
		}
		consumed++
		issue()
	}
	// Requests issued past the end of the stream still get an answer.
	for idx, ch := range pending {
		res := <-ch
		rep.speculative++
		res.trip.chunks = len(res.resp.Results)
		res.trip.terminal = true
		res.trip.total = res.resp.TotalBatches
		if res.err != nil && !errors.Is(res.err, context.Canceled) {
			return fmt.Errorf("speculative FETCH batch %d: %w", idx, res.err)
		}
		if res.err == nil && len(res.resp.Results) != 0 {
			return fmt.Errorf("speculative FETCH batch %d past the end returned %d chunk(s)", idx, len(res.resp.Results))
		}
		rep.trips = append(rep.trips, res.trip)
	}
	sort.Slice(rep.trips, func(i, j int) bool { return rep.trips[i].batch < rep.trips[j].batch })
	return nil
}

// checkOrder verifies that the chunk's first column continues the sequence
// 0, 1, 2, ... and returns the number of rows that did not.
func checkOrder(ch message.DataChunk, next *int64) int64 {
	var bad int64
	if len(ch.Columns) == 0 {
		return int64(ch.RowCount)
	}
	for i := range ch.RowCount {
		v, ok := ch.Columns[0].Value(i).(int64)
		if !ok || v != *next {
			bad++
		}
		*next = v + 1
	}
	return bad
}

func (t fetchTrip) String() string {
	if t.terminal {
		return fmt.Sprintf("FETCH batch_index=%-4d ack_index=%-4d -> end of stream total_batches=%v (%d bytes, %v, %d in flight)",
			t.batch, t.ack, optString(t.total), t.bytes, t.latency.Round(time.Microsecond), t.inFlight)
	}
	return fmt.Sprintf("FETCH batch_index=%-4d ack_index=%-4d -> %d chunk(s) %d rows [%d..%d] (%d bytes, %v, %d in flight)",
		t.batch, t.ack, t.chunks, t.rows, t.firstID, t.lastID, t.bytes, t.latency.Round(time.Microsecond), t.inFlight)
}

func optString(v message.OptionalIdx) string {
	if !v.Valid {
		return "unset"
	}
	return strconv.FormatUint(v.Value, 10)
}

func (r *report) String() string {
	var w strings.Builder
	fmt.Fprintf(&w, "server: %s, fetch_ahead=%d (up to %d FETCH requests in flight)\n", r.server, r.fetchAhead, r.fetchAhead+1)
	fmt.Fprintf(&w, "PREPARE: %d inline rows in %d chunk(s), %d bytes, %v\n", r.prepareRows, r.prepareChunks, r.prepareBytes, r.prepareTime.Round(time.Microsecond))
	if len(r.trips) == 0 {
		fmt.Fprintln(&w, "FETCH: none, the whole result was inlined into PREPARE")
	} else {
		lat := make([]time.Duration, len(r.trips))
		bytes := 0
		for i, t := range r.trips {
			lat[i] = t.latency
			bytes += t.bytes
		}
		slices.Sort(lat)
		fmt.Fprintf(&w, "FETCH: %d round trip(s): %d data batch(es), 1 terminal, %d past the end; total_batches=%s; max %d in flight; %d bytes\n",
			len(r.trips), r.dataBatches, r.speculative, optString(r.totalBatches), r.maxInFlight, bytes)
		fmt.Fprintf(&w, "FETCH latency: min %v, median %v, max %v\n", lat[0].Round(time.Microsecond),
			lat[len(lat)/2].Round(time.Microsecond), lat[len(lat)-1].Round(time.Microsecond))
	}
	rate := float64(r.rows) / r.elapsed.Seconds()
	fmt.Fprintf(&w, "rows: %d in %v (%.0f rows/s), out of order: %d\n", r.rows, r.elapsed.Round(time.Millisecond), rate, r.outOfOrder)
	return w.String()
}

// check validates the report against the protocol rules.
func (r *report) check(wantRows int64) error {
	if r.rows != wantRows {
		return fmt.Errorf("received %d rows, want %d", r.rows, wantRows)
	}
	if r.outOfOrder != 0 {
		return fmt.Errorf("%d row(s) arrived out of order", r.outOfOrder)
	}
	if len(r.trips) == 0 {
		return nil // everything was inlined into PREPARE
	}
	if !r.totalBatches.Valid {
		return fmt.Errorf("terminal FETCH response carries no total_batches")
	}
	if r.totalBatches.Value != uint64(r.dataBatches) {
		return fmt.Errorf("total_batches=%d but %d data batch(es) were fetched", r.totalBatches.Value, r.dataBatches)
	}
	return nil
}
