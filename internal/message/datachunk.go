package message

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// dateEpoch is 1970-01-01 UTC, used as the zero day for DuckDB DATE values.
var dateEpoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// VectorFormat enumerates DuckDB's vector encodings on the wire.
type VectorFormat uint8

const (
	VectorFlat       VectorFormat = 0
	VectorFSST       VectorFormat = 1
	VectorConstant   VectorFormat = 2
	VectorDictionary VectorFormat = 3
	VectorSequence   VectorFormat = 4
)

// IntervalValue mirrors DuckDB's INTERVAL type: months + days + microseconds.
// Months and days are kept separate from micros because their length depends
// on the calendar (months especially).
type IntervalValue struct {
	Months int32
	Days   int32
	Micros int64
}

// ApproxDuration returns a best-effort time.Duration ignoring calendar drift.
// Months are treated as 30 days. Use only for rough comparisons or sorting;
// for calendar-correct math, add Months/Days to a time.Time explicitly.
func (iv IntervalValue) ApproxDuration() time.Duration {
	return time.Duration(int64(iv.Months)*30*24*int64(time.Hour) +
		int64(iv.Days)*24*int64(time.Hour) +
		iv.Micros*int64(time.Microsecond))
}

// String renders the interval as a DuckDB-style literal, e.g.
// "1 month 2 days 03:04:05.678". Arithmetic is done on the raw micros
// counter to avoid overflowing time.Duration (which would happen for
// micros values above ~9.2e15 once multiplied by 1000) and to handle
// MinInt64 negation without wrap-around.
func (iv IntervalValue) String() string {
	micros := iv.Micros
	neg := ""
	parts := make([]string, 0, 4)
	if iv.Months != 0 {
		parts = append(parts, plural(int(iv.Months), "month"))
	}
	if iv.Days != 0 {
		parts = append(parts, plural(int(iv.Days), "day"))
	}
	if micros == 0 && len(parts) > 0 {
		// pure months/days interval; no time component to render.
	} else {
		// Saturate the magnitude before division. -MinInt64 wraps in two's
		// complement, so handle that boundary explicitly.
		switch {
		case micros == math.MinInt64:
			neg = "-"
			micros = math.MaxInt64 // 1 µs of rounding error vs. true |MinInt64|
		case micros < 0:
			neg = "-"
			micros = -micros
		}
		const (
			usPerSec  = int64(1_000_000)
			usPerMin  = 60 * usPerSec
			usPerHour = 60 * usPerMin
		)
		hh := micros / usPerHour
		mm := (micros % usPerHour) / usPerMin
		ss := (micros % usPerMin) / usPerSec
		us := micros % usPerSec
		if us != 0 {
			parts = append(parts, fmt.Sprintf("%s%02d:%02d:%02d.%06d", neg, hh, mm, ss, us))
		} else {
			parts = append(parts, fmt.Sprintf("%s%02d:%02d:%02d", neg, hh, mm, ss))
		}
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

func plural(n int, unit string) string {
	if n == 1 || n == -1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// DataChunk is a column-major page of rows.
type DataChunk struct {
	RowCount int
	Types    []logicaltype.Type
	Columns  [][]any // column-major; row i of column c is Columns[c][i]; nil = SQL NULL
}

// Rows materializes the chunk as row-major slices of any.
func (c *DataChunk) Rows() [][]any {
	out := make([][]any, c.RowCount)
	for i := 0; i < c.RowCount; i++ {
		row := make([]any, len(c.Columns))
		for col := range c.Columns {
			row[col] = c.Columns[col][i]
		}
		out[i] = row
	}
	return out
}

func decodeDataChunkWrapper(r *codec.Reader) (DataChunk, error) {
	var out DataChunk
	err := r.ReadObject(func(r *codec.Reader) error {
		return r.ReadRequiredField(300, func(r *codec.Reader) error {
			c, err := decodeDataChunk(r)
			out = c
			return err
		})
	})
	return out, err
}

func encodeDataChunkWrapper(w *codec.Writer, c DataChunk) {
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(300, func(w *codec.Writer) { encodeDataChunk(w, c) })
	})
}

func decodeDataChunk(r *codec.Reader) (DataChunk, error) {
	var out DataChunk
	err := r.ReadObject(func(r *codec.Reader) error {
		if err := r.ReadRequiredField(100, func(r *codec.Reader) error {
			v, err := r.ReadULEB128()
			out.RowCount = int(v)
			return err
		}); err != nil {
			return err
		}
		if err := r.ReadRequiredField(101, func(r *codec.Reader) error {
			n, err := r.ReadULEB128()
			if err != nil {
				return err
			}
			out.Types = make([]logicaltype.Type, int(n))
			for i := range out.Types {
				t, err := logicaltype.Decode(r)
				if err != nil {
					return err
				}
				out.Types[i] = t
			}
			return nil
		}); err != nil {
			return err
		}
		return r.ReadRequiredField(102, func(r *codec.Reader) error {
			n, err := r.ReadULEB128()
			if err != nil {
				return err
			}
			if int(n) != len(out.Types) {
				return fmt.Errorf("message: DataChunk has %d types but %d columns", len(out.Types), n)
			}
			out.Columns = make([][]any, int(n))
			for i := range out.Columns {
				col, err := decodeVector(r, out.Types[i], out.RowCount)
				if err != nil {
					return err
				}
				out.Columns[i] = col
			}
			return nil
		})
	})
	return out, err
}

func encodeDataChunk(w *codec.Writer, c DataChunk) {
	if len(c.Types) != len(c.Columns) {
		panic("message: DataChunk type/column length mismatch")
	}
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(100, func(w *codec.Writer) { w.WriteULEB128(uint64(c.RowCount)) })
		w.WriteField(101, func(w *codec.Writer) {
			w.WriteULEB128(uint64(len(c.Types)))
			for _, t := range c.Types {
				logicaltype.Encode(w, t)
			}
		})
		w.WriteField(102, func(w *codec.Writer) {
			w.WriteULEB128(uint64(len(c.Columns)))
			for i, col := range c.Columns {
				encodeFlatVector(w, c.Types[i], col, c.RowCount)
			}
		})
	})
}

