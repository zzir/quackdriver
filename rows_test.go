package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/quacktest"
)

// rowsFor builds a Rows over an already-complete PREPARE response, without
// a server.
func rowsFor(prep message.PrepareResponse) *Rows {
	return newRows(context.Background(), &query{conn: &Conn{}, prep: prep})
}

func intChunk(n int, cols ...message.Column) message.DataChunk {
	return message.DataChunk{RowCount: n, Types: []logicaltype.Type{typInteger}, Columns: cols}
}

// Regression: a chunk from the server with fewer columns or shorter column
// vectors than announced is an error, not a panic.
func TestRowsMalformedChunks(t *testing.T) {
	for _, c := range []struct {
		chunk message.DataChunk
		cols  int
		want  string
	}{
		{intChunk(1, message.Values{int32(1)}), 2, "chunk has 1 columns, expected at least 2"},
		{intChunk(2, message.Values{int32(1)}), 1, "column 0 has 1 rows but chunk declared 2 (row index 1)"},
		{intChunk(1, nil), 1, "column 0 has 0 rows"},
	} {
		r := rowsFor(message.PrepareResponse{Results: []message.DataChunk{c.chunk}})
		dest := make([]driver.Value, c.cols)
		var err error
		for err == nil {
			err = r.Next(dest)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("got %v, want %q", err, c.want)
		}
	}
}

// Empty chunks are skipped; Close is idempotent and ends the iteration.
func TestRowsIteration(t *testing.T) {
	r := rowsFor(message.PrepareResponse{Results: []message.DataChunk{
		intChunk(0, message.Values{}), intChunk(2, message.Values{int32(7), nil}), intChunk(1, message.Values{int32(8)}),
	}})
	dest := make([]driver.Value, 1)
	for _, want := range []any{int32(7), nil} {
		if err := r.Next(dest); err != nil || dest[0] != want {
			t.Fatalf("got %v %v, want %v", dest[0], err, want)
		}
	}
	for range 2 {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Next(dest); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after Close: %v", err)
	}
	r = rowsFor(message.PrepareResponse{Results: []message.DataChunk{intChunk(1, message.Values{int32(1)})}})
	_ = r.Next(dest)
	if err := r.Next(dest); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
	}
}

func dec(w, s uint8) logicaltype.Type {
	return logicaltype.Type{ID: logicaltype.IDDecimal, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraDecimal, Width: w, Scale: s}}
}

func TestScanTypes(t *testing.T) {
	ids := func(ids ...logicaltype.ID) []logicaltype.Type {
		out := make([]logicaltype.Type, len(ids))
		for i, id := range ids {
			out[i] = logicaltype.Type{ID: id}
		}
		return out
	}
	named := logicaltype.Type{ID: logicaltype.IDStruct, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraStruct,
		Children: []logicaltype.ChildType{{Name: "a", Type: typInteger}}}}
	unnamed := logicaltype.Type{ID: logicaltype.IDStruct, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraStruct,
		Children: []logicaltype.ChildType{{Type: typInteger}, {Type: typVarchar}}}}
	for _, c := range []struct {
		types []logicaltype.Type
		want  reflect.Type
	}{
		{ids(logicaltype.IDBoolean), reflect.TypeFor[bool]()},
		{ids(logicaltype.IDTinyint), reflect.TypeFor[int8]()},
		{ids(logicaltype.IDSmallint), reflect.TypeFor[int16]()},
		{ids(logicaltype.IDInteger), reflect.TypeFor[int32]()},
		{ids(logicaltype.IDBigint), reflect.TypeFor[int64]()},
		{ids(logicaltype.IDUTinyint), reflect.TypeFor[uint8]()},
		{ids(logicaltype.IDUSmallint), reflect.TypeFor[uint16]()},
		{ids(logicaltype.IDUInteger), reflect.TypeFor[uint32]()},
		{ids(logicaltype.IDUBigint), reflect.TypeFor[uint64]()},
		{ids(logicaltype.IDHugeint, logicaltype.IDUHugeint, logicaltype.IDBignum), reflect.TypeFor[*big.Int]()},
		{ids(logicaltype.IDFloat), reflect.TypeFor[float32]()},
		{ids(logicaltype.IDDouble), reflect.TypeFor[float64]()},
		{ids(logicaltype.IDDecimal, logicaltype.IDVarchar, logicaltype.IDChar, logicaltype.IDUUID, logicaltype.IDBit, logicaltype.IDEnum), reflect.TypeFor[string]()},
		{ids(logicaltype.IDBlob, logicaltype.IDGeometry, logicaltype.IDAggregateState), reflect.TypeFor[[]byte]()},
		{ids(logicaltype.IDDate, logicaltype.IDTimestamp, logicaltype.IDTimestampSec, logicaltype.IDTimestampMs,
			logicaltype.IDTimestampNs, logicaltype.IDTimestampTz, logicaltype.IDTimestampTzNs), reflect.TypeFor[time.Time]()},
		{ids(logicaltype.IDTime, logicaltype.IDTimeNs), reflect.TypeFor[time.Duration]()},
		{ids(logicaltype.IDTimeTz), reflect.TypeFor[TimeTzValue]()},
		{ids(logicaltype.IDInterval), reflect.TypeFor[IntervalValue]()},
		{append(ids(logicaltype.IDList, logicaltype.IDArray, logicaltype.IDMap, logicaltype.IDTuple), unnamed), reflect.TypeFor[[]any]()},
		{append(ids(logicaltype.IDStruct, logicaltype.IDUnion), named), reflect.TypeFor[map[string]any]()},
		{ids(logicaltype.IDVariant, logicaltype.IDSQLNull, logicaltype.IDInvalid), reflect.TypeFor[any]()},
	} {
		for _, typ := range c.types {
			if got := scanTypeFor(typ); got != c.want {
				t.Errorf("%s: scan type %v, want %v", typ.Name(), got, c.want)
			}
		}
	}
}

