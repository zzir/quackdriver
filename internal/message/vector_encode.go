package message

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/value"
)

// The vector encoder writes every vector FLAT, byte for byte the way DuckDB
// v2.0.0 Vector::Serialize writes a flat vector at StorageVersion::V2_0_0
// (src/common/types/vector.cpp), with one deliberate difference: the validity
// mask is written only when a row is NULL (DuckDB writes it whenever the
// vector *can* hold NULLs; both read back identically).
//
//   - fixed-size types: 102 data, NULL rows hold NullValue<T>() like
//     VectorOperations::WriteToStorage;
//   - GEOMETRY: 99 geometry_format = WKB, 102 list<blob> (NULL rows hold the
//     one-byte NullValue<string_t>() "\x80");
//   - other VARCHAR-physical types: 107 byte_data_length, 108 length_data
//     (u32 per row, 0 for NULL), 109 byte_data;
//   - STRUCT/UNION/TUPLE: 103 children; LIST/MAP: 104/105/106;
//     ARRAY: 103/104.
//
// Accepted Go values per logical type (nil is always SQL NULL):
//
//	BOOLEAN                       bool
//	TINYINT..UBIGINT              any Go integer within the type's range
//	FLOAT, DOUBLE                 float32, float64, any Go integer
//	DECIMAL                       string ("-12.50"; no more fractional digits
//	                              than the scale unless they are zeros), or the
//	                              unscaled value as a Go integer or *big.Int
//	HUGEINT, UHUGEINT, BIGNUM     *big.Int or any Go integer
//	UUID                          string (hex, dashes and braces optional) or the
//	                              raw hugeint as *big.Int (see UUIDToBig)
//	VARCHAR, CHAR, JSON, ...      string or []byte
//	BLOB, GEOMETRY (WKB)          []byte or string
//	BIT                           string of '0'/'1'
//	DATE                          time.Time (its UTC date) or days as an integer
//	TIME, TIME_NS                 time.Duration or the raw integer
//	TIME WITH TIME ZONE           value.TimeTzValue or the raw bits as an integer
//	TIMESTAMP (all precisions)    time.Time or the raw integer
//	INTERVAL                      value.IntervalValue or time.Duration
//	ENUM                          string label or integer index
//	LIST, MAP                     []any (MAP entries: map[string]any{"key", "value"})
//	ARRAY                         []any of exactly the array size
//	STRUCT                        map[string]any (missing fields are NULL)
//	TUPLE                         []any, one value per field
//	UNION                         map[string]any with one non-nil member; the tag
//	                              (key "") is optional
//	SQLNULL                       nil only
//
// VARIANT columns cannot be encoded.

// encodeVector writes one vector object holding vals.
func encodeVector(w *codec.Writer, t logicaltype.Type, vals []any, depth int) error {
	if depth > maxVectorDepth {
		return fmt.Errorf("type nesting exceeds %d levels", maxVectorDepth)
	}
	phys := t.PhysicalType()
	if t.ID == logicaltype.IDVariant {
		return errors.New("encoding VARIANT values is not supported")
	}

	if t.ID == logicaltype.IDGeometry {
		w.WriteFieldID(99)
		w.WriteULEB128(geometryStorageWKB)
	}
	hasNull := false
	for _, v := range vals {
		if v == nil {
			hasNull = true
			break
		}
	}
	w.WriteFieldID(100)
	w.WriteBool(hasNull)
	if hasNull {
		w.WriteFieldID(101)
		w.WriteBlob(validityMask(vals))
	}

	var err error
	switch {
	case logicaltype.IsFixedSize(phys): // SQLNULL is physically INT32
		err = encodeFixedVector(w, t, phys, vals)
	case t.ID == logicaltype.IDGeometry:
		err = encodeGeometryVector(w, vals)
	case phys == logicaltype.PhysicalVarchar:
		err = encodeStringVector(w, t, vals)
	case phys == logicaltype.PhysicalStruct:
		err = encodeStructVector(w, t, vals, depth)
	case phys == logicaltype.PhysicalList:
		err = encodeListVector(w, t, vals, depth)
	case phys == logicaltype.PhysicalArray:
		err = encodeArrayVector(w, t, vals, depth)
	default:
		err = fmt.Errorf("cannot encode values of type %s", t.Name())
	}
	if err != nil {
		return err
	}
	w.WriteEndObject()
	return nil
}

