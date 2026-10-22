package message_test

// Golden wire fixtures captured by cmd/dump-wire from the official DuckDB
// v2.0.0 release CLI (see testdata/golden/README.md). Every response must
// decode; every client request must re-encode byte for byte, and the
// requests the tests can rebuild from plain values must encode to the
// captured bytes. The select_<type> fixtures pair each value v with the
// server's own VARCHAR rendering s, which the decoded Go value is checked
// against.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/value"
)

var goldenDir = filepath.Join("..", "..", "testdata", "golden")

// goldenTypeCases lists the select_<name> fixtures in capture order with the
// logical type id of their v column.
var goldenTypeCases = []struct {
	name string
	id   logicaltype.ID
}{
	{"boolean", logicaltype.IDBoolean},
	{"tinyint", logicaltype.IDTinyint},
	{"smallint", logicaltype.IDSmallint},
	{"integer", logicaltype.IDInteger},
	{"bigint", logicaltype.IDBigint},
	{"utinyint", logicaltype.IDUTinyint},
	{"usmallint", logicaltype.IDUSmallint},
	{"uinteger", logicaltype.IDUInteger},
	{"ubigint", logicaltype.IDUBigint},
	{"hugeint", logicaltype.IDHugeint},
	{"uhugeint", logicaltype.IDUHugeint},
	{"float", logicaltype.IDFloat},
	{"double", logicaltype.IDDouble},
	{"decimal_4_1", logicaltype.IDDecimal},
	{"decimal_9_2", logicaltype.IDDecimal},
	{"decimal_18_4", logicaltype.IDDecimal},
	{"decimal_38_10", logicaltype.IDDecimal},
	{"varchar", logicaltype.IDVarchar},
	{"blob", logicaltype.IDBlob},
	{"json", logicaltype.IDVarchar},
	{"geometry", logicaltype.IDGeometry},
	{"uuid", logicaltype.IDUUID},
	{"date", logicaltype.IDDate},
	{"time", logicaltype.IDTime},
	{"time_ns", logicaltype.IDTimeNs},
	{"timetz", logicaltype.IDTimeTz},
	{"timestamp", logicaltype.IDTimestamp},
	{"timestamp_s", logicaltype.IDTimestampSec},
	{"timestamp_ms", logicaltype.IDTimestampMs},
	{"timestamp_ns", logicaltype.IDTimestampNs},
	{"timestamptz", logicaltype.IDTimestampTz},
	{"timestamptz_ns", logicaltype.IDTimestampTzNs},
	{"interval", logicaltype.IDInterval},
	{"bit", logicaltype.IDBit},
	{"bignum", logicaltype.IDBignum},
	{"enum", logicaltype.IDEnum},
	{"list", logicaltype.IDList},
	{"list_varchar", logicaltype.IDList},
	{"list_nested", logicaltype.IDList},
	{"array", logicaltype.IDArray},
	{"struct", logicaltype.IDStruct},
	{"tuple", logicaltype.IDTuple},
	{"map", logicaltype.IDMap},
	{"union", logicaltype.IDUnion},
	{"variant", logicaltype.IDVariant},
}

// goldenCatalog is every exchange cmd/dump-wire captures, in capture order.
func goldenCatalog() []string {
	names := []string{"handshake_bad_version", "handshake", "heartbeat"}
	for _, tc := range goldenTypeCases {
		names = append(names, "select_"+tc.name)
	}
	return append(names,
		"select_constant", "select_nulls", "select_multirow", "select_variant_columns", "select_multistatement",
		"error_catalog", "error_parser", "error_conversion",
		"multibatch_prepare", "fetch_missing_batch_index",
		"multibatch_fetch_01", "multibatch_fetch_01_retry", "multibatch_fetch_02", "multibatch_fetch_03",
		"senddata_prepare", "senddata_batch_1", "senddata_batch_2", "senddata_terminal",
		"senddata_fetch", "senddata_fetch_end", "senddata_verify", "senddata_unknown_stream",
		"cancel_prepare", "cancel", "cancel_fetch", "cancel_wildcard",
		"ack_prepare", "ack", "ack_default",
		"disconnect", "after_disconnect",
	)
}

// variantFixtures hold VARIANT columns: Decode rejects their responses with
// ErrVariant (TestGoldenVariantRejected), and the other tests skip them.
var variantFixtures = map[string]bool{"select_variant": true, "select_variant_columns": true}

// exchange is one captured round trip.
type exchange struct {
	name        string
	req, resp   []byte
	reqH, respH message.Header
	reqB, respB message.Body
}

// load reads and decodes a captured exchange.
func load(tb testing.TB, name string) *exchange {
	tb.Helper()
	ex := &exchange{name: name}
	var err error
	if ex.req, err = os.ReadFile(filepath.Join(goldenDir, name+".req.bin")); err != nil {
		tb.Fatalf("%s: %v (regenerate with make dump-wire)", name, err)
	}
	if ex.resp, err = os.ReadFile(filepath.Join(goldenDir, name+".resp.bin")); err != nil {
		tb.Fatalf("%s: %v (regenerate with make dump-wire)", name, err)
	}
	if ex.reqH, ex.reqB, err = message.Decode(ex.req); err != nil {
		tb.Fatalf("%s.req.bin: %v", name, err)
	}
	ex.respH, ex.respB, err = message.Decode(ex.resp)
	if err != nil && (!variantFixtures[name] || !errors.Is(err, message.ErrVariant)) {
		tb.Fatalf("%s.resp.bin: %v", name, err)
	}
	return ex
}

