package quackdriver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/value"
)

const (
	// appendChunkRows is the most rows one DataChunk may carry. The server's
	// scan_data_from_quack_client hands received chunks to the INSERT as
	// they are, so they must not exceed STANDARD_VECTOR_SIZE.
	appendChunkRows = 2048
	// DefaultAppendBatchBytes is the default target size of one SEND_DATA
	// request (the official client's quack_target_batch_bytes).
	DefaultAppendBatchBytes = 32 << 20
	// MaxAppendRequestBytes bounds every encoded SEND_DATA request. The
	// server (cpp-httplib) rejects request bodies above 100 MiB.
	MaxAppendRequestBytes = 96 << 20
)

var (
	// errAppenderClosed is returned by calls on an Appender after Close.
	errAppenderClosed = errors.New("quackdriver: Appender is closed")
	// errAppenderAborted is returned by calls on an Appender after Abort.
	errAppenderAborted = errors.New("quackdriver: Appender was aborted")
	// errAppendConnLost is returned when the connection became unusable
	// while an Appender was open.
	errAppendConnLost = errors.New("quackdriver: the connection is no longer usable")
)

// AppenderOption configures NewAppender.
type AppenderOption func(*appenderConfig)

type appenderConfig struct {
	batchBytes int
	ordered    bool
}

func defaultAppenderConfig() appenderConfig {
	return appenderConfig{batchBytes: DefaultAppendBatchBytes, ordered: true}
}

// WithBatchBytes sets the target size of one SEND_DATA request (default
// DefaultAppendBatchBytes, 32 MiB): rows are buffered until about n bytes
// are pending, then sent in one request. n <= 0 selects the default; values
// above MaxAppendRequestBytes are reduced to it. Smaller batches use less
// client memory, larger ones fewer round trips.
func WithBatchBytes(n int) AppenderOption {
	return func(c *appenderConfig) {
		if n <= 0 {
			n = DefaultAppendBatchBytes
		}
		c.batchBytes = n
	}
}

// WithUnordered lets the server insert the rows in any order (ordered :=
// false), which allows it to insert in parallel. By default rows keep the
// order in which they were appended.
func WithUnordered() AppenderOption {
	return func(c *appenderConfig) { c.ordered = false }
}

// Appender streams rows into a table as one INSERT statement. It is the
// Quack v3 bulk-load path: the server runs
//
//	INSERT INTO table (columns) SELECT c0::type0, ... FROM scan_data_from_quack_client(...)
//
// and the Appender sends the rows as DataChunks of VARCHAR columns
// (SEND_DATA requests of up to 2048-row chunks, about 32 MiB per request)
// holding each value's text, which the server casts (see AppendColumn);
// values are never rendered as SQL. Close ends the stream and returns the
// number of rows the server inserted.
//
// The Appender holds its connection exclusively until Close or Abort:
// meanwhile other statements on the connection fail with ErrConnBusy. That
// is the driver's Conn: inside sql.Conn.Raw, database/sql holds the
// connection's lock, so a statement run through the same sql.Conn there
// blocks instead of failing. With database/sql, create and finish the
// Appender inside sql.Conn.Raw and run other statements on another
// connection:
//
//	err := sc.Raw(func(dc any) error {
//		a, err := dc.(*quackdriver.Conn).NewAppender(ctx, "", "events", cols)
//		if err != nil {
//			return err
//		}
//		defer a.Abort() // no-op after a successful Close
//		for _, e := range events {
//			if err := a.AppendRow(e.ID, e.Name, e.At); err != nil {
//				return err
//			}
//		}
//		_, err = a.Close()
//		return err
//	})
//
// The statement is atomic: if the stream fails or is aborted, no row is
// inserted (inside an explicit transaction the failure aborts the
// transaction, as any failed statement does).
//
// An Appender is safe for use by one goroutine at a time; its methods
// serialize. To interrupt a call blocked on the network, cancel the context
// given to NewAppender: the call then cancels the INSERT (waiting at most
// two seconds for the server) and returns the context's error.
type Appender struct {
	conn     *Conn
	ctx      context.Context
	q        *query
	streamID string
	sql      string
	columns  []AppendColumn
	types    []logicaltype.Type // the columns' types, as the server resolved them
	// chunkTypes are the types of the chunks sent: VARCHAR for every column.
	chunkTypes []logicaltype.Type

	batchBytes int
	maxRequest int

	mu sync.Mutex
	// the chunk being filled, one column of values each
	cur      []message.Values
	curRows  int
	curBytes int
	curFirst int64
	// sealed chunks waiting to be sent
	pending      []pendingChunk
	pendingBytes int

	calls   int64  // AppendRow calls so far, skipped rows included
	rows    int64  // rows accepted so far
	batches uint64 // SEND_DATA batches the server accepted

	done  bool
	err   error // why the Appender ended (nil after a successful Close)
	count int64
}

