package quackdriver

import (
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
)

func TestParseAppendTypeComplex(t *testing.T) {
	type tc struct {
		name string
		id   logicaltype.ID
		// optional extras
		w, s     uint8
		listElem logicaltype.ID
		arrSize  uint32
	}
	cases := []tc{
		{name: "DECIMAL(9,2)", id: logicaltype.IDDecimal, w: 9, s: 2},
		{name: "HUGEINT", id: logicaltype.IDHugeint},
		{name: "UHUGEINT", id: logicaltype.IDUHugeint},
		{name: "UUID", id: logicaltype.IDUUID},
		{name: "INTERVAL", id: logicaltype.IDInterval},
		{name: "INTEGER[]", id: logicaltype.IDList, listElem: logicaltype.IDInteger},
		{name: "VARCHAR[3]", id: logicaltype.IDArray, listElem: logicaltype.IDVarchar, arrSize: 3},
	}
	for _, c := range cases {
		got, err := parseAppendType(c.name)
		if err != nil {
			t.Fatalf("%q: %v", c.name, err)
		}
		if got.ID != c.id {
			t.Fatalf("%q: id %d, want %d", c.name, got.ID, c.id)
		}
		if c.id == logicaltype.IDDecimal {
			if got.Info == nil || got.Info.Width != c.w || got.Info.Scale != c.s {
				t.Fatalf("%q: decimal info: %+v", c.name, got.Info)
			}
		}
		if c.listElem != 0 {
			if got.Info == nil || got.Info.Child == nil || got.Info.Child.ID != c.listElem {
				t.Fatalf("%q: list/array child: %+v", c.name, got.Info)
			}
			if c.arrSize > 0 && got.Info.ArraySize != c.arrSize {
				t.Fatalf("%q: array size %d, want %d", c.name, got.Info.ArraySize, c.arrSize)
			}
		}
	}
}

func TestParseDecimalStringConversion(t *testing.T) {
	cases := []struct {
		in    string
		scale uint8
		want  string
	}{
		{"12345.67", 2, "1234567"},
		{"12345.6", 2, "1234560"},
		{"-12345.67", 2, "-1234567"},
		{"0.01", 2, "1"},
		{"0", 2, "0"},
		{"+5", 0, "5"},
	}
	for _, c := range cases {
		got, err := parseDecimalString(c.in, c.scale)
		if err != nil {
			t.Fatalf("%q scale %d: %v", c.in, c.scale, err)
		}
		if got.String() != c.want {
			t.Fatalf("%q scale %d: got %s, want %s", c.in, c.scale, got, c.want)
		}
	}
}

func TestParseAppendType(t *testing.T) {
	cases := map[string]logicaltype.ID{
		"BOOLEAN":      logicaltype.IDBoolean,
		"bool":         logicaltype.IDBoolean,
		"TINYINT":      logicaltype.IDTinyint,
		"INTEGER":      logicaltype.IDInteger,
		"int":          logicaltype.IDInteger,
		"BIGINT":       logicaltype.IDBigint,
		"UBIGINT":      logicaltype.IDUBigint,
		"  varchar  ":  logicaltype.IDVarchar,
		"text":         logicaltype.IDVarchar,
		"BLOB":         logicaltype.IDBlob,
		"DATE":         logicaltype.IDDate,
		"TIMESTAMP":    logicaltype.IDTimestamp,
		"TIMESTAMP_MS": logicaltype.IDTimestampMs,
		"DOUBLE":       logicaltype.IDDouble,
	}
	for name, want := range cases {
		got, err := parseAppendType(name)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if got.ID != want {
			t.Fatalf("%q: got id %d, want %d", name, got.ID, want)
		}
	}

	if _, err := parseAppendType("NONEXISTENT_TYPE"); err == nil {
		t.Fatal("expected error for unknown type name")
	}
}

func TestConvertAppendValueRanges(t *testing.T) {
	if _, err := convertAppendValue(logicaltype.Type{ID: logicaltype.IDTinyint}, 200); err == nil {
		t.Fatal("TINYINT 200 should overflow")
	}
	if _, err := convertAppendValue(logicaltype.Type{ID: logicaltype.IDUTinyint}, -1); err == nil {
		t.Fatal("UTINYINT -1 should be rejected")
	}
	if v, err := convertAppendValue(logicaltype.Type{ID: logicaltype.IDInteger}, 42); err != nil || v != int32(42) {
		t.Fatalf("INTEGER 42 → int32(42), got %v err %v", v, err)
	}
	if v, err := convertAppendValue(logicaltype.Type{ID: logicaltype.IDBigint}, uint32(7)); err != nil || v != int64(7) {
		t.Fatalf("BIGINT widening: %v err %v", v, err)
	}
}