func respAs[T message.Body](tb testing.TB, ex *exchange) T {
	tb.Helper()
	v, ok := ex.respB.(T)
	if !ok {
		tb.Fatalf("%s: response is %T (%+v), want %T", ex.name, ex.respB, ex.respB, v)
	}
	return v
}

func reqAs[T message.Body](tb testing.TB, ex *exchange) T {
	tb.Helper()
	v, ok := ex.reqB.(T)
	if !ok {
		tb.Fatalf("%s: request is %T, want %T", ex.name, ex.reqB, v)
	}
	return v
}

// wantRequest encodes body with the captured request header and asserts the
// result equals the captured request bytes: the encoder must produce what
// was sent on the wire from the plain values alone.
func wantRequest(t *testing.T, ex *exchange, body message.Body) {
	t.Helper()
	got, err := message.Encode(ex.reqH, body)
	if err != nil {
		t.Fatalf("%s: encode: %v", ex.name, err)
	}
	if !bytes.Equal(got, ex.req) {
		t.Fatalf("%s: request bytes differ\n got  %x\n want %x", ex.name, got, ex.req)
	}
}

func TestGoldenCatalogComplete(t *testing.T) {
	want := map[string]bool{"README.md": true}
	for _, n := range goldenCatalog() {
		want[n+".req.bin"] = true
		want[n+".resp.bin"] = true
	}
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Name()] = true
		if !want[e.Name()] {
			t.Errorf("unexpected file %s in %s (add it to goldenCatalog)", e.Name(), goldenDir)
		}
	}
	for n := range want {
		if !have[n] {
			t.Errorf("missing fixture %s", n)
		}
	}
}

// TestGoldenRequestsReencode decodes every captured client request and
// re-encodes it: the bytes must be identical, including the chunks of
// SEND_DATA requests.
func TestGoldenRequestsReencode(t *testing.T) {
	for _, name := range goldenCatalog() {
		ex := load(t, name)
		if ex.reqH.Type != ex.reqB.MessageType() {
			t.Errorf("%s: header type %s, body %T", name, ex.reqH.Type, ex.reqB)
		}
		got, err := message.Encode(ex.reqH, ex.reqB)
		if err != nil {
			t.Errorf("%s: re-encode: %v", name, err)
			continue
		}
		if !bytes.Equal(got, ex.req) {
			t.Errorf("%s: re-encoded request differs\n got  %x\n want %x", name, got, ex.req)
		}
	}
}

func TestGoldenHandshake(t *testing.T) {
	ex := load(t, "handshake")
	wantRequest(t, ex, message.ConnectionRequest{
		AuthString:               "super_secret",
		ClientDuckdbVersion:      "v2.0.0",
		ClientPlatform:           "linux_amd64",
		MinSupportedQuackVersion: 3,
		MaxSupportedQuackVersion: 3,
		ClientID:                 "quackdriver-golden",
		HeartbeatTimeoutSeconds:  60,
	})
	resp := respAs[message.ConnectionResponse](t, ex)
	if resp.QuackVersion != message.QuackVersion || resp.HeartbeatTimeoutSeconds != 60 {
		t.Errorf("response %+v: want quack_version 3 and the requested 60 s lease echoed", resp)
	}
	if resp.ServerDuckdbVersion != "v2.0.0" || resp.ServerPlatform != "linux_amd64" {
		t.Errorf("response %+v: want the official v2.0.0 linux_amd64 release", resp)
	}
	// The server's fields are all non-default, so DuckDB's encoding equals ours.
	if got, err := message.Encode(ex.respH, resp); err != nil || !bytes.Equal(got, ex.resp) {
		t.Errorf("re-encoded response differs (%v)\n got  %x\n want %x", err, got, ex.resp)
	}

	bad := load(t, "handshake_bad_version")
	wantRequest(t, bad, message.ConnectionRequest{
		AuthString: "super_secret", ClientDuckdbVersion: "v1.5.3", ClientPlatform: "linux_amd64",
		MinSupportedQuackVersion: 1, MaxSupportedQuackVersion: 1,
	})
	e := respAs[message.ErrorResponse](t, bad)
	if e.Message != "Unsupported Quack version - server only supports version 3 of quack" || e.ExceptionType != "Invalid Input" || e.MustInvalidate {
		t.Errorf("version rejection %+v", e)
	}
}

func TestGoldenVoidMessages(t *testing.T) {
	for _, tc := range []struct {
		name string
		body message.Body
	}{
		{"heartbeat", message.HeartbeatRequest{}},
		{"disconnect", message.DisconnectMessage{}},
		{"cancel_wildcard", message.CancelRequest{}},
		{"ack_default", message.Acknowledgement{}},
	} {
		ex := load(t, tc.name)
		wantRequest(t, ex, tc.body)
		respAs[message.SuccessResponse](t, ex)
		if got, err := message.Encode(ex.respH, ex.respB); err != nil || !bytes.Equal(got, ex.resp) {
			t.Errorf("%s: re-encoded response differs (%v)", tc.name, err)
		}
	}
	// An empty body is just the object terminator; the default
	// ACKNOWLEDGEMENT omits its query_uuid entirely.
	ack := load(t, "ack_default")
	if !bytes.HasSuffix(ack.req, []byte{0xff, 0xff, 0xff, 0xff}) {
		t.Errorf("ack_default: request %x does not end with an empty body", ack.req)
	}
	// After DISCONNECT the connection id is unknown.
	e := respAs[message.ErrorResponse](t, load(t, "after_disconnect"))
	if e.Message != "Invalid connection id" || e.ExceptionType != "Invalid Input" {
		t.Errorf("after_disconnect: %+v", e)
	}
}

