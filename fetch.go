package quackdriver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzir/quackdriver/internal/message"
)

const (
	// fetchRetries is how often a FETCH is retried after a transport error.
	// A retry is safe: until the client acknowledges a batch (ack_index),
	// the server keeps it and serves the same index again.
	fetchRetries = 2
	// fetchRetryDelay is the pause before the first retry (doubled after).
	fetchRetryDelay = 50 * time.Millisecond
	// fetchEndGrace bounds how long the end of a stream waits for read-ahead
	// requests issued past it before cancelling them. The server answers
	// those at once; cancelling an in-flight request would close its pooled
	// connection.
	fetchEndGrace = time.Second
)

// errRowsClosed ends a FETCH stream stopped by Rows.Close.
var errRowsClosed = errors.New("quackdriver: rows are closed")

// fetcher reads the FETCH batches of one statement in order.
//
// Batch indices run 1, 2, 3, ... after the batches PREPARE returned inline.
// Each request acknowledges the highest index up to which every batch has
// arrived (never the index it asks for, which the server would release).
// The stream ends with a response without chunks; when that response
// carries total_batches, the count is checked.
//
// With depth > 0, up to depth requests are kept in flight while the caller
// consumes a batch (read-ahead). Responses are delivered in index order.
//
// next must be called from one goroutine at a time; stop may be called from
// any goroutine.
type fetcher struct {
	q     *query
	depth int

	ctx    context.Context // requests; cancelled by stop
	cancel context.CancelFunc

	mu      sync.Mutex
	stopped bool
	stopErr error
	wg      sync.WaitGroup

	// Consumer state.
	nextIndex uint64
	pending   []*fetchSlot
	done      bool
	err       error

	terminal atomic.Bool // a terminal response arrived: request no more

	ackMu    sync.Mutex
	received map[uint64]bool // arrived above ack
	ack      uint64          // every batch <= ack has arrived
}

type fetchSlot struct {
	index uint64
	ch    chan fetchResult // buffered: the request goroutine never blocks
}

type fetchResult struct {
	resp message.FetchResponse
	err  error
}

func (q *query) newFetcher(depth int) *fetcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &fetcher{
		q:         q,
		depth:     depth,
		ctx:       ctx,
		cancel:    cancel,
		nextIndex: 1,
		received:  make(map[uint64]bool),
	}
}

// topUp issues requests until n are outstanding.
func (f *fetcher) topUp(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return
	}
	for len(f.pending) < n && !f.terminal.Load() {
		slot := &fetchSlot{index: f.nextIndex, ch: make(chan fetchResult, 1)}
		f.nextIndex++
		f.pending = append(f.pending, slot)
		ack := f.currentAck()
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			slot.ch <- f.fetch(slot.index, ack)
		}()
	}
}

func (f *fetcher) currentAck() uint64 {
	f.ackMu.Lock()
	defer f.ackMu.Unlock()
	return f.ack
}

func (f *fetcher) markReceived(index uint64) {
	f.ackMu.Lock()
	defer f.ackMu.Unlock()
	f.received[index] = true
	for f.received[f.ack+1] {
		delete(f.received, f.ack+1)
		f.ack++
	}
}

// fetch requests one batch, retrying transport failures. The encoded request
// (index and ack) is the same on every attempt.
func (f *fetcher) fetch(index, ack uint64) fetchResult {
	c := f.q.conn
	h := message.Header{ConnectionID: c.connectionID, HasQueryID: true, ClientQueryID: f.q.id}
	payload, err := message.Encode(h, message.FetchRequest{QueryUUID: f.q.uuid, BatchIndex: index, AckIndex: ack})
	if err != nil {
		return fetchResult{err: fmt.Errorf("quackdriver: encode FETCH_REQUEST: %w", err)}
	}
	var body message.Body
	for attempt := 0; ; attempt++ {
		body, err = c.post(f.ctx, payload, message.TypeFetchRequest)
		if err == nil {
			break
		}
		if f.ctx.Err() != nil {
			return fetchResult{err: f.ctx.Err()}
		}
		var te *transportError
		if !errors.As(err, &te) || attempt >= fetchRetries {
			return fetchResult{err: c.classify(err, false)}
		}
		t := time.NewTimer(fetchRetryDelay << attempt)
		select {
		case <-f.ctx.Done():
			t.Stop()
			return fetchResult{err: f.ctx.Err()}
		case <-t.C:
		}
	}
	resp, ok := body.(message.FetchResponse)
	if !ok {
		return fetchResult{err: fmt.Errorf("quackdriver: unexpected %s in response to FETCH_REQUEST", body.MessageType())}
	}
	if len(resp.Results) == 0 {
		f.terminal.Store(true)
		return fetchResult{resp: resp}
	}
	if !resp.BatchIndex.Valid {
		return fetchResult{err: fmt.Errorf("quackdriver: FETCH_RESPONSE with data is missing its batch index")}
	}
	if resp.BatchIndex.Value != index {
		return fetchResult{err: fmt.Errorf("quackdriver: FETCH_RESPONSE carries batch %d but batch %d was requested",
			resp.BatchIndex.Value, index)}
	}
	f.markReceived(index)
	return fetchResult{resp: resp}
}