func TestConvertAppendValueDateTimestamp(t *testing.T) {
	ts := time.Date(2026, 5, 25, 12, 34, 56, 789_000_000, time.UTC)

	got, err := convertAppendValue(logicaltype.Type{ID: logicaltype.IDDate}, ts)
	if err != nil {
		t.Fatal(err)
	}
	// 2026-05-25 minus 1970-01-01 = 20598 days.
	if got != int32(20598) {
		t.Fatalf("DATE: %v", got)
	}

	got, err = convertAppendValue(logicaltype.Type{ID: logicaltype.IDTimestamp}, ts)
	if err != nil {
		t.Fatal(err)
	}
	if got != ts.UnixMicro() {
		t.Fatalf("TIMESTAMP: %v want %v", got, ts.UnixMicro())
	}
}

func TestConvertAppendValueRejectsNonString(t *testing.T) {
	if _, err := convertAppendValue(logicaltype.Type{ID: logicaltype.IDVarchar}, 7); err == nil {
		t.Fatal("VARCHAR should reject int")
	}
}

func TestConvertAppendValueDecimal(t *testing.T) {
	dec96 := logicaltype.Type{ID: logicaltype.IDDecimal, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraDecimal, Width: 9, Scale: 6}} // INT32 backing
	got, err := convertAppendValue(dec96, "12.345678")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := got.(int32); !ok || v != 12345678 {
		t.Fatalf("DECIMAL(9,6) → int32(12345678), got %T %v", got, got)
	}

	dec184 := logicaltype.Type{ID: logicaltype.IDDecimal, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraDecimal, Width: 18, Scale: 4}}
	got, err = convertAppendValue(dec184, "1234.5678")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := got.(int64); !ok || v != 12345678 {
		t.Fatalf("DECIMAL(18,4) → int64(12345678), got %T %v", got, got)
	}

	dec384 := logicaltype.Type{ID: logicaltype.IDDecimal, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraDecimal, Width: 38, Scale: 0}}
	// 10^38 - 1 is the largest value DECIMAL(38,0) can represent. INT128_MAX
	// (~1.7e38) would fit the physical INT128 backing but violates declared
	// precision — that's covered by TestConvertAppendValueDecimalPrecision.
	max38 := "99999999999999999999999999999999999999"
	got, err = convertAppendValue(dec384, max38)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*big.Int); !ok {
		t.Fatalf("DECIMAL(38,0) → *big.Int, got %T", got)
	}
}

func TestConvertAppendValueHugeIntUUID(t *testing.T) {
	huge := logicaltype.Type{ID: logicaltype.IDHugeint}
	got, err := convertAppendValue(huge, "170141183460469231731687303715884105727")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*big.Int); !ok {
		t.Fatalf("HUGEINT → *big.Int, got %T", got)
	}
	if _, err := convertAppendValue(huge, "170141183460469231731687303715884105728"); err == nil {
		t.Fatal("expected overflow for 2^127")
	}

	// Regression: -2^127 is the valid INT128_MIN and must be accepted. The
	// previous BitLen-based check incorrectly rejected it because BitLen
	// operates on the absolute value (|−2^127| = 2^127 has 128 bits).
	if _, err := convertAppendValue(huge, "-170141183460469231731687303715884105728"); err != nil {
		t.Fatalf("INT128_MIN should be accepted, got %v", err)
	}
	if _, err := convertAppendValue(huge, "-170141183460469231731687303715884105729"); err == nil {
		t.Fatal("expected overflow one below INT128_MIN")
	}

	uuid := logicaltype.Type{ID: logicaltype.IDUUID}
	got, err = convertAppendValue(uuid, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*big.Int); !ok {
		t.Fatalf("UUID → *big.Int, got %T", got)
	}
}

// Regression: DECIMAL must enforce declared precision, not just the
// physical integer width. DECIMAL(3,2) uses INT16 (±32767) but the
// declared precision only permits ±999. Earlier code accepted "12.34"
// (unscaled 1234) — fits int16 but violates the column.
func TestConvertAppendValueDecimalPrecision(t *testing.T) {
	d32 := logicaltype.Type{ID: logicaltype.IDDecimal, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraDecimal, Width: 3, Scale: 2}}
	if _, err := convertAppendValue(d32, "12.34"); err == nil {
		t.Fatal("DECIMAL(3,2) should reject 12.34 (unscaled 1234 > 10^3)")
	}
	if v, err := convertAppendValue(d32, "9.99"); err != nil || v != int16(999) {
		t.Fatalf("DECIMAL(3,2) accepting 9.99: got %v err %v", v, err)
	}
}

// Regression: DECIMAL must not silently truncate extra fractional digits.
// Previously "1.239" with scale=2 became 123 (i.e. 1.23) without error.
func TestParseDecimalRejectsExtraFractionalDigits(t *testing.T) {
	if _, err := parseDecimalString("1.239", 2); err == nil {
		t.Fatal("scale=2 must reject a 3-digit fractional part")
	}
	if _, err := parseDecimalString("0.0001", 2); err == nil {
		t.Fatal("scale=2 must reject a 4-digit fractional part")
	}
	if v, err := parseDecimalString("1.5", 2); err != nil || v.String() != "150" {
		t.Fatalf("scale=2, '1.5' → 150 (padded): got %s err %v", v, err)
	}
}