type pendingChunk struct {
	chunk message.DataChunk
	first int64 // row number of the chunk's first row
	bytes int   // the chunk's share of Appender.curBytes: its encoded size, roughly
}

// appendError gives an Appender failure its context while keeping the cause
// reachable through errors.Is / errors.As (*Error, context.Canceled, ...).
type appendError struct {
	what string
	err  error
}

func (e *appendError) Error() string {
	return "quackdriver: Appender: " + e.what + ": " + strings.TrimPrefix(e.err.Error(), "quackdriver: ")
}

func (e *appendError) Unwrap() error { return e.err }

// NewAppender starts an INSERT into table (in schema, when non-empty) that
// reads its rows from a client data stream, and returns the Appender that
// feeds it. columns describe the values each row holds (see AppendColumn);
// the server resolves their types first, in a statement of its own.
//
// ctx governs the whole stream: every request the Appender sends, including
// those of AppendRow, Flush and Close, uses it, and its cancellation aborts
// the stream. A NewAppender failure leaves the connection free.
func (c *Conn) NewAppender(ctx context.Context, schema, table string, columns []AppendColumn, opts ...AppenderOption) (*Appender, error) {
	if c.closed.Load() {
		return nil, errConnClosed
	}
	if err := checkAppendTarget(table, columns); err != nil {
		return nil, err
	}
	cfg := defaultAppenderConfig()
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.acquireExclusive(ctx); err != nil {
		return nil, err
	}
	types, err := c.appendTypes(ctx, columns)
	if err != nil {
		c.releaseExclusive()
		return nil, err
	}
	streamID, err := newStreamID()
	if err != nil {
		c.releaseExclusive()
		return nil, err
	}
	sql := appendSQL(schema, table, columns, types, streamID, cfg.ordered)
	// inline_rows = 0: the statement waits for the stream and has no row to
	// answer PREPARE with. The scan registers the stream while it binds, so
	// the server answers PREPARE once the stream accepts data.
	q, err := c.prepare(ctx, sql, message.Idx(0), true)
	if err != nil {
		c.releaseExclusive()
		return nil, err
	}
	if !q.prep.NeedsMoreFetch {
		q.cancel()
		c.releaseExclusive()
		return nil, errors.New("quackdriver: Appender: the server finished the INSERT before receiving any data")
	}
	a := &Appender{
		conn:       c,
		ctx:        ctx,
		q:          q,
		streamID:   streamID,
		sql:        sql,
		columns:    columns,
		types:      types,
		chunkTypes: make([]logicaltype.Type, len(types)),
		batchBytes: min(cfg.batchBytes, MaxAppendRequestBytes),
		maxRequest: MaxAppendRequestBytes,
		cur:        make([]message.Values, len(types)),
	}
	for i := range a.chunkTypes {
		a.chunkTypes[i] = logicaltype.Type{ID: logicaltype.IDVarchar}
	}
	return a, nil
}

// appendSQL builds the INSERT that reads the client stream. The scan's
// prototype has a VARCHAR member per column, matching the chunks the
// Appender sends (a type mismatch would raise an INTERNAL error on the
// server), and the SELECT casts each to the column's type, rendered with
// logicaltype.Type.SQL.
func appendSQL(schema, table string, columns []AppendColumn, types []logicaltype.Type, streamID string, ordered bool) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	if schema != "" {
		b.WriteString(logicaltype.QuoteIdentifier(schema))
		b.WriteByte('.')
	}
	b.WriteString(logicaltype.QuoteIdentifier(table))
	named := true
	for _, c := range columns {
		if c.Name == "" {
			named = false
			break
		}
	}
	if named {
		b.WriteString(" (")
		for i, c := range columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(logicaltype.QuoteIdentifier(c.Name))
		}
		b.WriteByte(')')
	}
	b.WriteString(" SELECT ")
	for i, t := range types {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(logicaltype.QuoteIdentifier("c" + strconv.Itoa(i)))
		b.WriteString("::")
		b.WriteString(t.SQL())
	}
	b.WriteString(" FROM scan_data_from_quack_client(")
	b.WriteString(logicaltype.QuoteString(streamID))
	b.WriteString(", NULL::STRUCT(")
	for i := range types {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(logicaltype.QuoteIdentifier("c" + strconv.Itoa(i)))
		b.WriteString(" VARCHAR")
	}
	b.WriteString("), ordered := ")
	b.WriteString(strconv.FormatBool(ordered))
	b.WriteByte(')')
	return b.String()
}

