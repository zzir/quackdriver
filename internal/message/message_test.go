package message

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// The expected byte strings in this file are derived by hand from
// duckdb-quack f96420e src/serialize_quack_message.cpp and DuckDB v2.0.0's
// BinarySerializer:
//   - an object is a sequence of (u16 LE field id, value) pairs terminated by
//     0xFFFF; WriteProperty always writes the pair, WritePropertyWithDefault
//     skips it when the value equals the default;
//   - idx_t / enums are ULEB128, hugeint_t is SLEB128(upper) ULEB128(lower),
//     bool is one byte, string is ULEB128 length + bytes;
//   - optional_idx is a ULEB128 idx_t with 2^64-1 (ff ff ff ff ff ff ff ff ff 01)
//     meaning unset;
//   - unordered_map<string,string> is a ULEB128 count then {0: key, 1: value}
//     objects;
//   - a message is the MessageHeader object, then the body object, then (for
//     FETCH_RESPONSE / SEND_DATA_REQUEST) chunk_count raw DataChunk objects.

// cat concatenates test bytes: int → one byte, string → raw bytes, []byte as is.
func cat(parts ...any) []byte {
	var out []byte
	for _, p := range parts {
		switch v := p.(type) {
		case int:
			out = append(out, byte(v))
		case string:
			out = append(out, v...)
		case []byte:
			out = append(out, v...)
		default:
			panic("cat: unsupported part")
		}
	}
	return out
}

// str is a short (< 128 byte) length-prefixed string.
func str(s string) []byte { return append([]byte{byte(len(s))}, s...) }

var (
	end   = []byte{0xff, 0xff}
	unset = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01} // optional_idx()
)

// hdr builds a MessageHeader object without a client query id.
func hdr(typ Type, connID string) []byte {
	out := cat(0x01, 0x00, int(typ))
	if connID != "" {
		out = cat(out, 0x02, 0x00, str(connID))
	}
	return cat(out, 0x03, 0x00, unset, end)
}

func mustEncode(t *testing.T, h Header, b Body) []byte {
	t.Helper()
	buf, err := Encode(h, b)
	if err != nil {
		t.Fatalf("encode %T: %v", b, err)
	}
	return buf
}

func mustDecode(t *testing.T, buf []byte) (Header, Body) {
	t.Helper()
	h, b, err := Decode(buf)
	if err != nil {
		t.Fatalf("decode: %v\nbytes: % x", err, buf)
	}
	return h, b
}

func intChunk(values ...int32) DataChunk {
	col := make([]any, len(values))
	for i, v := range values {
		col[i] = v
	}
	return DataChunk{
		RowCount: len(values),
		Types:    []logicaltype.Type{{ID: logicaltype.IDInteger}},
		Columns:  []Column{Values(col)},
	}
}

// chunkBytes is the raw DataChunk object for c, as produced by the vector
// encoder (its layout is tested separately in datachunk_test.go).
func chunkBytes(t *testing.T, c DataChunk) []byte {
	t.Helper()
	w := codec.NewWriter()
	if err := writeDataChunk(w, c); err != nil {
		t.Fatalf("writeDataChunk: %v", err)
	}
	return w.Bytes()
}