// goldenPrepare checks a PREPARE exchange against the request values and
// returns the response.
func goldenPrepare(t *testing.T, name, sql string, inline message.OptionalIdx) message.PrepareResponse {
	t.Helper()
	ex := load(t, name)
	req := reqAs[message.PrepareRequest](t, ex)
	if req.SQL != sql || req.InlineRows != inline {
		t.Fatalf("%s: request %+v, want sql %q inline_rows %+v", name, req, sql, inline)
	}
	wantRequest(t, ex, message.PrepareRequest{SQL: sql, QueryUUID: req.QueryUUID, InlineRows: inline})
	return respAs[message.PrepareResponse](t, ex)
}

// column returns column c of a single-chunk result, materialized.
func column(t *testing.T, name string, resp message.PrepareResponse, c int) []any {
	t.Helper()
	if len(resp.Results) != 1 {
		t.Fatalf("%s: %d chunks, want 1", name, len(resp.Results))
	}
	ch := resp.Results[0]
	if len(ch.Columns) != len(resp.ResultTypes) || len(ch.Types) != len(resp.ResultTypes) {
		t.Fatalf("%s: chunk has %d columns / %d types, schema %d", name, len(ch.Columns), len(ch.Types), len(resp.ResultTypes))
	}
	if !reflect.DeepEqual(ch.Types, resp.ResultTypes) {
		t.Fatalf("%s: chunk types %+v differ from result types %+v", name, ch.Types, resp.ResultTypes)
	}
	col := ch.Columns[c]
	if col.Len() != ch.RowCount {
		t.Fatalf("%s: column %d has %d rows, chunk %d", name, c, col.Len(), ch.RowCount)
	}
	return message.Materialize(col)
}

// TestGoldenTypes checks every select_<type> fixture: the declared type,
// the Go type of every decoded value and, row by row, that the value
// renders exactly like the server's own VARCHAR cast.
func TestGoldenTypes(t *testing.T) {
	for _, tc := range goldenTypeCases {
		if variantFixtures["select_"+tc.name] {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			name := "select_" + tc.name
			ex := load(t, name)
			req := reqAs[message.PrepareRequest](t, ex)
			if !strings.HasPrefix(req.SQL, "SELECT v, v::VARCHAR AS s FROM (VALUES ") {
				t.Fatalf("unexpected sql %q", req.SQL)
			}
			resp := respAs[message.PrepareResponse](t, ex)
			if !reflect.DeepEqual(resp.ResultNames, []string{"v", "s"}) || resp.NeedsMoreFetch {
				t.Fatalf("names %q needs_more_fetch %v", resp.ResultNames, resp.NeedsMoreFetch)
			}
			typ := resp.ResultTypes[0]
			if typ.ID != tc.id || resp.ResultTypes[1].ID != logicaltype.IDVarchar {
				t.Fatalf("types %s, %s; want id %d", typ.SQL(), resp.ResultTypes[1].SQL(), tc.id)
			}
			vs, ss := column(t, name, resp, 0), column(t, name, resp, 1)
			last := len(vs) - 1
			if last < 1 || vs[last] != nil || ss[last] != nil {
				t.Fatalf("want at least one value and a trailing NULL row, got %d rows (last %#v / %#v)", len(vs), vs[last], ss[last])
			}
			for i, v := range vs[:last] {
				s, ok := ss[i].(string)
				if !ok {
					t.Fatalf("row %d: rendering %#v is not a string", i, ss[i])
				}
				if err := checkGoType(typ, v); err != nil {
					t.Errorf("row %d (%s): %v", i, s, err)
					continue
				}
				if typ.ID == logicaltype.IDGeometry {
					continue // checked in TestGoldenTypeDetails
				}
				if got := render(typ, v, false); got != s {
					t.Errorf("row %d: Go value %#v renders as %q, server says %q", i, v, got, s)
				}
			}
		})
	}
}

// TestGoldenTypeDetails checks type metadata and the values the generic
// rendering comparison cannot cover.
func TestGoldenTypeDetails(t *testing.T) {
	typeOf := func(name string) (logicaltype.Type, []any) {
		resp := respAs[message.PrepareResponse](t, load(t, "select_"+name))
		return resp.ResultTypes[0], column(t, name, resp, 0)
	}
	for name, ws := range map[string][2]uint8{"decimal_4_1": {4, 1}, "decimal_9_2": {9, 2}, "decimal_18_4": {18, 4}, "decimal_38_10": {38, 10}} {
		typ, _ := typeOf(name)
		if typ.Info == nil || typ.Info.Width != ws[0] || typ.Info.Scale != ws[1] {
			t.Errorf("%s: type info %+v", name, typ.Info)
		}
	}
	if typ, _ := typeOf("json"); typ.Info == nil || typ.Info.Alias != "JSON" {
		t.Errorf("json: type info %+v", typ.Info)
	}
	for name, want := range map[string]string{
		"json":           "JSON",
		"enum":           "ENUM('red', 'green', 'it''s')",
		"struct":         `STRUCT("a" INTEGER, "b" VARCHAR)`,
		"union":          `UNION("num" INTEGER, "str" VARCHAR)`,
		"map":            "MAP(VARCHAR, INTEGER)",
		"array":          "INTEGER[3]",
		"tuple":          "TUPLE(INTEGER, VARCHAR)",
		"timestamptz_ns": "TIMESTAMPTZ_NS",
	} {
		if typ, _ := typeOf(name); typ.SQL() != want {
			t.Errorf("%s: type %s, want %s", name, typ.SQL(), want)
		}
	}

	_, geo := typeOf("geometry")
	wantGeo := []string{
		"0101000000000000000000f03f0000000000000040",                                                 // POINT (1 2)
		"0102000000" + "02000000" + strings.Repeat("00", 16) + strings.Repeat("000000000000f03f", 2), // LINESTRING (0 0, 1 1)
	}
	for i, w := range wantGeo {
		if b, ok := geo[i].([]byte); !ok || hex.EncodeToString(b) != w {
			t.Errorf("geometry row %d: %#v, want WKB %s", i, geo[i], w)
		}
	}

	_, union := typeOf("union")
	wantUnion := []any{
		map[string]any{"": uint8(0), "num": int32(2), "str": nil},
		map[string]any{"": uint8(1), "num": nil, "str": "two"},
		nil,
	}
	if !reflect.DeepEqual(union, wantUnion) {
		t.Errorf("union values\n got  %#v\n want %#v", union, wantUnion)
	}

	_, ts := typeOf("timestamp")
	if ts[5] != value.InfinityTime || ts[6] != value.NegInfinityTime {
		t.Errorf("timestamp ±infinity decode to %v / %v", ts[5], ts[6])
	}
}