func decodeVector(r *codec.Reader, t logicaltype.Type, count int) ([]any, error) {
	var out []any
	err := r.ReadObject(func(r *codec.Reader) error {
		values, err := decodeVectorBody(r, t, count)
		if err == nil {
			out = values
		}
		return err
	})
	return out, err
}

func decodeVectorBody(r *codec.Reader, t logicaltype.Type, count int) ([]any, error) {
	format := VectorFlat
	if _, err := r.ReadOptionalField(90, func(r *codec.Reader) error {
		v, err := r.ReadULEB128()
		format = VectorFormat(v)
		return err
	}); err != nil {
		return nil, err
	}

	switch format {
	case VectorFlat:
		return decodeFlatVectorBody(r, t, count)
	case VectorFSST:
		return nil, fmt.Errorf("message: FSST vectors are not supported")
	case VectorConstant:
		single := count
		if single > 0 {
			single = 1
		}
		inner, err := decodeVectorBody(r, t, single)
		if err != nil {
			return nil, err
		}
		var seed any
		if single > 0 {
			seed = inner[0]
		}
		out := make([]any, count)
		for i := range out {
			out[i] = seed
		}
		return out, nil
	case VectorDictionary:
		var selection []uint32
		if err := r.ReadRequiredField(91, func(r *codec.Reader) error {
			s, err := readSelectionVector(r, count)
			selection = s
			return err
		}); err != nil {
			return nil, err
		}
		var dictCount uint64
		if err := r.ReadRequiredField(92, func(r *codec.Reader) error {
			v, err := r.ReadULEB128()
			dictCount = v
			return err
		}); err != nil {
			return nil, err
		}
		dict, err := decodeVectorBody(r, t, int(dictCount))
		if err != nil {
			return nil, err
		}
		out := make([]any, count)
		for i, idx := range selection {
			if uint64(idx) >= dictCount {
				return nil, fmt.Errorf("message: dictionary selection %d out of range (size %d)", idx, dictCount)
			}
			out[i] = dict[idx]
		}
		return out, nil
	case VectorSequence:
		var start, increment int64
		if err := r.ReadRequiredField(91, func(r *codec.Reader) error {
			v, err := r.ReadSLEB128()
			start = v
			return err
		}); err != nil {
			return nil, err
		}
		if err := r.ReadRequiredField(92, func(r *codec.Reader) error {
			v, err := r.ReadSLEB128()
			increment = v
			return err
		}); err != nil {
			return nil, err
		}
		return decodeSequenceValues(t, start, increment, count)
	}
	return nil, fmt.Errorf("message: unknown vector format %d", format)
}

