// Package codec implements the DuckDB BinarySerializer primitives the Quack
// wire protocol is built from, at StorageVersion::V2_0_0 (both peers pin
// it). An object is a sequence of (u16 LE field id, value) pairs closed by
// FieldEnd (0xFFFF); properties written "with default" are omitted when they
// hold their default value.
package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

// FieldEnd marks the end of a BinarySerializer object.
const FieldEnd uint16 = 0xFFFF

// InvalidIndex is DuckDB's DConstants::INVALID_INDEX (2^64-1). An unset
// optional_idx is serialized as this value.
const InvalidIndex uint64 = math.MaxUint64

// OptionalIdx mirrors DuckDB's optional_idx: an idx_t that may be unset.
// On the wire a set value is its ULEB128 encoding and an unset value is the
// InvalidIndex sentinel; a set value equal to InvalidIndex cannot be encoded.
type OptionalIdx struct {
	Value uint64
	Valid bool
}

// HugeInt mirrors DuckDB's hugeint_t: a 128-bit value stored as a signed
// upper word + unsigned lower word. Wire order: SLEB upper, ULEB lower.
type HugeInt struct {
	Lower uint64
	Upper int64
}

// MaxPrealloc caps preallocations sized by a count from the wire: a count is
// only checked against the remaining bytes, while an element may take far
// more memory than its one byte. A false count then fails on the read.
const MaxPrealloc = 1024

// Reader decodes BinarySerializer primitives from a byte slice.
type Reader struct {
	buf []byte
	pos int
}

// NewReader returns a Reader over buf. The slice is not copied.
func NewReader(buf []byte) *Reader { return &Reader{buf: buf} }

// Pos returns the current byte offset.
func (r *Reader) Pos() int { return r.pos }

// Since returns the bytes consumed since offset start (an earlier Pos). The
// slice aliases the input.
func (r *Reader) Since(start int) []byte { return r.buf[start:r.pos:r.pos] }

// Remaining returns the number of unread bytes.
func (r *Reader) Remaining() int { return len(r.buf) - r.pos }

// AssertEOF returns an error if any bytes remain.
func (r *Reader) AssertEOF() error {
	if r.pos != len(r.buf) {
		return fmt.Errorf("codec: %d trailing byte(s) at offset %d", len(r.buf)-r.pos, r.pos)
	}
	return nil
}

func (r *Reader) need(n int) error {
	// Compare against the remaining length so a huge n cannot overflow pos+n.
	if n < 0 || n > len(r.buf)-r.pos {
		return fmt.Errorf("codec: unexpected EOF at offset %d (need %d, have %d): %w", r.pos, n, len(r.buf)-r.pos, io.ErrUnexpectedEOF)
	}
	return nil
}

// ReadByte reads one byte.
func (r *Reader) ReadByte() (byte, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	b := r.buf[r.pos]
	r.pos++
	return b, nil
}

// ReadBytes reads n raw bytes without a length prefix. The returned slice
// aliases the input.
func (r *Reader) ReadBytes(n int) ([]byte, error) {
	if err := r.need(n); err != nil {
		return nil, err
	}
	out := r.buf[r.pos : r.pos+n]
	r.pos += n
	return out, nil
}

// ReadBool reads a DuckDB boolean byte (0 or 1; anything else is an error).
func (r *Reader) ReadBool() (bool, error) {
	b, err := r.ReadByte()
	if err != nil {
		return false, err
	}
	switch b {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("codec: invalid boolean byte %d at offset %d", b, r.pos-1)
	}
}

// ReadULEB128 decodes an unsigned LEB128 up to 64 bits.
func (r *Reader) ReadULEB128() (uint64, error) {
	var result uint64
	var shift uint
	for range 10 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return result, nil
		}
		shift += 7
	}
	return 0, errors.New("codec: ULEB128 value exceeds 64 bits")
}

// ReadSLEB128 decodes a signed LEB128 up to 64 bits.
func (r *Reader) ReadSLEB128() (int64, error) {
	var result int64
	var shift uint
	for range 10 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= int64(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			if b&0x40 != 0 && shift < 64 {
				result |= -1 << shift
			}
			return result, nil
		}
	}
	return 0, errors.New("codec: SLEB128 value exceeds 64 bits")
}