// TestGoldenVariantRejected: the server's responses with VARIANT columns
// fail to decode with ErrVariant.
func TestGoldenVariantRejected(t *testing.T) {
	for name := range variantFixtures {
		ex := load(t, name)
		if _, _, err := message.Decode(ex.resp); !errors.Is(err, message.ErrVariant) || ex.respH.Type != message.TypePrepareResponse {
			t.Errorf("%s: %v (response type %s)", name, err, ex.respH.Type)
		}
	}
}

func TestGoldenOtherSelects(t *testing.T) {
	resp := goldenPrepare(t, "select_constant", "SELECT 42::INTEGER AS i, 'duck' AS s, NULL AS n", message.OptionalIdx{})
	if got := message.Materialize(resp.Results[0].Columns[0]); !reflect.DeepEqual(got, message.Values{int32(42)}) {
		t.Errorf("select_constant i = %#v", got)
	}
	if got := column(t, "select_constant", resp, 2); resp.ResultTypes[2].ID != logicaltype.IDSQLNull || !reflect.DeepEqual(got, []any{nil}) {
		t.Errorf("select_constant n: type %d values %#v", resp.ResultTypes[2].ID, got)
	}

	resp = goldenPrepare(t, "select_nulls", "SELECT NULL AS n, NULL::INTEGER AS i, NULL::VARCHAR AS s, NULL::INTEGER[] AS l FROM range(3)", message.OptionalIdx{})
	for c := range resp.ResultTypes {
		if got := column(t, "select_nulls", resp, c); !reflect.DeepEqual(got, []any{nil, nil, nil}) {
			t.Errorf("select_nulls column %d = %#v", c, got)
		}
	}

	resp = goldenPrepare(t, "select_multirow", "SELECT i, i * 2 AS twice, 'row ' || i AS label FROM range(0, 5) AS t(i)", message.OptionalIdx{})
	if got := resp.Results[0].Rows(); !reflect.DeepEqual(got, [][]any{
		{int64(0), int64(0), "row 0"}, {int64(1), int64(2), "row 1"}, {int64(2), int64(4), "row 2"},
		{int64(3), int64(6), "row 3"}, {int64(4), int64(8), "row 4"},
	}) {
		t.Errorf("select_multirow rows %#v", got)
	}

	resp = goldenPrepare(t, "select_multistatement", "SELECT 1 AS first; SELECT 2 AS second", message.OptionalIdx{})
	if !reflect.DeepEqual(resp.ResultNames, []string{"first"}) || !reflect.DeepEqual(resp.Results[0].Rows(), [][]any{{int32(1)}}) {
		t.Errorf("select_multistatement: %q %#v", resp.ResultNames, resp.Results[0].Rows())
	}
}

func TestGoldenErrors(t *testing.T) {
	for _, tc := range []struct {
		name, sql, excType, prefix string
		extra                      map[string]string
	}{
		{"error_catalog", "SELECT * FROM golden_does_not_exist", "Catalog", "Table with name golden_does_not_exist does not exist!",
			map[string]string{"name": "golden_does_not_exist", "type": "Table", "error_subtype": "MISSING_ENTRY"}},
		{"error_parser", "SELEC 1", "Parser", `syntax error at or near "SELEC"`, map[string]string{"error_subtype": "SYNTAX_ERROR"}},
		{"error_conversion", "SELECT 'not a number'::INTEGER", "Conversion", "Could not convert string 'not a number' to INT32", nil},
	} {
		ex := load(t, tc.name)
		req := reqAs[message.PrepareRequest](t, ex)
		wantRequest(t, ex, message.PrepareRequest{SQL: tc.sql, QueryUUID: req.QueryUUID})
		e := respAs[message.ErrorResponse](t, ex)
		if e.ExceptionType != tc.excType || !strings.HasPrefix(e.Message, tc.prefix) || e.MustInvalidate {
			t.Errorf("%s: %+v", tc.name, e)
		}
		for k, v := range tc.extra {
			if e.ExtraInfo[k] != v {
				t.Errorf("%s: extra_info[%q] = %q, want %q (all: %v)", tc.name, k, e.ExtraInfo[k], v, e.ExtraInfo)
			}
		}
		if tc.extra == nil && len(e.ExtraInfo) != 0 {
			t.Errorf("%s: unexpected extra_info %v", tc.name, e.ExtraInfo)
		}
		// Serialized "with default": an error without extra_info (or with
		// a single entry, so hash order cannot matter) re-encodes exactly.
		if len(e.ExtraInfo) <= 1 {
			if got, err := message.Encode(ex.respH, e); err != nil || !bytes.Equal(got, ex.resp) {
				t.Errorf("%s: re-encoded response differs (%v)\n got  %x\n want %x", tc.name, err, got, ex.resp)
			}
		}
	}
}

