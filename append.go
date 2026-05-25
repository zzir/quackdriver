package quackdriver

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
)

// AppendColumn describes a single column for Conn.Append.
//
// Type is a DuckDB SQL type name (case-insensitive). Supported in v0.1:
//
//	BOOLEAN, BOOL
//	TINYINT, SMALLINT, INTEGER, BIGINT
//	UTINYINT, USMALLINT, UINTEGER, UBIGINT
//	HUGEINT, UHUGEINT
//	FLOAT, DOUBLE
//	VARCHAR, TEXT, STRING
//	BLOB, BYTEA
//	UUID
//	DATE, TIMESTAMP, TIMESTAMP_MS, TIMESTAMP_S, TIMESTAMP_NS
//	INTERVAL
//	DECIMAL(width,scale)
//	<elem>[]       — LIST<elem> (recursive)
//	<elem>[N]      — ARRAY<elem, N> (recursive)
//
// STRUCT/MAP appending requires constructing an `AppendColumn` with a
// hand-built logical type — not exposed in v0.1; raise an issue if you need it.
type AppendColumn struct {
	Name string
	Type string
}

// Append sends a batch of rows directly into table via the Quack APPEND
// path — faster than INSERT for bulk loads because the server skips parsing
// and binding. The table must already exist with matching column types.
//
// Each row's values are mapped to columns in order. Use nil for SQL NULL.
// Go types are converted to DuckDB columns as follows:
//
//	BOOLEAN          ← bool
//	TINYINT…BIGINT   ← any signed integer; widened
//	UTINYINT…UBIGINT ← any unsigned integer; widened
//	FLOAT/DOUBLE     ← float32/float64 (or convertible)
//	VARCHAR          ← string or []byte
//	BLOB             ← []byte (or string)
//	DATE             ← time.Time (UTC date component)
//	TIMESTAMP*       ← time.Time
func (c *Conn) Append(ctx context.Context, table string, columns []AppendColumn, rows [][]any) error {
	return c.AppendWithSchema(ctx, "", table, columns, rows)
}

// AppendWithSchema is Append with an explicit schema name (e.g. "main").
func (c *Conn) AppendWithSchema(ctx context.Context, schema, table string, columns []AppendColumn, rows [][]any) error {
	if c.closed.Load() {
		return fmt.Errorf("quackdriver: connection is closed")
	}
	if len(columns) == 0 {
		return fmt.Errorf("quackdriver: Append requires at least one column")
	}
	if table == "" {
		return fmt.Errorf("quackdriver: Append requires a table name")
	}

	types := make([]logicaltype.Type, len(columns))
	for i, col := range columns {
		t, err := parseAppendType(col.Type)
		if err != nil {
			return fmt.Errorf("quackdriver: column %q: %w", col.Name, err)
		}
		types[i] = t
	}

	colData := make([][]any, len(columns))
	for i := range colData {
		colData[i] = make([]any, len(rows))
	}
	for r, row := range rows {
		if len(row) != len(columns) {
			return fmt.Errorf("quackdriver: row %d has %d values, want %d", r, len(row), len(columns))
		}
		for c, v := range row {
			conv, err := convertAppendValue(types[c], v)
			if err != nil {
				return fmt.Errorf("quackdriver: row %d column %q: %w", r, columns[c].Name, err)
			}
			colData[c][r] = conv
		}
	}

	chunk := message.DataChunk{RowCount: len(rows), Types: types, Columns: colData}
	_, body, err := c.roundTrip(ctx, message.Header{}, message.AppendRequest{
		SchemaName:  schema,
		TableName:   table,
		AppendChunk: chunk,
	})
	if err != nil {
		return err
	}
	if _, ok := body.(message.SuccessResponse); !ok {
		return fmt.Errorf("quackdriver: APPEND returned %T, want SuccessResponse", body)
	}
	return nil
}

// parseAppendType lives in appendtype.go.

