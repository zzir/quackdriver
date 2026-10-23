package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
)

// typeCase is one "select_<name>" fixture: rows of a single typed column v
// together with the server's own VARCHAR rendering s of the same value, so
// tests can check decoded Go values against what DuckDB prints. The rows
// are listed in order and the last one is NULL.
type typeCase struct {
	name   string
	typ    string   // SQL type every value is cast to
	values []string // SQL literals, NULL is appended
}

// typeCases holds one fixture per type family.
var typeCases = []typeCase{
	{"boolean", "BOOLEAN", []string{"true", "false"}},
	{"tinyint", "TINYINT", []string{"-128", "0", "127"}},
	{"smallint", "SMALLINT", []string{"-32768", "7", "32767"}},
	{"integer", "INTEGER", []string{"-2147483648", "42", "2147483647"}},
	{"bigint", "BIGINT", []string{"-9223372036854775808", "9000000000", "9223372036854775807"}},
	{"utinyint", "UTINYINT", []string{"0", "255"}},
	{"usmallint", "USMALLINT", []string{"0", "65535"}},
	{"uinteger", "UINTEGER", []string{"0", "4294967295"}},
	{"ubigint", "UBIGINT", []string{"0", "18446744073709551615"}},
	{"hugeint", "HUGEINT", []string{"-170141183460469231731687303715884105728", "-1", "0", "18446744073709551616", "170141183460469231731687303715884105727"}},
	{"uhugeint", "UHUGEINT", []string{"0", "18446744073709551616", "340282366920938463463374607431768211455"}},
	{"float", "FLOAT", []string{"1.5", "-2.25", "3.4028235e38", "'inf'", "'-inf'", "'nan'"}},
	{"double", "DOUBLE", []string{"3.141592653589793", "-0.1", "1e300", "5e-324", "'inf'", "'nan'"}},
	{"decimal_4_1", "DECIMAL(4,1)", []string{"-999.9", "0.5", "123.4"}},
	{"decimal_9_2", "DECIMAL(9,2)", []string{"-0.01", "12345.67", "9999999.99"}},
	{"decimal_18_4", "DECIMAL(18,4)", []string{"-12345678901234.5678", "0", "99999999999999.9999"}},
	{"decimal_38_10", "DECIMAL(38,10)", []string{"-1234567890123456789012345678.0123456789", "0.0000000001", "9999999999999999999999999999.9999999999"}},
	{"varchar", "VARCHAR", []string{"''", "'hello'", "'exactly12chr'", "'a string longer than the twelve byte inline limit'", "'hello, 🦆'", "'nul:' || chr(0) || ':end'"}},
	{"blob", "BLOB", []string{"''", "'abc'", `'\x00\xFF\x7F''"\x5C'`, `'\xDE\xAD\xBE\xEF'`}},
	{"json", "JSON", []string{`'{"a": [1, 2]}'`, "'null'", `'"str"'`}},
	{"geometry", "GEOMETRY", []string{"'POINT(1 2)'", "'LINESTRING(0 0, 1 1)'"}},
	{"uuid", "UUID", []string{"'00000000-0000-0000-0000-000000000000'", "'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'", "'7fffffff-ffff-ffff-ffff-ffffffffffff'", "'80000000-0000-0000-0000-000000000000'", "'ffffffff-ffff-ffff-ffff-ffffffffffff'"}},
	{"date", "DATE", []string{"'2026-05-25'", "'1970-01-01'", "'0001-01-01'", "'1969-12-31'", "'9999-12-31'", "'0044-03-15 (BC)'", "'12345-06-07'", "'infinity'", "'-infinity'"}},
	{"time", "TIME", []string{"'00:00:00'", "'12:34:56.789012'", "'23:59:59.999999'", "'24:00:00'"}},
	{"time_ns", "TIME_NS", []string{"'00:00:00'", "'12:34:56.123456789'"}},
	{"timetz", "TIMETZ", []string{"'12:34:56+05:30'", "'00:00:00-08'", "'23:59:59.999999+00'"}},
	{"timestamp", "TIMESTAMP", []string{"'2026-05-25 12:34:56.789'", "'1970-01-01 00:00:00'", "'1969-12-31 23:59:59.999999'", "'0001-01-01 00:00:00'", "DATE '0044-03-15 (BC)' + INTERVAL 12 HOUR", "'infinity'", "'-infinity'"}},
	{"timestamp_s", "TIMESTAMP_S", []string{"'2026-05-25 12:34:56'", "'1960-01-01 00:00:01'"}},
	{"timestamp_ms", "TIMESTAMP_MS", []string{"'2026-05-25 12:34:56.789'", "'1960-01-01 00:00:00.001'"}},
	{"timestamp_ns", "TIMESTAMP_NS", []string{"'2026-05-25 12:34:56.123456789'", "'1969-12-31 23:59:59.999999999'"}},
	{"timestamptz", "TIMESTAMPTZ", []string{"'2026-05-25 12:34:56.789+00'", "'2026-05-25 12:34:56+02'"}},
	{"timestamptz_ns", "TIMESTAMPTZ_NS", []string{"'2026-05-25 12:34:56.123456789+00'", "'1969-12-31 23:59:59.999999999+00'"}},
	{"interval", "INTERVAL", []string{"INTERVAL '1 year 2 months 3 days 04:05:06.789'", "INTERVAL '-1 day'", "INTERVAL '0 seconds'", "INTERVAL '-36 hours -30 microseconds'"}},
	{"bit", "BIT", []string{"'1'", "'101100'", "'0101010101010101'"}},
	{"bignum", "BIGNUM", []string{"'0'", "'-1'", "'123456789012345678901234567890123456789012345678901234567890'", "'-340282366920938463463374607431768211456'"}},
	{"enum", "ENUM('red', 'green', 'it''s')", []string{"'green'", "'it''s'", "'red'"}},
	{"list", "INTEGER[]", []string{"[1, 2, NULL]", "[]", "[42]"}},
	{"list_varchar", "VARCHAR[]", []string{"['a', 'b''c', NULL]", "[]"}},
	{"list_nested", "INTEGER[][]", []string{"[[1], [], NULL, [2, 3]]", "[]"}},
	{"array", "INTEGER[3]", []string{"[1, 2, 3]", "[NULL, 5, 6]"}},
	{"struct", `STRUCT("a" INTEGER, "b" VARCHAR)`, []string{"{'a': 1, 'b': 'x'}", "{'a': NULL, 'b': NULL}", "{'a': -7, 'b': 'a string longer than twelve'}"}},
	{"tuple", "", []string{"row(1, 'x')", "row(2, NULL)"}},
	{"map", "MAP(VARCHAR, INTEGER)", []string{"MAP {'k1': 1, 'k2': NULL}", "MAP {}"}},
	{"union", `UNION("num" INTEGER, "str" VARCHAR)`, []string{"union_value(num := 2)", "union_value(str := 'two')"}},
	{"variant", "VARIANT", []string{"42", "'text'", "{'a': 1, 'b': [true, NULL]}", "[1.5::VARIANT, 'x'::VARIANT]", "NULL::INTEGER", "DATE '2026-05-25'"}},
}