// validityMask builds a ValidityMask for vals: 64-row little-endian words,
// all bits set except those of NULL rows (trailing bits stay set, as in a
// freshly initialized DuckDB mask).
func validityMask(vals []any) []byte {
	out := bytes.Repeat([]byte{0xff}, int(validityMaskSize(len(vals))))
	for i, v := range vals {
		if v == nil {
			out[i>>3] &^= 1 << (uint(i) & 7)
		}
	}
	return out
}

func rowError(i int, err error) error { return fmt.Errorf("row %d: %w", i, err) }

func encodeFixedVector(w *codec.Writer, t logicaltype.Type, phys logicaltype.PhysicalType, vals []any) error {
	width, err := logicaltype.FixedSize(phys)
	if err != nil {
		return err
	}
	body := make([]byte, 0, len(vals)*width)
	for i, v := range vals {
		if v == nil {
			body = appendNullValue(body, phys)
			continue
		}
		if body, err = appendFixedValue(body, t, phys, v); err != nil {
			return rowError(i, err)
		}
	}
	w.WriteFieldID(102)
	w.WriteBlob(body)
	return nil
}

// appendNullValue appends DuckDB's NullValue<T>() for the physical type,
// which WriteToStorage stores in NULL rows.
func appendNullValue(buf []byte, phys logicaltype.PhysicalType) []byte {
	switch phys {
	case logicaltype.PhysicalBool, logicaltype.PhysicalInt8:
		return append(buf, 0x80)
	case logicaltype.PhysicalInt16:
		return binary.LittleEndian.AppendUint16(buf, 1<<15)
	case logicaltype.PhysicalInt32:
		return binary.LittleEndian.AppendUint32(buf, 1<<31)
	case logicaltype.PhysicalInt64:
		return binary.LittleEndian.AppendUint64(buf, 1<<63)
	case logicaltype.PhysicalFloat:
		return binary.LittleEndian.AppendUint32(buf, 0x7fc00000) // NAN
	case logicaltype.PhysicalDouble:
		return binary.LittleEndian.AppendUint64(buf, 0x7ff8000000000000) // NAN
	case logicaltype.PhysicalInt128:
		buf = binary.LittleEndian.AppendUint64(buf, 0)
		return binary.LittleEndian.AppendUint64(buf, 1<<63)
	case logicaltype.PhysicalInterval:
		buf = binary.LittleEndian.AppendUint32(buf, 1<<31)
		buf = binary.LittleEndian.AppendUint32(buf, 1<<31)
		return binary.LittleEndian.AppendUint64(buf, 1<<63)
	}
	// Unsigned types (and uhugeint_t, which has no numeric_limits): zero.
	size, _ := logicaltype.FixedSize(phys)
	return append(buf, make([]byte, size)...)
}

