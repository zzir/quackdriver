package message

import (
	"bytes"
	"sort"
	"testing"

	"github.com/zzir/quackdriver/internal/logicaltype"
)

// FuzzDecode feeds arbitrary bytes to Decode. Decode must never panic; when
// it succeeds every decoded value must be readable, and re-encoding the
// decoded message must be stable: Encode(Decode(Encode(Decode(x)))) equals
// Encode(Decode(x)) whenever the first re-encoding succeeds (it may fail,
// e.g. for values the encoder refuses).
//
// Without -fuzz this runs the seed corpus below as an ordinary test.
func FuzzDecode(f *testing.F) {
	corpus := safetyCorpus(f)
	names := make([]string, 0, len(corpus))
	for name := range corpus {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f.Add(corpus[name])
	}
	for _, seed := range fuzzVectorSeeds(f) {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		h, body, err := Decode(b)
		if err != nil {
			return
		}
		readAll(body)
		enc1, err := Encode(h, body)
		if err != nil {
			return
		}
		h2, body2, err := Decode(enc1)
		if err != nil {
			t.Fatalf("re-encoded message does not decode: %v\ninput: % x\nencoded: % x", err, b, enc1)
		}
		enc2, err := Encode(h2, body2)
		if err != nil {
			t.Fatalf("second re-encoding failed: %v\ninput: % x", err, b)
		}
		if !bytes.Equal(enc1, enc2) {
			t.Fatalf("re-encoding is not stable\ninput: % x\nfirst:  % x\nsecond: % x", b, enc1, enc2)
		}
	})
}

// readAll boxes every value of every chunk in body.
func readAll(body Body) {
	var chunks []DataChunk
	switch m := body.(type) {
	case PrepareResponse:
		chunks = m.Results
	case FetchResponse:
		chunks = m.Results
	case SendDataRequest:
		chunks = m.Chunks
	}
	for i := range chunks {
		chunks[i].Rows()
	}
}

// fuzzVectorSeeds are FETCH_RESPONSE messages whose chunks use every vector
// format, so the fuzzer starts from CONSTANT / DICTIONARY / SEQUENCE bodies
// the encoder never produces.
func fuzzVectorSeeds(tb testing.TB) [][]byte {
	tb.Helper()
	msg := func(chunks ...[]byte) []byte {
		out := cat(hdr(TypeFetchResponse, ""), fld(1, len(chunks)), fld(3, 1), end)
		for _, c := range chunks {
			out = cat(out, c)
		}
		return out
	}
	integer := tp(logicaltype.IDInteger)
	varchar := tp(logicaltype.IDVarchar)
	seeds := [][]byte{
		msg(rawChunk(3, []logicaltype.Type{integer, varchar, tp(logicaltype.IDBigint)},
			obj(fld(90, 2), noNulls, fld(102, blob(le(int32(7))))),
			obj(fld(90, 3), fld(91, blob(le(uint32(1), uint32(0), uint32(1)))), fld(92, 2),
				withNulls(2, 1), fld(107, 2), fld(108, blob(le(uint32(2), uint32(0)))), fld(109, blob([]byte("ab")))),
			obj(fld(90, 4), fld(91, sleb(-5)), fld(92, sleb(3))))),
		msg(rawChunk(2, []logicaltype.Type{listOf(varchar)},
			obj(withNulls(2, 1), fld(104, 2), fld(105, 2, obj(fld(100, 0), fld(101, 2)), obj(fld(100, 0), fld(101, 0))),
				fld(106, flatStrings(noNulls, []uint32{1, 1}, "xy"))))),
	}
	// The round-trip chunk with every encodable type.
	full, err := Encode(Header{}, FetchResponse{Results: []DataChunk{roundTripChunk()}, BatchIndex: Idx(1)})
	if err != nil {
		tb.Fatal(err)
	}
	return append(seeds, full)
}