func TestColumnMetadataThroughDatabaseSQL(t *testing.T) {
	s := newServer(t)
	json := logicaltype.Type{ID: logicaltype.IDVarchar, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraString, Alias: "json"}}
	s.Script("SELECT meta", quacktest.Result{
		Names: []string{"n", "d", "ts", "j", "iv"},
		Types: []logicaltype.Type{typInteger, dec(10, 2), {ID: logicaltype.IDTimestampTz}, json, {ID: logicaltype.IDInterval}},
	})
	db := openDB(t, s)
	rows, err := db.Query("SELECT meta")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"INTEGER", "DECIMAL", "TIMESTAMP WITH TIME ZONE", "JSON", "INTERVAL"}
	for i, ct := range cts {
		if ct.DatabaseTypeName() != names[i] {
			t.Errorf("column %d: %q, want %q", i, ct.DatabaseTypeName(), names[i])
		}
		nullable, ok := ct.Nullable()
		if !nullable || !ok {
			t.Errorf("column %d nullable %v %v", i, nullable, ok)
		}
		p, sc, ok := ct.DecimalSize()
		if i == 1 {
			if !ok || p != 10 || sc != 2 {
				t.Errorf("decimal size %d,%d,%v", p, sc, ok)
			}
		} else if ok {
			t.Errorf("column %d reports a decimal size", i)
		}
	}
	if cts[2].ScanType() != reflect.TypeOf(time.Time{}) || cts[4].ScanType() != reflect.TypeOf(IntervalValue{}) {
		t.Errorf("scan types %v %v", cts[2].ScanType(), cts[4].ScanType())
	}
	r := rowsFor(message.PrepareResponse{ResultTypes: []logicaltype.Type{typInteger}, ResultNames: []string{"x"}})
	if r.ColumnTypeDatabaseTypeName(5) != "" || r.ColumnTypeScanType(-1) != reflect.TypeFor[any]() {
		t.Error("out-of-range column metadata")
	}
	if _, _, ok := r.ColumnTypePrecisionScale(3); ok {
		t.Error("out-of-range precision")
	}
	if _, _, ok := rowsFor(message.PrepareResponse{ResultTypes: []logicaltype.Type{{ID: logicaltype.IDDecimal}}}).ColumnTypePrecisionScale(0); ok {
		t.Error("DECIMAL without type info must not report a size")
	}
}

// TestDecodedValuesScan checks the documented Go types end to end.
func TestDecodedValuesScan(t *testing.T) {
	s := newServer(t)
	tz, err := NewTimeTz(13*time.Hour+30*time.Minute, 3600)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 10, 21, 8, 30, 0, 123456789, time.UTC)
	types := []logicaltype.Type{
		typInteger, typVarchar, dec(10, 2), {ID: logicaltype.IDInterval}, {ID: logicaltype.IDTimeTz},
		{ID: logicaltype.IDTimestampTzNs}, {ID: logicaltype.IDHugeint}, {ID: logicaltype.IDBlob},
	}
	huge, _ := new(big.Int).SetString("-170141183460469231731687303715884105728", 10)
	s.Script("SELECT many", quacktest.Rows(
		[]string{"i", "v", "d", "iv", "tz", "ts", "h", "b"}, types,
		[][]any{{int32(-5), "duck", "12345678.90", IntervalValue{Months: 1, Days: 2, Micros: 3}, tz, when, huge, []byte{0, 1}}},
		0, 0))
	db := openDB(t, s)
	var (
		i  int32
		v  string
		d  string
		iv IntervalValue
		tv TimeTzValue
		ts time.Time
		h  *big.Int
		b  []byte
	)
	if err := db.QueryRow("SELECT many").Scan(&i, &v, &d, &iv, &tv, &ts, &h, &b); err != nil {
		t.Fatal(err)
	}
	if i != -5 || v != "duck" || d != "12345678.90" || iv != (IntervalValue{Months: 1, Days: 2, Micros: 3}) ||
		tv != tz || !ts.Equal(when) || h.Cmp(huge) != 0 || string(b) != "\x00\x01" {
		t.Fatalf("scanned %v %q %q %+v %v %v %v %v", i, v, d, iv, tv, ts, h, b)
	}
}
