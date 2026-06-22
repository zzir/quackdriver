package message_test

import (
	"bytes"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
)

// goldenDir locates testdata/golden by walking up from this test file —
// `go test` runs in the package directory, but tests live under internal/message
// so the fixture path is two levels up.
func goldenDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller location")
	}
	dir := filepath.Join(filepath.Dir(here), "..", "..", "testdata", "golden")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("golden fixtures not present at %s: %v (run `go run ./cmd/dump-wire` against a live server)", dir, err)
	}
	return dir
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join(goldenDir(t), name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// decodeFixture decodes a fixture and asserts both the message type and the
// body's concrete Go type via the supplied checker.
func decodeFixture(t *testing.T, name string, wantType message.Type) message.Body {
	t.Helper()
	raw := readFixture(t, name)
	h, body, err := message.Decode(raw)
	if err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	if h.Type != wantType {
		t.Fatalf("%s: header type = %d, want %d", name, h.Type, wantType)
	}
	return body
}

// requestRoundTrip asserts a client-side message (which we generated and dumped
// ourselves) is byte-stable on re-encode. Server-side messages are not required
// to be byte-stable because servers may include or omit defaultable fields
// differently from us.
func requestRoundTrip(t *testing.T, name string, msg message.Body) {
	t.Helper()
	raw := readFixture(t, name)
	h, body, err := message.Decode(raw)
	if err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	if !reflect.DeepEqual(body, msg) {
		t.Fatalf("%s: body mismatch\nwant %#v\ngot  %#v", name, msg, body)
	}
	re, err := message.Encode(h, body)
	if err != nil {
		t.Fatalf("%s: re-encode: %v", name, err)
	}
	if !bytes.Equal(raw, re) {
		t.Fatalf("%s: re-encode differs:\n  original: %x\n  re-encode: %x", name, raw, re)
	}
}

func TestGoldenHandshake(t *testing.T) {
	body := decodeFixture(t, "handshake.resp.bin", message.TypeConnectionResponse)
	resp := body.(message.ConnectionResponse)
	if resp.ServerDuckdbVersion == "" {
		t.Fatalf("server duckdb version is empty: %+v", resp)
	}
	if resp.ServerPlatform == "" {
		t.Fatalf("server platform is empty: %+v", resp)
	}
}

func TestGoldenHandshakeRequest(t *testing.T) {
	// We can't byte-compare verbatim because the request we sent at capture
	// time encoded a specific client_query_id and platform. Reconstruct it
	// using the same values our cmd/dump-wire uses.
	raw := readFixture(t, "handshake.req.bin")
	h, body, err := message.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Type != message.TypeConnectionRequest {
		t.Fatalf("type: %d", h.Type)
	}
	req := body.(message.ConnectionRequest)
	if req.AuthString != "super_secret" {
		t.Fatalf("auth: %q", req.AuthString)
	}
	if req.ClientDuckdbVersion != "v1.5.3" {
		t.Fatalf("client version: %q", req.ClientDuckdbVersion)
	}
	if req.MinSupportedQuackVersion != 1 || req.MaxSupportedQuackVersion != 1 {
		t.Fatalf("quack version range: %+v", req)
	}

	re, err := message.Encode(h, req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, re) {
		t.Fatalf("re-encode differs:\n  original: %x\n  re-encode: %x", raw, re)
	}
}

func TestGoldenSelectInt(t *testing.T) {
	requestRoundTrip(t, "select_int.req.bin", message.PrepareRequest{SQL: "SELECT 42::INTEGER AS i"})

	body := decodeFixture(t, "select_int.resp.bin", message.TypePrepareResponse)
	resp := body.(message.PrepareResponse)
	if !reflect.DeepEqual(resp.ResultNames, []string{"i"}) {
		t.Fatalf("names: %v", resp.ResultNames)
	}
	if len(resp.ResultTypes) != 1 || resp.ResultTypes[0].ID != logicaltype.IDInteger {
		t.Fatalf("types: %+v", resp.ResultTypes)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("chunks: %d", len(resp.Results))
	}
	chunk := resp.Results[0]
	if chunk.RowCount != 1 {
		t.Fatalf("rows: %d", chunk.RowCount)
	}
	if got := chunk.Columns[0][0]; got != int32(42) {
		t.Fatalf("value: %v (%T)", got, got)
	}
}

func TestGoldenSelectVarcharUnicode(t *testing.T) {
	requestRoundTrip(t, "select_varchar.req.bin", message.PrepareRequest{SQL: "SELECT 'hello, 🦆' AS s"})

	body := decodeFixture(t, "select_varchar.resp.bin", message.TypePrepareResponse)
	resp := body.(message.PrepareResponse)
	if got := resp.Results[0].Columns[0][0]; got != "hello, 🦆" {
		t.Fatalf("value: %q", got)
	}
}

func TestGoldenSelectNull(t *testing.T) {
	body := decodeFixture(t, "select_null.resp.bin", message.TypePrepareResponse)
	resp := body.(message.PrepareResponse)
	if got := resp.Results[0].Columns[0][0]; got != nil {
		t.Fatalf("value: %v (want nil)", got)
	}
}

func TestGoldenSelectHugeInt(t *testing.T) {
	body := decodeFixture(t, "select_hugeint.resp.bin", message.TypePrepareResponse)
	resp := body.(message.PrepareResponse)
	got := resp.Results[0].Columns[0][0]
	bi, ok := got.(*big.Int)
	if !ok {
		t.Fatalf("type: %T", got)
	}
	max128, _ := new(big.Int).SetString("170141183460469231731687303715884105727", 10)
	if bi.Cmp(max128) != 0 {
		t.Fatalf("value: %s", bi)
	}
}

func TestGoldenSelectBool(t *testing.T) {
	body := decodeFixture(t, "select_bool.resp.bin", message.TypePrepareResponse)
	resp := body.(message.PrepareResponse)
	if resp.Results[0].Columns[0][0] != true {
		t.Fatalf("col0: %v", resp.Results[0].Columns[0][0])
	}
	if resp.Results[0].Columns[1][0] != false {
		t.Fatalf("col1: %v", resp.Results[0].Columns[1][0])
	}
}

func TestGoldenSelectMultirow(t *testing.T) {
	body := decodeFixture(t, "select_multirow.resp.bin", message.TypePrepareResponse)
	resp := body.(message.PrepareResponse)
	if resp.Results[0].RowCount != 5 {
		t.Fatalf("row count: %d", resp.Results[0].RowCount)
	}
	for i := range 5 {
		// range(0, 5) returns BIGINT-typed values.
		if got := resp.Results[0].Columns[0][i]; got != int64(i) {
			t.Fatalf("row %d: %v (%T)", i, got, got)
		}
	}
}

func TestGoldenSelectList(t *testing.T) {
	body := decodeFixture(t, "select_list.resp.bin", message.TypePrepareResponse)
	resp := body.(message.PrepareResponse)
	got := resp.Results[0].Columns[0][0]
	values, ok := got.([]any)
	if !ok {
		t.Fatalf("type: %T", got)
	}
	want := []any{int32(1), int32(2), int32(3)}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("list: %v want %v", values, want)
	}
}