// sql renders the fixture query. Every value is cast to typ (except TUPLE,
// whose literal row(...) already has the type) and NULL is the last row.
func (tc typeCase) sql() string {
	cast := func(v string) string {
		if tc.typ == "" {
			return v
		}
		return "(" + v + ")::" + tc.typ
	}
	rows := make([]string, 0, len(tc.values)+1)
	for i, v := range tc.values {
		rows = append(rows, fmt.Sprintf("(%d, %s)", i, cast(v)))
	}
	null := "NULL"
	if tc.typ != "" {
		null = "NULL::" + tc.typ
	}
	rows = append(rows, fmt.Sprintf("(%d, %s)", len(tc.values), null))
	return "SELECT v, v::VARCHAR AS s FROM (VALUES " + strings.Join(rows, ", ") + ") AS t(k, v) ORDER BY k"
}

// SEND_DATA fixture parameters.
const (
	sendTable  = "golden_send"
	sendStream = "golden-stream-1"
)

func (c *capturer) run(ctx context.Context) error {
	steps := []func(context.Context) error{
		c.handshake,
		c.heartbeat,
		c.selects,
		c.errors,
		c.multiBatch,
		c.sendData,
		c.cancel,
		c.acknowledge,
		c.disconnect,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (c *capturer) handshake(ctx context.Context) error {
	// A client that only speaks Quack v1 is turned away before a session
	// exists.
	if _, err := do[message.ErrorResponse](ctx, c, "handshake_bad_version", false, message.ConnectionRequest{
		AuthString:               c.token,
		ClientDuckdbVersion:      "v1.5.3",
		ClientPlatform:           clientPlatform,
		MinSupportedQuackVersion: 1,
		MaxSupportedQuackVersion: 1,
	}); err != nil {
		return err
	}

	h, body, err := c.exchange(ctx, "handshake", c.header(false), message.ConnectionRequest{
		AuthString:               c.token,
		ClientDuckdbVersion:      clientDuckdbVersion,
		ClientPlatform:           clientPlatform,
		MinSupportedQuackVersion: message.QuackVersion,
		MaxSupportedQuackVersion: message.QuackVersion,
		ClientID:                 clientID,
		HeartbeatTimeoutSeconds:  heartbeatSeconds,
	})
	if err != nil {
		return err
	}
	resp, err := expect[message.ConnectionResponse]("handshake", body)
	if err != nil {
		return err
	}
	if resp.QuackVersion != message.QuackVersion {
		return fmt.Errorf("handshake: server speaks Quack v%d, want v%d", resp.QuackVersion, message.QuackVersion)
	}
	if h.ConnectionID == "" {
		return fmt.Errorf("handshake: no connection id")
	}
	c.connID = h.ConnectionID
	c.serverVersion = resp.ServerDuckdbVersion + " " + resp.ServerPlatform
	if c.verbose {
		fmt.Printf("connected to %s; connection_id=%q\n", c.serverVersion, c.connID)
	}
	return nil
}

func (c *capturer) heartbeat(ctx context.Context) error {
	_, err := do[message.SuccessResponse](ctx, c, "heartbeat", false, message.HeartbeatRequest{})
	return err
}

// prepare runs a PREPARE that must succeed.
func (c *capturer) prepare(ctx context.Context, name, sql string, inline message.OptionalIdx) (message.PrepareRequest, message.PrepareResponse, error) {
	req := message.PrepareRequest{SQL: sql, QueryUUID: c.nextUUID(), InlineRows: inline}
	resp, err := do[message.PrepareResponse](ctx, c, name, true, req)
	if err == nil && resp.QueryUUID != req.QueryUUID {
		err = fmt.Errorf("%s: response query_uuid %v, want %v", name, resp.QueryUUID, req.QueryUUID)
	}
	return req, resp, err
}

// exec runs a statement whose result is not interesting (DDL, DML) and
// drains it, without capturing anything.
func (c *capturer) exec(ctx context.Context, sql string) error {
	req, resp, err := c.prepare(ctx, "", sql, message.OptionalIdx{})
	if err != nil {
		return fmt.Errorf("%q: %w", sql, err)
	}
	for i := uint64(1); resp.NeedsMoreFetch; i++ {
		f, err := do[message.FetchResponse](ctx, c, "", true, message.FetchRequest{QueryUUID: req.QueryUUID, BatchIndex: i, AckIndex: i - 1})
		if err != nil {
			return fmt.Errorf("%q: %w", sql, err)
		}
		if len(f.Results) == 0 {
			break
		}
	}
	return nil
}

// prepareVariant captures a PREPARE whose response holds VARIANT columns,
// which message.Decode rejects: the bytes are kept as the server sent them.
func (c *capturer) prepareVariant(ctx context.Context, name, sql string) error {
	_, b, err := c.exchange(ctx, name, c.header(true), message.PrepareRequest{SQL: sql, QueryUUID: c.nextUUID()})
	if err == nil && b != nil {
		err = fmt.Errorf("%s: the response decoded, want message.ErrVariant", name)
	}
	return err
}

func (c *capturer) selects(ctx context.Context) error {
	for _, tc := range typeCases {
		if tc.typ == "VARIANT" {
			if err := c.prepareVariant(ctx, "select_"+tc.name, tc.sql()); err != nil {
				return err
			}
			continue
		}
		_, resp, err := c.prepare(ctx, "select_"+tc.name, tc.sql(), message.OptionalIdx{})
		if err != nil {
			return err
		}
		if strings.HasPrefix(tc.typ, "TIMESTAMPTZ") {
			if err := checkUTC("select_"+tc.name, resp); err != nil {
				return err
			}
		}
	}
	others := []struct{ name, sql string }{
		// Constant expressions: the server still sends FLAT vectors.
		{"select_constant", "SELECT 42::INTEGER AS i, 'duck' AS s, NULL AS n"},
		// SQLNULL and typed NULL columns.
		{"select_nulls", "SELECT NULL AS n, NULL::INTEGER AS i, NULL::VARCHAR AS s, NULL::INTEGER[] AS l FROM range(3)"},
		{"select_multirow", "SELECT i, i * 2 AS twice, 'row ' || i AS label FROM range(0, 5) AS t(i)"},
		// Casting a column to VARIANT shreds it inside DuckDB, but the
		// result still travels as the plain (unshredded) VARIANT STRUCT.
		{"select_variant_columns", "SELECT i::VARIANT AS v, {'a': i, 'b': 'x' || i}::VARIANT AS o, (i::VARIANT)::VARCHAR AS s FROM range(0, 3) AS t(i)"},
		// A multi-statement query answers with its first result.
		{"select_multistatement", "SELECT 1 AS first; SELECT 2 AS second"},
	}
	for _, o := range others {
		var err error
		if strings.Contains(o.sql, "::VARIANT") {
			err = c.prepareVariant(ctx, o.name, o.sql)
		} else {
			_, _, err = c.prepare(ctx, o.name, o.sql, message.OptionalIdx{})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// checkUTC fails unless the server rendered every TIMESTAMPTZ value of a
// select fixture (column s) in UTC. It renders them in its TimeZone, which
// defaults to the server host's zone; internal/message/golden_test.go
// expects +00.
func checkUTC(name string, resp message.PrepareResponse) error {
	for _, ch := range resp.Results {
		for r := range ch.RowCount {
			s, ok := ch.Columns[1].Value(r).(string)
			if ok && !strings.HasSuffix(s, "+00") && s != "infinity" && s != "-infinity" {
				return fmt.Errorf("%s: the server renders TIMESTAMPTZ as %q, not in UTC; capture from a server started with TZ=UTC (see testdata/golden/README.md)", name, s)
			}
		}
	}
	return nil
}

func (c *capturer) errors(ctx context.Context) error {
	cases := []struct{ name, sql string }{
		{"error_catalog", "SELECT * FROM golden_does_not_exist"},
		{"error_parser", "SELEC 1"},
		{"error_conversion", "SELECT 'not a number'::INTEGER"},
	}
	for _, e := range cases {
		req := message.PrepareRequest{SQL: e.sql, QueryUUID: c.nextUUID()}
		if _, err := do[message.ErrorResponse](ctx, c, e.name, true, req); err != nil {
			return err
		}
	}
	return nil
}

// multiBatch captures a result streamed in several FETCH batches. A small
// quack_target_batch_bytes (set for this session) makes every chunk its own
// batch; inline_rows makes PREPARE carry the leading batch(es).
func (c *capturer) multiBatch(ctx context.Context) error {
	if err := c.exec(ctx, "SET quack_target_batch_bytes = 1"); err != nil {
		return err
	}
	req, resp, err := c.prepare(ctx, "multibatch_prepare",
		"SELECT i, 'row ' || i AS label FROM range(0, 6000) AS t(i) ORDER BY i", message.Idx(1))
	if err != nil {
		return err
	}
	if !resp.NeedsMoreFetch {
		return fmt.Errorf("multibatch_prepare: needs_more_fetch is false")
	}
	// A FETCH without batch_index (field omitted, i.e. 0) is rejected.
	if _, err := do[message.ErrorResponse](ctx, c, "fetch_missing_batch_index", true,
		message.FetchRequest{QueryUUID: req.QueryUUID}); err != nil {
		return err
	}
	for i := uint64(1); ; i++ {
		name := fmt.Sprintf("multibatch_fetch_%02d", i)
		f, err := do[message.FetchResponse](ctx, c, name, true,
			message.FetchRequest{QueryUUID: req.QueryUUID, BatchIndex: i, AckIndex: i - 1})
		if err != nil {
			return err
		}
		if i == 1 {
			// Asking for the same batch again before acknowledging it
			// returns it again.
			if _, err := do[message.FetchResponse](ctx, c, "multibatch_fetch_01_retry", true,
				message.FetchRequest{QueryUUID: req.QueryUUID, BatchIndex: 1}); err != nil {
				return err
			}
		}
		if len(f.Results) == 0 {
			if !f.TotalBatches.Valid {
				return fmt.Errorf("%s: terminal FETCH without total_batches", name)
			}
			break
		}
		if i > 50 {
			return fmt.Errorf("multibatch: more than 50 batches")
		}
	}
	return c.exec(ctx, "RESET quack_target_batch_bytes")
}

// sendChunk builds a SEND_DATA chunk of the golden_send schema.
func sendChunk(first int64, n int) message.DataChunk {
	ids := make(message.Values, n)
	names := make(message.Values, n)
	oks := make(message.Values, n)
	for i := range n {
		id := first + int64(i)
		ids[i] = id
		switch i % 3 { // the third name is NULL
		case 0:
			names[i] = fmt.Sprintf("name-%d", id)
		case 1:
			names[i] = fmt.Sprintf("a longer name that is not inlined #%d", id)
		}
		if i%4 != 3 { // every fourth ok is NULL
			oks[i] = id%2 == 0
		}
	}
	return message.DataChunk{
		RowCount: n,
		Types: []logicaltype.Type{
			{ID: logicaltype.IDBigint},
			{ID: logicaltype.IDVarchar},
			{ID: logicaltype.IDBoolean},
		},
		Columns: []message.Column{ids, names, oks},
	}
}

// sendData captures an INSERT fed by a SEND_DATA stream.
func (c *capturer) sendData(ctx context.Context) error {
	if err := c.exec(ctx, "CREATE OR REPLACE TABLE "+sendTable+" (id BIGINT, name VARCHAR, ok BOOLEAN)"); err != nil {
		return err
	}
	sql := "INSERT INTO " + sendTable + " SELECT * FROM scan_data_from_quack_client('" + sendStream +
		`', NULL::STRUCT("id" BIGINT, "name" VARCHAR, "ok" BOOLEAN), ordered := true)`
	req, resp, err := c.prepare(ctx, "senddata_prepare", sql, message.Idx(0))
	if err != nil {
		return err
	}
	if !resp.NeedsMoreFetch || len(resp.Results) != 0 {
		return fmt.Errorf("senddata_prepare: unexpected response %+v", resp)
	}
	batches := []message.SendDataRequest{
		{StreamID: sendStream, BatchIndex: message.Idx(1), Chunks: []message.DataChunk{sendChunk(1, 5)}},
		{StreamID: sendStream, BatchIndex: message.Idx(2), Chunks: []message.DataChunk{sendChunk(6, 3), sendChunk(9, 4)}},
	}
	for i, b := range batches {
		if _, err := do[message.SendDataResponse](ctx, c, fmt.Sprintf("senddata_batch_%d", i+1), true, b); err != nil {
			return err
		}
	}
	if _, err := do[message.SendDataResponse](ctx, c, "senddata_terminal", true,
		message.SendDataRequest{StreamID: sendStream, TotalBatches: message.Idx(uint64(len(batches)))}); err != nil {
		return err
	}
	count, err := do[message.FetchResponse](ctx, c, "senddata_fetch", true,
		message.FetchRequest{QueryUUID: req.QueryUUID, BatchIndex: 1})
	if err != nil {
		return err
	}
	if len(count.Results) != 1 {
		return fmt.Errorf("senddata_fetch: %d chunks, want 1 (Count)", len(count.Results))
	}
	if _, err := do[message.FetchResponse](ctx, c, "senddata_fetch_end", true,
		message.FetchRequest{QueryUUID: req.QueryUUID, BatchIndex: 2, AckIndex: 1}); err != nil {
		return err
	}
	if _, _, err := c.prepare(ctx, "senddata_verify", "SELECT * FROM "+sendTable+" ORDER BY id", message.OptionalIdx{}); err != nil {
		return err
	}
	// A SEND_DATA for a stream no statement reads is rejected.
	if _, err := do[message.ErrorResponse](ctx, c, "senddata_unknown_stream", true,
		message.SendDataRequest{StreamID: "golden-no-such-stream", BatchIndex: message.Idx(1), Chunks: []message.DataChunk{sendChunk(100, 1)}}); err != nil {
		return err
	}
	return c.exec(ctx, "DROP TABLE "+sendTable)
}

// cancel captures CANCEL of a running statement and the FETCH that observes
// it, plus the wildcard CANCEL.
func (c *capturer) cancel(ctx context.Context) error {
	req, _, err := c.prepare(ctx, "cancel_prepare",
		"SELECT count(*) AS n FROM range(0, 1000000000000) AS a(i)", message.Idx(0))
	if err != nil {
		return err
	}
	if _, err := do[message.SuccessResponse](ctx, c, "cancel", true, message.CancelRequest{QueryUUID: req.QueryUUID}); err != nil {
		return err
	}
	if _, err := do[message.ErrorResponse](ctx, c, "cancel_fetch", true,
		message.FetchRequest{QueryUUID: req.QueryUUID, BatchIndex: 1}); err != nil {
		return err
	}
	_, err = do[message.SuccessResponse](ctx, c, "cancel_wildcard", true, message.CancelRequest{})
	return err
}

// acknowledge captures ACKNOWLEDGEMENT for a finished result and the
// default (all-zero) form whose query_uuid field is omitted on the wire.
func (c *capturer) acknowledge(ctx context.Context) error {
	req, resp, err := c.prepare(ctx, "ack_prepare", "SELECT 7 AS seven", message.OptionalIdx{})
	if err != nil {
		return err
	}
	if resp.NeedsMoreFetch {
		return fmt.Errorf("ack_prepare: needs_more_fetch")
	}
	if _, _, err := c.exchange(ctx, "ack", c.header(true), message.Acknowledgement{QueryUUID: req.QueryUUID}); err != nil {
		return err
	}
	_, _, err = c.exchange(ctx, "ack_default", c.header(true), message.Acknowledgement{QueryUUID: codec.HugeInt{}})
	return err
}

func (c *capturer) disconnect(ctx context.Context) error {
	if _, err := do[message.SuccessResponse](ctx, c, "disconnect", false, message.DisconnectMessage{}); err != nil {
		return err
	}
	// The connection id is no longer valid.
	_, err := do[message.ErrorResponse](ctx, c, "after_disconnect", false, message.HeartbeatRequest{})
	return err
}