// TestGoldenMultiBatch follows a result streamed over FETCH: PREPARE inlines
// the leading batch, FETCH i acknowledges i-1, a FETCH repeated before its
// acknowledgement returns the same batch, and the terminal response carries
// total_batches = number of FETCH data batches.
func TestGoldenMultiBatch(t *testing.T) {
	const sql = "SELECT i, 'row ' || i AS label FROM range(0, 6000) AS t(i) ORDER BY i"
	prep := goldenPrepare(t, "multibatch_prepare", sql, message.Idx(1))
	if !prep.NeedsMoreFetch || len(prep.Results) == 0 {
		t.Fatalf("multibatch_prepare: needs_more_fetch %v with %d chunk(s)", prep.NeedsMoreFetch, len(prep.Results))
	}
	uuid := prep.QueryUUID
	next := int64(0)
	consume := func(name string, chunks []message.DataChunk) {
		for _, ch := range chunks {
			if !reflect.DeepEqual(ch.Types, prep.ResultTypes) {
				t.Fatalf("%s: chunk types %+v", name, ch.Types)
			}
			for _, row := range ch.Rows() {
				if !reflect.DeepEqual(row, []any{next, fmt.Sprintf("row %d", next)}) {
					t.Fatalf("%s: row %#v, want id %d", name, row, next)
				}
				next++
			}
		}
	}
	consume("multibatch_prepare", prep.Results)

	missing := load(t, "fetch_missing_batch_index")
	wantRequest(t, missing, message.FetchRequest{QueryUUID: uuid})
	if e := respAs[message.ErrorResponse](t, missing); e.Message != "FETCH_REQUEST is missing its batch index" {
		t.Errorf("fetch_missing_batch_index: %+v", e)
	}

	batches := []string{"multibatch_fetch_01", "multibatch_fetch_02"}
	for i, name := range batches {
		idx := uint64(i + 1)
		ex := load(t, name)
		wantRequest(t, ex, message.FetchRequest{QueryUUID: uuid, BatchIndex: idx, AckIndex: idx - 1})
		f := respAs[message.FetchResponse](t, ex)
		if f.BatchIndex != message.Idx(idx) || f.TotalBatches.Valid || len(f.Results) == 0 {
			t.Fatalf("%s: batch_index %+v total_batches %+v chunks %d", name, f.BatchIndex, f.TotalBatches, len(f.Results))
		}
		consume(name, f.Results)
	}
	if next != 6000 {
		t.Fatalf("received %d rows, want 6000", next)
	}

	retry := load(t, "multibatch_fetch_01_retry")
	wantRequest(t, retry, message.FetchRequest{QueryUUID: uuid, BatchIndex: 1})
	if first := load(t, "multibatch_fetch_01"); !bytes.Equal(retry.resp, first.resp) {
		t.Errorf("a repeated FETCH of batch 1 returned different bytes")
	}

	end := load(t, "multibatch_fetch_03")
	wantRequest(t, end, message.FetchRequest{QueryUUID: uuid, BatchIndex: 3, AckIndex: 2})
	f := respAs[message.FetchResponse](t, end)
	if len(f.Results) != 0 || f.BatchIndex.Valid || f.TotalBatches != message.Idx(uint64(len(batches))) {
		t.Errorf("terminal FETCH %+v, want no chunks and total_batches %d", f, len(batches))
	}
	if got, err := message.Encode(end.respH, f); err != nil || !bytes.Equal(got, end.resp) {
		t.Errorf("terminal FETCH response re-encodes differently (%v)\n got  %x\n want %x", err, got, end.resp)
	}
}

// goldenSendChunk mirrors cmd/dump-wire's sendChunk.
func goldenSendChunk(first int64, n int) message.DataChunk {
	ids, names, oks := make(message.Values, n), make(message.Values, n), make(message.Values, n)
	for i := range n {
		id := first + int64(i)
		ids[i] = id
		switch i % 3 {
		case 0:
			names[i] = fmt.Sprintf("name-%d", id)
		case 1:
			names[i] = fmt.Sprintf("a longer name that is not inlined #%d", id)
		}
		if i%4 != 3 {
			oks[i] = id%2 == 0
		}
	}
	return message.DataChunk{
		RowCount: n,
		Types:    []logicaltype.Type{{ID: logicaltype.IDBigint}, {ID: logicaltype.IDVarchar}, {ID: logicaltype.IDBoolean}},
		Columns:  []message.Column{ids, names, oks},
	}
}

