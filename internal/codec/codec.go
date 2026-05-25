// Package codec implements DuckDB BinarySerializer primitive read/write
// for the Quack wire protocol. Field IDs are little-endian uint16; the
// sentinel FieldEnd (0xFFFF) terminates an object. Optional/default-valued
// fields may be omitted on the wire — readers detect this via PeekFieldID.
package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// FieldEnd marks the end of a BinarySerializer object.
const FieldEnd uint16 = 0xFFFF

// SerializationCompatibilityIndex is the SerializationCompatibility::FromIndex
// value the Quack server expects. Pinned for now; bump when the protocol
// negotiates a new compatibility version.
const SerializationCompatibilityIndex = 7

// HugeInt mirrors DuckDB's hugeint_t: a 128-bit value stored as a signed
// upper word + unsigned lower word. Wire order: SLEB upper, ULEB lower.
type HugeInt struct {
	Lower uint64
	Upper int64
}

// Reader decodes BinarySerializer primitives from a byte slice.
type Reader struct {
	buf []byte
	pos int
}

// NewReader returns a Reader over buf. The slice is not copied.
func NewReader(buf []byte) *Reader { return &Reader{buf: buf} }

// Pos returns the current byte offset.
func (r *Reader) Pos() int { return r.pos }

// Remaining returns the number of unread bytes.
func (r *Reader) Remaining() int { return len(r.buf) - r.pos }

// EOF reports whether all bytes have been consumed.
func (r *Reader) EOF() bool { return r.pos >= len(r.buf) }

// AssertEOF returns an error if any bytes remain.
func (r *Reader) AssertEOF() error {
	if r.pos != len(r.buf) {
		return fmt.Errorf("codec: %d trailing byte(s) at offset %d", len(r.buf)-r.pos, r.pos)
	}
	return nil
}