// convertAppendValue normalizes a Go value into the representation that the
// internal DataChunk encoder expects for the given LogicalType.
func convertAppendValue(t logicaltype.Type, v any) (any, error) {
	if v == nil {
		return nil, nil
	}

	switch t.ID {
	case logicaltype.IDBoolean:
		switch x := v.(type) {
		case bool:
			return x, nil
		}
		return nil, fmt.Errorf("BOOLEAN expects bool, got %T", v)

	case logicaltype.IDTinyint:
		n, err := toSigned(v)
		if err != nil {
			return nil, err
		}
		if n < -128 || n > 127 {
			return nil, fmt.Errorf("TINYINT value %d out of range", n)
		}
		return int8(n), nil
	case logicaltype.IDSmallint:
		n, err := toSigned(v)
		if err != nil {
			return nil, err
		}
		if n < -32768 || n > 32767 {
			return nil, fmt.Errorf("SMALLINT value %d out of range", n)
		}
		return int16(n), nil
	case logicaltype.IDInteger:
		n, err := toSigned(v)
		if err != nil {
			return nil, err
		}
		if n < -2147483648 || n > 2147483647 {
			return nil, fmt.Errorf("INTEGER value %d out of range", n)
		}
		return int32(n), nil
	case logicaltype.IDBigint:
		n, err := toSigned(v)
		if err != nil {
			return nil, err
		}
		return n, nil

	case logicaltype.IDUTinyint:
		n, err := toUnsigned(v)
		if err != nil {
			return nil, err
		}
		if n > 0xff {
			return nil, fmt.Errorf("UTINYINT value %d out of range", n)
		}
		return uint8(n), nil
	case logicaltype.IDUSmallint:
		n, err := toUnsigned(v)
		if err != nil {
			return nil, err
		}
		if n > 0xffff {
			return nil, fmt.Errorf("USMALLINT value %d out of range", n)
		}
		return uint16(n), nil
	case logicaltype.IDUInteger:
		n, err := toUnsigned(v)
		if err != nil {
			return nil, err
		}
		if n > 0xffffffff {
			return nil, fmt.Errorf("UINTEGER value %d out of range", n)
		}
		return uint32(n), nil
	case logicaltype.IDUBigint:
		n, err := toUnsigned(v)
		if err != nil {
			return nil, err
		}
		return n, nil

	case logicaltype.IDFloat:
		switch x := v.(type) {
		case float32:
			return x, nil
		case float64:
			return float32(x), nil
		case int:
			return float32(x), nil
		}
		return nil, fmt.Errorf("FLOAT expects float, got %T", v)
	case logicaltype.IDDouble:
		switch x := v.(type) {
		case float64:
			return x, nil
		case float32:
			return float64(x), nil
		case int:
			return float64(x), nil
		}
		return nil, fmt.Errorf("DOUBLE expects float, got %T", v)

	case logicaltype.IDVarchar:
		switch x := v.(type) {
		case string:
			return x, nil
		case []byte:
			return string(x), nil
		}
		return nil, fmt.Errorf("VARCHAR expects string, got %T", v)
	case logicaltype.IDBlob:
		switch x := v.(type) {
		case []byte:
			return x, nil
		case string:
			return []byte(x), nil
		}
		return nil, fmt.Errorf("BLOB expects []byte, got %T", v)

	case logicaltype.IDDate:
		t, err := toTime(v)
		if err != nil {
			return nil, err
		}
		return int32(t.UTC().Truncate(24 * time.Hour).Sub(dateEpoch) / (24 * time.Hour)), nil
	case logicaltype.IDTimestamp, logicaltype.IDTimestampTz:
		t, err := toTime(v)
		if err != nil {
			return nil, err
		}
		return t.UnixMicro(), nil
	case logicaltype.IDTimestampSec:
		t, err := toTime(v)
		if err != nil {
			return nil, err
		}
		return t.Unix(), nil
	case logicaltype.IDTimestampMs:
		t, err := toTime(v)
		if err != nil {
			return nil, err
		}
		return t.UnixMilli(), nil
	case logicaltype.IDTimestampNs:
		tt, err := toTime(v)
		if err != nil {
			return nil, err
		}
		return tt.UnixNano(), nil

	case logicaltype.IDHugeint:
		bi, err := toBigInt(v)
		if err != nil {
			return nil, err
		}
		// Signed 128-bit range: [-2^127, 2^127 - 1]. BitLen is not a usable
		// proxy here because BitLen(-2^127) == 128 and would falsely reject
		// the valid INT128_MIN.
		if bi.Cmp(hugeIntMin) < 0 || bi.Cmp(hugeIntMax) > 0 {
			return nil, fmt.Errorf("HUGEINT overflow: %s", bi.String())
		}
		return bi, nil
	case logicaltype.IDUHugeint:
		bi, err := toBigInt(v)
		if err != nil {
			return nil, err
		}
		if bi.Sign() < 0 {
			return nil, fmt.Errorf("UHUGEINT cannot be negative: %s", bi.String())
		}
		if bi.Cmp(uHugeIntMax) > 0 {
			return nil, fmt.Errorf("UHUGEINT overflow: %s", bi.String())
		}
		return bi, nil

	case logicaltype.IDUUID:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("UUID expects string, got %T", v)
		}
		bi, err := message.UUIDToBig(s)
		if err != nil {
			return nil, err
		}
		return bi, nil

	case logicaltype.IDDecimal:
		if t.Info == nil {
			return nil, fmt.Errorf("DECIMAL column missing width/scale metadata")
		}
		bi, err := decimalToUnscaled(v, t.Info.Scale)
		if err != nil {
			return nil, err
		}
		// Enforce the declared precision: |unscaled| < 10^width. The physical
		// type's integer range is necessarily wider (e.g. DECIMAL(3,2) maps
		// to INT16 but only permits ±999), so the physical range check below
		// is a redundant safety net for hand-built LogicalTypes.
		if exceedsDecimalPrecision(bi, int(t.Info.Width)) {
			return nil, fmt.Errorf("DECIMAL(%d,%d) precision overflow: %s", t.Info.Width, t.Info.Scale, bi.String())
		}
		// The encoder dispatches by physical type. DECIMAL(width) maps to
		// INT16/INT32/INT64/INT128 — narrow the unscaled value to the matching
		// Go integer width. Reject anything outside the declared precision
		// rather than silently wrapping (Go casts are modular, not saturating).
		switch t.PhysicalType() {
		case logicaltype.PhysicalInt16:
			if !bi.IsInt64() {
				return nil, fmt.Errorf("DECIMAL(%d,%d) overflow: %s", t.Info.Width, t.Info.Scale, bi.String())
			}
			n := bi.Int64()
			if n < math.MinInt16 || n > math.MaxInt16 {
				return nil, fmt.Errorf("DECIMAL(%d,%d) overflow: %s", t.Info.Width, t.Info.Scale, bi.String())
			}
			return int16(n), nil
		case logicaltype.PhysicalInt32:
			if !bi.IsInt64() {
				return nil, fmt.Errorf("DECIMAL(%d,%d) overflow: %s", t.Info.Width, t.Info.Scale, bi.String())
			}
			n := bi.Int64()
			if n < math.MinInt32 || n > math.MaxInt32 {
				return nil, fmt.Errorf("DECIMAL(%d,%d) overflow: %s", t.Info.Width, t.Info.Scale, bi.String())
			}
			return int32(n), nil
		case logicaltype.PhysicalInt64:
			if !bi.IsInt64() {
				return nil, fmt.Errorf("DECIMAL(%d,%d) overflow: %s", t.Info.Width, t.Info.Scale, bi.String())
			}
			return bi.Int64(), nil
		case logicaltype.PhysicalInt128:
			if bi.Cmp(hugeIntMin) < 0 || bi.Cmp(hugeIntMax) > 0 {
				return nil, fmt.Errorf("DECIMAL(%d,%d) overflow: %s", t.Info.Width, t.Info.Scale, bi.String())
			}
			return bi, nil
		}
		return nil, fmt.Errorf("DECIMAL width %d not supported", t.Info.Width)

	case logicaltype.IDInterval:
		switch x := v.(type) {
		case message.IntervalValue:
			return x, nil
		case time.Duration:
			return message.IntervalValue{Micros: int64(x / time.Microsecond)}, nil
		}
		return nil, fmt.Errorf("INTERVAL expects IntervalValue or time.Duration, got %T", v)

	case logicaltype.IDList:
		if t.Info == nil || t.Info.Child == nil {
			return nil, fmt.Errorf("LIST column missing child type")
		}
		return convertListValue(*t.Info.Child, v)

	case logicaltype.IDArray:
		if t.Info == nil || t.Info.Child == nil {
			return nil, fmt.Errorf("ARRAY column missing child type")
		}
		out, err := convertListValue(*t.Info.Child, v)
		if err != nil {
			return nil, err
		}
		got := out.([]any)
		if uint32(len(got)) != t.Info.ArraySize {
			return nil, fmt.Errorf("ARRAY expects %d elements, got %d", t.Info.ArraySize, len(got))
		}
		return got, nil

	case logicaltype.IDStruct:
		if t.Info == nil {
			return nil, fmt.Errorf("STRUCT column missing children")
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("STRUCT expects map[string]any, got %T", v)
		}
		out := make(map[string]any, len(t.Info.Children))
		for _, child := range t.Info.Children {
			conv, err := convertAppendValue(child.Type, m[child.Name])
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", child.Name, err)
			}
			out[child.Name] = conv
		}
		return out, nil

	case logicaltype.IDEnum:
		if t.Info == nil || len(t.Info.EnumValues) == 0 {
			return nil, fmt.Errorf("ENUM column has no labels")
		}
		// Resolve string labels and integer indices here so errors carry the
		// row/column context. The encoder's ENUM safety net remains for
		// hand-constructed DataChunks but should not be reached via Append.
		var idx int
		switch x := v.(type) {
		case string:
			found := -1
			for i, label := range t.Info.EnumValues {
				if label == x {
					found = i
					break
				}
			}
			if found < 0 {
				return nil, fmt.Errorf("ENUM label %q not in dictionary", x)
			}
			idx = found
		case int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64:
			n, err := toUnsigned(x)
			if err != nil {
				return nil, err
			}
			if n >= uint64(len(t.Info.EnumValues)) {
				return nil, fmt.Errorf("ENUM index %d out of range (dictionary size %d)", n, len(t.Info.EnumValues))
			}
			idx = int(n)
		default:
			return nil, fmt.Errorf("ENUM expects string label or integer index, got %T", v)
		}
		// Return as the concrete Go type matching the dictionary's physical
		// width so the encoder's regular uint8/uint16/uint32 path handles it.
		switch t.PhysicalType() {
		case logicaltype.PhysicalUint8:
			return uint8(idx), nil
		case logicaltype.PhysicalUint16:
			return uint16(idx), nil
		case logicaltype.PhysicalUint32:
			return uint32(idx), nil
		}
		return nil, fmt.Errorf("ENUM physical type %d not supported", t.PhysicalType())

	case logicaltype.IDUnion:
		if t.Info == nil || len(t.Info.Children) < 2 {
			return nil, fmt.Errorf("UNION column has no variants")
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("UNION expects map[string]any with one named variant, got %T", v)
		}
		// Children[0] is the unnamed tag; Children[1:] are the named variants.
		tagIdx := -1
		activeName := ""
		for i := 1; i < len(t.Info.Children); i++ {
			name := t.Info.Children[i].Name
			val, ok := m[name]
			if !ok || val == nil {
				continue
			}
			if tagIdx >= 0 {
				return nil, fmt.Errorf("UNION row sets multiple variants (%q and %q); pass exactly one", activeName, name)
			}
			tagIdx = i - 1
			activeName = name
		}
		if tagIdx < 0 {
			return nil, fmt.Errorf("UNION row must set one named variant; got %v", m)
		}
		out := make(map[string]any, len(t.Info.Children))
		out[""] = uint8(tagIdx)
		for i := 1; i < len(t.Info.Children); i++ {
			child := t.Info.Children[i]
			if child.Name == activeName {
				conv, err := convertAppendValue(child.Type, m[child.Name])
				if err != nil {
					return nil, fmt.Errorf("variant %q: %w", child.Name, err)
				}
				out[child.Name] = conv
			} else {
				out[child.Name] = nil
			}
		}
		return out, nil

	case logicaltype.IDMap:
		// MAP is physically a LIST<STRUCT<key,value>>. Accept either
		// map[string]any (keys must be strings) or []any with elements of
		// shape map[string]any{"key": ..., "value": ...}.
		if t.Info == nil || t.Info.Child == nil {
			return nil, fmt.Errorf("MAP column missing entry type")
		}
		entry := *t.Info.Child
		switch x := v.(type) {
		case map[string]any:
			pairs := make([]any, 0, len(x))
			for k, val := range x {
				pairs = append(pairs, map[string]any{"key": k, "value": val})
			}
			return convertListValue(entry, pairs)
		case []any:
			return convertListValue(entry, x)
		}
		return nil, fmt.Errorf("MAP expects map[string]any or []any of {key,value} pairs, got %T", v)
	}
	return nil, fmt.Errorf("internal: no conversion for logical type id %d", t.ID)
}