// TestEncodeExactBytes covers encodings the golden fixtures (the requests
// the driver sent to a real DuckDB 2.0 server, and its responses) do not
// pin byte for byte.
func TestEncodeExactBytes(t *testing.T) {
	cases := []struct {
		name string
		h    Header
		body Body
		want []byte
	}{
		{
			name: "CONNECTION_REQUEST all defaults",
			body: ConnectionRequest{},
			want: cat(hdr(TypeConnectionRequest, ""), end),
		},
		{
			name: "CONNECTION_RESPONSE with header ids",
			h:    Header{ConnectionID: "c1", HasQueryID: true, ClientQueryID: 7},
			body: ConnectionResponse{ServerDuckdbVersion: "v2.0.0", ServerPlatform: "linux_amd64", QuackVersion: 3, HeartbeatTimeoutSeconds: 60},
			want: cat(0x01, 0x00, 0x02, 0x02, 0x00, str("c1"), 0x03, 0x00, 0x07, end,
				0x01, 0x00, str("v2.0.0"),
				0x02, 0x00, str("linux_amd64"),
				0x03, 0x00, 0x03,
				0x04, 0x00, 0x3c,
				end),
		},
		{
			name: "PREPARE_REQUEST zero uuid and empty SQL still write query_uuid",
			body: PrepareRequest{},
			want: cat(hdr(TypePrepareRequest, ""), 0x02, 0x00, 0x00, 0x00, end),
		},
		{
			name: "PREPARE_RESPONSE schema only",
			body: PrepareResponse{
				ResultTypes:    []logicaltype.Type{{ID: logicaltype.IDInteger}},
				ResultNames:    []string{"i"},
				NeedsMoreFetch: true,
				QueryUUID:      codec.HugeInt{Lower: 5},
			},
			want: cat(hdr(TypePrepareResponse, ""),
				0x01, 0x00, 0x01, 0x64, 0x00, 0x0d, end,
				0x02, 0x00, 0x01, str("i"),
				0x03, 0x00, 0x01,
				0x05, 0x00, 0x00, 0x05,
				end),
		},
		{
			name: "FETCH_RESPONSE all defaults",
			body: FetchResponse{},
			want: cat(hdr(TypeFetchResponse, ""), end),
		},
		{
			name: "SEND_DATA_RESPONSE with budget",
			body: SendDataResponse{AcceptBudget: Idx(300)},
			want: cat(hdr(TypeSendDataResponse, ""), 0x01, 0x00, 0xac, 0x02, end),
		},
		{
			name: "ACKNOWLEDGEMENT upper only",
			body: Acknowledgement{QueryUUID: codec.HugeInt{Upper: 1}},
			want: cat(hdr(TypeAcknowledgement, ""), 0x01, 0x00, 0x01, 0x00, end),
		},
		{
			name: "ERROR_RESPONSE all fields",
			body: ErrorResponse{Message: "boom", ExceptionType: "Catalog", ExtraInfo: map[string]string{"name": "t"}, MustInvalidate: true},
			want: cat(hdr(TypeErrorResponse, ""),
				0x01, 0x00, str("boom"),
				0x02, 0x00, str("Catalog"),
				0x03, 0x00, 0x01, 0x00, 0x00, str("name"), 0x01, 0x00, str("t"), end,
				0x04, 0x00, 0x01,
				end),
		},
		{
			name: "ERROR_RESPONSE message only (empty map omitted)",
			body: ErrorResponse{Message: "boom", ExtraInfo: map[string]string{}},
			want: cat(hdr(TypeErrorResponse, ""), 0x01, 0x00, str("boom"), end),
		},
	}
	for _, c := range cases {
		got := mustEncode(t, c.h, c.body)
		if !bytes.Equal(got, c.want) {
			t.Errorf("%s:\n got  % x\n want % x", c.name, got, c.want)
			continue
		}
		// Decoding must give back the same message and re-encode identically.
		h, body := mustDecode(t, got)
		if h.Type != c.body.MessageType() {
			t.Errorf("%s: header type %s", c.name, h.Type)
		}
		again := mustEncode(t, h, body)
		if !bytes.Equal(again, got) {
			t.Errorf("%s: re-encode differs:\n got  % x\n want % x", c.name, again, got)
		}
	}
}