// TestGoldenSendData follows an INSERT fed by scan_data_from_quack_client:
// PREPARE with inline_rows=0, two data batches, the terminal message, and
// the FETCH that returns the inserted row count.
func TestGoldenSendData(t *testing.T) {
	const stream = "golden-stream-1"
	sql := "INSERT INTO golden_send SELECT * FROM scan_data_from_quack_client('" + stream +
		`', NULL::STRUCT("id" BIGINT, "name" VARCHAR, "ok" BOOLEAN), ordered := true)`
	prep := goldenPrepare(t, "senddata_prepare", sql, message.Idx(0))
	if !prep.NeedsMoreFetch || len(prep.ResultTypes) != 0 || len(prep.ResultNames) != 0 || len(prep.Results) != 0 {
		t.Fatalf("senddata_prepare response %+v", prep)
	}
	sent := [][]message.DataChunk{
		{goldenSendChunk(1, 5)},
		{goldenSendChunk(6, 3), goldenSendChunk(9, 4)},
	}
	for i, chunks := range sent {
		ex := load(t, fmt.Sprintf("senddata_batch_%d", i+1))
		wantRequest(t, ex, message.SendDataRequest{StreamID: stream, BatchIndex: message.Idx(uint64(i + 1)), Chunks: chunks})
		// The decoded chunks hold the values that were sent.
		got := reqAs[message.SendDataRequest](t, ex)
		for c := range chunks {
			if !reflect.DeepEqual(got.Chunks[c].Rows(), chunks[c].Rows()) {
				t.Errorf("%s chunk %d decodes to %#v", ex.name, c, got.Chunks[c].Rows())
			}
		}
		if r := respAs[message.SendDataResponse](t, ex); r.AcceptBudget.Valid {
			t.Errorf("%s: accept_budget %+v, want unset", ex.name, r.AcceptBudget)
		}
		if b, err := message.Encode(ex.respH, ex.respB); err != nil || !bytes.Equal(b, ex.resp) {
			t.Errorf("%s: response re-encodes differently (%v)", ex.name, err)
		}
	}
	term := load(t, "senddata_terminal")
	wantRequest(t, term, message.SendDataRequest{StreamID: stream, TotalBatches: message.Idx(2)})
	respAs[message.SendDataResponse](t, term)

	count := load(t, "senddata_fetch")
	wantRequest(t, count, message.FetchRequest{QueryUUID: prep.QueryUUID, BatchIndex: 1})
	f := respAs[message.FetchResponse](t, count)
	if len(f.Results) != 1 || f.BatchIndex != message.Idx(1) ||
		!reflect.DeepEqual(f.Results[0].Types, []logicaltype.Type{{ID: logicaltype.IDBigint}}) ||
		!reflect.DeepEqual(f.Results[0].Rows(), [][]any{{int64(12)}}) {
		t.Errorf("senddata_fetch: %+v rows %v", f, f.Results)
	}
	end := load(t, "senddata_fetch_end")
	wantRequest(t, end, message.FetchRequest{QueryUUID: prep.QueryUUID, BatchIndex: 2, AckIndex: 1})
	if f := respAs[message.FetchResponse](t, end); len(f.Results) != 0 || f.TotalBatches != message.Idx(1) {
		t.Errorf("senddata_fetch_end: %+v", f)
	}

	verify := goldenPrepare(t, "senddata_verify", "SELECT * FROM golden_send ORDER BY id", message.OptionalIdx{})
	var want [][]any
	for _, chunks := range sent {
		for _, ch := range chunks {
			want = append(want, ch.Rows()...)
		}
	}
	if got := verify.Results[0].Rows(); !reflect.DeepEqual(got, want) {
		t.Errorf("senddata_verify\n got  %#v\n want %#v", got, want)
	}

	unknown := load(t, "senddata_unknown_stream")
	wantRequest(t, unknown, message.SendDataRequest{StreamID: "golden-no-such-stream", BatchIndex: message.Idx(1), Chunks: []message.DataChunk{goldenSendChunk(100, 1)}})
	if e := respAs[message.ErrorResponse](t, unknown); e.Message != "No active data stream 'golden-no-such-stream'" {
		t.Errorf("senddata_unknown_stream: %+v", e)
	}
}

func TestGoldenCancelAndAck(t *testing.T) {
	prep := goldenPrepare(t, "cancel_prepare", "SELECT count(*) AS n FROM range(0, 1000000000000) AS a(i)", message.Idx(0))
	if !prep.NeedsMoreFetch || len(prep.Results) != 0 || !reflect.DeepEqual(prep.ResultNames, []string{"n"}) {
		t.Fatalf("cancel_prepare: %+v", prep)
	}
	c := load(t, "cancel")
	wantRequest(t, c, message.CancelRequest{QueryUUID: prep.QueryUUID})
	respAs[message.SuccessResponse](t, c)
	cf := load(t, "cancel_fetch")
	wantRequest(t, cf, message.FetchRequest{QueryUUID: prep.QueryUUID, BatchIndex: 1})
	if e := respAs[message.ErrorResponse](t, cf); e.Message != "Interrupted: query was cancelled" || e.ExceptionType != "INTERRUPT" {
		t.Errorf("cancel_fetch: %+v", e)
	}

	ack := goldenPrepare(t, "ack_prepare", "SELECT 7 AS seven", message.OptionalIdx{})
	ex := load(t, "ack")
	wantRequest(t, ex, message.Acknowledgement{QueryUUID: ack.QueryUUID})
	respAs[message.SuccessResponse](t, ex)
}

// TestGoldenResponsesRoundTrip re-encodes every decoded response and decodes
// it again: the values must survive (the bytes may differ, e.g. DuckDB
// writes a validity mask whenever a vector can hold NULLs).
func TestGoldenResponsesRoundTrip(t *testing.T) {
	for _, name := range goldenCatalog() {
		if variantFixtures[name] {
			continue
		}
		ex := load(t, name)
		b, err := message.Encode(ex.respH, ex.respB)
		if err != nil {
			t.Errorf("%s: encode response: %v", name, err)
			continue
		}
		h, body, err := message.Decode(b)
		if err != nil {
			t.Errorf("%s: decode re-encoded response: %v", name, err)
			continue
		}
		if h != ex.respH || canon(body) != canon(ex.respB) {
			t.Errorf("%s: response changed in a round trip\n got  %s\n want %s", name, canon(body), canon(ex.respB))
		}
	}
}