func decodeFlatVectorBody(r *codec.Reader, t logicaltype.Type, count int) ([]any, error) {
	if t.ID == logicaltype.IDGeometry {
		if _, err := r.ReadOptionalField(99, func(r *codec.Reader) error {
			_, err := r.ReadULEB128()
			return err
		}); err != nil {
			return nil, err
		}
	}

	var hasValidity bool
	if err := r.ReadRequiredField(100, func(r *codec.Reader) error {
		v, err := r.ReadBool()
		hasValidity = v
		return err
	}); err != nil {
		return nil, err
	}

	var validity []bool
	if hasValidity {
		if err := r.ReadRequiredField(101, func(r *codec.Reader) error {
			v, err := readValidityMask(r, count)
			validity = v
			return err
		}); err != nil {
			return nil, err
		}
	}

	phys := t.PhysicalType()
	if logicaltype.IsFixedSize(phys) {
		var raw []byte
		if err := r.ReadRequiredField(102, func(r *codec.Reader) error {
			b, err := r.ReadBlob()
			raw = b
			return err
		}); err != nil {
			return nil, err
		}
		size, err := logicaltype.FixedSize(phys)
		if err != nil {
			return nil, err
		}
		if len(raw) != size*count {
			return nil, fmt.Errorf("message: fixed-size vector has %d bytes, expected %d", len(raw), size*count)
		}
		return decodeFixedValues(t, phys, raw, count, validity), nil
	}

	switch phys {
	case logicaltype.PhysicalVarchar:
		var values [][]byte
		if err := r.ReadRequiredField(102, func(r *codec.Reader) error {
			n, err := r.ReadULEB128()
			if err != nil {
				return err
			}
			values = make([][]byte, int(n))
			for i := range values {
				b, err := r.ReadBlob()
				if err != nil {
					return err
				}
				values[i] = b
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if len(values) != count {
			return nil, fmt.Errorf("message: VARCHAR vector has %d values, expected %d", len(values), count)
		}
		out := make([]any, count)
		for i := range out {
			if !isValid(validity, i) {
				out[i] = nil
				continue
			}
			out[i] = decodeStringLikeValue(t, values[i])
		}
		return out, nil
	case logicaltype.PhysicalStruct:
		if t.Info == nil {
			return nil, fmt.Errorf("message: STRUCT vector missing type metadata")
		}
		children := t.Info.Children
		var childVectors [][]any
		if err := r.ReadRequiredField(103, func(r *codec.Reader) error {
			n, err := r.ReadULEB128()
			if err != nil {
				return err
			}
			if int(n) != len(children) {
				return fmt.Errorf("message: STRUCT has %d children, got %d vectors", len(children), n)
			}
			childVectors = make([][]any, int(n))
			for i := range childVectors {
				v, err := decodeVector(r, children[i].Type, count)
				if err != nil {
					return err
				}
				if len(v) != count {
					return fmt.Errorf("message: STRUCT child %d has %d rows, expected %d", i, len(v), count)
				}
				childVectors[i] = v
			}
			return nil
		}); err != nil {
			return nil, err
		}
		out := make([]any, count)
		for i := range out {
			if !isValid(validity, i) {
				out[i] = nil
				continue
			}
			row := make(map[string]any, len(children))
			for ci, child := range children {
				row[child.Name] = childVectors[ci][i]
			}
			out[i] = row
		}
		return out, nil
	case logicaltype.PhysicalList:
		if t.Info == nil || t.Info.Child == nil {
			return nil, fmt.Errorf("message: LIST vector missing child type")
		}
		var listSize uint64
		if err := r.ReadRequiredField(104, func(r *codec.Reader) error {
			v, err := r.ReadULEB128()
			listSize = v
			return err
		}); err != nil {
			return nil, err
		}
		type entry struct{ offset, length uint64 }
		var entries []entry
		if err := r.ReadRequiredField(105, func(r *codec.Reader) error {
			n, err := r.ReadULEB128()
			if err != nil {
				return err
			}
			if int(n) != count {
				return fmt.Errorf("message: LIST has %d rows, got %d entries", count, n)
			}
			entries = make([]entry, int(n))
			for i := range entries {
				if err := r.ReadObject(func(r *codec.Reader) error {
					if err := r.ReadRequiredField(100, func(r *codec.Reader) error {
						v, err := r.ReadULEB128()
						entries[i].offset = v
						return err
					}); err != nil {
						return err
					}
					return r.ReadRequiredField(101, func(r *codec.Reader) error {
						v, err := r.ReadULEB128()
						entries[i].length = v
						return err
					})
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		var child []any
		if err := r.ReadRequiredField(106, func(r *codec.Reader) error {
			v, err := decodeVector(r, *t.Info.Child, int(listSize))
			child = v
			return err
		}); err != nil {
			return nil, err
		}
		out := make([]any, count)
		childLen := uint64(len(child))
		for i, e := range entries {
			if !isValid(validity, i) {
				out[i] = nil
				continue
			}
			// Server-provided offset/length must stay inside the child vector;
			// reject malformed data so a bad server can't crash the client.
			end := e.offset + e.length
			if end < e.offset || end > childLen {
				return nil, fmt.Errorf("message: LIST entry %d (offset=%d length=%d) exceeds child vector size %d",
					i, e.offset, e.length, childLen)
			}
			out[i] = append([]any(nil), child[e.offset:end]...)
		}
		return out, nil
	case logicaltype.PhysicalArray:
		if t.Info == nil || t.Info.Child == nil {
			return nil, fmt.Errorf("message: ARRAY vector missing child type")
		}
		var arraySize uint64
		if err := r.ReadRequiredField(103, func(r *codec.Reader) error {
			v, err := r.ReadULEB128()
			arraySize = v
			return err
		}); err != nil {
			return nil, err
		}
		if uint64(t.Info.ArraySize) != arraySize {
			return nil, fmt.Errorf("message: ARRAY vector size %d does not match type %d", arraySize, t.Info.ArraySize)
		}
		var child []any
		if err := r.ReadRequiredField(104, func(r *codec.Reader) error {
			v, err := decodeVector(r, *t.Info.Child, int(arraySize)*count)
			child = v
			return err
		}); err != nil {
			return nil, err
		}
		if int(arraySize)*count != len(child) {
			return nil, fmt.Errorf("message: ARRAY child vector has %d values, expected %d",
				len(child), int(arraySize)*count)
		}
		out := make([]any, count)
		for i := range out {
			if !isValid(validity, i) {
				out[i] = nil
				continue
			}
			out[i] = append([]any(nil), child[i*int(arraySize):(i+1)*int(arraySize)]...)
		}
		return out, nil
	}
	return nil, fmt.Errorf("message: cannot decode physical type %d for logical type %s", phys, t.Name())
}

func encodeFlatVector(w *codec.Writer, t logicaltype.Type, values []any, count int) {
	w.WriteObject(func(w *codec.Writer) {
		if t.ID == logicaltype.IDGeometry {
			w.WriteField(99, func(w *codec.Writer) { w.WriteULEB128(1) })
		}
		validity := make([]bool, count)
		hasNull := false
		for i, v := range values {
			validity[i] = v != nil
			if v == nil {
				hasNull = true
			}
		}
		w.WriteField(100, func(w *codec.Writer) { w.WriteBool(hasNull) })
		if hasNull {
			w.WriteField(101, func(w *codec.Writer) { w.WriteBlob(writeValidityMask(validity)) })
		}

		phys := t.PhysicalType()
		if logicaltype.IsFixedSize(phys) {
			size, err := logicaltype.FixedSize(phys)
			if err != nil {
				panic(err)
			}
			body := make([]byte, 0, size*count)
			body = encodeFixedValues(body, t, phys, values)
			w.WriteField(102, func(w *codec.Writer) { w.WriteBlob(body) })
			return
		}
		switch phys {
		case logicaltype.PhysicalVarchar:
			w.WriteField(102, func(w *codec.Writer) {
				w.WriteULEB128(uint64(len(values)))
				for _, v := range values {
					if v == nil {
						w.WriteBlob(nil)
						continue
					}
					w.WriteBlob(encodeStringLikeValue(t, v))
				}
			})
		case logicaltype.PhysicalStruct:
			encodeStructVector(w, t, values, count)
		case logicaltype.PhysicalList:
			encodeListVector(w, t, values, count)
		case logicaltype.PhysicalArray:
			encodeArrayVector(w, t, values, count)
		default:
			panic(fmt.Sprintf("message: encoding physical type %d is not yet implemented", phys))
		}
	})
}

func encodeStructVector(w *codec.Writer, t logicaltype.Type, values []any, count int) {
	if t.Info == nil {
		panic("message: STRUCT vector missing type metadata")
	}
	children := t.Info.Children
	w.WriteField(103, func(w *codec.Writer) {
		w.WriteULEB128(uint64(len(children)))
		for _, child := range children {
			childValues := make([]any, count)
			for i, row := range values {
				if row == nil {
					continue
				}
				m, ok := row.(map[string]any)
				if !ok {
					panic(fmt.Sprintf("message: STRUCT row %d is %T, want map[string]any", i, row))
				}
				childValues[i] = m[child.Name]
			}
			encodeFlatVector(w, child.Type, childValues, count)
		}
	})
}

func encodeListVector(w *codec.Writer, t logicaltype.Type, values []any, count int) {
	if t.Info == nil || t.Info.Child == nil {
		panic("message: LIST vector missing child type")
	}
	type entry struct{ offset, length uint64 }
	entries := make([]entry, count)
	var child []any
	for i, row := range values {
		if row == nil {
			continue
		}
		list, ok := row.([]any)
		if !ok {
			panic(fmt.Sprintf("message: LIST row %d is %T, want []any", i, row))
		}
		entries[i] = entry{offset: uint64(len(child)), length: uint64(len(list))}
		child = append(child, list...)
	}
	w.WriteField(104, func(w *codec.Writer) { w.WriteULEB128(uint64(len(child))) })
	w.WriteField(105, func(w *codec.Writer) {
		w.WriteULEB128(uint64(len(entries)))
		for _, e := range entries {
			w.WriteObject(func(w *codec.Writer) {
				w.WriteField(100, func(w *codec.Writer) { w.WriteULEB128(e.offset) })
				w.WriteField(101, func(w *codec.Writer) { w.WriteULEB128(e.length) })
			})
		}
	})
	w.WriteField(106, func(w *codec.Writer) {
		encodeFlatVector(w, *t.Info.Child, child, len(child))
	})
}

func encodeArrayVector(w *codec.Writer, t logicaltype.Type, values []any, count int) {
	if t.Info == nil || t.Info.Child == nil {
		panic("message: ARRAY vector missing child type")
	}
	size := int(t.Info.ArraySize)
	child := make([]any, count*size)
	for i, row := range values {
		if row == nil {
			continue
		}
		list, ok := row.([]any)
		if !ok || len(list) != size {
			panic(fmt.Sprintf("message: ARRAY row %d is %T (len %d, want %d)", i, row, len(list), size))
		}
		copy(child[i*size:(i+1)*size], list)
	}
	w.WriteField(103, func(w *codec.Writer) { w.WriteULEB128(uint64(size)) })
	w.WriteField(104, func(w *codec.Writer) {
		encodeFlatVector(w, *t.Info.Child, child, len(child))
	})
}

func readSelectionVector(r *codec.Reader, count int) ([]uint32, error) {
	b, err := r.ReadBlob()
	if err != nil {
		return nil, err
	}
	if len(b) != count*4 {
		return nil, fmt.Errorf("message: selection vector has %d bytes, expected %d", len(b), count*4)
	}
	out := make([]uint32, count)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	return out, nil
}

func readValidityMask(r *codec.Reader, count int) ([]bool, error) {
	expected := validityMaskSize(count)
	b, err := r.ReadBlob()
	if err != nil {
		return nil, err
	}
	if len(b) != expected {
		return nil, fmt.Errorf("message: validity mask has %d bytes, expected %d (count=%d)", len(b), expected, count)
	}
	out := make([]bool, count)
	for i := range out {
		out[i] = b[i/8]&(1<<(i%8)) != 0
	}
	return out, nil
}

func writeValidityMask(validity []bool) []byte {
	out := make([]byte, validityMaskSize(len(validity)))
	for i, v := range validity {
		if v {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return out
}

func validityMaskSize(count int) int {
	// Validity is allocated in 64-row words.
	return ((count + 63) / 64) * 8
}

func isValid(validity []bool, i int) bool {
	if validity == nil {
		return true
	}
	return validity[i]
}

func decodeFixedValues(t logicaltype.Type, phys logicaltype.PhysicalType, raw []byte, count int, validity []bool) []any {
	out := make([]any, count)
	for i := 0; i < count; i++ {
		if !isValid(validity, i) {
			out[i] = nil
			continue
		}
		out[i] = decodeFixedValue(t, phys, raw, i)
	}
	return out
}

func decodeFixedValue(t logicaltype.Type, phys logicaltype.PhysicalType, raw []byte, i int) any {
	switch phys {
	case logicaltype.PhysicalBool:
		return raw[i] != 0
	case logicaltype.PhysicalInt8:
		return int8(raw[i])
	case logicaltype.PhysicalUint8:
		v := raw[i]
		if t.ID == logicaltype.IDEnum {
			return enumValue(t, int(v))
		}
		return v
	case logicaltype.PhysicalInt16:
		v := int16(binary.LittleEndian.Uint16(raw[i*2:]))
		if t.ID == logicaltype.IDDecimal {
			return decimalString(t, big.NewInt(int64(v)))
		}
		return v
	case logicaltype.PhysicalUint16:
		v := binary.LittleEndian.Uint16(raw[i*2:])
		if t.ID == logicaltype.IDEnum {
			return enumValue(t, int(v))
		}
		return v
	case logicaltype.PhysicalInt32:
		v := int32(binary.LittleEndian.Uint32(raw[i*4:]))
		switch t.ID {
		case logicaltype.IDDate:
			return dateFromDays(v)
		case logicaltype.IDDecimal:
			return decimalString(t, big.NewInt(int64(v)))
		}
		return v
	case logicaltype.PhysicalUint32:
		v := binary.LittleEndian.Uint32(raw[i*4:])
		if t.ID == logicaltype.IDEnum {
			return enumValue(t, int(v))
		}
		return v
	case logicaltype.PhysicalInt64:
		v := int64(binary.LittleEndian.Uint64(raw[i*8:]))
		return decodeInt64Logical(t, v)
	case logicaltype.PhysicalUint64:
		return binary.LittleEndian.Uint64(raw[i*8:])
	case logicaltype.PhysicalFloat:
		return math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	case logicaltype.PhysicalDouble:
		return math.Float64frombits(binary.LittleEndian.Uint64(raw[i*8:]))
	case logicaltype.PhysicalInt128:
		lower := binary.LittleEndian.Uint64(raw[i*16:])
		upper := int64(binary.LittleEndian.Uint64(raw[i*16+8:]))
		if t.ID == logicaltype.IDUUID {
			return uuidFromParts(upper, lower)
		}
		value := hugeIntToBig(upper, lower)
		if t.ID == logicaltype.IDDecimal {
			return decimalString(t, value)
		}
		return value
	case logicaltype.PhysicalUint128:
		lower := binary.LittleEndian.Uint64(raw[i*16:])
		upper := binary.LittleEndian.Uint64(raw[i*16+8:])
		return uHugeIntToBig(upper, lower)
	case logicaltype.PhysicalInterval:
		months := int32(binary.LittleEndian.Uint32(raw[i*16:]))
		days := int32(binary.LittleEndian.Uint32(raw[i*16+4:]))
		micros := int64(binary.LittleEndian.Uint64(raw[i*16+8:]))
		return IntervalValue{Months: months, Days: days, Micros: micros}
	}
	return nil
}

func encodeFixedValues(buf []byte, t logicaltype.Type, phys logicaltype.PhysicalType, values []any) []byte {
	size, _ := logicaltype.FixedSize(phys)
	for _, v := range values {
		if v == nil {
			buf = append(buf, make([]byte, size)...)
			continue
		}
		buf = encodeFixedValue(buf, t, phys, v)
	}
	return buf
}

func encodeFixedValue(buf []byte, t logicaltype.Type, phys logicaltype.PhysicalType, v any) []byte {
	switch phys {
	case logicaltype.PhysicalBool:
		if v.(bool) {
			return append(buf, 1)
		}
		return append(buf, 0)
	case logicaltype.PhysicalInt8:
		return append(buf, byte(v.(int8)))
	case logicaltype.PhysicalUint8:
		if t.ID == logicaltype.IDEnum {
			idx, err := enumIndex(t, v)
			if err != nil {
				panic(err.Error())
			}
			return append(buf, uint8(idx))
		}
		return append(buf, v.(uint8))
	case logicaltype.PhysicalInt16:
		return binary.LittleEndian.AppendUint16(buf, uint16(toInt64(v)))
	case logicaltype.PhysicalUint16:
		if t.ID == logicaltype.IDEnum {
			idx, err := enumIndex(t, v)
			if err != nil {
				panic(err.Error())
			}
			return binary.LittleEndian.AppendUint16(buf, uint16(idx))
		}
		return binary.LittleEndian.AppendUint16(buf, uint16(toUint64(v)))
	case logicaltype.PhysicalInt32:
		return binary.LittleEndian.AppendUint32(buf, uint32(toInt64(v)))
	case logicaltype.PhysicalUint32:
		if t.ID == logicaltype.IDEnum {
			idx, err := enumIndex(t, v)
			if err != nil {
				panic(err.Error())
			}
			return binary.LittleEndian.AppendUint32(buf, uint32(idx))
		}
		return binary.LittleEndian.AppendUint32(buf, uint32(toUint64(v)))
	case logicaltype.PhysicalInt64:
		return binary.LittleEndian.AppendUint64(buf, uint64(toInt64(v)))
	case logicaltype.PhysicalUint64:
		return binary.LittleEndian.AppendUint64(buf, toUint64(v))
	case logicaltype.PhysicalFloat:
		return binary.LittleEndian.AppendUint32(buf, math.Float32bits(float32(toFloat64(v))))
	case logicaltype.PhysicalDouble:
		return binary.LittleEndian.AppendUint64(buf, math.Float64bits(toFloat64(v)))
	case logicaltype.PhysicalInt128:
		// HUGEINT/UUID/wide-DECIMAL: 16 bytes = [lower 8 LE][upper 8 LE].
		lower, upper := bigToHugeInt(v.(*big.Int))
		buf = binary.LittleEndian.AppendUint64(buf, lower)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(upper))
		return buf
	case logicaltype.PhysicalUint128:
		lower, upper := bigToUHugeInt(v.(*big.Int))
		buf = binary.LittleEndian.AppendUint64(buf, lower)
		buf = binary.LittleEndian.AppendUint64(buf, upper)
		return buf
	case logicaltype.PhysicalInterval:
		iv := v.(IntervalValue)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(iv.Months))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(iv.Days))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(iv.Micros))
		return buf
	}
	panic(fmt.Sprintf("message: encoding physical %d not yet implemented", phys))
}

// bigToHugeInt packs a signed *big.Int into DuckDB's hugeint_t layout: low 64
// bits as unsigned, high 64 bits as signed (two's complement of the full 128).
// Values outside [-2^127, 2^127) are wrapped, matching DuckDB's semantics.
func bigToHugeInt(v *big.Int) (uint64, int64) {
	buf := make([]byte, 16)
	if v.Sign() >= 0 {
		v.FillBytes(buf)
	} else {
		var twoComp big.Int
		twoComp.Lsh(big.NewInt(1), 128)
		twoComp.Add(&twoComp, v)
		twoComp.FillBytes(buf)
	}
	upper := int64(binary.BigEndian.Uint64(buf[:8]))
	lower := binary.BigEndian.Uint64(buf[8:])
	return lower, upper
}

// bigToUHugeInt packs an unsigned *big.Int into DuckDB's uhugeint_t layout.
// Negative inputs are rejected by the caller; values >= 2^128 are wrapped.
func bigToUHugeInt(v *big.Int) (uint64, uint64) {
	buf := make([]byte, 16)
	v.FillBytes(buf)
	upper := binary.BigEndian.Uint64(buf[:8])
	lower := binary.BigEndian.Uint64(buf[8:])
	return lower, upper
}

// UUIDToBig parses a 8-4-4-4-12 hex UUID into the *big.Int form that the
// hugeint encoder accepts (the display form is XOR-flipped on the high bit
// so DuckDB's lexicographic ordering of HUGEINT matches UUID display order).
func UUIDToBig(uuid string) (*big.Int, error) {
	hex := stripDashesLower(uuid)
	if len(hex) != 32 {
		return nil, fmt.Errorf("message: UUID must have 32 hex digits, got %d", len(hex))
	}
	for _, c := range hex {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, fmt.Errorf("message: invalid UUID character %q", c)
		}
	}
	displayUpper, ok := new(big.Int).SetString(hex[:16], 16)
	if !ok {
		return nil, fmt.Errorf("message: invalid UUID upper")
	}
	lower, ok := new(big.Int).SetString(hex[16:], 16)
	if !ok {
		return nil, fmt.Errorf("message: invalid UUID lower")
	}
	// rawUpper = signed(displayUpper XOR 1<<63)
	signBit := new(big.Int).Lsh(big.NewInt(1), 63)
	rawUpper := new(big.Int).Xor(displayUpper, signBit)
	// If high bit was 1 in rawUpper, sign-extend by subtracting 2^64.
	if rawUpper.Bit(63) == 1 {
		rawUpper.Sub(rawUpper, new(big.Int).Lsh(big.NewInt(1), 64))
	}
	out := new(big.Int).Lsh(rawUpper, 64)
	out.Add(out, lower)
	return out, nil
}

func stripDashesLower(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out = append(out, c)
	}
	return string(out)
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	}
	panic(fmt.Sprintf("message: cannot convert %T to int64", v))
}

func toUint64(v any) uint64 {
	switch x := v.(type) {
	case uint:
		return uint64(x)
	case uint8:
		return uint64(x)
	case uint16:
		return uint64(x)
	case uint32:
		return uint64(x)
	case uint64:
		return x
	case int:
		return uint64(x)
	case int8:
		return uint64(x)
	case int16:
		return uint64(x)
	case int32:
		return uint64(x)
	case int64:
		return uint64(x)
	}
	panic(fmt.Sprintf("message: cannot convert %T to uint64", v))
}

func toFloat64(v any) float64 {
	switch x := v.(type) {
	case float32:
		return float64(x)
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	panic(fmt.Sprintf("message: cannot convert %T to float64", v))
}

func decodeStringLikeValue(t logicaltype.Type, raw []byte) any {
	switch t.ID {
	case logicaltype.IDBlob, logicaltype.IDGeometry, logicaltype.IDBit:
		// Copy to avoid retaining a slice into the wire buffer.
		out := make([]byte, len(raw))
		copy(out, raw)
		return out
	}
	return string(raw)
}

func encodeStringLikeValue(t logicaltype.Type, v any) []byte {
	switch t.ID {
	case logicaltype.IDBlob, logicaltype.IDGeometry, logicaltype.IDBit:
		if b, ok := v.([]byte); ok {
			return b
		}
		return []byte(fmt.Sprint(v))
	}
	if s, ok := v.(string); ok {
		return []byte(s)
	}
	if b, ok := v.([]byte); ok {
		return b
	}
	return []byte(fmt.Sprint(v))
}

func decodeInt64Logical(t logicaltype.Type, v int64) any {
	switch t.ID {
	case logicaltype.IDTime:
		return time.Duration(v) * time.Microsecond
	case logicaltype.IDTimeNs:
		return time.Duration(v) * time.Nanosecond
	case logicaltype.IDTimeTz:
		return TimeTzValue{Bits: uint64(v)}
	case logicaltype.IDTimestampSec:
		return time.Unix(v, 0).UTC()
	case logicaltype.IDTimestampMs:
		return time.UnixMilli(v).UTC()
	case logicaltype.IDTimestamp, logicaltype.IDTimestampTz:
		return time.UnixMicro(v).UTC()
	case logicaltype.IDTimestampNs:
		return time.Unix(0, v).UTC()
	case logicaltype.IDDecimal:
		return decimalString(t, big.NewInt(v))
	}
	return v
}

func decodeSequenceValues(t logicaltype.Type, start, increment int64, count int) ([]any, error) {
	out := make([]any, count)
	for i := 0; i < count; i++ {
		raw := start + increment*int64(i)
		switch t.ID {
		case logicaltype.IDInteger:
			out[i] = int32(raw)
		case logicaltype.IDBigint:
			out[i] = raw
		case logicaltype.IDDate:
			out[i] = dateFromDays(int32(raw))
		default:
			out[i] = decodeInt64Logical(t, raw)
		}
	}
	return out, nil
}

// decimalString formats an unscaled big.Int as a decimal string with the
// type's scale (e.g. 12345 + scale=2 → "123.45"). Width is currently advisory.
func decimalString(t logicaltype.Type, v *big.Int) string {
	scale := 0
	if t.Info != nil && t.Info.Kind == logicaltype.ExtraDecimal {
		scale = int(t.Info.Scale)
	}
	neg := v.Sign() < 0
	abs := new(big.Int).Abs(v).String()
	if scale <= 0 {
		if neg {
			return "-" + abs
		}
		return abs
	}
	for len(abs) <= scale {
		abs = "0" + abs
	}
	out := abs[:len(abs)-scale] + "." + abs[len(abs)-scale:]
	if neg {
		out = "-" + out
	}
	return out
}


func enumValue(t logicaltype.Type, index int) any {
	if t.Info == nil || index < 0 || index >= len(t.Info.EnumValues) {
		return index
	}
	return t.Info.EnumValues[index]
}

// enumIndex resolves a Go value into a dictionary index for an ENUM column.
// Accepts the integer index directly or the string label; rejects anything
// else with an explicit error so the encoder doesn't silently emit garbage.
func enumIndex(t logicaltype.Type, v any) (int, error) {
	if t.Info == nil {
		return 0, fmt.Errorf("message: ENUM type missing dictionary metadata")
	}
	switch x := v.(type) {
	case string:
		for i, label := range t.Info.EnumValues {
			if label == x {
				return i, nil
			}
		}
		return 0, fmt.Errorf("message: ENUM label %q not in dictionary", x)
	case uint8:
		return int(x), nil
	case uint16:
		return int(x), nil
	case uint32:
		return int(x), nil
	case int:
		return x, nil
	case int8:
		return int(x), nil
	case int16:
		return int(x), nil
	case int32:
		return int(x), nil
	case int64:
		return int(x), nil
	}
	return 0, fmt.Errorf("message: ENUM value must be string label or integer index, got %T", v)
}

func hugeIntToBig(upper int64, lower uint64) *big.Int {
	hi := new(big.Int).SetInt64(upper)
	hi.Lsh(hi, 64)
	lo := new(big.Int).SetUint64(lower)
	return hi.Add(hi, lo)
}

func uHugeIntToBig(upper, lower uint64) *big.Int {
	hi := new(big.Int).SetUint64(upper)
	hi.Lsh(hi, 64)
	lo := new(big.Int).SetUint64(lower)
	return hi.Add(hi, lo)
}

func uuidFromParts(upper int64, lower uint64) string {
	// DuckDB flips the high bit of the upper word for display ordering.
	display := uint64(upper) ^ (uint64(1) << 63)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uint32(display>>32),
		uint16(display>>16),
		uint16(display),
		uint16(lower>>48),
		lower&0xffffffffffff,
	)
}

func dateFromDays(days int32) time.Time { return dateEpoch.AddDate(0, 0, int(days)) }

// TimeTzValue is a DuckDB TIME WITH TIME ZONE — DuckDB packs time-of-day plus
// a biased UTC offset into a 64-bit word. The bit layout has not been verified
// against the protocol spec at this driver version, so the raw bits are
// exposed and helper accessors are intentionally not provided yet. If you need
// TIME_TZ today, cast to VARCHAR server-side and parse the resulting string.
type TimeTzValue struct {
	Bits uint64
}

// String renders the raw bits in hex for diagnostic purposes only.
func (t TimeTzValue) String() string { return fmt.Sprintf("TimeTzValue(0x%016x)", t.Bits) }