func TestRoundTripStructs(t *testing.T) {
	cases := []struct {
		h    Header
		body Body
	}{
		{Header{}, ConnectionRequest{AuthString: "a", ClientDuckdbVersion: "v2.0.0", ClientPlatform: "p",
			MinSupportedQuackVersion: 3, MaxSupportedQuackVersion: 3, ClientID: "client-1", HeartbeatTimeoutSeconds: 1 << 40}},
		{Header{ConnectionID: "x", HasQueryID: true, ClientQueryID: 0}, ConnectionResponse{QuackVersion: 3, HeartbeatTimeoutSeconds: 60}},
		{Header{ConnectionID: "x"}, PrepareRequest{SQL: "SELECT 'ü'", QueryUUID: NewQueryUUID(), InlineRows: Idx(24576)}},
		{Header{}, FetchRequest{QueryUUID: codec.HugeInt{Upper: -5, Lower: 7}, BatchIndex: 1 << 33, AckIndex: 1<<33 - 1}},
		{Header{}, FetchResponse{TotalBatches: Idx(0)}},
		{Header{}, SendDataRequest{StreamID: "s", TotalBatches: Idx(1 << 50)}},
		{Header{}, SendDataResponse{AcceptBudget: Idx(0)}},
		{Header{}, CancelRequest{QueryUUID: codec.HugeInt{Upper: -1, Lower: 1<<64 - 1}}},
		{Header{}, Acknowledgement{QueryUUID: codec.HugeInt{Lower: 1}}},
		{Header{}, HeartbeatRequest{}},
		{Header{}, ErrorResponse{Message: "m", ExceptionType: "Invalid Input", ExtraInfo: map[string]string{"a": "1", "b": "", "": "c"}}},
	}
	for _, c := range cases {
		buf := mustEncode(t, c.h, c.body)
		h, body := mustDecode(t, buf)
		c.h.Type = c.body.MessageType()
		if h != c.h {
			t.Errorf("%T header: got %+v want %+v", c.body, h, c.h)
		}
		if !reflect.DeepEqual(body, c.body) {
			t.Errorf("%T body:\n got  %+v\n want %+v", c.body, body, c.body)
		}
	}
}

func TestDecodeAbsentDefaultFields(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want Body
	}{
		{"ConnectionRequest", cat(hdr(TypeConnectionRequest, ""), end), ConnectionRequest{}},
		{"ConnectionResponse v3 server omits nothing it needs", cat(hdr(TypeConnectionResponse, "c"), 0x03, 0x00, 0x03, end),
			ConnectionResponse{QuackVersion: 3}},
		{"PrepareRequest", cat(hdr(TypePrepareRequest, ""), 0x02, 0x00, 0x00, 0x01, end), PrepareRequest{QueryUUID: codec.HugeInt{Lower: 1}}},
		{"PrepareResponse", cat(hdr(TypePrepareResponse, ""), 0x05, 0x00, 0x00, 0x01, end), PrepareResponse{QueryUUID: codec.HugeInt{Lower: 1}}},
		{"FetchRequest", cat(hdr(TypeFetchRequest, ""), 0x01, 0x00, 0x00, 0x01, end), FetchRequest{QueryUUID: codec.HugeInt{Lower: 1}}},
		{"FetchResponse", cat(hdr(TypeFetchResponse, ""), end), FetchResponse{}},
		{"FetchResponse explicit unset optional_idx", cat(hdr(TypeFetchResponse, ""), 0x02, 0x00, unset, end), FetchResponse{}},
		{"SendDataRequest", cat(hdr(TypeSendDataRequest, ""), end), SendDataRequest{}},
		{"Acknowledgement", cat(hdr(TypeAcknowledgement, ""), end), Acknowledgement{}},
		{"ErrorResponse", cat(hdr(TypeErrorResponse, ""), end), ErrorResponse{}},
		{"ErrorResponse v1-style message only", cat(hdr(TypeErrorResponse, ""), 0x01, 0x00, str("bad"), end), ErrorResponse{Message: "bad"}},
		{"ErrorResponse must_invalidate only", cat(hdr(TypeErrorResponse, ""), 0x04, 0x00, 0x01, end), ErrorResponse{MustInvalidate: true}},
		// extra_info in hash (unsorted) order, as a C++ unordered_map emits it.
		{"ErrorResponse extra_info", cat(hdr(TypeErrorResponse, ""), 0x02, 0x00, str("Catalog"), 0x03, 0x00, 0x02,
			0x00, 0x00, str("name"), 0x01, 0x00, str("t"), end,
			0x00, 0x00, str("error_subtype"), 0x01, 0x00, str("MISSING_ENTRY"), end, end),
			ErrorResponse{ExceptionType: "Catalog", ExtraInfo: map[string]string{"name": "t", "error_subtype": "MISSING_ENTRY"}}},
	}
	for _, c := range cases {
		_, body := mustDecode(t, c.raw)
		if !reflect.DeepEqual(body, c.want) {
			t.Errorf("%s:\n got  %+v\n want %+v", c.name, body, c.want)
		}
	}
}