// canon renders a decoded message deterministically, with chunks
// materialized, floats by bits (NaN == NaN) and big.Ints in decimal.
func canon(v any) string {
	var b strings.Builder
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		if !v.IsValid() {
			b.WriteString("nil")
			return
		}
		switch x := v.Interface().(type) {
		case *big.Int:
			b.WriteString("big(" + x.String() + ")")
			return
		case float32:
			fmt.Fprintf(&b, "f32(%x)", math.Float32bits(x))
			return
		case float64:
			fmt.Fprintf(&b, "f64(%x)", math.Float64bits(x))
			return
		case time.Time:
			b.WriteString(x.Format(time.RFC3339Nano))
			return
		case message.DataChunk:
			fmt.Fprintf(&b, "chunk(%d %s ", x.RowCount, canon(x.Types))
			walk(reflect.ValueOf(x.Rows()))
			b.WriteString(")")
			return
		}
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if v.IsNil() {
				b.WriteString("nil")
				return
			}
			walk(v.Elem())
		case reflect.Slice, reflect.Array:
			b.WriteString("[")
			for i := range v.Len() {
				walk(v.Index(i))
				b.WriteString(" ")
			}
			b.WriteString("]")
		case reflect.Map:
			keys := v.MapKeys()
			sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
			b.WriteString("{")
			for _, k := range keys {
				fmt.Fprintf(&b, "%v:", k)
				walk(v.MapIndex(k))
				b.WriteString(" ")
			}
			b.WriteString("}")
		case reflect.Struct:
			fmt.Fprintf(&b, "%s{", v.Type().Name())
			for i := range v.NumField() {
				if !v.Type().Field(i).IsExported() {
					continue
				}
				b.WriteString(v.Type().Field(i).Name + ":")
				walk(v.Field(i))
				b.WriteString(" ")
			}
			b.WriteString("}")
		default:
			fmt.Fprintf(&b, "%#v", v.Interface())
		}
	}
	walk(reflect.ValueOf(v))
	return b.String()
}

// checkGoType asserts the documented Go type of a non-NULL decoded value.
func checkGoType(t logicaltype.Type, v any) error {
	var ok bool
	switch t.ID {
	case logicaltype.IDBoolean:
		_, ok = v.(bool)
	case logicaltype.IDTinyint:
		_, ok = v.(int8)
	case logicaltype.IDSmallint:
		_, ok = v.(int16)
	case logicaltype.IDInteger:
		_, ok = v.(int32)
	case logicaltype.IDBigint:
		_, ok = v.(int64)
	case logicaltype.IDUTinyint:
		_, ok = v.(uint8)
	case logicaltype.IDUSmallint:
		_, ok = v.(uint16)
	case logicaltype.IDUInteger:
		_, ok = v.(uint32)
	case logicaltype.IDUBigint:
		_, ok = v.(uint64)
	case logicaltype.IDHugeint, logicaltype.IDUHugeint, logicaltype.IDBignum:
		_, ok = v.(*big.Int)
	case logicaltype.IDFloat:
		_, ok = v.(float32)
	case logicaltype.IDDouble:
		_, ok = v.(float64)
	case logicaltype.IDDecimal, logicaltype.IDVarchar, logicaltype.IDBit, logicaltype.IDUUID, logicaltype.IDEnum:
		_, ok = v.(string)
	case logicaltype.IDBlob, logicaltype.IDGeometry:
		_, ok = v.([]byte)
	case logicaltype.IDDate, logicaltype.IDTimestamp, logicaltype.IDTimestampSec, logicaltype.IDTimestampMs,
		logicaltype.IDTimestampNs, logicaltype.IDTimestampTz, logicaltype.IDTimestampTzNs:
		var ts time.Time
		ts, ok = v.(time.Time)
		ok = ok && ts.Location() == time.UTC
	case logicaltype.IDTime, logicaltype.IDTimeNs:
		_, ok = v.(time.Duration)
	case logicaltype.IDTimeTz:
		_, ok = v.(message.TimeTzValue)
	case logicaltype.IDInterval:
		_, ok = v.(message.IntervalValue)
	case logicaltype.IDList, logicaltype.IDArray, logicaltype.IDMap, logicaltype.IDTuple:
		_, ok = v.([]any)
	case logicaltype.IDStruct, logicaltype.IDUnion:
		_, ok = v.(map[string]any)
	}
	if !ok {
		return fmt.Errorf("value %#v has Go type %T, unexpected for %s", v, v, t.SQL())
	}
	return nil
}

// render formats a decoded value the way DuckDB's cast to VARCHAR does.
// nested selects the rules for values inside LIST/STRUCT/MAP renderings,
// where scalar renderings are quoted when they hold special characters.
func render(t logicaltype.Type, v any, nested bool) string {
	if v == nil {
		return "NULL"
	}
	s := renderScalar(t, v)
	switch t.ID {
	case logicaltype.IDList, logicaltype.IDArray, logicaltype.IDStruct, logicaltype.IDTuple, logicaltype.IDMap, logicaltype.IDUnion:
		return s
	}
	if nested {
		return quoteNested(s, false)
	}
	return s
}