// newStreamID returns a random UUID v4 string, as the official client names
// its streams. It only needs to be unique within the session.
func newStreamID() (string, error) {
	u, err := value.RandomUUID()
	if err != nil {
		return "", fmt.Errorf("quackdriver: Appender: stream id: %w", err)
	}
	return value.FormatUUID(u), nil
}

// AppendRow adds one row, one value per column in the order the columns
// were given (nil is SQL NULL; see AppendColumn for the accepted Go values).
// Rows are buffered and sent in batches, so AppendRow may send a request
// and report a server error for earlier rows.
//
// A value that cannot be converted is reported as a *ValueError naming its
// row (the number of earlier AppendRow calls) and column, and the row is
// skipped; the Appender stays usable. Any other error ends the stream: the
// INSERT is cancelled and every later call returns the error. Row ranges in
// errors about sent batches count the accepted rows only.
func (a *Appender) AppendRow(values ...any) error {
	return a.appendRow("quackdriver: Appender: ", values)
}

// appendRow is AppendRow with prefix starting ValueError messages.
func (a *Appender) appendRow(prefix string, values []any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done {
		return a.endedErr()
	}
	call := a.calls
	a.calls++
	if len(values) != len(a.types) {
		return newValueError(prefix, call, -1, "", fmt.Errorf("has %d values, want %d", len(values), len(a.types)))
	}
	row := make([]any, len(values))
	for i, v := range values {
		text, err := appendText(a.types[i], v)
		if err != nil {
			return newValueError(prefix, call, i, a.columnName(i), err)
		}
		row[i] = text
	}
	return a.add(row)
}

func (a *Appender) columnName(i int) string {
	if a.columns[i].Name == "" {
		return strconv.Itoa(i)
	}
	return strconv.Quote(a.columns[i].Name)
}

// add buffers a row of texts (nil for NULL) and sends what is due (a.mu
// held).
func (a *Appender) add(row []any) error {
	if a.curRows == 0 {
		a.curFirst = a.rows
	}
	for i, v := range row {
		a.cur[i] = append(a.cur[i], v)
		a.curBytes++ // the encoded size, roughly; the request limit is checked on the encoded bytes
		if s, ok := v.(string); ok {
			a.curBytes += len(s) + 4
		}
	}
	a.curRows++
	a.rows++
	if a.curRows >= appendChunkRows || a.curBytes >= a.batchBytes {
		a.sealChunk()
	}
	if a.pendingBytes >= a.batchBytes {
		return a.sendPending()
	}
	return nil
}

// sealChunk moves the chunk being filled to the pending batch (a.mu held).
func (a *Appender) sealChunk() {
	if a.curRows == 0 {
		return
	}
	cols := make([]message.Column, len(a.cur))
	for i := range a.cur {
		cols[i] = a.cur[i]
		a.cur[i] = nil
	}
	a.pending = append(a.pending, pendingChunk{
		chunk: message.DataChunk{RowCount: a.curRows, Types: a.chunkTypes, Columns: cols},
		first: a.curFirst,
		bytes: a.curBytes,
	})
	a.pendingBytes += a.curBytes
	a.curRows, a.curBytes = 0, 0
}

// sendPending sends the pending chunks as one batch, split further when the
// encoded request would exceed the limit (a.mu held).
func (a *Appender) sendPending() error {
	chunks := a.pending
	a.pending, a.pendingBytes = nil, 0
	if len(chunks) == 0 {
		return nil
	}
	return a.sendChunks(chunks)
}

func (a *Appender) header() message.Header {
	return message.Header{ConnectionID: a.conn.connectionID, HasQueryID: true, ClientQueryID: a.q.id}
}

