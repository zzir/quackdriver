package codec

import (
	"bytes"
	"math"
	"testing"
)

func TestLEB128(t *testing.T) {
	for _, v := range []uint64{0, 1, 0x7f, 0x80, 0x3fff, 0x4000, 1 << 21, math.MaxUint32, math.MaxUint64} {
		w := NewWriter()
		w.WriteULEB128(v)
		r := NewReader(w.Bytes())
		if got, err := r.ReadULEB128(); err != nil || got != v || r.AssertEOF() != nil {
			t.Errorf("ULEB128 %d: got %d, %v", v, got, err)
		}
	}
	for _, v := range []int64{0, 1, -1, 63, 64, -64, -65, 0x3fff, -0x4000, math.MaxInt32, math.MinInt32, math.MaxInt64, math.MinInt64} {
		w := NewWriter()
		w.WriteSLEB128(v)
		r := NewReader(w.Bytes())
		if got, err := r.ReadSLEB128(); err != nil || got != v || r.AssertEOF() != nil {
			t.Errorf("SLEB128 %d: got %d, %v", v, got, err)
		}
	}
	for _, h := range []HugeInt{{}, {Lower: math.MaxUint64}, {Upper: -1}, {Lower: math.MaxUint64, Upper: math.MaxInt64}, {Upper: math.MinInt64}} {
		w := NewWriter()
		w.WriteHugeInt(h)
		if got, err := NewReader(w.Bytes()).ReadHugeInt(); err != nil || got != h {
			t.Errorf("hugeint %+v: got %+v, %v", h, got, err)
		}
	}
	// The classic spec examples.
	w := NewWriter()
	w.WriteULEB128(624485)
	w.WriteSLEB128(-123456)
	if !bytes.Equal(w.Bytes(), []byte{0xe5, 0x8e, 0x26, 0xc0, 0xbb, 0x78}) {
		t.Errorf("LEB128 bytes % x", w.Bytes())
	}
}

func TestStringAndBlob(t *testing.T) {
	w := NewWriter()
	w.WriteString("hello, 🦆")
	w.WriteBlob([]byte{0x00, 0xff})
	w.WriteString("")
	r := NewReader(w.Bytes())
	if got, err := r.ReadString(); err != nil || got != "hello, 🦆" {
		t.Fatalf("string: %q %v", got, err)
	}
	if got, err := r.ReadBlob(); err != nil || !bytes.Equal(got, []byte{0x00, 0xff}) {
		t.Fatalf("blob: %#v %v", got, err)
	}
	if got, err := r.ReadString(); err != nil || got != "" {
		t.Fatalf("empty string: %q %v", got, err)
	}
}

func TestObjectFraming(t *testing.T) {
	w := NewWriter()
	w.WriteObject(func(w *Writer) {
		w.WriteField(1, func(w *Writer) { w.WriteULEB128(42) })
		w.WriteStringWithDefault(2, "")
		w.WriteUintWithDefault(3, 0)
		w.WriteStringWithDefault(4, "duck")
		w.WriteUintWithDefault(5, 7)
	})
	if want := []byte{1, 0, 42, 4, 0, 4, 'd', 'u', 'c', 'k', 5, 0, 7, 0xff, 0xff}; !bytes.Equal(w.Bytes(), want) {
		t.Fatalf("got % x, want % x", w.Bytes(), want)
	}
	r := NewReader(w.Bytes())
	var first, absentUint, last uint64
	var absent, s string
	err := r.ReadObject(func(r *Reader) error {
		if err := r.ReadRequiredField(1, func(r *Reader) (err error) {
			first, err = r.ReadULEB128()
			return err
		}); err != nil {
			return err
		}
		if err := r.ReadStringWithDefault(2, &absent); err != nil {
			return err
		}
		if err := r.ReadUintWithDefault(3, &absentUint); err != nil {
			return err
		}
		if err := r.ReadStringWithDefault(4, &s); err != nil {
			return err
		}
		return r.ReadUintWithDefault(5, &last)
	})
	if err != nil || r.AssertEOF() != nil || first != 42 || absent != "" || absentUint != 0 || s != "duck" || last != 7 {
		t.Fatalf("decoded %d %q %d %q %d, %v", first, absent, absentUint, s, last, err)
	}
}

func TestOptionalIdx(t *testing.T) {
	for _, c := range []struct {
		in   OptionalIdx
		want []byte
	}{
		{OptionalIdx{}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}},
		{OptionalIdx{Value: 0, Valid: true}, []byte{0x00}},
		{OptionalIdx{Value: 300, Valid: true}, []byte{0xac, 0x02}},
		{OptionalIdx{Value: math.MaxUint64 - 1, Valid: true}, []byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}},
	} {
		w := NewWriter()
		if err := w.WriteOptionalIdx(c.in); err != nil || !bytes.Equal(w.Bytes(), c.want) {
			t.Errorf("write %+v: % x, %v", c.in, w.Bytes(), err)
		}
		if got, err := NewReader(c.want).ReadOptionalIdx(); err != nil || got != c.in {
			t.Errorf("read %+v: %+v, %v", c.in, got, err)
		}
	}
	if err := NewWriter().WriteOptionalIdx(OptionalIdx{Value: InvalidIndex, Valid: true}); err == nil {
		t.Error("a valid optional_idx holding INVALID_INDEX was written")
	}
}