// ReadString reads a ULEB-length-prefixed UTF-8 string.
func (r *Reader) ReadString() (string, error) {
	n, err := r.ReadCount()
	if err != nil {
		return "", err
	}
	b, err := r.ReadBytes(n)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadBlob reads a ULEB-length-prefixed binary blob. The result is a copy.
func (r *Reader) ReadBlob() ([]byte, error) {
	n, err := r.ReadCount()
	if err != nil {
		return nil, err
	}
	b, err := r.ReadBytes(n)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// ReadCount reads a ULEB byte length or element count. Every byte, and every
// element of a BinarySerializer list, takes at least one byte of input, so a
// count larger than the remaining input is rejected before the caller
// allocates for it.
func (r *Reader) ReadCount() (int, error) {
	start := r.pos
	n, err := r.ReadULEB128()
	if err != nil {
		return 0, err
	}
	if n > uint64(r.Remaining()) {
		return 0, fmt.Errorf("codec: count %d at offset %d exceeds the %d remaining byte(s): %w", n, start, r.Remaining(), io.ErrUnexpectedEOF)
	}
	return int(n), nil
}

// ReadOptionalIdx reads a DuckDB optional_idx (ULEB128, InvalidIndex = unset).
func (r *Reader) ReadOptionalIdx() (OptionalIdx, error) {
	v, err := r.ReadULEB128()
	if err != nil {
		return OptionalIdx{}, err
	}
	if v == InvalidIndex {
		return OptionalIdx{}, nil
	}
	return OptionalIdx{Value: v, Valid: true}, nil
}

// ReadStringMap reads an unordered_map<string, string>: a ULEB entry count,
// then one object {0: key, 1: value} per entry. A repeated key keeps the
// last value, like DuckDB's deserializer.
func (r *Reader) ReadStringMap() (map[string]string, error) {
	n, err := r.ReadCount()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, min(n, MaxPrealloc))
	for range n {
		var key, value string
		if err := r.ReadObject(func(r *Reader) error {
			if err := r.ReadRequiredField(0, func(r *Reader) error {
				v, err := r.ReadString()
				key = v
				return err
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(1, func(r *Reader) error {
				v, err := r.ReadString()
				value = v
				return err
			})
		}); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

// ReadHugeInt reads a 128-bit hugeint as [SLEB upper][ULEB lower].
func (r *Reader) ReadHugeInt() (HugeInt, error) {
	upper, err := r.ReadSLEB128()
	if err != nil {
		return HugeInt{}, err
	}
	lower, err := r.ReadULEB128()
	if err != nil {
		return HugeInt{}, err
	}
	return HugeInt{Lower: lower, Upper: upper}, nil
}

// readFieldID reads the next field id.
func (r *Reader) readFieldID() (uint16, error) {
	id, err := r.PeekFieldID()
	if err == nil {
		r.pos += 2
	}
	return id, err
}

// PeekFieldID returns the next field id without advancing the reader.
func (r *Reader) PeekFieldID() (uint16, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(r.buf[r.pos:]), nil
}

// ReadEndObject consumes and validates the FieldEnd marker.
func (r *Reader) ReadEndObject() error {
	id, err := r.readFieldID()
	if err != nil {
		return err
	}
	if id != FieldEnd {
		return fmt.Errorf("codec: expected end-of-object marker at offset %d, got field id 0x%04x", r.pos-2, id)
	}
	return nil
}

// ReadObject reads an object body via fn and then consumes the FieldEnd marker.
func (r *Reader) ReadObject(fn func(*Reader) error) error {
	if err := fn(r); err != nil {
		return err
	}
	return r.ReadEndObject()
}

// ReadRequiredField asserts the next field has fieldID, then invokes fn.
func (r *Reader) ReadRequiredField(fieldID uint16, fn func(*Reader) error) error {
	actual, err := r.readFieldID()
	if err != nil {
		return err
	}
	if actual != fieldID {
		return fmt.Errorf("codec: expected field 0x%04x at offset %d, got 0x%04x", fieldID, r.pos-2, actual)
	}
	return fn(r)
}

// ReadOptionalField runs fn when the next field is fieldID and reports
// whether it was present; an absent field leaves the caller's default.
func (r *Reader) ReadOptionalField(fieldID uint16, fn func(*Reader) error) (bool, error) {
	id, err := r.PeekFieldID()
	if err != nil {
		return false, err
	}
	if id != fieldID {
		return false, nil
	}
	r.pos += 2
	if err := fn(r); err != nil {
		return false, err
	}
	return true, nil
}

// Writer encodes BinarySerializer primitives to an internal byte buffer.
type Writer struct {
	buf []byte
}

// NewWriter returns a fresh Writer.
func NewWriter() *Writer { return &Writer{} }

// Bytes returns the accumulated buffer, which the Writer still owns.
func (w *Writer) Bytes() []byte { return w.buf }

// WriteBytes appends raw bytes without a length prefix.
func (w *Writer) WriteBytes(b []byte) { w.buf = append(w.buf, b...) }

// WriteBool writes one boolean byte.
func (w *Writer) WriteBool(v bool) {
	if v {
		w.buf = append(w.buf, 1)
	} else {
		w.buf = append(w.buf, 0)
	}
}

// WriteULEB128 writes an unsigned LEB128 integer.
func (w *Writer) WriteULEB128(v uint64) {
	for v >= 0x80 {
		w.buf = append(w.buf, byte(v&0x7f)|0x80)
		v >>= 7
	}
	w.buf = append(w.buf, byte(v))
}

// WriteSLEB128 writes a signed LEB128 integer.
func (w *Writer) WriteSLEB128(v int64) {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		signBitSet := b&0x40 != 0
		if (v == 0 && !signBitSet) || (v == -1 && signBitSet) {
			w.buf = append(w.buf, b)
			return
		}
		w.buf = append(w.buf, b|0x80)
	}
}

// WriteString writes a ULEB-length-prefixed UTF-8 string.
func (w *Writer) WriteString(s string) {
	w.WriteULEB128(uint64(len(s)))
	w.buf = append(w.buf, s...)
}

// WriteBlob writes a ULEB-length-prefixed byte blob.
func (w *Writer) WriteBlob(b []byte) {
	w.WriteULEB128(uint64(len(b)))
	w.buf = append(w.buf, b...)
}

// WriteOptionalIdx writes a DuckDB optional_idx. An unset value is written as
// the InvalidIndex sentinel; a set value equal to InvalidIndex is an error
// (DuckDB's serializer throws for it too).
func (w *Writer) WriteOptionalIdx(v OptionalIdx) error {
	if !v.Valid {
		w.WriteULEB128(InvalidIndex)
		return nil
	}
	if v.Value == InvalidIndex {
		return errors.New("codec: cannot serialize an optional_idx set to INVALID_INDEX")
	}
	w.WriteULEB128(v.Value)
	return nil
}

// WriteStringMap writes an unordered_map<string, string> as a ULEB entry
// count followed by one object {0: key, 1: value} per entry. DuckDB writes
// the entries in hash order; any order decodes to the same map, so keys are
// sorted here to keep the encoding deterministic.
func (w *Writer) WriteStringMap(m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w.WriteULEB128(uint64(len(keys)))
	for _, k := range keys {
		w.WriteObject(func(w *Writer) {
			w.WriteField(0, func(w *Writer) { w.WriteString(k) })
			w.WriteField(1, func(w *Writer) { w.WriteString(m[k]) })
		})
	}
}

// WriteHugeInt writes a hugeint as [SLEB upper][ULEB lower].
func (w *Writer) WriteHugeInt(h HugeInt) {
	w.WriteSLEB128(h.Upper)
	w.WriteULEB128(h.Lower)
}

// WriteFieldID writes a 16-bit little-endian field id.
func (w *Writer) WriteFieldID(id uint16) {
	w.buf = binary.LittleEndian.AppendUint16(w.buf, id)
}

// WriteEndObject writes the FieldEnd terminator.
func (w *Writer) WriteEndObject() { w.WriteFieldID(FieldEnd) }

// WriteObject runs fn to write the body, then appends the FieldEnd marker.
func (w *Writer) WriteObject(fn func(*Writer)) {
	fn(w)
	w.WriteEndObject()
}

// WriteField writes a field id followed by the body produced by fn.
func (w *Writer) WriteField(id uint16, fn func(*Writer)) {
	w.WriteFieldID(id)
	fn(w)
}

// ReadStringWithDefault reads property id into *dst when it is present (a
// property DuckDB writes "with default").
func (r *Reader) ReadStringWithDefault(id uint16, dst *string) error {
	_, err := r.ReadOptionalField(id, func(r *Reader) (err error) {
		*dst, err = r.ReadString()
		return err
	})
	return err
}

// ReadUintWithDefault reads ULEB property id into *dst when it is present.
func (r *Reader) ReadUintWithDefault(id uint16, dst *uint64) error {
	_, err := r.ReadOptionalField(id, func(r *Reader) (err error) {
		*dst, err = r.ReadULEB128()
		return err
	})
	return err
}

// WriteStringWithDefault writes property id unless s is empty, like DuckDB's
// WritePropertyWithDefault.
func (w *Writer) WriteStringWithDefault(id uint16, s string) {
	if s != "" {
		w.WriteField(id, func(w *Writer) { w.WriteString(s) })
	}
}

// WriteUintWithDefault writes ULEB property id unless v is 0.
func (w *Writer) WriteUintWithDefault(id uint16, v uint64) {
	if v != 0 {
		w.WriteField(id, func(w *Writer) { w.WriteULEB128(v) })
	}
}