func convertListValue(child logicaltype.Type, v any) (any, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("LIST/ARRAY expects []any, got %T", v)
	}
	out := make([]any, len(list))
	for i, el := range list {
		conv, err := convertAppendValue(child, el)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		out[i] = conv
	}
	return out, nil
}

func toBigInt(v any) (*big.Int, error) {
	switch x := v.(type) {
	case *big.Int:
		return new(big.Int).Set(x), nil
	case big.Int:
		return new(big.Int).Set(&x), nil
	case int:
		return big.NewInt(int64(x)), nil
	case int8, int16, int32, int64:
		// covered above by int, but Go's type switch needs them spelled out
		s, _ := toSigned(x)
		return big.NewInt(s), nil
	case uint, uint8, uint16, uint32, uint64:
		u, _ := toUnsigned(x)
		bi := new(big.Int)
		bi.SetUint64(u)
		return bi, nil
	case string:
		bi, ok := new(big.Int).SetString(strings.TrimSpace(x), 10)
		if !ok {
			return nil, fmt.Errorf("invalid integer string %q", x)
		}
		return bi, nil
	}
	return nil, fmt.Errorf("integer expected, got %T", v)
}

// decimalToUnscaled converts a string/numeric/big.Int value to its unscaled
// integer representation given the column's scale. "12345.67" with scale 2
// becomes 1234567.
func decimalToUnscaled(v any, scale uint8) (*big.Int, error) {
	switch x := v.(type) {
	case *big.Int:
		return new(big.Int).Set(x), nil
	case big.Int:
		return new(big.Int).Set(&x), nil
	case string:
		return parseDecimalString(x, scale)
	case float32:
		return parseDecimalString(strconv.FormatFloat(float64(x), 'f', int(scale), 64), scale)
	case float64:
		return parseDecimalString(strconv.FormatFloat(x, 'f', int(scale), 64), scale)
	case int, int8, int16, int32, int64:
		s, _ := toSigned(x)
		bi := big.NewInt(s)
		shift := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
		return bi.Mul(bi, shift), nil
	}
	return nil, fmt.Errorf("DECIMAL expects string, *big.Int, or numeric, got %T", v)
}