// TestChunkFraming: FETCH_RESPONSE and SEND_DATA_REQUEST carry chunk_count
// in the body, then that many bare DataChunk objects (no wrapper object, no
// present byte) after it.
func TestChunkFraming(t *testing.T) {
	c1, c2 := intChunk(1, 2, 3), intChunk(4)
	for _, chunks := range [][]DataChunk{nil, {c1}, {c1, c2, c1}} {
		count := func(id int) []byte { // chunk_count, written with default 0
			if len(chunks) == 0 {
				return nil
			}
			return cat(id, 0x00, len(chunks))
		}
		var blob []byte
		for _, ch := range chunks {
			blob = cat(blob, chunkBytes(t, ch))
		}
		for _, c := range []struct {
			body Body
			want []byte
		}{
			{FetchResponse{Results: chunks, BatchIndex: Idx(1)}, cat(hdr(TypeFetchResponse, ""), count(1), 0x03, 0x00, 0x01, end, blob)},
			{SendDataRequest{StreamID: "s", Chunks: chunks, BatchIndex: Idx(2)},
				cat(hdr(TypeSendDataRequest, ""), 0x01, 0x00, str("s"), count(2), 0x04, 0x00, 0x02, end, blob)},
		} {
			got := mustEncode(t, Header{}, c.body)
			if !bytes.Equal(got, c.want) {
				t.Errorf("%T with %d chunks:\n got  % x\n want % x", c.body, len(chunks), got, c.want)
				continue
			}
			_, body := mustDecode(t, got)
			var dec []DataChunk
			switch m := body.(type) {
			case FetchResponse:
				dec = m.Results
			case SendDataRequest:
				dec = m.Chunks
			}
			for i := range chunks {
				if !sameColumns(dec[i], chunks[i]) {
					t.Fatalf("%T chunk %d mismatch", c.body, i)
				}
			}
		}
	}
	// A bare chunk starts with field 100 (rows), not a present byte or 300.
	if b := chunkBytes(t, intChunk(7, 8)); !bytes.HasPrefix(b, []byte{0x64, 0x00, 0x02, 0x65, 0x00, 0x01, 0x64, 0x00, 0x0d, 0xff, 0xff, 0x66, 0x00, 0x01}) {
		t.Fatalf("chunk blob % x", b)
	}
}

func TestPrepareResponseResultsUseWrapper(t *testing.T) {
	c := intChunk(42)
	msg := PrepareResponse{
		ResultTypes: c.Types, ResultNames: []string{"x"}, Results: []DataChunk{c, c},
		QueryUUID: codec.HugeInt{Lower: 3},
	}
	got := mustEncode(t, Header{}, msg)
	wrapped := cat(0x01, 0x2c, 0x01, chunkBytes(t, c), end) // present, field 300, chunk, end of wrapper
	want := cat(hdr(TypePrepareResponse, ""),
		0x01, 0x00, 0x01, 0x64, 0x00, 0x0d, end,
		0x02, 0x00, 0x01, str("x"),
		0x04, 0x00, 0x02, wrapped, wrapped,
		0x05, 0x00, 0x00, 0x03,
		end)
	if !bytes.Equal(got, want) {
		t.Fatalf("\n got  % x\n want % x", got, want)
	}
	_, body := mustDecode(t, got)
	dec := body.(PrepareResponse)
	if len(dec.Results) != 2 || !sameColumns(dec.Results[1], c) || dec.QueryUUID != msg.QueryUUID {
		t.Fatalf("decoded %+v", dec)
	}
}

