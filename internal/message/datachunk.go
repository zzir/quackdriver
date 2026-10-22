package message

import (
	"fmt"
	"math"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/value"
)

// VectorFormat enumerates DuckDB's vector encodings on the wire (field 90 of
// a serialized Vector, DuckDB's VectorType enum).
type VectorFormat uint8

// Vector encoding formats used on the DuckDB wire protocol.
const (
	VectorFlat       VectorFormat = 0
	VectorFSST       VectorFormat = 1 // never serialized; rejected on decode
	VectorConstant   VectorFormat = 2
	VectorDictionary VectorFormat = 3
	VectorSequence   VectorFormat = 4
)

// IntervalValue is DuckDB's INTERVAL.
type IntervalValue = value.IntervalValue

// TimeTzValue is DuckDB's TIME WITH TIME ZONE.
type TimeTzValue = value.TimeTzValue

// Column is one column of a DataChunk.
//
// Decoded columns box their values lazily: Value converts the wire
// representation of row i into the Go value documented for the column's
// logical type (nil for SQL NULL). i must be in [0, Len()); like a slice
// index, an out-of-range i panics. Every call returns a fresh value: []byte,
// []any, map[string]any and *big.Int results are newly allocated and may be
// mutated by the caller. A decoded Column is safe for concurrent use.
type Column interface {
	Len() int
	Value(i int) any
}

// Values is a materialized column, the encoder's natural input. Unlike a
// decoded column it returns its elements as stored, without copying.
type Values []any

// Len returns len(v).
func (v Values) Len() int { return len(v) }

// Value returns v[i].
func (v Values) Value(i int) any { return v[i] }

// Materialize returns every value of c, in row order. A Values column is
// returned as is.
func Materialize(c Column) Values {
	if v, ok := c.(Values); ok {
		return v
	}
	out := make(Values, c.Len())
	for i := range out {
		out[i] = c.Value(i)
	}
	return out
}

// DataChunk is a column-major page of rows.
//
// A decoded DataChunk references the byte slice it was decoded from (fixed
// size data, string bytes and validity masks are not copied). The slice must
// therefore not be modified or reused while the chunk is in use; the
// transport returns a freshly allocated slice per response, which satisfies
// this. Values handed out by Column.Value never alias that slice.
type DataChunk struct {
	RowCount int
	Types    []logicaltype.Type
	Columns  []Column // Columns[c].Value(i) is row i of column c
}

// Rows materializes the chunk as row-major slices of any.
func (c *DataChunk) Rows() [][]any {
	out := make([][]any, c.RowCount)
	for i := range c.RowCount {
		row := make([]any, len(c.Columns))
		for col := range c.Columns {
			row[col] = c.Columns[col].Value(i)
		}
		out[i] = row
	}
	return out
}

// writeDataChunk writes one raw DataChunk object ([100 rows][101 types]
// [102 columns] 0xFFFF), the unit FETCH_RESPONSE and SEND_DATA_REQUEST carry
// after their body, the way DataChunk::Serialize does. Every column is
// written as a FLAT vector. A malformed chunk or a value that does not fit
// its column type is reported as an error; nothing panics.
func writeDataChunk(w *codec.Writer, c DataChunk) error {
	if len(c.Types) == 0 {
		return fmt.Errorf("message: DataChunk has no columns")
	}
	if len(c.Types) != len(c.Columns) {
		return fmt.Errorf("message: DataChunk has %d types but %d columns", len(c.Types), len(c.Columns))
	}
	if c.RowCount < 0 || uint64(c.RowCount) > math.MaxUint32 {
		return fmt.Errorf("message: DataChunk row count %d out of range", c.RowCount)
	}
	for i, col := range c.Columns {
		if col == nil {
			return fmt.Errorf("message: DataChunk column %d is nil", i)
		}
		if n := col.Len(); n != c.RowCount {
			return fmt.Errorf("message: DataChunk column %d has %d values, want %d", i, n, c.RowCount)
		}
	}
	w.WriteFieldID(100)
	w.WriteULEB128(uint64(c.RowCount))
	w.WriteFieldID(101)
	w.WriteULEB128(uint64(len(c.Types)))
	for _, t := range c.Types {
		logicaltype.Encode(w, t)
	}
	w.WriteFieldID(102)
	w.WriteULEB128(uint64(len(c.Columns)))
	for i, col := range c.Columns {
		if err := encodeVector(w, c.Types[i], Materialize(col), 0); err != nil {
			return fmt.Errorf("message: DataChunk column %d (%s): %w", i, c.Types[i].Name(), err)
		}
	}
	w.WriteEndObject()
	return nil
}

// decodeDataChunk mirrors DataChunk::Deserialize: rows (a sel_t), the column
// types, then one vector object per column decoded with rows entries.
func decodeDataChunk(r *codec.Reader) (DataChunk, error) {
	var out DataChunk
	err := readFields(r,
		required(100, func(r *codec.Reader) error {
			v, err := r.ReadULEB128()
			if err == nil && v > math.MaxUint32 { // rows is a sel_t
				err = fmt.Errorf("message: DataChunk row count %d out of range", v)
			}
			out.RowCount = int(v)
			return err
		}),
		required(101, func(r *codec.Reader) error {
			return readList(r, &out.Types, func(r *codec.Reader, _ int) (logicaltype.Type, error) { return logicaltype.Decode(r) })
		}),
		required(102, func(r *codec.Reader) error {
			n, err := r.ReadULEB128()
			if err != nil {
				return err
			}
			if n != uint64(len(out.Types)) {
				return fmt.Errorf("message: DataChunk has %d types but %d columns", len(out.Types), n)
			}
			out.Columns = make([]Column, n)
			start := r.Remaining()
			var rowCost uint64 // reading a row materializes every column of it
			for i, t := range out.Types {
				if out.Columns[i], err = decodeVector(r, t, out.RowCount, 0); err != nil {
					return fmt.Errorf("column %d (%s): %w", i, t.Name(), err)
				}
				rowCost = satAdd(rowCost, costOf(out.Columns[i]).maxRow)
			}
			return checkRowCost("DataChunk", rowCost, start-r.Remaining())
		}))
	return out, err
}