// sendChunks sends chunks as the next batch. A request that encodes larger
// than the limit is split: into halves of its chunks, then into halves of a
// single chunk's rows. A single row above the limit is an error.
func (a *Appender) sendChunks(chunks []pendingChunk) error {
	if err := a.checkConn(); err != nil {
		return err
	}
	data := make([]message.DataChunk, len(chunks))
	size := 0
	for i, c := range chunks {
		data[i] = c.chunk
		size += c.bytes
	}
	index := a.batches + 1
	// The request is encoded into a buffer sized from the estimate, which
	// counts a NULL as one byte where the wire spends four on its length
	// entry: an eighth more covers NULL-heavy columns, so the buffer does
	// not grow (and copy) once more at the end.
	payload, err := message.EncodeSize(a.header(), message.SendDataRequest{
		StreamID:   a.streamID,
		Chunks:     data,
		BatchIndex: message.Idx(index),
	}, size+size/8+1024)
	if err != nil {
		return a.fail(&appendError{what: "encode " + rowRange(chunks), err: err}, true)
	}
	if len(payload) > a.maxRequest {
		if len(chunks) > 1 {
			mid := len(chunks) / 2
			if err := a.sendChunks(chunks[:mid]); err != nil {
				return err
			}
			return a.sendChunks(chunks[mid:])
		}
		if c := chunks[0]; c.chunk.RowCount > 1 {
			left, right := splitChunk(c)
			if err := a.sendChunks([]pendingChunk{left}); err != nil {
				return err
			}
			return a.sendChunks([]pendingChunk{right})
		}
		return a.fail(&appendError{
			what: "row " + strconv.FormatInt(chunks[0].first, 10),
			err: fmt.Errorf("the row encodes to a %d-byte request, above the %d-byte limit",
				len(payload), a.maxRequest),
		}, true)
	}
	body, err := a.post(payload, "SEND_DATA batch "+strconv.FormatUint(index, 10)+" ("+rowRange(chunks)+")")
	if err != nil {
		return err
	}
	if _, ok := body.(message.SendDataResponse); !ok {
		return a.fail(&appendError{
			what: "SEND_DATA batch " + strconv.FormatUint(index, 10),
			err:  fmt.Errorf("unexpected %s in response", body.MessageType()),
		}, true)
	}
	a.batches = index
	return nil
}

// rowRange describes the rows of chunks for error messages.
func rowRange(chunks []pendingChunk) string {
	first := chunks[0].first
	last := chunks[len(chunks)-1].first + int64(chunks[len(chunks)-1].chunk.RowCount) - 1
	if first == last {
		return "row " + strconv.FormatInt(first, 10)
	}
	return "rows " + strconv.FormatInt(first, 10) + "-" + strconv.FormatInt(last, 10)
}

// splitChunk cuts a pending chunk into two halves by rows.
func splitChunk(c pendingChunk) (pendingChunk, pendingChunk) {
	mid := c.chunk.RowCount / 2
	left := make([]message.Column, len(c.chunk.Columns))
	right := make([]message.Column, len(c.chunk.Columns))
	for i, col := range c.chunk.Columns {
		vals := message.Materialize(col)
		left[i] = vals[:mid:mid]
		right[i] = vals[mid:]
	}
	return pendingChunk{
		chunk: message.DataChunk{RowCount: mid, Types: c.chunk.Types, Columns: left},
		first: c.first,
		bytes: c.bytes / 2,
	}, pendingChunk{
		chunk: message.DataChunk{RowCount: c.chunk.RowCount - mid, Types: c.chunk.Types, Columns: right},
		first: c.first + int64(mid),
		bytes: c.bytes - c.bytes/2,
	}
}

// checkConn fails the Appender when its connection was closed or lost.
func (a *Appender) checkConn() error {
	switch {
	case a.conn.closed.Load():
		return a.fail(&appendError{what: "send rows", err: errConnClosed}, false)
	case a.conn.bad.Load():
		return a.fail(&appendError{what: "send rows", err: errAppendConnLost}, false)
	}
	return nil
}

// post sends an encoded request of the stream. On failure the stream ends:
// the INSERT is cancelled (CANCEL_REQUEST) and the error returned with what
// as context, or the context error when ctx ended.
//
// The cancel is waited for (at most cancelTimeout) even when ctx ended: the
// server checks a CANCEL_REQUEST's query id and only then interrupts whatever
// runs on the session, so a cancel still in flight when the freed connection
// starts its next statement would interrupt that statement instead. A cancel
// still unanswered after cancelTimeout stays tracked: the connection's next
// statement waits for it.
func (a *Appender) post(payload []byte, what string) (message.Body, error) {
	body, err := a.conn.post(a.ctx, payload, message.TypeSendDataRequest)
	if err != nil {
		if interrupted(a.ctx, err) {
			return nil, a.fail(&appendError{what: what, err: a.ctx.Err()}, true)
		}
		return nil, a.fail(&appendError{what: what, err: a.conn.classify(err, false)}, true)
	}
	return body, nil
}