func TestEncodeErrors(t *testing.T) {
	bad := []struct {
		name string
		h    Header
		body Body
	}{
		{"nil body", Header{}, nil},
		{"query id sentinel", Header{HasQueryID: true, ClientQueryID: codec.InvalidIndex}, HeartbeatRequest{}},
		{"inline_rows sentinel", Header{}, PrepareRequest{InlineRows: Idx(codec.InvalidIndex)}},
		{"total_batches sentinel", Header{}, FetchResponse{TotalBatches: Idx(codec.InvalidIndex)}},
		{"batch_index sentinel", Header{}, SendDataRequest{BatchIndex: Idx(codec.InvalidIndex)}},
		{"accept_budget sentinel", Header{}, SendDataResponse{AcceptBudget: Idx(codec.InvalidIndex)}},
		{"zero-row chunk", Header{}, SendDataRequest{Chunks: []DataChunk{intChunk()}}},
		{"zero-column chunk", Header{}, SendDataRequest{Chunks: []DataChunk{{RowCount: 1}}}},
		{"types/columns mismatch", Header{}, SendDataRequest{Chunks: []DataChunk{{RowCount: 1,
			Types: []logicaltype.Type{{ID: logicaltype.IDInteger}, {ID: logicaltype.IDInteger}}, Columns: []Column{Values{int32(1)}}}}}},
		{"short column", Header{}, SendDataRequest{Chunks: []DataChunk{{RowCount: 2,
			Types: []logicaltype.Type{{ID: logicaltype.IDInteger}}, Columns: []Column{Values{int32(1)}}}}}},
		{"wrong value type", Header{}, FetchResponse{Results: []DataChunk{{RowCount: 1,
			Types: []logicaltype.Type{{ID: logicaltype.IDInteger}}, Columns: []Column{Values{"not an int"}}}}}},
		{"bad prepare result", Header{}, PrepareResponse{Results: []DataChunk{{RowCount: 1}}}},
	}
	for _, c := range bad {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: Encode panicked: %v", c.name, r)
				}
			}()
			if _, err := Encode(c.h, c.body); err == nil {
				t.Errorf("%s: expected error", c.name)
			}
		}()
	}
}

func TestNewQueryUUID(t *testing.T) {
	seen := map[codec.HugeInt]bool{}
	for range 1000 {
		u := NewQueryUUID()
		if u == (codec.HugeInt{}) || u.Upper == 0 {
			t.Fatalf("zero upper word: %+v", u)
		}
		if v := (uint64(u.Upper) >> 12) & 0xf; v != 4 {
			t.Fatalf("version nibble %d in %+v", v, u)
		}
		if v := u.Lower >> 62; v != 2 {
			t.Fatalf("variant bits %b in %+v", v, u)
		}
		if seen[u] {
			t.Fatalf("duplicate uuid %+v", u)
		}
		seen[u] = true
	}
}

func TestDecodeRejectsUnknownTypes(t *testing.T) {
	if TypeSendDataResponse.String() != "SEND_DATA_RESPONSE" || Type(13).String() != "MessageType(13)" {
		t.Fatalf("String: %q %q", TypeSendDataResponse, Type(13))
	}
	for _, typ := range []int{0, 5, 6, 13, 17, 99, 101, 255} {
		_, _, err := Decode(cat(hdr(Type(typ), ""), end))
		if !errors.Is(err, ErrUnknownMessageType) {
			t.Errorf("type %d: err = %v", typ, err)
		}
	}
	// A type id that does not fit MessageType (uint8).
	_, _, err := Decode(cat(0x01, 0x00, 0xac, 0x02, 0x03, 0x00, unset, end, end))
	if !errors.Is(err, ErrUnknownMessageType) {
		t.Errorf("type 300: err = %v", err)
	}
}