func parseDecimalString(s string, scale uint8) (*big.Int, error) {
	orig := s
	s = strings.TrimSpace(s)
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg = true
		s = s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	dot := strings.IndexByte(s, '.')
	var intPart, fracPart string
	if dot < 0 {
		intPart = s
	} else {
		intPart = s[:dot]
		fracPart = s[dot+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	// Refuse to silently drop fractional digits the column can't represent —
	// the caller should round explicitly if that's what they want.
	if len(fracPart) > int(scale) {
		return nil, fmt.Errorf("DECIMAL %q has %d fractional digits, scale is %d", orig, len(fracPart), scale)
	}
	for len(fracPart) < int(scale) {
		fracPart += "0"
	}
	combined := intPart + fracPart
	if combined == "" {
		combined = "0"
	}
	bi, ok := new(big.Int).SetString(combined, 10)
	if !ok {
		return nil, fmt.Errorf("invalid DECIMAL string %q", orig)
	}
	if neg {
		bi.Neg(bi)
	}
	return bi, nil
}

func toSigned(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		// uint is 64-bit on 64-bit platforms; values above MaxInt64 would
		// silently become negative int64 values.
		if uint64(x) > math.MaxInt64 {
			return 0, fmt.Errorf("uint %d does not fit in int64", x)
		}
		return int64(x), nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		if x > math.MaxInt64 {
			return 0, fmt.Errorf("uint64 %d does not fit in int64", x)
		}
		return int64(x), nil
	}
	return 0, fmt.Errorf("integer expected, got %T", v)
}

func toUnsigned(v any) (uint64, error) {
	switch x := v.(type) {
	case uint:
		return uint64(x), nil
	case uint8:
		return uint64(x), nil
	case uint16:
		return uint64(x), nil
	case uint32:
		return uint64(x), nil
	case uint64:
		return x, nil
	case int:
		if x < 0 {
			return 0, fmt.Errorf("unsigned column got negative %d", x)
		}
		return uint64(x), nil
	case int8:
		if x < 0 {
			return 0, fmt.Errorf("unsigned column got negative %d", x)
		}
		return uint64(x), nil
	case int16:
		if x < 0 {
			return 0, fmt.Errorf("unsigned column got negative %d", x)
		}
		return uint64(x), nil
	case int32:
		if x < 0 {
			return 0, fmt.Errorf("unsigned column got negative %d", x)
		}
		return uint64(x), nil
	case int64:
		if x < 0 {
			return 0, fmt.Errorf("unsigned column got negative %d", x)
		}
		return uint64(x), nil
	}
	return 0, fmt.Errorf("unsigned integer expected, got %T", v)
}

func toTime(v any) (time.Time, error) {
	if t, ok := v.(time.Time); ok {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("time.Time expected, got %T", v)
}

// dateEpoch mirrors the internal/message dateEpoch — kept here to avoid
// exporting that package symbol just for one usage.
var dateEpoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// Signed/unsigned 128-bit bounds used by HUGEINT / UHUGEINT range checks.
var (
	hugeIntMin  = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 127))                  // -2^127
	hugeIntMax  = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 127), big.NewInt(1))   //  2^127 - 1
	uHugeIntMax = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))   //  2^128 - 1
)

// exceedsDecimalPrecision reports whether |v| >= 10^width — i.e. the
// unscaled integer cannot be represented at the column's declared precision.
func exceedsDecimalPrecision(v *big.Int, width int) bool {
	if width <= 0 {
		return v.Sign() != 0
	}
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(width)), nil)
	abs := new(big.Int).Abs(v)
	return abs.Cmp(limit) >= 0
}