func appendFixedValue(buf []byte, t logicaltype.Type, phys logicaltype.PhysicalType, v any) ([]byte, error) {
	switch t.ID {
	case logicaltype.IDSQLNull:
		return nil, fmt.Errorf("SQLNULL column holds %T, want nil", v)
	case logicaltype.IDBoolean:
		b, ok := v.(bool)
		if !ok {
			return nil, typeError(t, v, "bool")
		}
		if b {
			return append(buf, 1), nil
		}
		return append(buf, 0), nil
	case logicaltype.IDFloat:
		f, ok := asFloat(v)
		if !ok {
			return nil, typeError(t, v, "float32 or float64")
		}
		return binary.LittleEndian.AppendUint32(buf, math.Float32bits(float32(f))), nil
	case logicaltype.IDDouble:
		f, ok := asFloat(v)
		if !ok {
			return nil, typeError(t, v, "float64 or float32")
		}
		return binary.LittleEndian.AppendUint64(buf, math.Float64bits(f)), nil
	case logicaltype.IDDecimal:
		if n, ok := asInt64(v); ok {
			// Fast path for an unscaled value given as a Go integer.
			if w := int(t.Info.Width); w < len(pow10Int64) && (n <= -pow10Int64[w] || n >= pow10Int64[w]) {
				return nil, fmt.Errorf("value %d does not fit DECIMAL(%d,%d)", n, t.Info.Width, t.Info.Scale)
			}
			return appendIntFixed(buf, phys, n)
		}
		u, err := decimalUnscaled(t, v)
		if err != nil {
			return nil, err
		}
		return appendBigFixed(buf, phys, u)
	case logicaltype.IDHugeint, logicaltype.IDUHugeint:
		if n, ok := asInt64(v); ok && (n >= 0 || phys == logicaltype.PhysicalInt128) {
			return appendIntFixed(buf, phys, n)
		}
		b, ok := asBig(v)
		if !ok {
			return nil, typeError(t, v, "*big.Int or an integer")
		}
		return appendBigFixed(buf, phys, b)
	case logicaltype.IDUUID:
		if s, ok := v.(string); ok {
			b, err := parseUUID(s)
			if err != nil {
				return nil, err
			}
			return appendBigFixed(buf, phys, b)
		}
		if b, ok := v.(*big.Int); ok && b != nil {
			return appendBigFixed(buf, phys, b)
		}
		return nil, typeError(t, v, "string or *big.Int")
	case logicaltype.IDInterval:
		var iv IntervalValue
		switch x := v.(type) {
		case IntervalValue:
			iv = x
		case time.Duration:
			iv = IntervalValue{Micros: int64(x / time.Microsecond)}
		default:
			return nil, typeError(t, v, "IntervalValue or time.Duration")
		}
		buf = binary.LittleEndian.AppendUint32(buf, uint32(iv.Months))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(iv.Days))
		return binary.LittleEndian.AppendUint64(buf, uint64(iv.Micros)), nil
	case logicaltype.IDEnum:
		idx, err := enumIndex(t, v)
		if err != nil {
			return nil, err
		}
		return appendUintFixed(buf, phys, uint64(idx))
	}

	// The remaining types store an integer: either given raw or converted
	// from the logical Go value.
	raw, isTemporal, err := temporalRaw(t, v)
	if err != nil {
		return nil, err
	}
	if isTemporal {
		return appendIntFixed(buf, phys, raw)
	}
	switch phys {
	case logicaltype.PhysicalUint8, logicaltype.PhysicalUint16, logicaltype.PhysicalUint32, logicaltype.PhysicalUint64:
		u, ok := asUint64(v)
		if !ok {
			return nil, typeError(t, v, "a non-negative integer")
		}
		return appendUintFixed(buf, phys, u)
	case logicaltype.PhysicalInt8, logicaltype.PhysicalInt16, logicaltype.PhysicalInt32, logicaltype.PhysicalInt64:
		n, ok := asInt64(v)
		if !ok {
			return nil, typeError(t, v, "an integer")
		}
		return appendIntFixed(buf, phys, n)
	}
	return nil, fmt.Errorf("cannot encode values of type %s", t.Name())
}

func typeError(t logicaltype.Type, v any, want string) error {
	return fmt.Errorf("%s value has Go type %T, want %s", t.Name(), v, want)
}

