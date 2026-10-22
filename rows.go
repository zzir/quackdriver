package quackdriver

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strings"
	"time"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
)

// Rows iterates over the result of a statement: first the chunks PREPARE
// returned inline, then the FETCH batches (read ahead in the background, see
// the fetch_ahead DSN parameter).
type Rows struct {
	conn *Conn
	q    *query
	ctx  context.Context

	columnNames []string
	columnTypes []logicaltype.Type

	chunks []message.DataChunk // queue; chunks[0] is being read
	row    int                 // next row of chunks[0]

	// fetcher is nil once the stream is exhausted, buffered or failed. It
	// is only cleared under conn.mu (takeFetcher), because Conn.Close reads
	// it from another goroutine.
	fetcher *fetcher
	err     error // reported after the queued chunks
	closed  bool
}

var (
	_ driver.Rows                           = (*Rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*Rows)(nil)
	_ driver.RowsColumnTypeNullable         = (*Rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*Rows)(nil)
	_ driver.RowsColumnTypePrecisionScale   = (*Rows)(nil)
)

func newRows(ctx context.Context, q *query) *Rows {
	r := &Rows{
		conn:        q.conn,
		q:           q,
		ctx:         ctx,
		columnNames: q.prep.ResultNames,
		columnTypes: q.prep.ResultTypes,
		chunks:      q.prep.Results,
	}
	if q.prep.NeedsMoreFetch {
		r.fetcher = q.newFetcher(q.conn.uri.FetchAhead)
		r.fetcher.topUp(r.fetcher.depth) // start the read-ahead
	}
	return r
}

// Columns returns the result column names.
func (r *Rows) Columns() []string { return r.columnNames }

// Next reads the next row into dest. A malformed chunk (fewer columns or
// rows than announced) is reported as an error instead of a panic.
func (r *Rows) Next(dest []driver.Value) error {
	if r.closed {
		return io.EOF
	}
	for {
		for len(r.chunks) > 0 {
			chunk := &r.chunks[0]
			if r.row >= chunk.RowCount {
				r.chunks[0] = message.DataChunk{}
				r.chunks = r.chunks[1:]
				r.row = 0
				continue
			}
			if len(chunk.Columns) < len(dest) {
				return fmt.Errorf("quackdriver: chunk has %d columns, expected at least %d", len(chunk.Columns), len(dest))
			}
			for col := range dest {
				column := chunk.Columns[col]
				if column == nil || r.row >= column.Len() {
					n := 0
					if column != nil {
						n = column.Len()
					}
					return fmt.Errorf("quackdriver: column %d has %d rows but chunk declared %d (row index %d)",
						col, n, chunk.RowCount, r.row)
				}
				dest[col] = column.Value(r.row)
			}
			r.row++
			return nil
		}
		if r.fetcher == nil {
			if r.err != nil {
				return r.err
			}
			return io.EOF
		}
		chunks, end, err := r.fetcher.next(r.ctx)
		if end {
			r.takeFetcher()
			continue
		}
		if err != nil {
			r.takeFetcher()
			if interrupted(r.ctx, err) {
				r.q.cancelAsync()
			}
			r.err = err
			return err
		}
		r.chunks = chunks
		r.row = 0
	}
}

// takeFetcher clears r.fetcher and the connection's active Rows (when it is
// r) in one critical section, and returns the fetcher it held.
func (r *Rows) takeFetcher() *fetcher {
	c := r.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	f := r.fetcher
	r.fetcher = nil
	if c.active == r {
		c.active = nil
	}
	return f
}

// buffer reads the remaining batches into memory, because a new statement
// is about to replace this one on the server. A failure is kept and
// reported by Next after the rows read so far. With limit > 0, buffering
// stops once more than limit rows are queued: the stream is kept (the Rows
// stays readable and active) and an error for the new statement is
// returned.
func (r *Rows) buffer(ctx context.Context, limit int) error {
	f := r.takeFetcher()
	if f == nil {
		return nil
	}
	for {
		chunks, end, err := f.next(ctx)
		if end {
			return nil
		}
		if err != nil {
			if interrupted(ctx, err) {
				r.q.cancelAsync()
			}
			r.err = err
			return nil
		}
		r.chunks = append(r.chunks, chunks...)
		if limit > 0 && rowCount(r.chunks)-r.row > limit {
			r.keepFetcher(f)
			return fmt.Errorf("quackdriver: cannot start a statement while an earlier result of this connection "+
				"still has more than %d unread rows on the server (max_buffered_rows); read or close that Rows first, "+
				"or raise max_buffered_rows in the DSN", limit)
		}
	}
}

// keepFetcher reattaches f to r (and r as the connection's active Rows),
// undoing takeFetcher.
func (r *Rows) keepFetcher(f *fetcher) {
	c := r.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	r.fetcher = f
	c.active = r
}

