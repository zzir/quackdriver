package message

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/logicaltype"
)

// Decode benchmarks over a FETCH_RESPONSE carrying one 2048-row chunk.
// BenchmarkDecode* measure Decode alone (values stay unboxed in the lazy
// columns); BenchmarkDecodeScan* also box every value once, which is what a
// driver reading all rows pays. The same data generator is used for the v1
// comparison benchmarks of the previous implementation.

func dateArg(t time.Time) any       { return int32(t.Unix() / 86400) }
func timestampArg(t time.Time) any  { return t.UnixMicro() }
func decimalArg(unscaled int64) any { return unscaled }

// benchColumns returns the column types and values of the benchmark chunks:
// 2048 rows each, generated deterministically.
func benchColumns(kind string) ([]logicaltype.Type, [][]any) {
	const rows = 2048
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	dec := logicaltype.Type{ID: logicaltype.IDDecimal, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraDecimal, Width: 18, Scale: 3}}
	gen := map[logicaltype.ID]func(i int) any{
		logicaltype.IDBigint:  func(i int) any { return int64(i) * 7919 },
		logicaltype.IDInteger: func(i int) any { return int32(i * 31) },
		logicaltype.IDDouble:  func(i int) any { return float64(i) / 7 },
		logicaltype.IDBoolean: func(i int) any { return i%3 == 0 },
		logicaltype.IDVarchar: func(i int) any {
			if i%10 == 9 {
				return nil
			}
			return fmt.Sprintf("value-%d-%s", i, strings.Repeat("x", i%17))
		},
		logicaltype.IDDate:      func(i int) any { return dateArg(ts.AddDate(0, 0, i)) },
		logicaltype.IDTimestamp: func(i int) any { return timestampArg(ts.Add(time.Duration(i) * time.Second)) },
		logicaltype.IDDecimal:   func(i int) any { return decimalArg(int64(i)*1001 - 5000) },
	}
	var ids []logicaltype.ID
	switch kind {
	case "int64":
		ids = []logicaltype.ID{logicaltype.IDBigint}
	case "varchar":
		ids = []logicaltype.ID{logicaltype.IDVarchar}
	case "mixed":
		ids = []logicaltype.ID{logicaltype.IDInteger, logicaltype.IDBigint, logicaltype.IDDouble, logicaltype.IDVarchar,
			logicaltype.IDBoolean, logicaltype.IDDate, logicaltype.IDTimestamp, logicaltype.IDDecimal}
	}
	types := make([]logicaltype.Type, len(ids))
	cols := make([][]any, len(ids))
	for c, id := range ids {
		types[c] = logicaltype.Type{ID: id}
		if id == logicaltype.IDDecimal {
			types[c] = dec
		}
		cols[c] = make([]any, rows)
		for i := range cols[c] {
			cols[c][i] = gen[id](i)
		}
	}
	return types, cols
}

func benchMessage(b *testing.B, kind string) []byte {
	b.Helper()
	types, cols := benchColumns(kind)
	columns := make([]Column, len(cols))
	for i, c := range cols {
		columns[i] = Values(c)
	}
	chunk := DataChunk{RowCount: len(cols[0]), Types: types, Columns: columns}
	msg, err := Encode(Header{ConnectionID: "bench"}, FetchResponse{Results: []DataChunk{chunk}, BatchIndex: Idx(1)})
	if err != nil {
		b.Fatal(err)
	}
	return msg
}

func benchDecode(b *testing.B, kind string, scan bool) {
	b.Helper()
	msg := benchMessage(b, kind)
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, body, err := Decode(msg)
		if err != nil {
			b.Fatal(err)
		}
		if scan {
			for _, c := range body.(FetchResponse).Results {
				for _, col := range c.Columns {
					for i := range col.Len() {
						_ = col.Value(i)
					}
				}
			}
		}
	}
}

func BenchmarkDecodeInt64(b *testing.B)       { benchDecode(b, "int64", false) }
func BenchmarkDecodeVarchar(b *testing.B)     { benchDecode(b, "varchar", false) }
func BenchmarkDecodeMixed(b *testing.B)       { benchDecode(b, "mixed", false) }
func BenchmarkDecodeScanInt64(b *testing.B)   { benchDecode(b, "int64", true) }
func BenchmarkDecodeScanVarchar(b *testing.B) { benchDecode(b, "varchar", true) }
func BenchmarkDecodeScanMixed(b *testing.B)   { benchDecode(b, "mixed", true) }

func BenchmarkEncodeMixed(b *testing.B) {
	types, cols := benchColumns("mixed")
	columns := make([]Column, len(cols))
	for i, c := range cols {
		columns[i] = Values(c)
	}
	body := FetchResponse{Results: []DataChunk{{RowCount: len(cols[0]), Types: types, Columns: columns}}, BatchIndex: Idx(1)}
	b.ReportAllocs()
	for range b.N {
		if _, err := Encode(Header{}, body); err != nil {
			b.Fatal(err)
		}
	}
}