// temporalRaw converts the Go value of a DATE, TIME or TIMESTAMP-family
// type to its stored integer; isTemporal is false for other types. Raw
// integers are stored unchanged.
func temporalRaw(t logicaltype.Type, v any) (raw int64, isTemporal bool, err error) {
	var perSecond int64 // 0 unless t is a TIMESTAMP type
	switch t.ID {
	case logicaltype.IDDate, logicaltype.IDTime, logicaltype.IDTimeNs, logicaltype.IDTimeTz:
	case logicaltype.IDTimestampSec, logicaltype.IDTimestampMs, logicaltype.IDTimestamp, logicaltype.IDTimestampTz,
		logicaltype.IDTimestampNs, logicaltype.IDTimestampTzNs:
		perSecond = timestampUnits(t.ID)
	default:
		return 0, false, nil
	}
	want := "time.Time"
	switch t.ID {
	case logicaltype.IDTime, logicaltype.IDTimeNs:
		want = "time.Duration"
	case logicaltype.IDTimeTz:
		want = "TimeTzValue"
	}
	switch x := v.(type) {
	case time.Time:
		// ±infinity become DuckDB's sentinels: ±(2^31-1) days, ±(2^63-1) units.
		inf := int64(value.Infinity(x))
		switch {
		case t.ID == logicaltype.IDDate && inf != 0:
			return inf * math.MaxInt32, true, nil
		case t.ID == logicaltype.IDDate:
			return floorDiv(x.Unix(), 86400), true, nil
		case perSecond != 0 && inf != 0:
			return inf * math.MaxInt64, true, nil
		case perSecond != 0:
			// Round like DuckDB's casts to a lower precision.
			if raw, ok := value.UnixRounded(x, perSecond); ok {
				return raw, true, nil
			}
			return 0, true, fmt.Errorf("%s value %v out of range", t.Name(), v)
		}
	case time.Duration:
		switch t.ID {
		case logicaltype.IDTime:
			return int64(x / time.Microsecond), true, nil
		case logicaltype.IDTimeNs:
			return int64(x), true, nil
		}
	case TimeTzValue:
		if t.ID == logicaltype.IDTimeTz {
			// The server stores the word unchecked; an invalid one can
			// later invalidate its whole database when rendered.
			return int64(x.Bits()), true, x.Validate()
		}
	case uint64:
		if t.ID == logicaltype.IDTimeTz { // raw dtime_tz_t bits may set the top bit
			return int64(x), true, nil
		}
	}
	if n, ok := asInt64(v); ok {
		return n, true, nil
	}
	return 0, true, typeError(t, v, want+" or an integer")
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func appendIntFixed(buf []byte, phys logicaltype.PhysicalType, n int64) ([]byte, error) {
	switch phys {
	case logicaltype.PhysicalInt8:
		if n < math.MinInt8 || n > math.MaxInt8 {
			return nil, fmt.Errorf("value %d out of range for TINYINT storage", n)
		}
		return append(buf, byte(n)), nil
	case logicaltype.PhysicalInt16:
		if n < math.MinInt16 || n > math.MaxInt16 {
			return nil, fmt.Errorf("value %d out of range for SMALLINT storage", n)
		}
		return binary.LittleEndian.AppendUint16(buf, uint16(n)), nil
	case logicaltype.PhysicalInt32:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return nil, fmt.Errorf("value %d out of range for INTEGER storage", n)
		}
		return binary.LittleEndian.AppendUint32(buf, uint32(n)), nil
	case logicaltype.PhysicalInt64:
		return binary.LittleEndian.AppendUint64(buf, uint64(n)), nil
	case logicaltype.PhysicalInt128:
		// hugeint_t: [lower u64 LE][upper i64 LE], sign-extended.
		buf = binary.LittleEndian.AppendUint64(buf, uint64(n))
		return binary.LittleEndian.AppendUint64(buf, uint64(n>>63)), nil
	case logicaltype.PhysicalUint128:
		if n < 0 {
			return nil, fmt.Errorf("value %d out of range for UHUGEINT storage", n)
		}
		buf = binary.LittleEndian.AppendUint64(buf, uint64(n))
		return binary.LittleEndian.AppendUint64(buf, 0), nil
	}
	return nil, fmt.Errorf("physical type %d does not store a signed integer", phys)
}

func appendUintFixed(buf []byte, phys logicaltype.PhysicalType, u uint64) ([]byte, error) {
	switch phys {
	case logicaltype.PhysicalUint8:
		if u > math.MaxUint8 {
			return nil, fmt.Errorf("value %d out of range for UTINYINT storage", u)
		}
		return append(buf, byte(u)), nil
	case logicaltype.PhysicalUint16:
		if u > math.MaxUint16 {
			return nil, fmt.Errorf("value %d out of range for USMALLINT storage", u)
		}
		return binary.LittleEndian.AppendUint16(buf, uint16(u)), nil
	case logicaltype.PhysicalUint32:
		if u > math.MaxUint32 {
			return nil, fmt.Errorf("value %d out of range for UINTEGER storage", u)
		}
		return binary.LittleEndian.AppendUint32(buf, uint32(u)), nil
	case logicaltype.PhysicalUint64:
		return binary.LittleEndian.AppendUint64(buf, u), nil
	}
	return nil, fmt.Errorf("physical type %d does not store an unsigned integer", phys)
}

var (
	two128     = new(big.Int).Lsh(big.NewInt(1), 128)
	hugeIntMin = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 127))
	hugeIntMax = new(big.Int).Sub(new(big.Int).Neg(hugeIntMin), big.NewInt(1))
)