// fail ends the stream with err, cancelling the INSERT first when cancel is
// set (a.mu held).
func (a *Appender) fail(err error, cancel bool) error {
	if cancel {
		a.q.cancel()
	}
	a.finish(err)
	return err
}

// finish ends the Appender and frees its connection (a.mu held).
func (a *Appender) finish(err error) {
	a.done = true
	a.err = err
	a.cur, a.pending = nil, nil
	a.curRows, a.curBytes, a.pendingBytes = 0, 0, 0
	a.conn.releaseExclusive()
}

// endedErr is what calls on an ended Appender return.
func (a *Appender) endedErr() error {
	if a.err != nil {
		return a.err
	}
	return errAppenderClosed
}

// Flush sends the buffered rows now instead of waiting for a full batch.
// The stream stays open.
func (a *Appender) Flush() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done {
		return a.endedErr()
	}
	a.sealChunk()
	return a.sendPending()
}

// Close sends the remaining rows, ends the stream and waits for the INSERT
// to finish. It returns the number of rows the server inserted, and frees
// the connection. When the stream fails, the INSERT is cancelled and nothing
// is inserted. Calling Close again returns the same result.
func (a *Appender) Close() (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done {
		if a.err != nil {
			return 0, a.err
		}
		return a.count, nil
	}
	a.sealChunk()
	if err := a.sendPending(); err != nil {
		return 0, err
	}
	if err := a.checkConn(); err != nil {
		return 0, err
	}
	// The terminal message carries the batch count, so the server can tell
	// a complete stream from one that lost a batch.
	payload, err := message.Encode(a.header(), message.SendDataRequest{
		StreamID:     a.streamID,
		TotalBatches: message.Idx(a.batches),
	})
	if err != nil {
		return 0, a.fail(&appendError{what: "encode the end of the stream", err: err}, true)
	}
	what := "end of the stream (" + strconv.FormatUint(a.batches, 10) + " batches, " +
		strconv.FormatInt(a.rows, 10) + " rows)"
	body, err := a.post(payload, what)
	if err != nil {
		return 0, err
	}
	if _, ok := body.(message.SendDataResponse); !ok {
		return 0, a.fail(&appendError{what: what, err: fmt.Errorf("unexpected %s in response", body.MessageType())}, true)
	}
	count, err := a.readCount()
	if err != nil {
		return 0, err
	}
	a.count = count
	a.finish(nil)
	return count, nil
}

// readCount reads the INSERT's result, one BIGINT row holding the number of
// rows inserted, through the normal FETCH loop (a.mu held). A statement
// failure (a constraint violation, say) is reported here.
func (a *Appender) readCount() (int64, error) {
	var (
		count int64
		found bool
	)
	consider := func(chunks []message.DataChunk) error {
		for _, ch := range chunks {
			if found || ch.RowCount == 0 || len(ch.Columns) == 0 || ch.Columns[0] == nil || ch.Columns[0].Len() == 0 {
				continue
			}
			n, ok := ch.Columns[0].Value(0).(int64)
			if !ok {
				return fmt.Errorf("the INSERT returned %T instead of a BIGINT row count", ch.Columns[0].Value(0))
			}
			count, found = n, true
		}
		return nil
	}
	if err := consider(a.q.prep.Results); err != nil {
		return 0, a.fail(&appendError{what: "INSERT", err: err}, true)
	}
	f := a.q.newFetcher(0)
	defer f.stop(nil)
	for {
		chunks, end, err := f.next(a.ctx)
		if end {
			break
		}
		if err == nil {
			err = consider(chunks)
		} else if interrupted(a.ctx, err) {
			err = a.ctx.Err() // the cancel is waited for, like in post
		}
		if err != nil {
			return 0, a.fail(&appendError{what: "INSERT", err: err}, true)
		}
	}
	if !found {
		// The statement finished; there is nothing left to cancel.
		return 0, a.fail(&appendError{what: "INSERT", err: errors.New("the INSERT finished without a row count")}, false)
	}
	return count, nil
}

// Abort cancels the INSERT (CANCEL_REQUEST): nothing is inserted and the
// connection is freed. It is a no-op once the Appender has ended, so it can
// be deferred right after NewAppender.
func (a *Appender) Abort() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done {
		return nil
	}
	_ = a.fail(errAppenderAborted, true)
	return nil
}
