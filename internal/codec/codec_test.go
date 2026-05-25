package codec

import (
	"bytes"
	"math"
	"testing"
)

func TestULEB128RoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 0x7f, 0x80, 0x3fff, 0x4000, 1 << 21, math.MaxUint32, math.MaxUint64 - 1, math.MaxUint64}
	for _, v := range cases {
		w := NewWriter()
		w.WriteULEB128(v)
		r := NewReader(w.Bytes())
		got, err := r.ReadULEB128()
		if err != nil {
			t.Fatalf("read %d: %v", v, err)
		}
		if got != v {
			t.Fatalf("ULEB128 round-trip: want %d, got %d", v, got)
		}
		if err := r.AssertEOF(); err != nil {
			t.Fatalf("trailing bytes for %d: %v", v, err)
		}
	}
}

func TestULEB128KnownBytes(t *testing.T) {
	// 624485 = 0xE5 8E 26 in ULEB128 (classic spec example)
	w := NewWriter()
	w.WriteULEB128(624485)
	if !bytes.Equal(w.Bytes(), []byte{0xe5, 0x8e, 0x26}) {
		t.Fatalf("encoding mismatch: %#v", w.Bytes())
	}
}

func TestSLEB128RoundTrip(t *testing.T) {
	cases := []int64{0, 1, -1, 63, 64, -63, -64, -65, 0x3fff, -0x4000, math.MaxInt32, math.MinInt32, math.MaxInt64, math.MinInt64}
	for _, v := range cases {
		w := NewWriter()
		w.WriteSLEB128(v)
		r := NewReader(w.Bytes())
		got, err := r.ReadSLEB128()
		if err != nil {
			t.Fatalf("read %d: %v", v, err)
		}
		if got != v {
			t.Fatalf("SLEB128 round-trip: want %d, got %d", v, got)
		}
		if err := r.AssertEOF(); err != nil {
			t.Fatalf("trailing bytes for %d: %v", v, err)
		}
	}
}

func TestSLEB128KnownBytes(t *testing.T) {
	// -123456 = 0xC0 BB 78 in SLEB128 (classic spec example)
	w := NewWriter()
	w.WriteSLEB128(-123456)
	if !bytes.Equal(w.Bytes(), []byte{0xc0, 0xbb, 0x78}) {
		t.Fatalf("encoding mismatch: %#v", w.Bytes())
	}
}

func TestStringAndBlob(t *testing.T) {
	w := NewWriter()
	w.WriteString("hello, 🦆")
	w.WriteBlob([]byte{0x00, 0xff, 0xaa, 0x55})
	w.WriteString("")
	w.WriteBlob(nil)

	r := NewReader(w.Bytes())
	if got, err := r.ReadString(); err != nil || got != "hello, 🦆" {
		t.Fatalf("string: got %q err %v", got, err)
	}
	if got, err := r.ReadBlob(); err != nil || !bytes.Equal(got, []byte{0x00, 0xff, 0xaa, 0x55}) {
		t.Fatalf("blob: got %#v err %v", got, err)
	}
	if got, err := r.ReadString(); err != nil || got != "" {
		t.Fatalf("empty string: got %q err %v", got, err)
	}
	if got, err := r.ReadBlob(); err != nil || len(got) != 0 {
		t.Fatalf("empty blob: got %#v err %v", got, err)
	}
}