// appendBigFixed stores b in a signed integer physical type (DECIMAL
// storage, HUGEINT, UUID) or UHUGEINT, rejecting values out of range.
func appendBigFixed(buf []byte, phys logicaltype.PhysicalType, b *big.Int) ([]byte, error) {
	switch phys {
	case logicaltype.PhysicalInt16, logicaltype.PhysicalInt32, logicaltype.PhysicalInt64:
		if !b.IsInt64() {
			return nil, fmt.Errorf("value %s out of range", b)
		}
		return appendIntFixed(buf, phys, b.Int64())
	case logicaltype.PhysicalInt128:
		if b.Cmp(hugeIntMin) < 0 || b.Cmp(hugeIntMax) > 0 {
			return nil, fmt.Errorf("value %s out of range for a 128-bit signed integer", b)
		}
		if b.Sign() < 0 {
			b = new(big.Int).Add(b, two128) // two's complement
		}
	case logicaltype.PhysicalUint128:
		if b.Sign() < 0 || b.Cmp(two128) >= 0 {
			return nil, fmt.Errorf("value %s out of range for a 128-bit unsigned integer", b)
		}
	default:
		return nil, fmt.Errorf("physical type %d does not store a wide integer", phys)
	}
	// [lower u64 LE][upper u64 LE]
	var be [16]byte
	b.FillBytes(be[:])
	buf = binary.LittleEndian.AppendUint64(buf, binary.BigEndian.Uint64(be[8:]))
	return binary.LittleEndian.AppendUint64(buf, binary.BigEndian.Uint64(be[:8])), nil
}

// asInt64 converts any Go integer that fits in int64.
func asInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case uint:
		return int64(x), uint64(x) <= math.MaxInt64
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		return int64(x), x <= math.MaxInt64
	}
	return 0, false
}

// asUint64 converts any non-negative Go integer.
func asUint64(v any) (uint64, bool) {
	switch x := v.(type) {
	case uint:
		return uint64(x), true
	case uint8:
		return uint64(x), true
	case uint16:
		return uint64(x), true
	case uint32:
		return uint64(x), true
	case uint64:
		return x, true
	}
	n, ok := asInt64(v)
	return uint64(n), ok && n >= 0
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	}
	if n, ok := asInt64(v); ok {
		return float64(n), true
	}
	if u, ok := v.(uint64); ok {
		return float64(u), true
	}
	return 0, false
}

// asBig converts a *big.Int or any Go integer. The result must not be
// mutated (it may be the caller's value).
func asBig(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case *big.Int:
		if x == nil {
			return nil, false
		}
		return x, true
	case uint64:
		return new(big.Int).SetUint64(x), true
	case uint:
		return new(big.Int).SetUint64(uint64(x)), true
	}
	if n, ok := asInt64(v); ok {
		return big.NewInt(n), true
	}
	return nil, false
}

// pow10Int64[i] is 10^i for every power that fits in an int64.
var pow10Int64 = func() (out [19]int64) {
	out[0] = 1
	for i := 1; i < len(out); i++ {
		out[i] = out[i-1] * 10
	}
	return out
}()

// pow10Big[i] is 10^i up to DuckDB's maximum DECIMAL width.
var pow10Big = func() (out [logicaltype.MaxDecimalWidth + 1]*big.Int) {
	for i := range out {
		out[i] = new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(i)), nil)
	}
	return out
}()

// decimalUnscaled returns the unscaled integer of a DECIMAL value and checks
// it against the declared width.
func decimalUnscaled(t logicaltype.Type, v any) (*big.Int, error) {
	var u *big.Int
	if s, ok := v.(string); ok {
		parsed, err := parseDecimal(s, int(t.Info.Scale))
		if err != nil {
			return nil, err
		}
		u = parsed
	} else if b, ok := asBig(v); ok {
		u = b
	} else {
		return nil, typeError(t, v, "a decimal string or the unscaled integer")
	}
	if u.CmpAbs(pow10Big[t.Info.Width]) >= 0 {
		return nil, fmt.Errorf("value %v does not fit DECIMAL(%d,%d)", v, t.Info.Width, t.Info.Scale)
	}
	return u, nil
}

