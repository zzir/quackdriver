// Diagnostic tool to confirm the multichunk-fetch loop fires. Runs an
// APPEND'd VARCHAR table SELECT through a Transport with a trace hook
// and prints how many FETCH_REQUESTs flowed.
//
//   bash scripts/with-server.sh go run ./cmd/probe-fetch
package main

import (
	"context"
	"fmt"
	"log"
	"runtime"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/transport"
)

func main() {
	uri, err := transport.ParseURI("quack://127.0.0.1:9494?token=super_secret")
	if err != nil {
		log.Fatal(err)
	}
	tr := transport.New(uri)
	var fetchReqs, prepReqs int
	tr.Trace = func(sent, _ []byte) {
		h, _, err := message.Decode(sent)
		if err != nil {
			return
		}
		switch h.Type {
		case message.TypeFetchRequest:
			fetchReqs++
		case message.TypePrepareRequest:
			prepReqs++
		}
	}

	ctx := context.Background()
	connID := ""
	roundTrip := func(body message.Body) (message.Header, message.Body) {
		h := message.Header{ConnectionID: connID, HasQueryID: true, ClientQueryID: uint64(prepReqs + fetchReqs + 1)}
		req, _ := message.Encode(h, body)
		resp, err := tr.Request(ctx, req)
		if err != nil {
			log.Fatalf("transport: %v", err)
		}
		rh, rb, err := message.Decode(resp)
		if err != nil {
			log.Fatalf("decode: %v", err)
		}
		return rh, rb
	}

	hh, body := roundTrip(message.ConnectionRequest{
		AuthString: "super_secret", ClientDuckdbVersion: "v1.5.3",
		ClientPlatform: "probe-" + runtime.GOOS, MinSupportedQuackVersion: 1, MaxSupportedQuackVersion: 1,
	})
	if _, ok := body.(message.ConnectionResponse); !ok {
		log.Fatalf("handshake: %+v", body)
	}
	connID = hh.ConnectionID

	// Set up table and APPEND 20000 VARCHAR rows.
	mustOk(roundTrip(message.PrepareRequest{SQL: "DROP TABLE IF EXISTS probe_fetch"}))
	mustOk(roundTrip(message.PrepareRequest{SQL: "CREATE TABLE probe_fetch (id BIGINT, payload VARCHAR)"}))

	const N = 200000
	rows := make([][]any, N)
	for i := range rows {
		rows[i] = []any{int64(i), fmt.Sprintf("the quick brown duck jumps over the lazy SQL parser and lands on row %07d", i)}
	}
	cols := [][]any{make([]any, N), make([]any, N)}
	for r, row := range rows {
		cols[0][r] = row[0]
		cols[1][r] = row[1]
	}
	chunk := message.DataChunk{
		RowCount: N,
		Types: []logicaltype.Type{
			{ID: logicaltype.IDBigint},
			{ID: logicaltype.IDVarchar},
		},
		Columns: cols,
	}
	mustOk(roundTrip(message.AppendRequest{TableName: "probe_fetch", AppendChunk: chunk}))

	// Now SELECT it back, manually driving the fetch loop the same way
	// rows.go would.
	fetchReqs = 0 // reset to measure just the SELECT
	prepReqs = 0
	_, body = roundTrip(message.PrepareRequest{SQL: "SELECT id, payload FROM probe_fetch ORDER BY id"})
	prep, ok := body.(message.PrepareResponse)
	if !ok {
		log.Fatalf("prepare: %+v", body)
	}
	rowsSeen := chunkRows(prep.Results)
	for prep.NeedsMoreFetch {
		_, b := roundTrip(message.FetchRequest{ResultUUID: prep.ResultUUID})
		fr, ok := b.(message.FetchResponse)
		if !ok {
			log.Fatalf("fetch: %+v", b)
		}
		if len(fr.Results) == 0 {
			break
		}
		rowsSeen += chunkRows(fr.Results)
	}

	fmt.Printf("SELECT issued %d PREPARE + %d FETCH round-trips\n", prepReqs, fetchReqs)
	fmt.Printf("rows seen: %d (want %d)\n", rowsSeen, N)
	if fetchReqs < 1 {
		log.Fatalf("FETCH loop did not run; the test is not exercising multichunk")
	}
	if rowsSeen != N {
		log.Fatalf("missing rows!")
	}

	mustOk(roundTrip(message.DisconnectMessage{}))
	_ = codec.HugeInt{} // keep import alive for future expansion
}

func mustOk(_ message.Header, b message.Body) {
	if e, ok := b.(message.ErrorResponse); ok {
		log.Fatalf("server error: %s", e.Message)
	}
}

func chunkRows(chunks []message.DataChunk) int {
	n := 0
	for _, c := range chunks {
		n += c.RowCount
	}
	return n
}