// quoteNested applies DuckDB's escaping of non-nested values inside nested
// renderings (VectorCastHelpers::CalculateEscapedStringLength).
func quoteNested(s string, structKey bool) string {
	needs := structKey || s == "" || isSpace(s[0]) || (len(s) >= 2 && isSpace(s[len(s)-1])) ||
		strings.EqualFold(s, "null") || strings.ContainsAny(s, "\"'(),:=[\\]{}")
	if !needs {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(s) + "'"
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

func renderScalar(t logicaltype.Type, v any) string {
	switch t.ID {
	case logicaltype.IDBoolean:
		return strconv.FormatBool(v.(bool))
	case logicaltype.IDTinyint, logicaltype.IDSmallint, logicaltype.IDInteger, logicaltype.IDBigint,
		logicaltype.IDUTinyint, logicaltype.IDUSmallint, logicaltype.IDUInteger, logicaltype.IDUBigint:
		return fmt.Sprint(v)
	case logicaltype.IDHugeint, logicaltype.IDUHugeint, logicaltype.IDBignum:
		return v.(*big.Int).String()
	case logicaltype.IDFloat:
		return renderFloat(float64(v.(float32)), 32)
	case logicaltype.IDDouble:
		return renderFloat(v.(float64), 64)
	case logicaltype.IDDecimal, logicaltype.IDVarchar, logicaltype.IDBit, logicaltype.IDUUID, logicaltype.IDEnum:
		return v.(string)
	case logicaltype.IDBlob:
		var b strings.Builder
		for _, c := range v.([]byte) {
			if c >= 32 && c <= 126 && c != '\\' && c != '\'' && c != '"' {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, `\x%02X`, c)
			}
		}
		return b.String()
	case logicaltype.IDDate:
		return renderDate(v.(time.Time))
	case logicaltype.IDTime:
		return renderTimeOfDay(v.(time.Duration), 6)
	case logicaltype.IDTimeNs:
		return renderTimeOfDay(v.(time.Duration), 9)
	case logicaltype.IDTimeTz:
		tz := v.(message.TimeTzValue)
		own := renderTimeOfDay(tz.TimeOfDay(), 6) + renderOffset(tz.OffsetSeconds())
		if s := tz.String(); s != own {
			return fmt.Sprintf("TimeTzValue.String() = %q but the fields render as %q", s, own)
		}
		return own
	case logicaltype.IDTimestamp, logicaltype.IDTimestampSec, logicaltype.IDTimestampMs:
		return renderTimestamp(v.(time.Time), 6)
	case logicaltype.IDTimestampNs:
		return renderTimestamp(v.(time.Time), 9)
	case logicaltype.IDTimestampTz:
		return renderTimestamp(v.(time.Time), 6) + "+00"
	case logicaltype.IDTimestampTzNs:
		return renderTimestamp(v.(time.Time), 9) + "+00"
	case logicaltype.IDInterval:
		return v.(message.IntervalValue).String()
	case logicaltype.IDList, logicaltype.IDArray:
		child := *t.Info.Child
		var parts []string
		for _, e := range v.([]any) {
			parts = append(parts, render(child, e, true))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case logicaltype.IDStruct:
		m := v.(map[string]any)
		var parts []string
		for _, c := range t.Info.Children {
			parts = append(parts, quoteNested(c.Name, true)+": "+render(c.Type, m[c.Name], true))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case logicaltype.IDTuple:
		var parts []string
		for i, e := range v.([]any) {
			parts = append(parts, render(t.Info.Children[i].Type, e, true))
		}
		return "(" + strings.Join(parts, ", ") + ")"
	case logicaltype.IDMap:
		entry := t.Info.Child.Info.Children
		var parts []string
		for _, e := range v.([]any) {
			kv := e.(map[string]any)
			parts = append(parts, render(entry[0].Type, kv["key"], true)+"="+render(entry[1].Type, kv["value"], true))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case logicaltype.IDUnion:
		m := v.(map[string]any)
		tag := m[""].(uint8)
		member := t.Info.Children[int(tag)+1] // child 0 is the hidden tag
		return render(member.Type, m[member.Name], false)
	}
	return fmt.Sprintf("<unrendered %s %#v>", t.SQL(), v)
}

func renderFloat(f float64, bits int) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'g', -1, bits)
}

// renderDate formats the date part: years before 1 are written as
// "YYYY-MM-DD (BC)" with the BC year, years after 9999 in full.
func renderDate(ts time.Time) string {
	switch {
	case ts.Equal(value.InfinityTime):
		return "infinity"
	case ts.Equal(value.NegInfinityTime):
		return "-infinity"
	}
	y := ts.Year()
	if y <= 0 {
		return fmt.Sprintf("%04d-%02d-%02d (BC)", 1-y, ts.Month(), ts.Day())
	}
	return fmt.Sprintf("%04d-%02d-%02d", y, ts.Month(), ts.Day())
}

// renderTimeOfDay formats HH:MM:SS with the fraction (digits wide) trimmed
// of trailing zeros.
func renderTimeOfDay(d time.Duration, digits int) string {
	ns := int64(d)
	h := ns / int64(time.Hour)
	ns -= h * int64(time.Hour)
	m := ns / int64(time.Minute)
	ns -= m * int64(time.Minute)
	sec := ns / int64(time.Second)
	ns -= sec * int64(time.Second)
	s := fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
	frac := ns
	if digits == 6 {
		frac /= 1000
	}
	if frac != 0 {
		s += strings.TrimRight(fmt.Sprintf(".%0*d", digits, frac), "0")
	}
	return s
}

func renderTimestamp(ts time.Time, digits int) string {
	d := renderDate(ts)
	if ts.Equal(value.InfinityTime) || ts.Equal(value.NegInfinityTime) {
		return d
	}
	tod := time.Duration(ts.Hour())*time.Hour + time.Duration(ts.Minute())*time.Minute +
		time.Duration(ts.Second())*time.Second + time.Duration(ts.Nanosecond())
	return d + " " + renderTimeOfDay(tod, digits)
}

// renderOffset formats a UTC offset as ±HH[:MM[:SS]].
func renderOffset(sec int32) string {
	sign := "+"
	if sec < 0 {
		sign, sec = "-", -sec
	}
	s := fmt.Sprintf("%s%02d", sign, sec/3600)
	if m, x := sec/60%60, sec%60; m != 0 || x != 0 {
		s += fmt.Sprintf(":%02d", m)
		if x != 0 {
			s += fmt.Sprintf(":%02d", x)
		}
	}
	return s
}