// parseDecimal parses "[+-]digits[.digits]" into the unscaled integer at the
// given scale. Fractional digits beyond the scale must be zeros: the encoder
// never rounds.
func parseDecimal(s string, scale int) (*big.Int, error) {
	digits, neg := strings.CutPrefix(s, "-")
	if !neg {
		digits = strings.TrimPrefix(digits, "+")
	}
	intPart, frac, _ := strings.Cut(digits, ".")
	if intPart+frac == "" || strings.Trim(intPart+frac, "0123456789") != "" {
		return nil, fmt.Errorf("invalid DECIMAL value %q", s)
	}
	if len(frac) > scale {
		if strings.Trim(frac[scale:], "0") != "" {
			return nil, fmt.Errorf("DECIMAL value %q has more than %d fractional digits", s, scale)
		}
		frac = frac[:scale]
	}
	u, _ := new(big.Int).SetString("0"+intPart+frac+strings.Repeat("0", scale-len(frac)), 10)
	if neg {
		u.Neg(u)
	}
	return u, nil
}

// enumIndex resolves an ENUM label or index into a dictionary index.
func enumIndex(t logicaltype.Type, v any) (int, error) {
	if s, ok := v.(string); ok {
		for i, label := range t.Info.EnumValues {
			if label == s {
				return i, nil
			}
		}
		return 0, fmt.Errorf("ENUM label %q not in dictionary", s)
	}
	n, ok := asInt64(v)
	if !ok {
		return 0, typeError(t, v, "a string label or an integer index")
	}
	if n < 0 || n >= int64(len(t.Info.EnumValues)) {
		return 0, fmt.Errorf("ENUM index %d out of range (%d labels)", n, len(t.Info.EnumValues))
	}
	return int(n), nil
}

// UUIDToBig parses a UUID (32 hex digits; dashes and surrounding braces are
// optional, as in DuckDB's UUID::FromString) into DuckDB's hugeint
// representation: the textual upper 64 bits with the top bit flipped, so
// that signed hugeint order matches UUID order.
func UUIDToBig(uuid string) (*big.Int, error) {
	v, err := parseUUID(uuid)
	if err != nil {
		return nil, fmt.Errorf("message: %w", err)
	}
	return v, nil
}

func parseUUID(uuid string) (*big.Int, error) {
	s := uuid
	if len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' {
		s = s[1 : len(s)-1]
	}
	s = strings.ReplaceAll(s, "-", "")
	var raw [16]byte
	if len(s) != 2*len(raw) {
		return nil, fmt.Errorf("invalid UUID %q: %d hex digits, want 32", uuid, len(s))
	}
	if _, err := hex.Decode(raw[:], []byte(s)); err != nil {
		return nil, fmt.Errorf("invalid UUID %q", uuid)
	}
	return value.HugeInt(int64(binary.BigEndian.Uint64(raw[:8])^(1<<63)), binary.BigEndian.Uint64(raw[8:])), nil
}

func encodeGeometryVector(w *codec.Writer, vals []any) error {
	w.WriteFieldID(102)
	w.WriteULEB128(uint64(len(vals)))
	for i, v := range vals {
		switch x := v.(type) {
		case nil:
			w.WriteBlob([]byte{0x80}) // NullValue<string_t>()
		case []byte:
			w.WriteBlob(x)
		case string:
			w.WriteString(x)
		default:
			return rowError(i, fmt.Errorf("GEOMETRY value has Go type %T, want []byte (WKB)", v))
		}
	}
	return nil
}

// encodeStringVector writes the 2.0 string layout: every row's length (0
// for NULL) as u32, then the bytes of the non-NULL rows back to back. The
// total length precedes the bytes on the wire, so the values are measured
// in a first pass and written straight into w in a second one. BIT and
// BIGNUM, whose values are transformed, are rendered into a buffer first.
func encodeStringVector(w *codec.Writer, t logicaltype.Type, vals []any) error {
	if t.ID == logicaltype.IDBit || t.ID == logicaltype.IDBignum {
		return encodeTransformedStringVector(w, t, vals)
	}
	lengths := make([]byte, 0, len(vals)*4)
	var total uint64
	for i, v := range vals {
		n, err := stringValueLen(t, v)
		if err != nil {
			return rowError(i, err)
		}
		if uint64(n) > math.MaxUint32 {
			return rowError(i, fmt.Errorf("%s value of %d bytes exceeds the 4 GiB limit", t.Name(), n))
		}
		total += uint64(n)
		lengths = binary.LittleEndian.AppendUint32(lengths, uint32(n))
	}
	w.WriteFieldID(107)
	w.WriteULEB128(total) // optional_idx, always valid
	w.WriteFieldID(108)
	w.WriteBlob(lengths)
	w.WriteFieldID(109)
	w.WriteULEB128(total) // the blob's length prefix; its bytes follow
	for _, v := range vals {
		switch x := v.(type) {
		case string:
			w.WriteRawString(x)
		case []byte:
			w.WriteBytes(x)
		}
	}
	return nil
}

