package message

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

func TestConnectionRequestRoundTrip(t *testing.T) {
	in := ConnectionRequest{
		AuthString:               "super_secret",
		ClientDuckdbVersion:      "v1.5.3",
		ClientPlatform:           "go-darwin-arm64",
		MinSupportedQuackVersion: 1,
		MaxSupportedQuackVersion: 1,
	}
	header := Header{HasQueryID: false}
	buf, err := Encode(header, in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	h, body, err := Decode(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Type != TypeConnectionRequest {
		t.Fatalf("header type: %v", h.Type)
	}
	got := body.(ConnectionRequest)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("connection request mismatch:\nwant %+v\ngot  %+v", in, got)
	}

	// Re-encoding must be byte-stable.
	buf2, _ := Encode(header, got)
	if !bytes.Equal(buf, buf2) {
		t.Fatalf("re-encode differs:\nfirst:  %x\nsecond: %x", buf, buf2)
	}
}

func TestConnectionResponseRoundTrip(t *testing.T) {
	in := ConnectionResponse{
		ServerDuckdbVersion: "v1.5.3",
		ServerPlatform:      "linux-x86_64",
		QuackVersion:        1,
		HasQuackVersion:     true,
	}
	header := Header{ConnectionID: "conn-1", HasQueryID: true, ClientQueryID: 7}
	buf, err := Encode(header, in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	h, body, err := Decode(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.ConnectionID != "conn-1" || !h.HasQueryID || h.ClientQueryID != 7 {
		t.Fatalf("header mismatch: %+v", h)
	}
	got := body.(ConnectionResponse)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("connection response mismatch:\nwant %+v\ngot  %+v", in, got)
	}
}

func TestPrepareRequestAndResponseRoundTrip(t *testing.T) {
	prepReq := PrepareRequest{SQL: "SELECT 42"}
	buf, _ := Encode(Header{ConnectionID: "c1"}, prepReq)
	_, body, err := Decode(buf)
	if err != nil {
		t.Fatalf("prepare-request decode: %v", err)
	}
	if got := body.(PrepareRequest); got.SQL != "SELECT 42" {
		t.Fatalf("prepare-request sql: %q", got.SQL)
	}

	prepResp := PrepareResponse{
		ResultTypes:    []logicaltype.Type{{ID: logicaltype.IDInteger}, {ID: logicaltype.IDVarchar}},
		ResultNames:    []string{"i", "s"},
		NeedsMoreFetch: false,
		Results:        nil,
		ResultUUID:     codec.HugeInt{Lower: 0x12345678, Upper: 1},
	}
	buf, _ = Encode(Header{ConnectionID: "c1"}, prepResp)
	_, body, err = Decode(buf)
	if err != nil {
		t.Fatalf("prepare-response decode: %v", err)
	}
	got := body.(PrepareResponse)
	if got.ResultUUID != prepResp.ResultUUID {
		t.Fatalf("uuid mismatch: %+v", got.ResultUUID)
	}
	if !reflect.DeepEqual(got.ResultNames, prepResp.ResultNames) {
		t.Fatalf("names mismatch: %+v", got.ResultNames)
	}
}

func TestFetchRequestAndDisconnectRoundTrip(t *testing.T) {
	fetch := FetchRequest{ResultUUID: codec.HugeInt{Lower: 1, Upper: 0}}
	buf, _ := Encode(Header{}, fetch)
	_, body, err := Decode(buf)
	if err != nil {
		t.Fatalf("fetch decode: %v", err)
	}
	if got := body.(FetchRequest); got.ResultUUID.Lower != 1 {
		t.Fatalf("fetch uuid: %+v", got)
	}

	buf, _ = Encode(Header{}, DisconnectMessage{})
	h, body, err := Decode(buf)
	if err != nil {
		t.Fatalf("disconnect decode: %v", err)
	}
	if h.Type != TypeDisconnectMessage {
		t.Fatalf("disconnect type: %v", h.Type)
	}
	if _, ok := body.(DisconnectMessage); !ok {
		t.Fatalf("disconnect body type: %T", body)
	}
}

func TestErrorAndSuccessRoundTrip(t *testing.T) {
	buf, _ := Encode(Header{}, ErrorResponse{Message: "bad sql"})
	_, body, err := Decode(buf)
	if err != nil {
		t.Fatalf("error decode: %v", err)
	}
	if got := body.(ErrorResponse); got.Message != "bad sql" {
		t.Fatalf("error message: %q", got.Message)
	}

	buf, _ = Encode(Header{}, SuccessResponse{})
	_, body, err = Decode(buf)
	if err != nil {
		t.Fatalf("success decode: %v", err)
	}
	if _, ok := body.(SuccessResponse); !ok {
		t.Fatalf("success body type: %T", body)
	}
}
