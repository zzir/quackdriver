package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
)

// Rows iterates over the result of a PREPARE_REQUEST. Additional chunks are
// pulled via FETCH_REQUEST when needed.
type Rows struct {
	conn       *Conn
	ctx        context.Context
	chunks     []message.DataChunk
	chunkIdx   int
	rowInChunk int

	columnNames []string
	columnTypes []logicaltype.Type
	resultUUID  codec.HugeInt
	needsMore   bool
	closed      bool
}

var (
	_ driver.Rows                           = (*Rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*Rows)(nil)
	_ driver.RowsColumnTypeNullable         = (*Rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*Rows)(nil)
)

func newRows(ctx context.Context, c *Conn, prep message.PrepareResponse) *Rows {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Rows{
		conn:        c,
		ctx:         ctx,
		chunks:      prep.Results,
		columnNames: prep.ResultNames,
		columnTypes: prep.ResultTypes,
		resultUUID:  prep.ResultUUID,
		needsMore:   prep.NeedsMoreFetch,
	}
}

// Columns returns the result column names.
func (r *Rows) Columns() []string { return r.columnNames }

// Close releases the result. If the caller closes mid-iteration while the
// server still has pending chunks (`needsMore`), Close drains them via
// follow-up FETCH_REQUESTs so the server-side result buffer is freed; without
// this, the result UUID would dangle until the connection is closed.
//
// Drain errors are returned but the Rows is always marked closed so a second
// Close is a cheap no-op. A canceled context (typical during graceful
// shutdown) is not surfaced as an error.
func (r *Rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	drainErr := r.drain()
	r.chunks = nil
	if drainErr != nil && !errors.Is(drainErr, context.Canceled) && !errors.Is(drainErr, context.DeadlineExceeded) {
		return drainErr
	}
	return nil
}

// drain pulls FETCH_REQUESTs until the server signals exhaustion with an
// empty result list. Called from Close; safe to call on an already-exhausted
// Rows (it returns immediately).
func (r *Rows) drain() error {
	for r.needsMore {
		if err := r.fetchMore(); err != nil {
			return err
		}
	}
	return nil
}

// Next reads the next row into dest. dest is pre-sized by database/sql to
// len(Columns()). We defensively guard against a misbehaving server that
// could send a chunk whose column or row counts do not match what was
// promised in the PrepareResponse — without this, a short Columns slice or
// short column vector would crash the client with an index-out-of-range
// panic deep inside the standard library.
func (r *Rows) Next(dest []driver.Value) error {
	if r.closed {
		return io.EOF
	}
	for {
		if r.chunkIdx < len(r.chunks) {
			chunk := r.chunks[r.chunkIdx]
			if r.rowInChunk >= chunk.RowCount {
				r.chunkIdx++
				r.rowInChunk = 0
				continue
			}
			if len(chunk.Columns) < len(dest) {
				return fmt.Errorf("quackdriver: chunk has %d columns, expected at least %d", len(chunk.Columns), len(dest))
			}
			for col := range dest {
				column := chunk.Columns[col]
				if r.rowInChunk >= len(column) {
					return fmt.Errorf("quackdriver: column %d has %d rows but chunk declared %d (row index %d)",
						col, len(column), chunk.RowCount, r.rowInChunk)
				}
				dest[col] = column[r.rowInChunk]
			}
			r.rowInChunk++
			return nil
		}
		if !r.needsMore {
			return io.EOF
		}
		if err := r.fetchMore(); err != nil {
			return err
		}
	}
}

func (r *Rows) fetchMore() error {
	_, body, err := r.conn.roundTrip(r.ctx, message.Header{}, message.FetchRequest{ResultUUID: r.resultUUID})
	if err != nil {
		return err
	}
	resp, ok := body.(message.FetchResponse)
	if !ok {
		return fmt.Errorf("quackdriver: unexpected response %T to FETCH", body)
	}
	r.chunks = append(r.chunks[:0], resp.Results...)
	r.chunkIdx = 0
	r.rowInChunk = 0
	// The server signals exhaustion by returning an empty results list.
	// batchIndex is purely informational and must not be used for termination
	// (it can be absent on legitimate non-final batches).
	r.needsMore = len(resp.Results) > 0
	return nil
}

// ColumnTypeDatabaseTypeName returns the DuckDB type name for column index.
func (r *Rows) ColumnTypeDatabaseTypeName(index int) string {
	if index < 0 || index >= len(r.columnTypes) {
		return ""
	}
	return r.columnTypes[index].Name()
}

// ColumnTypeNullable always reports true — Quack columns are nullable by default.
func (r *Rows) ColumnTypeNullable(int) (nullable, ok bool) { return true, true }

// ColumnTypeScanType returns a representative Go type for scanning.
func (r *Rows) ColumnTypeScanType(index int) reflect.Type {
	if index < 0 || index >= len(r.columnTypes) {
		return reflect.TypeOf((*any)(nil)).Elem()
	}
	return scanTypeFor(r.columnTypes[index])
}

func scanTypeFor(t logicaltype.Type) reflect.Type {
	switch t.ID {
	case logicaltype.IDBoolean:
		return reflect.TypeOf(false)
	case logicaltype.IDTinyint:
		return reflect.TypeOf(int8(0))
	case logicaltype.IDSmallint:
		return reflect.TypeOf(int16(0))
	case logicaltype.IDInteger:
		return reflect.TypeOf(int32(0))
	case logicaltype.IDBigint:
		return reflect.TypeOf(int64(0))
	case logicaltype.IDUTinyint:
		return reflect.TypeOf(uint8(0))
	case logicaltype.IDUSmallint:
		return reflect.TypeOf(uint16(0))
	case logicaltype.IDUInteger:
		return reflect.TypeOf(uint32(0))
	case logicaltype.IDUBigint:
		return reflect.TypeOf(uint64(0))
	case logicaltype.IDFloat:
		return reflect.TypeOf(float32(0))
	case logicaltype.IDDouble:
		return reflect.TypeOf(float64(0))
	case logicaltype.IDVarchar, logicaltype.IDChar, logicaltype.IDUUID:
		return reflect.TypeOf("")
	case logicaltype.IDBlob, logicaltype.IDBit, logicaltype.IDGeometry:
		return reflect.TypeOf([]byte(nil))
	}
	return reflect.TypeOf((*any)(nil)).Elem()
}