// stringValueLen is the number of bytes a string-layout value that is sent
// as is (a string or []byte; NULL is empty) takes on the wire.
func stringValueLen(t logicaltype.Type, v any) (int, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case string:
		return len(x), nil
	case []byte:
		return len(x), nil
	}
	if t.ID == logicaltype.IDBlob {
		return 0, typeError(t, v, "[]byte or string")
	}
	return 0, typeError(t, v, "string or []byte")
}

// encodeTransformedStringVector is encodeStringVector for BIT and BIGNUM,
// whose values are rendered into their storage form before their lengths
// are known.
func encodeTransformedStringVector(w *codec.Writer, t logicaltype.Type, vals []any) error {
	lengths := make([]byte, 0, len(vals)*4)
	var data []byte
	for i, v := range vals {
		start := len(data)
		if v != nil {
			var err error
			if data, err = appendTransformedStringValue(data, t, v); err != nil {
				return rowError(i, err)
			}
		}
		n := len(data) - start
		if uint64(n) > math.MaxUint32 {
			return rowError(i, fmt.Errorf("%s value of %d bytes exceeds the 4 GiB limit", t.Name(), n))
		}
		lengths = binary.LittleEndian.AppendUint32(lengths, uint32(n))
	}
	w.WriteFieldID(107)
	w.WriteULEB128(uint64(len(data))) // optional_idx, always valid
	w.WriteFieldID(108)
	w.WriteBlob(lengths)
	w.WriteFieldID(109)
	w.WriteBlob(data)
	return nil
}

func appendTransformedStringValue(buf []byte, t logicaltype.Type, v any) ([]byte, error) {
	switch t.ID {
	case logicaltype.IDBit:
		s, ok := v.(string)
		if !ok {
			return nil, typeError(t, v, "a string of '0'/'1'")
		}
		b, err := encodeBit(s)
		if err != nil {
			return nil, err
		}
		return append(buf, b...), nil
	case logicaltype.IDBignum:
		n, ok := asBig(v)
		if !ok {
			return nil, typeError(t, v, "*big.Int or an integer")
		}
		b, err := encodeBignum(n)
		if err != nil {
			return nil, err
		}
		return append(buf, b...), nil
	}
	return nil, fmt.Errorf("%s is not a transformed string type", t.Name())
}

func encodeStructVector(w *codec.Writer, t logicaltype.Type, vals []any, depth int) error {
	if t.Info == nil {
		return fmt.Errorf("%s type missing children", t.Name())
	}
	children := t.Info.Children
	tuple := isTuple(t, children)
	union := t.ID == logicaltype.IDUnion
	if union && (len(children) < 2 || children[0].Type.ID != logicaltype.IDUTinyint) {
		return errors.New("UNION type must have a UTINYINT tag and at least one member")
	}
	index := make(map[string]int, len(children))
	for k, c := range children {
		index[c.Name] = k
	}
	childVals := make([][]any, len(children))
	for k := range childVals {
		childVals[k] = make([]any, len(vals))
	}
	for i, row := range vals {
		if row == nil {
			continue // children of a NULL row are NULL
		}
		if tuple {
			list, ok := asSlice(row)
			if !ok || len(list) != len(children) {
				return rowError(i, fmt.Errorf("%s value must be []any with %d values, got %T", t.Name(), len(children), row))
			}
			for k, v := range list {
				childVals[k][i] = v
			}
			continue
		}
		m, ok := row.(map[string]any)
		if !ok {
			return rowError(i, typeError(t, row, "map[string]any"))
		}
		for name, v := range m {
			k, ok := index[name]
			if !ok {
				return rowError(i, fmt.Errorf("%s has no field %q", t.Name(), name))
			}
			childVals[k][i] = v
		}
		if union {
			tag, err := unionTag(m, children)
			if err != nil {
				return rowError(i, err)
			}
			childVals[0][i] = tag
		}
	}
	w.WriteFieldID(103)
	w.WriteULEB128(uint64(len(children)))
	for k, c := range children {
		if err := encodeVector(w, c.Type, childVals[k], depth+1); err != nil {
			return fmt.Errorf("field %q: %w", c.Name, err)
		}
	}
	return nil
}