// next returns the chunks of the next batch, end = true at the end of the
// stream, or the error that ended it. (End of stream is not signalled with
// io.EOF: transport errors may wrap io.EOF.) When ctx ends, the stream is
// stopped and ctx.Err() returned; the caller cancels the statement.
func (f *fetcher) next(ctx context.Context) (chunks []message.DataChunk, end bool, err error) {
	if f.err != nil {
		return nil, false, f.err
	}
	if f.done {
		return nil, true, nil
	}
	f.topUp(1)
	if len(f.pending) == 0 {
		err := f.stopReason()
		if err == nil {
			err = errRowsClosed
		}
		f.fail(err)
		return nil, false, err
	}
	slot := f.pending[0]
	var r fetchResult
	select {
	case r = <-slot.ch:
	case <-ctx.Done():
		err := ctx.Err()
		f.fail(err)
		return nil, false, err
	}
	f.pending[0] = nil
	f.pending = f.pending[1:]
	if r.err != nil {
		if f.ctx.Err() != nil { // stopped from outside (Rows.Close, Conn.Close)
			if reason := f.stopReason(); reason != nil {
				r.err = reason
			}
		}
		f.fail(r.err)
		return nil, false, r.err
	}
	if len(r.resp.Results) == 0 {
		received := slot.index - 1
		if r.resp.TotalBatches.Valid && r.resp.TotalBatches.Value != received {
			err := fmt.Errorf("quackdriver: Quack stream ended with %d of %d batches received",
				received, r.resp.TotalBatches.Value)
			f.fail(err)
			return nil, false, err
		}
		f.done = true
		f.finish() // requests issued past the end
		f.q.acknowledge()
		return nil, true, nil
	}
	// Keep the read-ahead going while the caller works on this batch.
	f.topUp(f.depth)
	return r.resp.Results, false, nil
}

func (f *fetcher) fail(err error) {
	f.err = err
	f.done = true
	f.stop(err)
}

// markStopped makes topUp issue no more requests and records reason (may
// be nil) as what a later next reports, unless the stream already stopped.
func (f *fetcher) markStopped(reason error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.stopped {
		f.stopped = true
		f.stopErr = reason
	}
}

func (f *fetcher) stopReason() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopErr
}

// finish ends a stream that reached its end; requests already issued past
// the end are left to complete (see drain).
func (f *fetcher) finish() {
	f.markStopped(nil)
	f.drain()
}

// abandon ends a stream the caller stops reading early (Rows.Close). Unless
// the server already ended the stream, the statement is cancelled there,
// which answers the outstanding requests at once; they are then left to
// complete (see drain).
func (f *fetcher) abandon(reason error) {
	f.markStopped(reason)
	if !f.terminal.Load() {
		f.q.cancel()
	}
	f.drain()
}

// drain waits for the outstanding requests for at most fetchEndGrace, then
// cancels the rest. Cancelling an in-flight HTTP request closes its pooled
// connection, which would cost a new dial (and TLS handshake) per request
// for every statement read with read-ahead; the server answers requests
// past the end of a stream, or of a cancelled statement, at once.
func (f *fetcher) drain() {
	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()
	t := time.NewTimer(fetchEndGrace)
	select {
	case <-done:
	case <-t.C:
	}
	t.Stop()
	f.cancel()
	<-done
}

// stop cancels outstanding requests and waits for their goroutines. reason
// (may be nil) is what a later next reports. Safe to call repeatedly and
// from any goroutine.
func (f *fetcher) stop(reason error) {
	f.markStopped(reason)
	f.cancel()
	f.wg.Wait()
}