// Regression: DECIMAL conversion must reject values that don't fit the
// declared precision rather than silently wrapping. Previously the
// `int16(bi.Int64())` cast turned 32768 into -32768.
func TestConvertAppendValueDecimalOverflow(t *testing.T) {
	cases := []struct {
		width, scale uint8
		input        string
	}{
		{4, 2, "327.68"},                // unscaled 32768 > MaxInt16
		{4, 0, "32768"},                 // same overflow, no fractional pad
		{9, 0, "2147483648"},            // > MaxInt32
		{18, 0, "9223372036854775808"},  // > MaxInt64
	}
	for _, c := range cases {
		dt := logicaltype.Type{ID: logicaltype.IDDecimal, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraDecimal, Width: c.width, Scale: c.scale}}
		if _, err := convertAppendValue(dt, c.input); err == nil {
			t.Errorf("DECIMAL(%d,%d) %q: expected overflow, got success", c.width, c.scale, c.input)
		}
	}
}

// Regression: large uint64 values must not silently wrap to negative
// int64 when fed to a BIGINT column.
func TestConvertAppendValueBigUintOverflowsInt64(t *testing.T) {
	bigint := logicaltype.Type{ID: logicaltype.IDBigint}
	if _, err := convertAppendValue(bigint, uint64(1<<63)); err == nil {
		t.Fatal("uint64(1<<63) must be rejected for BIGINT (exactly MaxInt64+1)")
	}
	if _, err := convertAppendValue(bigint, uint64(math.MaxUint64)); err == nil {
		t.Fatal("uint64 MaxUint64 must be rejected for BIGINT")
	}
	// MaxInt64 is the boundary that DOES fit.
	if v, err := convertAppendValue(bigint, uint64(math.MaxInt64)); err != nil || v != int64(math.MaxInt64) {
		t.Fatalf("uint64 MaxInt64 → int64 MaxInt64: got %v err %v", v, err)
	}
}

// Regression: UTINYINT and friends must accept int8/int16 values, which
// were missing from the toUnsigned switch.
func TestConvertAppendValueUnsignedFromSignedNarrow(t *testing.T) {
	uti := logicaltype.Type{ID: logicaltype.IDUTinyint}
	if v, err := convertAppendValue(uti, int8(5)); err != nil || v != uint8(5) {
		t.Fatalf("UTINYINT int8(5): got %v err %v", v, err)
	}
	usi := logicaltype.Type{ID: logicaltype.IDUSmallint}
	if v, err := convertAppendValue(usi, int16(7)); err != nil || v != uint16(7) {
		t.Fatalf("USMALLINT int16(7): got %v err %v", v, err)
	}
	if _, err := convertAppendValue(uti, int8(-1)); err == nil {
		t.Fatal("UTINYINT int8(-1) must be rejected")
	}
}

func TestConvertAppendValueInterval(t *testing.T) {
	iv := logicaltype.Type{ID: logicaltype.IDInterval}
	got, err := convertAppendValue(iv, 90*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := got.(message.IntervalValue)
	if !ok || v.Micros != int64(90*time.Minute/time.Microsecond) {
		t.Fatalf("INTERVAL duration: %+v", got)
	}
}

func TestConvertAppendValueList(t *testing.T) {
	int32t := logicaltype.Type{ID: logicaltype.IDInteger}
	listt := logicaltype.Type{ID: logicaltype.IDList, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraList, Child: &int32t}}
	got, err := convertAppendValue(listt, []any{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	out := got.([]any)
	for i, want := range []int32{1, 2, 3} {
		if out[i] != want {
			t.Fatalf("element %d: %v want %v", i, out[i], want)
		}
	}
}

func TestConvertAppendValueArraySizeMismatch(t *testing.T) {
	int32t := logicaltype.Type{ID: logicaltype.IDInteger}
	arrt := logicaltype.Type{ID: logicaltype.IDArray, Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraArray, Child: &int32t, ArraySize: 3}}
	if _, err := convertAppendValue(arrt, []any{1, 2}); err == nil {
		t.Fatal("expected size mismatch error")
	}
}

func TestConvertAppendValueNil(t *testing.T) {
	for _, id := range []logicaltype.ID{
		logicaltype.IDBoolean, logicaltype.IDInteger, logicaltype.IDVarchar, logicaltype.IDBlob,
		logicaltype.IDDate, logicaltype.IDTimestamp,
	} {
		got, err := convertAppendValue(logicaltype.Type{ID: id}, nil)
		if err != nil {
			t.Fatalf("id %d nil: %v", id, err)
		}
		if got != nil {
			t.Fatalf("id %d nil: got %v", id, got)
		}
	}
}