func TestFixedWidth(t *testing.T) {
	w := NewWriter()
	w.WriteFixedInt8(-1)
	w.WriteFixedUint8(255)
	w.WriteFixedInt16(-12345)
	w.WriteFixedUint16(54321)
	w.WriteFixedInt32(-1234567890)
	w.WriteFixedUint32(0xDEADBEEF)
	w.WriteFixedInt64(math.MinInt64)
	w.WriteFixedUint64(math.MaxUint64)
	w.WriteFixedFloat32(float32(math.Pi))
	w.WriteFixedFloat64(math.Pi)

	r := NewReader(w.Bytes())
	if v, _ := r.ReadFixedInt8(); v != -1 {
		t.Fatalf("int8 mismatch: %d", v)
	}
	if v, _ := r.ReadFixedUint8(); v != 255 {
		t.Fatalf("uint8 mismatch: %d", v)
	}
	if v, _ := r.ReadFixedInt16(); v != -12345 {
		t.Fatalf("int16 mismatch: %d", v)
	}
	if v, _ := r.ReadFixedUint16(); v != 54321 {
		t.Fatalf("uint16 mismatch: %d", v)
	}
	if v, _ := r.ReadFixedInt32(); v != -1234567890 {
		t.Fatalf("int32 mismatch: %d", v)
	}
	if v, _ := r.ReadFixedUint32(); v != 0xDEADBEEF {
		t.Fatalf("uint32 mismatch: %x", v)
	}
	if v, _ := r.ReadFixedInt64(); v != math.MinInt64 {
		t.Fatalf("int64 mismatch: %d", v)
	}
	if v, _ := r.ReadFixedUint64(); v != math.MaxUint64 {
		t.Fatalf("uint64 mismatch: %d", v)
	}
	if v, _ := r.ReadFixedFloat32(); v != float32(math.Pi) {
		t.Fatalf("float32 mismatch: %v", v)
	}
	if v, _ := r.ReadFixedFloat64(); v != math.Pi {
		t.Fatalf("float64 mismatch: %v", v)
	}
	if err := r.AssertEOF(); err != nil {
		t.Fatalf("trailing bytes: %v", err)
	}
}

func TestHugeIntRoundTrip(t *testing.T) {
	cases := []HugeInt{
		{Lower: 0, Upper: 0},
		{Lower: 1, Upper: 0},
		{Lower: math.MaxUint64, Upper: 0},
		{Lower: 0, Upper: -1},
		{Lower: math.MaxUint64, Upper: math.MaxInt64},
		{Lower: 0, Upper: math.MinInt64},
	}
	for _, h := range cases {
		w := NewWriter()
		w.WriteHugeInt(h)
		r := NewReader(w.Bytes())
		got, err := r.ReadHugeInt()
		if err != nil {
			t.Fatalf("read %+v: %v", h, err)
		}
		if got != h {
			t.Fatalf("hugeint round-trip: want %+v, got %+v", h, got)
		}
	}
}

func TestObjectFraming(t *testing.T) {
	w := NewWriter()
	w.WriteObject(func(w *Writer) {
		w.WriteField(1, func(w *Writer) { w.WriteULEB128(42) })
		w.WriteField(3, func(w *Writer) { w.WriteString("duck") })
	})

	r := NewReader(w.Bytes())
	var (
		gotFirst  uint64
		gotSecond string
		gotMaybe  string
	)
	err := r.ReadObject(func(r *Reader) error {
		if err := r.ReadRequiredField(1, func(r *Reader) error {
			v, err := r.ReadULEB128()
			gotFirst = v
			return err
		}); err != nil {
			return err
		}
		if _, err := r.ReadOptionalField(2, func(r *Reader) error {
			gotMaybe = "should-not-fire"
			return nil
		}); err != nil {
			return err
		}
		return r.ReadRequiredField(3, func(r *Reader) error {
			v, err := r.ReadString()
			gotSecond = v
			return err
		})
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotFirst != 42 || gotSecond != "duck" || gotMaybe != "" {
		t.Fatalf("decoded mismatch: first=%d second=%q maybe=%q", gotFirst, gotSecond, gotMaybe)
	}
	if err := r.AssertEOF(); err != nil {
		t.Fatalf("trailing bytes: %v", err)
	}
}

func TestReadEndObjectMissing(t *testing.T) {
	r := NewReader([]byte{0x01, 0x00})
	if err := r.ReadEndObject(); err == nil {
		t.Fatalf("expected error for missing FieldEnd marker")
	}
}

func TestReadBoolStrict(t *testing.T) {
	r := NewReader([]byte{0x02})
	if _, err := r.ReadBool(); err == nil {
		t.Fatalf("expected error for invalid boolean byte")
	}
}
