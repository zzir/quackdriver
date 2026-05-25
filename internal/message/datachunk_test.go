package message

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

func TestDataChunkFlatRoundTrip(t *testing.T) {
	chunk := DataChunk{
		RowCount: 3,
		Types: []logicaltype.Type{
			{ID: logicaltype.IDInteger},
			{ID: logicaltype.IDVarchar},
			{ID: logicaltype.IDBoolean},
		},
		Columns: [][]any{
			{int32(1), int32(2), nil},
			{"alpha", "beta", "gamma"},
			{true, nil, false},
		},
	}

	w := codec.NewWriter()
	encodeDataChunkWrapper(w, chunk)
	enc := w.Bytes()

	r := codec.NewReader(enc)
	out, err := decodeDataChunkWrapper(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := r.AssertEOF(); err != nil {
		t.Fatalf("trailing bytes: %v", err)
	}
	if out.RowCount != chunk.RowCount {
		t.Fatalf("row count: %d", out.RowCount)
	}
	if !reflect.DeepEqual(out.Columns, chunk.Columns) {
		t.Fatalf("columns mismatch:\nwant %+v\ngot  %+v", chunk.Columns, out.Columns)
	}

	// Byte-stable round trip.
	w2 := codec.NewWriter()
	encodeDataChunkWrapper(w2, out)
	if !bytes.Equal(enc, w2.Bytes()) {
		t.Fatalf("re-encode differs:\nfirst:  %x\nsecond: %x", enc, w2.Bytes())
	}
}

func TestDataChunkAllNull(t *testing.T) {
	chunk := DataChunk{
		RowCount: 4,
		Types:    []logicaltype.Type{{ID: logicaltype.IDBigint}},
		Columns:  [][]any{{nil, nil, nil, nil}},
	}
	w := codec.NewWriter()
	encodeDataChunkWrapper(w, chunk)
	r := codec.NewReader(w.Bytes())
	out, err := decodeDataChunkWrapper(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i, v := range out.Columns[0] {
		if v != nil {
			t.Fatalf("row %d: expected nil, got %v", i, v)
		}
	}
}

func TestDataChunkPrepareResponseRoundTrip(t *testing.T) {
	resp := PrepareResponse{
		ResultTypes: []logicaltype.Type{{ID: logicaltype.IDInteger}, {ID: logicaltype.IDVarchar}},
		ResultNames: []string{"id", "name"},
		Results: []DataChunk{{
			RowCount: 2,
			Types:    []logicaltype.Type{{ID: logicaltype.IDInteger}, {ID: logicaltype.IDVarchar}},
			Columns: [][]any{
				{int32(10), int32(20)},
				{"foo", "bar"},
			},
		}},
		ResultUUID:     codec.HugeInt{Lower: 0xABCD, Upper: 0},
		NeedsMoreFetch: false,
	}
	buf, err := Encode(Header{ConnectionID: "c1"}, resp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, body, err := Decode(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := body.(PrepareResponse)
	if len(got.Results) != 1 {
		t.Fatalf("results len: %d", len(got.Results))
	}
	if got.Results[0].RowCount != 2 {
		t.Fatalf("row count: %d", got.Results[0].RowCount)
	}
	if !reflect.DeepEqual(got.Results[0].Columns, resp.Results[0].Columns) {
		t.Fatalf("columns mismatch")
	}
}