func TestStringMap(t *testing.T) {
	w := NewWriter()
	w.WriteStringMap(map[string]string{"b": "2", "a": "1"})
	want := []byte{
		0x02,
		0x00, 0x00, 0x01, 'a', 0x01, 0x00, 0x01, '1', 0xff, 0xff,
		0x00, 0x00, 0x01, 'b', 0x01, 0x00, 0x01, '2', 0xff, 0xff,
	}
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatalf("got % x, want % x", w.Bytes(), want)
	}
	if got, err := NewReader(want).ReadStringMap(); err != nil || len(got) != 2 || got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("decoded %v, %v", got, err)
	}
	// A repeated key keeps the last value, like DuckDB's deserializer.
	dup := []byte{
		0x02,
		0x00, 0x00, 0x01, 'k', 0x01, 0x00, 0x01, '1', 0xff, 0xff,
		0x00, 0x00, 0x01, 'k', 0x01, 0x00, 0x01, '2', 0xff, 0xff,
	}
	if got, err := NewReader(dup).ReadStringMap(); err != nil || len(got) != 1 || got["k"] != "2" {
		t.Fatalf("duplicate key: %v, %v", got, err)
	}
}

// TestMalformedInput: every read of untrusted bytes fails cleanly, and no
// length or count can overflow a bounds check or force an allocation.
func TestMalformedInput(t *testing.T) {
	huge := NewWriter()
	huge.WriteULEB128(math.MaxInt64)
	cases := map[string]func() error{
		"missing end marker": func() error { return NewReader([]byte{0x01, 0x00}).ReadEndObject() },
		"bool 2":             func() error { _, err := NewReader([]byte{0x02}).ReadBool(); return err },
		"ULEB over 64 bits":  func() error { _, err := NewReader(bytes.Repeat([]byte{0x80}, 11)).ReadULEB128(); return err },
		"huge string":        func() error { _, err := NewReader(huge.Bytes()).ReadString(); return err },
		"huge blob":          func() error { _, err := NewReader(huge.Bytes()).ReadBlob(); return err },
		"huge count":         func() error { _, err := NewReader(huge.Bytes()).ReadCount(); return err },
		"huge raw read":      func() error { _, err := NewReader([]byte{0}).ReadBytes(math.MaxInt); return err },
		"wrong field":        func() error { return NewReader([]byte{2, 0}).ReadRequiredField(1, nil) },
		"map truncated":      func() error { _, err := NewReader([]byte{0x01, 0x00, 0x00, 0x01}).ReadStringMap(); return err },
		"map wrong key id": func() error {
			_, err := NewReader([]byte{0x01, 0x01, 0x00, 0x01, 'k', 0x01, 0x00, 0x01, 'v', 0xff, 0xff}).ReadStringMap()
			return err
		},
		"map missing value": func() error {
			_, err := NewReader([]byte{0x01, 0x00, 0x00, 0x01, 'k', 0xff, 0xff}).ReadStringMap()
			return err
		},
	}
	for name, read := range cases {
		if err := read(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	r := NewReader([]byte{0x02, 0xaa, 0xbb})
	if n, err := r.ReadCount(); err != nil || n != 2 {
		t.Errorf("count that fits: %d %v", n, err)
	}
}

func TestReaderSince(t *testing.T) {
	r := NewReader([]byte{1, 2, 3, 4})
	if _, err := r.ReadByte(); err != nil {
		t.Fatal(err)
	}
	start := r.Pos()
	if _, err := r.ReadBytes(2); err != nil {
		t.Fatal(err)
	}
	if got := r.Since(start); !bytes.Equal(got, []byte{2, 3}) || cap(got) != 2 {
		t.Fatalf("Since = %v (cap %d)", got, cap(got))
	}
}

func TestNewWriterSize(t *testing.T) {
	w := NewWriterSize(64)
	if len(w.Bytes()) != 0 || cap(w.Bytes()) < 64 {
		t.Fatalf("NewWriterSize(64): len %d cap %d", len(w.Bytes()), cap(w.Bytes()))
	}
	if w := NewWriterSize(-1); len(w.Bytes()) != 0 {
		t.Fatalf("NewWriterSize(-1): len %d", len(w.Bytes()))
	}
	// The capacity is a hint: the output is that of a fresh Writer, and the
	// buffer grows past it.
	want := NewWriter()
	for _, w := range []*Writer{NewWriterSize(0), NewWriterSize(3), want} {
		w.WriteULEB128(300)
		w.WriteRawString("raw")
		w.WriteBytes([]byte{1, 2})
		w.WriteString("s")
		w.WriteBlob(bytes.Repeat([]byte{7}, 100))
	}
	for _, n := range []int{0, 3} {
		w := NewWriterSize(n)
		w.WriteULEB128(300)
		w.WriteRawString("raw")
		w.WriteBytes([]byte{1, 2})
		w.WriteString("s")
		w.WriteBlob(bytes.Repeat([]byte{7}, 100))
		if !bytes.Equal(w.Bytes(), want.Bytes()) {
			t.Fatalf("NewWriterSize(%d) wrote % x, want % x", n, w.Bytes(), want.Bytes())
		}
	}
	// WriteRawString is WriteBytes for a string: no length prefix.
	w = NewWriter()
	w.WriteRawString("ab")
	if !bytes.Equal(w.Bytes(), []byte("ab")) {
		t.Fatalf("WriteRawString: % x", w.Bytes())
	}
}