func TestDecodeStrictness(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"header missing client_query_id", cat(0x01, 0x00, 0x0a, end, end), "expected field"},
		{"header missing type", cat(0x03, 0x00, unset, end, end), "expected field"},
		{"prepare request missing query_uuid", cat(hdr(TypePrepareRequest, ""), 0x01, 0x00, str("x"), end), "expected field"},
		{"prepare response missing query_uuid", cat(hdr(TypePrepareResponse, ""), 0x03, 0x00, 0x01, end), "expected field"},
		{"fetch request missing query_uuid", cat(hdr(TypeFetchRequest, ""), 0x02, 0x00, 0x01, end), "expected field"},
		{"cancel missing query_uuid", cat(hdr(TypeCancelRequest, ""), end), "expected field"},
		{"send data response missing accept_budget", cat(hdr(TypeSendDataResponse, ""), end), "expected field"},
		{"fields out of order", cat(hdr(TypeConnectionResponse, ""), 0x03, 0x00, 0x03, 0x01, 0x00, str("v"), end), "end-of-object"},
		{"unknown field", cat(hdr(TypeErrorResponse, ""), 0x05, 0x00, 0x01, end), "end-of-object"},
		{"field on empty body", cat(hdr(TypeHeartbeatRequest, ""), 0x01, 0x00, 0x00, end), "end-of-object"},
		{"bad bool", cat(hdr(TypePrepareResponse, ""), 0x03, 0x00, 0x02, 0x05, 0x00, 0x00, 0x01, end), "invalid boolean"},
		{"bad must_invalidate", cat(hdr(TypeErrorResponse, ""), 0x04, 0x00, 0x07, end), "invalid boolean"},
		{"null result chunk", cat(hdr(TypePrepareResponse, ""), 0x04, 0x00, 0x01, 0x00, 0x05, 0x00, 0x00, 0x01, end), "null DataChunk"},
		{"wrapper wrong field", cat(hdr(TypePrepareResponse, ""), 0x04, 0x00, 0x01, 0x01, 0x2d, 0x01), "expected field"},
		{"trailing bytes", cat(hdr(TypeSuccessResponse, ""), end, 0x00), "trailing"},
		{"chunk_count larger than blob", cat(hdr(TypeFetchResponse, ""), 0x01, 0x00, 0x02, end, chunkBytes(t, intChunk(1))), "chunk 1 of 2"},
		{"chunk_count absurd", cat(hdr(TypeFetchResponse, ""), 0x01, 0x00, 0xff, 0xff, 0xff, 0xff, 0x0f, end), "chunk_count"},
		{"chunks without chunk_count", cat(hdr(TypeSendDataRequest, ""), end, chunkBytes(t, intChunk(1))), "trailing"},
		{"absurd result_types count", cat(hdr(TypePrepareResponse, ""), 0x01, 0x00, 0xff, 0xff, 0xff, 0xff, 0x0f), "exceeds"},
		{"absurd extra_info count", cat(hdr(TypeErrorResponse, ""), 0x03, 0x00, 0xff, 0xff, 0xff, 0x7f), "exceeds"},
		{"chunk with 2^40 rows", cat(hdr(TypeFetchResponse, ""), 0x01, 0x00, 0x01, end, 0x64, 0x00, 0x80, 0x80, 0x80, 0x80, 0x80, 0x20), "row count"},
		{"chunk with 2^40 types", cat(hdr(TypeFetchResponse, ""), 0x01, 0x00, 0x01, end, 0x64, 0x00, 0x01, 0x65, 0x00, 0x80, 0x80, 0x80, 0x80, 0x80, 0x20), "exceeds"},
		{"error map key id 1", cat(hdr(TypeErrorResponse, ""), 0x03, 0x00, 0x01, 0x01, 0x00, str("k"), 0x01, 0x00, str("v"), end, end), "expected field"},
	}
	for _, c := range cases {
		_, _, err := Decode(c.raw)
		if err == nil {
			t.Errorf("%s: expected error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
	}
}