// unionTag returns the tag of a UNION value: the "" key when present, else
// the index of its only non-nil member. Every other member must be nil, as
// DuckDB keeps the inactive members of a union row NULL.
func unionTag(m map[string]any, children []logicaltype.ChildType) (uint8, error) {
	tag := -1
	if raw, ok := m[""]; ok && raw != nil {
		n, ok := asInt64(raw)
		if !ok || n < 0 || n >= int64(len(children)-1) {
			return 0, fmt.Errorf("UNION tag %v out of range (%d members)", raw, len(children)-1)
		}
		tag = int(n)
	}
	active := -1
	for k := 1; k < len(children); k++ {
		if m[children[k].Name] == nil {
			continue
		}
		if active >= 0 {
			return 0, fmt.Errorf("UNION value sets both %q and %q", children[active+1].Name, children[k].Name)
		}
		active = k - 1
	}
	switch {
	case tag < 0 && active < 0:
		return 0, errors.New("UNION value has no member set and no tag")
	case tag < 0:
		tag = active
	case active >= 0 && active != tag:
		return 0, fmt.Errorf("UNION tag %d does not match member %q", tag, children[active+1].Name)
	}
	return uint8(tag), nil
}

// asSlice accepts the []any forms of LIST, ARRAY and TUPLE values.
func asSlice(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case Values:
		return x, true
	}
	return nil, false
}

func encodeListVector(w *codec.Writer, t logicaltype.Type, vals []any, depth int) error {
	if t.Info == nil || t.Info.Child == nil {
		return fmt.Errorf("%s type missing child type", t.Name())
	}
	isMap := t.ID == logicaltype.IDMap
	entries := make([]listEntry, len(vals))
	var child []any
	for i, row := range vals {
		if row == nil {
			continue
		}
		list, ok := asSlice(row)
		if !ok {
			return rowError(i, typeError(t, row, "[]any"))
		}
		if isMap {
			for j, e := range list {
				if m, ok := e.(map[string]any); !ok || m["key"] == nil {
					return rowError(i, fmt.Errorf("MAP entry %d must be map[string]any with a non-NULL \"key\", got %T", j, e))
				}
			}
		}
		entries[i] = listEntry{offset: len(child), length: len(list)}
		child = append(child, list...)
	}
	w.WriteFieldID(104)
	w.WriteULEB128(uint64(len(child)))
	w.WriteFieldID(105)
	w.WriteULEB128(uint64(len(entries)))
	for _, e := range entries {
		w.WriteFieldID(100)
		w.WriteULEB128(uint64(e.offset))
		w.WriteFieldID(101)
		w.WriteULEB128(uint64(e.length))
		w.WriteEndObject()
	}
	w.WriteFieldID(106)
	if err := encodeVector(w, *t.Info.Child, child, depth+1); err != nil {
		return fmt.Errorf("%s child: %w", t.Name(), err)
	}
	return nil
}

func encodeArrayVector(w *codec.Writer, t logicaltype.Type, vals []any, depth int) error {
	if t.Info == nil || t.Info.Child == nil {
		return errors.New("ARRAY type missing child type")
	}
	size := int(t.Info.ArraySize)
	if size == 0 {
		return errors.New("ARRAY type has size 0")
	}
	child := make([]any, len(vals)*size)
	for i, row := range vals {
		if row == nil {
			continue // a NULL array's elements are NULL
		}
		list, ok := asSlice(row)
		if !ok || len(list) != size {
			return rowError(i, fmt.Errorf("ARRAY value must be []any with %d values, got %T", size, row))
		}
		copy(child[i*size:], list)
	}
	w.WriteFieldID(103)
	w.WriteULEB128(uint64(size))
	w.WriteFieldID(104)
	if err := encodeVector(w, *t.Info.Child, child, depth+1); err != nil {
		return fmt.Errorf("ARRAY child: %w", err)
	}
	return nil
}