func (r *Reader) need(n int) error {
	if r.pos+n > len(r.buf) {
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
// shares the underlying array — callers must copy before mutation.
func (r *Reader) ReadBytes(n int) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("codec: negative read length %d", n)
	}
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
	for i := 0; i < 10; i++ {
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
	for i := 0; i < 10; i++ {
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
	n, err := r.ReadULEB128()
	if err != nil {
		return "", err
	}
	b, err := r.ReadBytes(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadBlob reads a ULEB-length-prefixed binary blob. The result is a copy.
func (r *Reader) ReadBlob() ([]byte, error) {
	n, err := r.ReadULEB128()
	if err != nil {
		return nil, err
	}
	b, err := r.ReadBytes(int(n))
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(b))
	copy(out, b)
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

// ReadFixedInt8 reads one signed byte.
func (r *Reader) ReadFixedInt8() (int8, error) {
	b, err := r.ReadByte()
	return int8(b), err
}

// ReadFixedUint8 reads one unsigned byte.
func (r *Reader) ReadFixedUint8() (uint8, error) { return r.ReadByte() }

// ReadFixedInt16 reads a signed 16-bit little-endian integer.
func (r *Reader) ReadFixedInt16() (int16, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	v := int16(binary.LittleEndian.Uint16(r.buf[r.pos:]))
	r.pos += 2
	return v, nil
}

// ReadFixedUint16 reads an unsigned 16-bit little-endian integer.
func (r *Reader) ReadFixedUint16() (uint16, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint16(r.buf[r.pos:])
	r.pos += 2
	return v, nil
}

// ReadFixedInt32 reads a signed 32-bit little-endian integer.
func (r *Reader) ReadFixedInt32() (int32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := int32(binary.LittleEndian.Uint32(r.buf[r.pos:]))
	r.pos += 4
	return v, nil
}

// ReadFixedUint32 reads an unsigned 32-bit little-endian integer.
func (r *Reader) ReadFixedUint32() (uint32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint32(r.buf[r.pos:])
	r.pos += 4
	return v, nil
}

// ReadFixedInt64 reads a signed 64-bit little-endian integer.
func (r *Reader) ReadFixedInt64() (int64, error) {
	if err := r.need(8); err != nil {
		return 0, err
	}
	v := int64(binary.LittleEndian.Uint64(r.buf[r.pos:]))
	r.pos += 8
	return v, nil
}

// ReadFixedUint64 reads an unsigned 64-bit little-endian integer.
func (r *Reader) ReadFixedUint64() (uint64, error) {
	if err := r.need(8); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint64(r.buf[r.pos:])
	r.pos += 8
	return v, nil
}

// ReadFixedFloat32 reads an IEEE-754 32-bit little-endian float.
func (r *Reader) ReadFixedFloat32() (float32, error) {
	bits, err := r.ReadFixedUint32()
	return math.Float32frombits(bits), err
}

// ReadFixedFloat64 reads an IEEE-754 64-bit little-endian float.
func (r *Reader) ReadFixedFloat64() (float64, error) {
	bits, err := r.ReadFixedUint64()
	return math.Float64frombits(bits), err
}

// ReadFieldID reads the next 16-bit little-endian field id.
func (r *Reader) ReadFieldID() (uint16, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint16(r.buf[r.pos:])
	r.pos += 2
	return v, nil
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
	id, err := r.ReadFieldID()
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
	actual, err := r.ReadFieldID()
	if err != nil {
		return err
	}
	if actual != fieldID {
		return fmt.Errorf("codec: expected field 0x%04x at offset %d, got 0x%04x", fieldID, r.pos-2, actual)
	}
	return fn(r)
}

// ReadOptionalField invokes fn iff the next field id equals fieldID, otherwise
// returns false (caller falls back to a default). Returns (true, err) when
// fn ran, or (false, nil) when the field was absent.
func (r *Reader) ReadOptionalField(fieldID uint16, fn func(*Reader) error) (bool, error) {
	id, err := r.PeekFieldID()
	if err != nil {
		return false, err
	}
	if id != fieldID {
		return false, nil
	}
	if _, err := r.ReadFieldID(); err != nil {
		return false, err
	}
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

// Bytes returns the accumulated buffer. The slice is owned by the Writer and
// must not be mutated after subsequent writes.
func (w *Writer) Bytes() []byte { return w.buf }

// Len returns the number of bytes written so far.
func (w *Writer) Len() int { return len(w.buf) }

// WriteByte appends one byte. The error return is always nil; it exists to
// satisfy io.ByteWriter so callers can pass *Writer where that interface is
// expected.
func (w *Writer) WriteByte(b byte) error { w.buf = append(w.buf, b); return nil }

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

// WriteHugeInt writes a hugeint as [SLEB upper][ULEB lower].
func (w *Writer) WriteHugeInt(h HugeInt) {
	w.WriteSLEB128(h.Upper)
	w.WriteULEB128(h.Lower)
}

// WriteFixedInt8 writes a signed byte.
func (w *Writer) WriteFixedInt8(v int8) { w.buf = append(w.buf, byte(v)) }

// WriteFixedUint8 writes an unsigned byte.
func (w *Writer) WriteFixedUint8(v uint8) { w.buf = append(w.buf, v) }

// WriteFixedInt16 writes a signed 16-bit little-endian integer.
func (w *Writer) WriteFixedInt16(v int16) {
	w.buf = binary.LittleEndian.AppendUint16(w.buf, uint16(v))
}

// WriteFixedUint16 writes an unsigned 16-bit little-endian integer.
func (w *Writer) WriteFixedUint16(v uint16) {
	w.buf = binary.LittleEndian.AppendUint16(w.buf, v)
}

// WriteFixedInt32 writes a signed 32-bit little-endian integer.
func (w *Writer) WriteFixedInt32(v int32) {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, uint32(v))
}

// WriteFixedUint32 writes an unsigned 32-bit little-endian integer.
func (w *Writer) WriteFixedUint32(v uint32) {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, v)
}

// WriteFixedInt64 writes a signed 64-bit little-endian integer.
func (w *Writer) WriteFixedInt64(v int64) {
	w.buf = binary.LittleEndian.AppendUint64(w.buf, uint64(v))
}

// WriteFixedUint64 writes an unsigned 64-bit little-endian integer.
func (w *Writer) WriteFixedUint64(v uint64) {
	w.buf = binary.LittleEndian.AppendUint64(w.buf, v)
}

// WriteFixedFloat32 writes an IEEE-754 32-bit little-endian float.
func (w *Writer) WriteFixedFloat32(v float32) {
	w.WriteFixedUint32(math.Float32bits(v))
}

// WriteFixedFloat64 writes an IEEE-754 64-bit little-endian float.
func (w *Writer) WriteFixedFloat64(v float64) {
	w.WriteFixedUint64(math.Float64bits(v))
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
