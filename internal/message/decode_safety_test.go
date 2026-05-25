package message

import (
	"strings"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// buildVarcharChunk produces an on-wire DataChunk wrapper with `claimed`
// rows declared in the header but only `actual` VARCHAR slots transmitted.
// A correct decoder must reject the mismatch instead of panicking when it
// later indexes off the end of the values slice.
func buildVarcharChunk(t *testing.T, claimed, actual int) []byte {
	t.Helper()
	w := codec.NewWriter()
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(300, func(w *codec.Writer) {
			w.WriteObject(func(w *codec.Writer) {
				w.WriteField(100, func(w *codec.Writer) { w.WriteULEB128(uint64(claimed)) })
				w.WriteField(101, func(w *codec.Writer) {
					w.WriteULEB128(1)
					logicaltype.Encode(w, logicaltype.Type{ID: logicaltype.IDVarchar})
				})
				w.WriteField(102, func(w *codec.Writer) {
					w.WriteULEB128(1) // one column
					w.WriteObject(func(w *codec.Writer) {
						w.WriteField(100, func(w *codec.Writer) { w.WriteBool(false) }) // no validity
						w.WriteField(102, func(w *codec.Writer) {
							w.WriteULEB128(uint64(actual))
							for i := 0; i < actual; i++ {
								w.WriteBlob([]byte("x"))
							}
						})
					})
				})
			})
		})
	})
	return w.Bytes()
}

func TestDecodeVarcharLengthMismatchRejected(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("decoder panicked on length mismatch: %v", r)
		}
	}()

	raw := buildVarcharChunk(t, 5 /*claimed*/, 2 /*actual*/)
	r := codec.NewReader(raw)
	if _, err := decodeDataChunkWrapper(r); err == nil {
		t.Fatal("expected error for VARCHAR row count mismatch, got nil")
	} else if !strings.Contains(err.Error(), "VARCHAR vector") {
		t.Fatalf("error should mention VARCHAR vector: %v", err)
	}
}

// buildListChunkBadOffset constructs a one-row LIST<INTEGER> chunk where the
// list-entry's (offset, length) extends past the child-vector buffer. A safe
// decoder must reject this rather than panic on slice out of range.
func buildListChunkBadOffset(t *testing.T) []byte {
	t.Helper()
	w := codec.NewWriter()
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(300, func(w *codec.Writer) {
			w.WriteObject(func(w *codec.Writer) {
				w.WriteField(100, func(w *codec.Writer) { w.WriteULEB128(1) }) // row_count = 1
				child := logicaltype.Type{ID: logicaltype.IDInteger}
				listType := logicaltype.Type{
					ID:   logicaltype.IDList,
					Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraList, Child: &child},
				}
				w.WriteField(101, func(w *codec.Writer) {
					w.WriteULEB128(1)
					logicaltype.Encode(w, listType)
				})
				w.WriteField(102, func(w *codec.Writer) {
					w.WriteULEB128(1)
					w.WriteObject(func(w *codec.Writer) {
						w.WriteField(100, func(w *codec.Writer) { w.WriteBool(false) }) // no validity
						w.WriteField(104, func(w *codec.Writer) { w.WriteULEB128(0) })   // child size 0
						w.WriteField(105, func(w *codec.Writer) {
							w.WriteULEB128(1)
							w.WriteObject(func(w *codec.Writer) {
								w.WriteField(100, func(w *codec.Writer) { w.WriteULEB128(0) })   // offset 0
								w.WriteField(101, func(w *codec.Writer) { w.WriteULEB128(99) })  // length 99 — out of range
							})
						})
						w.WriteField(106, func(w *codec.Writer) {
							encodeFlatVector(w, child, []any{}, 0) // empty child vector
						})
					})
				})
			})
		})
	})
	return w.Bytes()
}

func TestDecodeListBadEntryRejected(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("decoder panicked on out-of-range LIST entry: %v", r)
		}
	}()

	raw := buildListChunkBadOffset(t)
	r := codec.NewReader(raw)
	if _, err := decodeDataChunkWrapper(r); err == nil {
		t.Fatal("expected error for LIST entry exceeding child vector, got nil")
	} else if !strings.Contains(err.Error(), "LIST entry") {
		t.Fatalf("error should mention LIST entry: %v", err)
	}
}
