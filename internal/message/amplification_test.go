package message

import (
	"runtime"
	"strings"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// Hostile servers can describe far more values than they send: a CONSTANT
// child vector of a LIST holds one row whatever list_size says, LIST
// entries may overlap, an ARRAY's size comes from its type. Materializing
// such a row (one Value call) would allocate all of it at once, and Go
// cannot recover from running out of memory, so the decoder rejects vectors
// whose rows would materialize far more values than their bytes justify.

// listEntryAll is a list_entry_t covering child rows [0, n).
func listEntryAll(n uint64) []byte { return obj(fld(100, uleb(0)), fld(101, uleb(n))) }

// constInt is a CONSTANT INTEGER child vector (one row, any count).
var constInt = obj(fld(90, 2), noNulls, fld(102, blob(le(int32(7)))))

func TestDecodeRejectsAmplifiedList(t *testing.T) {
	integer := tp(logicaltype.IDInteger)
	for _, size := range []uint64{1 << 26, 1 << 37} {
		raw := obj(noNulls, fld(104, uleb(size)), fld(105, 1, listEntryAll(size)), fld(106, constInt))
		if _, err := decodeVec(listOf(integer), 1, raw); err == nil || !strings.Contains(err.Error(), "would materialize") {
			t.Fatalf("list_size %d (%d bytes): err = %v", size, len(raw), err)
		}
	}
	// A moderate list over a CONSTANT child is still accepted (the floor).
	raw := obj(noNulls, fld(104, uleb(1000)), fld(105, 1, listEntryAll(1000)), fld(106, constInt))
	col := mustDecodeVec(t, listOf(integer), 1, raw)
	if l := col.Value(0).([]any); len(l) != 1000 || l[999] != int32(7) {
		t.Fatalf("got %d values", len(l))
	}
}

// TestDecodeRejectsOverlappingNestedLists: LIST<LIST<INTEGER>> whose outer
// row spans many inner rows that all cover the same child range.
func TestDecodeRejectsOverlappingNestedLists(t *testing.T) {
	integer := tp(logicaltype.IDInteger)
	const n = 2000
	var entries []any
	entries = append(entries, uleb(n))
	for range n {
		entries = append(entries, listEntryAll(n))
	}
	inner := obj(noNulls, fld(104, uleb(n)), fld(105, entries...), fld(106, constInt))
	raw := obj(noNulls, fld(104, uleb(n)), fld(105, 1, listEntryAll(n)), fld(106, inner))
	if _, err := decodeVec(listOf(listOf(integer)), 1, raw); err == nil || !strings.Contains(err.Error(), "would materialize") {
		t.Fatalf("%d bytes: err = %v", len(raw), err)
	}
	// Each inner row alone is fine.
	if _, err := decodeVec(listOf(integer), n, inner); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeRejectsAmplifiedArray(t *testing.T) {
	const size = 1 << 31
	raw := obj(noNulls, fld(103, uleb(size)), fld(104, constInt))
	if _, err := decodeVec(arrayOf(tp(logicaltype.IDInteger), size), 1, raw); err == nil ||
		!strings.Contains(err.Error(), "would materialize") {
		t.Fatalf("err = %v", err)
	}
}

// TestDecodeRejectsWideRows: many columns that are each within bounds can
// still make one row too large.
func TestDecodeRejectsWideRows(t *testing.T) {
	integer := tp(logicaltype.IDInteger)
	const size = 1 << 19 // half the floor per column
	list := obj(noNulls, fld(104, uleb(size)), fld(105, 1, listEntryAll(size)), fld(106, constInt))
	var types []logicaltype.Type
	var vectors [][]byte
	for range 8 {
		types = append(types, listOf(integer))
		vectors = append(vectors, list)
	}
	r := codec.NewReader(rawChunk(1, types, vectors...))
	if _, err := decodeDataChunk(r); err == nil || !strings.Contains(err.Error(), "would materialize") {
		t.Fatalf("err = %v", err)
	}
	r = codec.NewReader(rawChunk(1, types[:1], vectors[0]))
	if _, err := decodeDataChunk(r); err != nil {
		t.Fatal(err)
	}
}

// TestWireCountsDoNotPreallocate: a count from the wire is only checked
// against the remaining bytes; lists of large elements must not be
// preallocated from it (a 1 MiB response claiming 2^20 result chunks would
// otherwise allocate 56 MiB before failing on the first entry).
func TestWireCountsDoNotPreallocate(t *testing.T) {
	const n = 1 << 20
	raw := cat(uleb(n), make([]byte, n))
	for name, read := range map[string]func(r *codec.Reader) error{
		"result chunks": func(r *codec.Reader) error {
			var chunks []DataChunk
			return readList(r, &chunks, func(r *codec.Reader, _ int) (DataChunk, error) { return decodeDataChunk(r) })
		},
		"string map": func(r *codec.Reader) error { _, err := r.ReadStringMap(); return err },
	} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if err := read(codec.NewReader(raw)); err == nil {
			t.Fatalf("%s: a false count must fail", name)
		}
		runtime.ReadMemStats(&after)
		if d := after.TotalAlloc - before.TotalAlloc; d > 4*n {
			t.Errorf("%s: allocated %d bytes for a %d-byte input", name, d, len(raw))
		}
	}
}