func TestGoldenSelectStruct(t *testing.T) {
	body := decodeFixture(t, "select_struct.resp.bin", message.TypePrepareResponse)
	resp := body.(message.PrepareResponse)
	got := resp.Results[0].Columns[0][0]
	row, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("type: %T", got)
	}
	if row["a"] != int32(1) {
		t.Fatalf("a: %v", row["a"])
	}
	if row["b"] != "x" {
		t.Fatalf("b: %v", row["b"])
	}
}

func TestGoldenSelectDecimal(t *testing.T) {
	body := decodeFixture(t, "select_decimal.resp.bin", message.TypePrepareResponse)
	resp := body.(message.PrepareResponse)
	got := resp.Results[0].Columns[0][0]
	if got != "12345.67" {
		t.Fatalf("decimal: %v (%T)", got, got)
	}
}

func TestGoldenSelectMultichunkFetch(t *testing.T) {
	body := decodeFixture(t, "select_multichunk.resp.bin", message.TypePrepareResponse)
	resp := body.(message.PrepareResponse)
	if !resp.NeedsMoreFetch {
		t.Fatal("expected NeedsMoreFetch=true for a 200k-row range")
	}
	// Result UUID is non-zero (lower and/or upper).
	if resp.ResultUUID == (codec.HugeInt{}) {
		t.Fatal("ResultUUID is zero")
	}

	requestRoundTrip(t, "fetch_more.req.bin", message.FetchRequest{ResultUUID: resp.ResultUUID})

	body2 := decodeFixture(t, "fetch_more.resp.bin", message.TypeFetchResponse)
	fr := body2.(message.FetchResponse)
	if len(fr.Results) == 0 {
		t.Fatal("FetchResponse has no chunks")
	}
}

func TestGoldenErrorBadSQL(t *testing.T) {
	body := decodeFixture(t, "error_bad_sql.resp.bin", message.TypeErrorResponse)
	resp := body.(message.ErrorResponse)
	if resp.Message == "" {
		t.Fatal("error message is empty")
	}
}

func TestGoldenDisconnect(t *testing.T) {
	requestRoundTrip(t, "disconnect.req.bin", message.DisconnectMessage{})
	body := decodeFixture(t, "disconnect.resp.bin", message.TypeSuccessResponse)
	if _, ok := body.(message.SuccessResponse); !ok {
		t.Fatalf("body type: %T", body)
	}
}