// Close releases the result. Closing before the end stops the read-ahead
// and, for read-only statements (SELECT, FROM, VALUES, TABLE, SHOW,
// DESCRIBE, SUMMARIZE, EXPLAIN, PRAGMA, or a WITH leading to one of them),
// cancels the statement on the server unless it already finished there;
// read-ahead requests still in flight are then left to complete, which
// keeps their connections in the pool. Other statements, such as
// INSERT ... RETURNING, are drained instead so their writes complete; a
// failure while draining is returned. If the query's context has ended, the
// statement is cancelled.
func (r *Rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	f := r.takeFetcher()
	r.chunks = nil
	if f == nil {
		return nil
	}
	if r.ctx.Err() == nil && !r.q.info.readOnly {
		for {
			_, end, err := f.next(r.ctx)
			if end {
				return nil
			}
			if err != nil {
				if interrupted(r.ctx, err) {
					r.q.cancel()
					return nil
				}
				return err
			}
		}
	}
	f.abandon(errRowsClosed)
	return nil
}

// ColumnTypeDatabaseTypeName returns the DuckDB type name of a column, e.g.
// "INTEGER", "DECIMAL", "TIMESTAMP WITH TIME ZONE", or the alias for aliased
// types such as "JSON".
func (r *Rows) ColumnTypeDatabaseTypeName(index int) string {
	if index < 0 || index >= len(r.columnTypes) {
		return ""
	}
	t := r.columnTypes[index]
	if t.Info != nil && t.Info.Alias != "" {
		return strings.ToUpper(t.Info.Alias)
	}
	return t.Name()
}

// ColumnTypeNullable reports every column as nullable: Quack does not
// transfer nullability.
func (r *Rows) ColumnTypeNullable(int) (nullable, ok bool) { return true, true }

// ColumnTypePrecisionScale returns the width and scale of DECIMAL columns.
func (r *Rows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
	if index < 0 || index >= len(r.columnTypes) {
		return 0, 0, false
	}
	t := r.columnTypes[index]
	if t.ID != logicaltype.IDDecimal || t.Info == nil {
		return 0, 0, false
	}
	return int64(t.Info.Width), int64(t.Info.Scale), true
}

// ColumnTypeScanType returns the Go type of the values a column yields.
func (r *Rows) ColumnTypeScanType(index int) reflect.Type {
	if index < 0 || index >= len(r.columnTypes) {
		return reflect.TypeFor[any]()
	}
	return scanTypeFor(r.columnTypes[index])
}

// scanTypeFor maps a logical type to the Go type its decoded values have.
func scanTypeFor(t logicaltype.Type) reflect.Type {
	switch t.ID {
	case logicaltype.IDBoolean:
		return reflect.TypeFor[bool]()
	case logicaltype.IDTinyint:
		return reflect.TypeFor[int8]()
	case logicaltype.IDSmallint:
		return reflect.TypeFor[int16]()
	case logicaltype.IDInteger:
		return reflect.TypeFor[int32]()
	case logicaltype.IDBigint:
		return reflect.TypeFor[int64]()
	case logicaltype.IDUTinyint:
		return reflect.TypeFor[uint8]()
	case logicaltype.IDUSmallint:
		return reflect.TypeFor[uint16]()
	case logicaltype.IDUInteger:
		return reflect.TypeFor[uint32]()
	case logicaltype.IDUBigint:
		return reflect.TypeFor[uint64]()
	case logicaltype.IDHugeint, logicaltype.IDUHugeint, logicaltype.IDBignum:
		return reflect.TypeFor[*big.Int]()
	case logicaltype.IDFloat:
		return reflect.TypeFor[float32]()
	case logicaltype.IDDouble:
		return reflect.TypeFor[float64]()
	case logicaltype.IDDecimal, logicaltype.IDVarchar, logicaltype.IDChar, logicaltype.IDUUID,
		logicaltype.IDBit, logicaltype.IDEnum:
		return reflect.TypeFor[string]()
	case logicaltype.IDBlob, logicaltype.IDGeometry, logicaltype.IDAggregateState:
		return reflect.TypeFor[[]byte]()
	case logicaltype.IDDate, logicaltype.IDTimestamp, logicaltype.IDTimestampSec, logicaltype.IDTimestampMs,
		logicaltype.IDTimestampNs, logicaltype.IDTimestampTz, logicaltype.IDTimestampTzNs:
		return reflect.TypeFor[time.Time]()
	case logicaltype.IDTime, logicaltype.IDTimeNs:
		return reflect.TypeFor[time.Duration]()
	case logicaltype.IDTimeTz:
		return reflect.TypeFor[TimeTzValue]()
	case logicaltype.IDInterval:
		return reflect.TypeFor[IntervalValue]()
	case logicaltype.IDList, logicaltype.IDArray, logicaltype.IDMap, logicaltype.IDTuple:
		return reflect.TypeFor[[]any]()
	case logicaltype.IDStruct:
		if positionalStruct(t) {
			return reflect.TypeFor[[]any]()
		}
		return reflect.TypeFor[map[string]any]()
	case logicaltype.IDUnion:
		return reflect.TypeFor[map[string]any]()
	}
	// SQLNULL and anything unexpected.
	return reflect.TypeFor[any]()
}

// positionalStruct reports a STRUCT whose children are all unnamed; it is
// decoded positionally like a TUPLE.
func positionalStruct(t logicaltype.Type) bool {
	if t.Info == nil || len(t.Info.Children) == 0 {
		return false
	}
	for _, c := range t.Info.Children {
		if c.Name != "" {
			return false
		}
	}
	return true
}
