package message

import (
	"math/big"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// safetyCorpus returns one well-formed encoding of every message type,
// including chunk-carrying ones, used to derive malformed inputs.
func safetyCorpus(tb testing.TB) map[string][]byte {
	tb.Helper()
	chunk := DataChunk{
		RowCount: 2,
		Types:    []logicaltype.Type{{ID: logicaltype.IDInteger}, {ID: logicaltype.IDVarchar}},
		Columns:  []Column{Values{int32(1), nil}, Values{"a", "bc"}},
	}
	nested := vectorCorpusChunk()
	bodies := map[string]struct {
		h Header
		b Body
	}{
		"connreq":   {Header{}, ConnectionRequest{AuthString: "t", ClientDuckdbVersion: "v2.0.0", ClientPlatform: "go", MinSupportedQuackVersion: 3, MaxSupportedQuackVersion: 3, ClientID: "abcd", HeartbeatTimeoutSeconds: 60}},
		"connresp":  {Header{ConnectionID: "c", HasQueryID: true, ClientQueryID: 1}, ConnectionResponse{ServerDuckdbVersion: "v2.0.0", ServerPlatform: "p", QuackVersion: 3, HeartbeatTimeoutSeconds: 60}},
		"prepreq":   {Header{ConnectionID: "c"}, PrepareRequest{SQL: "SELECT 1", QueryUUID: codec.HugeInt{Upper: 3, Lower: 4}, InlineRows: Idx(0)}},
		"prepresp":  {Header{}, PrepareResponse{ResultTypes: chunk.Types, ResultNames: []string{"i", "s"}, NeedsMoreFetch: true, Results: []DataChunk{chunk}, QueryUUID: codec.HugeInt{Lower: 1}}},
		"fetchreq":  {Header{}, FetchRequest{QueryUUID: codec.HugeInt{Lower: 1}, BatchIndex: 2, AckIndex: 1}},
		"fetchresp": {Header{}, FetchResponse{Results: []DataChunk{chunk, chunk}, BatchIndex: Idx(2)}},
		"vectors":   {Header{}, FetchResponse{Results: []DataChunk{nested}, BatchIndex: Idx(1)}},
		"senddata":  {Header{ConnectionID: "c"}, SendDataRequest{StreamID: "s", Chunks: []DataChunk{chunk}, BatchIndex: Idx(1)}},
		"sendresp":  {Header{}, SendDataResponse{}},
		"cancel":    {Header{}, CancelRequest{QueryUUID: codec.HugeInt{Lower: 1}}},
		"ack":       {Header{}, Acknowledgement{QueryUUID: codec.HugeInt{Lower: 1}}},
		"hb":        {Header{ConnectionID: "c"}, HeartbeatRequest{}},
		"success":   {Header{}, SuccessResponse{}},
		"disc":      {Header{}, DisconnectMessage{}},
		"error":     {Header{}, ErrorResponse{Message: "m", ExceptionType: "Catalog", ExtraInfo: map[string]string{"k": "v"}, MustInvalidate: true}},
	}
	out := make(map[string][]byte, len(bodies))
	for name, m := range bodies {
		buf, err := Encode(m.h, m.b)
		if err != nil {
			tb.Fatalf("%s: encode: %v", name, err)
		}
		out[name] = buf
	}
	return out
}

func decodeNoPanic(t *testing.T, label string, raw []byte) error {
	t.Helper()
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("%s: Decode panicked: %v\nbytes: % x", label, r, raw)
			}
		}()
		_, _, err = Decode(raw)
	}()
	return err
}

// TestDecodeMalformedNeverPanics: every strict prefix of a valid message is
// an error, and no single-byte change or deletion makes Decode panic.
func TestDecodeMalformedNeverPanics(t *testing.T) {
	for name, full := range safetyCorpus(t) {
		if err := decodeNoPanic(t, name, full); err != nil {
			t.Fatalf("%s: corpus entry does not decode: %v", name, err)
		}
		for i := range full {
			if err := decodeNoPanic(t, name, full[:i]); err == nil {
				t.Errorf("%s: %d-byte prefix decoded without error", name, i)
			}
			for _, v := range []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xfe, 0xff} {
				mutated := append([]byte(nil), full...)
				mutated[i] = v
				_ = decodeNoPanic(t, name, mutated)
			}
			_ = decodeNoPanic(t, name, append(append([]byte(nil), full[:i]...), full[i+1:]...))
		}
	}
}

// vectorCorpusChunk is a chunk exercising every vector code path the encoder
// writes: strings (2.0 layout), nested types, wide fixed-size types and
// NULLs at every level.
func vectorCorpusChunk() DataChunk {
	types := []logicaltype.Type{
		listOf(tp(logicaltype.IDVarchar)),
		structOf(logicaltype.IDStruct, field("a", tp(logicaltype.IDInteger)), field("b", tp(logicaltype.IDBlob))),
		decimalOf(20, 4),
		tp(logicaltype.IDUUID),
		tp(logicaltype.IDBit),
		mapOf(tp(logicaltype.IDVarchar), tp(logicaltype.IDBignum)),
		arrayOf(tp(logicaltype.IDSmallint), 2),
		unionOf(field("i", tp(logicaltype.IDInteger)), field("s", tp(logicaltype.IDVarchar))),
		tp(logicaltype.IDGeometry),
		enumOf("x", "y"),
	}
	cols := []Column{
		Values{[]any{"a", nil, ""}, nil},
		Values{map[string]any{"a": int32(1), "b": []byte{1}}, nil},
		Values{"-1.2345", nil},
		Values{"550e8400-e29b-41d4-a716-446655440000", nil},
		Values{"0110", nil},
		Values{[]any{map[string]any{"key": "k", "value": big.NewInt(-300)}}, nil},
		Values{[]any{int16(1), nil}, nil},
		Values{map[string]any{"s": "u"}, nil},
		Values{[]byte{1, 2}, nil},
		Values{"y", nil},
	}
	return DataChunk{RowCount: 2, Types: types, Columns: cols}
}
