package quackdriver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/sqlfmt"
	"github.com/zzir/quackdriver/internal/value"
)

// AppendColumn describes a single column for Conn.Append and NewAppender.
//
// Name is the target column in the table. When every column of a call has a
// name, the INSERT lists them, so the remaining table columns take their
// defaults. When any name is empty, no column list is sent and the columns
// must match the table's columns in number and order.
//
// Type is a DuckDB type expression: a built-in type such as INTEGER,
// DECIMAL(10,2) or STRUCT(a INTEGER, b VARCHAR[]), or a user-defined type.
// The server resolves it when the Appender starts. Values travel as text:
// the server casts each to Type, and the INSERT then to the table column's
// type, so DuckDB's own cast rules apply and a value DuckDB cannot cast (out
// of range, malformed, a duplicate MAP key) fails the statement. Times are
// sent with nanoseconds, which DuckDB rounds to the type's unit;
// InfinityTime and NegInfinityTime are 'infinity' and '-infinity'. The Go
// values each type accepts (nil and a nil []byte are SQL NULL):
//
//	BOOLEAN                           bool
//	TINYINT ... UBIGINT               any Go integer
//	HUGEINT, UHUGEINT, BIGNUM         any Go integer, *big.Int, big.Int, decimal string
//	FLOAT, DOUBLE                     any Go integer or float
//	DECIMAL(w,s)                      decimal string, Go integer, *big.Int/big.Int (unscaled),
//	                                  float (rounded like DuckDB's DOUBLE cast)
//	VARCHAR (and aliases such as JSON) string or []byte (valid UTF-8)
//	BLOB                              []byte or string
//	BIT, UUID                         string
//	DATE                              time.Time (its UTC date)
//	TIME, TIME_NS                     time.Duration in [0, 24h], or time.Time (its UTC time of day)
//	TIME WITH TIME ZONE               TimeTzValue, or time.Time (its time of day and UTC offset)
//	TIMESTAMP, TIMESTAMP_S/_MS/_NS    time.Time (its UTC wall clock)
//	TIMESTAMPTZ, TIMESTAMPTZ_NS       time.Time
//	INTERVAL                          IntervalValue or time.Duration
//	ENUM                              string label or integer index
//	LIST, ARRAY, TUPLE                []any
//	STRUCT                            map[string]any (missing fields are NULL; unknown keys are errors)
//	MAP                               map[string]any (string keys), or []any of
//	                                  map[string]any{"key": k, "value": v}
//
// UNION (DuckDB's cast from text picks the member itself), VARIANT and
// GEOMETRY columns are not supported; insert those with SQL.
type AppendColumn struct {
	Name string
	Type string
}

// Append inserts rows into table in one INSERT statement, streaming them to
// the server as Quack client data (SEND_DATA): the server runs
// INSERT INTO table SELECT ... FROM scan_data_from_quack_client(...) and reads
// the rows as they arrive. It is a convenience wrapper around NewAppender for
// rows already in memory: a row that cannot be converted (a *ValueError; see
// AppendColumn for the accepted Go values) or a server error aborts the
// statement, so either every row is inserted or none. Each row's values map
// to columns in order; nil is SQL NULL. The connection is held exclusively
// while the rows are sent.
func (c *Conn) Append(ctx context.Context, table string, columns []AppendColumn, rows [][]any) error {
	return c.AppendWithSchema(ctx, "", table, columns, rows)
}

// AppendWithSchema is Append with an explicit schema name (e.g. "main").
func (c *Conn) AppendWithSchema(ctx context.Context, schema, table string, columns []AppendColumn, rows [][]any) error {
	a, err := c.NewAppender(ctx, schema, table, columns)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := a.appendRow("quackdriver: ", row); err != nil {
			_ = a.Abort()
			return err
		}
	}
	_, err = a.Close()
	return err
}

// checkAppendTarget validates the table and columns of an append.
func checkAppendTarget(table string, columns []AppendColumn) error {
	if len(columns) == 0 {
		return errors.New("quackdriver: Append requires at least one column")
	}
	if table == "" {
		return errors.New("quackdriver: Append requires a table name")
	}
	for _, col := range columns {
		if err := checkTypeSyntax(col.Type); err != nil {
			return fmt.Errorf("quackdriver: column %q: %w", col.Name, err)
		}
	}
	return nil
}

// checkTypeSyntax keeps a column type from escaping the statement that
// resolves it (SELECT NULL::<type>, ... LIMIT 0): outside quoted names and
// labels only letters, digits, '_', whitespace and ( ) [ ] , . may appear, so
// the type cannot end the statement, comment out the rest, or open a
// dollar-quoted or escape string. It is not a type-expression parser: SQL
// keywords and parentheses pass, so a Type such as "INTEGER FROM (SELECT
// f())" still changes what that one read-only statement computes, and the
// server's error for it names the column. Type comes from the program, like
// the SQL text itself, so this is the extent of the check; the INSERT uses
// the server-resolved types (Type.SQL), never the raw string.
func checkTypeSyntax(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("missing Type")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			if i > 0 && isIdentByte(s[i-1]) {
				return fmt.Errorf("type %q: prefixed string literal", s)
			}
			j := i + 1
			for ; j < len(s); j++ {
				if s[j] == c {
					if j+1 < len(s) && s[j+1] == c {
						j++ // a doubled quote
						continue
					}
					break
				}
			}
			if j == len(s) {
				return fmt.Errorf("type %q: unterminated quote", s)
			}
			i = j
		case isIdentByte(c), strings.IndexByte(" \t\r\n()[],.", c) >= 0:
		default:
			return fmt.Errorf("type %q: unexpected %q", s, c)
		}
	}
	return nil
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// appendTypes resolves the column types on the server, with
// SELECT NULL::<type>, ... LIMIT 0, so that any type DuckDB knows, user-defined
// ones included, can be appended, and checks that values of each can be sent.
func (c *Conn) appendTypes(ctx context.Context, columns []AppendColumn) ([]logicaltype.Type, error) {
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, col := range columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("NULL::")
		b.WriteString(col.Type)
	}
	b.WriteString(" LIMIT 0")
	q, err := c.prepare(ctx, b.String(), message.OptionalIdx{}, true)
	if err != nil {
		return nil, fmt.Errorf("quackdriver: Append column types: %w", err)
	}
	types := q.prep.ResultTypes
	if len(types) != len(columns) {
		return nil, fmt.Errorf("quackdriver: Append: %d column types resolved to %d types", len(columns), len(types))
	}
	for i, t := range types {
		if err := checkAppendType(t); err != nil {
			return nil, fmt.Errorf("quackdriver: column %q: %w", columns[i].Name, err)
		}
	}
	return types, nil
}

// appendGoTypes names the Go values a scalar column accepts, for errors; its
// keys are the scalar types Append supports.
var appendGoTypes = func() map[logicaltype.ID]string {
	m := map[logicaltype.ID]string{
		logicaltype.IDBoolean:  "bool",
		logicaltype.IDFloat:    "a Go integer or float",
		logicaltype.IDDouble:   "a Go integer or float",
		logicaltype.IDDecimal:  "a decimal string, Go integer, float or unscaled *big.Int",
		logicaltype.IDVarchar:  "string",
		logicaltype.IDChar:     "string",
		logicaltype.IDBlob:     "[]byte",
		logicaltype.IDBit:      "a string of '0'/'1'",
		logicaltype.IDUUID:     "string",
		logicaltype.IDDate:     "time.Time",
		logicaltype.IDTime:     "time.Duration or time.Time",
		logicaltype.IDTimeNs:   "time.Duration or time.Time",
		logicaltype.IDTimeTz:   "TimeTzValue or time.Time",
		logicaltype.IDInterval: "IntervalValue or time.Duration",
		logicaltype.IDEnum:     "a string label or integer index",
	}
	for _, id := range []logicaltype.ID{logicaltype.IDTinyint, logicaltype.IDSmallint, logicaltype.IDInteger, logicaltype.IDBigint,
		logicaltype.IDUTinyint, logicaltype.IDUSmallint, logicaltype.IDUInteger, logicaltype.IDUBigint} {
		m[id] = "a Go integer"
	}
	for _, id := range []logicaltype.ID{logicaltype.IDHugeint, logicaltype.IDUHugeint, logicaltype.IDBignum} {
		m[id] = "a Go integer, *big.Int or decimal string"
	}
	for _, id := range []logicaltype.ID{logicaltype.IDTimestamp, logicaltype.IDTimestampSec, logicaltype.IDTimestampMs,
		logicaltype.IDTimestampNs, logicaltype.IDTimestampTz, logicaltype.IDTimestampTzNs} {
		m[id] = "time.Time"
	}
	return m
}()

// checkAppendType rejects a column type whose values cannot be sent as text.
func checkAppendType(t logicaltype.Type) error {
	switch t.ID {
	case logicaltype.IDUnion:
		return errors.New("UNION is not supported by Append (DuckDB's cast from text picks the member); insert UNION values with SQL")
	case logicaltype.IDList, logicaltype.IDArray, logicaltype.IDMap:
		if t.Info != nil && t.Info.Child != nil {
			return checkAppendType(*t.Info.Child)
		}
	case logicaltype.IDStruct, logicaltype.IDTuple:
		if t.Info != nil {
			for _, c := range t.Info.Children {
				if err := checkAppendType(c.Type); err != nil {
					return err
				}
			}
			return nil
		}
	default:
		if _, ok := appendGoTypes[t.ID]; ok {
			return nil
		}
	}
	return fmt.Errorf("type %s is not supported by Append", t.SQL())
}

// isNull reports whether v is SQL NULL: nil, or a nil []byte (database/sql
// scans a NULL into a nil []byte; writing it back must keep the NULL).
func isNull(v any) bool {
	b, ok := v.([]byte)
	return v == nil || ok && b == nil
}

// appendText renders v, a value of a column of type t, as text that DuckDB's
// cast from VARCHAR to t turns into the intended value; nil is NULL.
func appendText(t logicaltype.Type, v any) (any, error) {
	if isNull(v) {
		return nil, nil
	}
	var s string
	switch t.ID {
	case logicaltype.IDList, logicaltype.IDArray, logicaltype.IDTuple, logicaltype.IDStruct, logicaltype.IDMap:
		b, err := appendValueText(nil, t, v, false)
		if err != nil {
			return nil, err
		}
		s = string(b)
	default:
		var err error
		if s, err = scalarText(t, v); err != nil {
			return nil, err
		}
	}
	if !utf8.ValidString(s) {
		return nil, errors.New("value is not valid UTF-8 (use a BLOB column for binary data)")
	}
	return s, nil
}

// appendValueText appends the text of v to b. Inside LIST, STRUCT, MAP and
// TUPLE text (nested), scalars are single-quoted with backslash escapes and
// NULL is spelled out, as DuckDB's casts from VARCHAR read them.
func appendValueText(b []byte, t logicaltype.Type, v any, nested bool) ([]byte, error) {
	if isNull(v) {
		return append(b, "NULL"...), nil
	}
	var err error
	switch t.ID {
	case logicaltype.IDList, logicaltype.IDArray, logicaltype.IDTuple:
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s expects []any, got %T", t.Name(), v)
		}
		open, end := byte('['), byte(']')
		if t.ID == logicaltype.IDTuple {
			if len(list) != len(t.Info.Children) {
				return nil, fmt.Errorf("TUPLE expects %d values, got %d", len(t.Info.Children), len(list))
			}
			open, end = '(', ')'
		}
		b = append(b, open)
		for i, e := range list {
			if i > 0 {
				b = append(b, ", "...)
			}
			elem := t.Info.Child
			if t.ID == logicaltype.IDTuple {
				elem = &t.Info.Children[i].Type
			}
			if b, err = appendValueText(b, *elem, e, true); err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
		}
		return append(b, end), nil
	case logicaltype.IDStruct:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("STRUCT expects map[string]any, got %T", v)
		}
		for k := range m {
			if !slices.ContainsFunc(t.Info.Children, func(c logicaltype.ChildType) bool { return c.Name == k }) {
				return nil, fmt.Errorf("STRUCT has no field %q", k)
			}
		}
		b = append(b, '{')
		for i, c := range t.Info.Children {
			if i > 0 {
				b = append(b, ", "...)
			}
			b = append(appendQuoted(b, c.Name), ": "...)
			if b, err = appendValueText(b, c.Type, m[c.Name], true); err != nil {
				return nil, fmt.Errorf("field %q: %w", c.Name, err)
			}
		}
		return append(b, '}'), nil
	case logicaltype.IDMap:
		entry := t.Info.Child.Info.Children // key, value
		var pairs [][2]any
		switch x := v.(type) {
		case map[string]any:
			// Sorted by key, so the stored order is deterministic.
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				pairs = append(pairs, [2]any{k, x[k]})
			}
		case []any:
			for i, e := range x {
				kv, ok := e.(map[string]any)
				for k := range kv {
					ok = ok && (k == "key" || k == "value")
				}
				if !ok {
					return nil, fmt.Errorf("MAP element %d: want map[string]any{\"key\": k, \"value\": v}, got %#v", i, e)
				}
				pairs = append(pairs, [2]any{kv["key"], kv["value"]})
			}
		default:
			return nil, fmt.Errorf("MAP expects map[string]any or []any of {key, value} pairs, got %T", v)
		}
		b = append(b, '{')
		for i, p := range pairs {
			if i > 0 {
				b = append(b, ", "...)
			}
			if b, err = appendValueText(b, entry[0].Type, p[0], true); err != nil {
				return nil, fmt.Errorf("key %d: %w", i, err)
			}
			b = append(b, '=')
			if b, err = appendValueText(b, entry[1].Type, p[1], true); err != nil {
				return nil, fmt.Errorf("value %d: %w", i, err)
			}
		}
		return append(b, '}'), nil
	}
	s, err := scalarText(t, v)
	if err != nil {
		return nil, err
	}
	if nested {
		return appendQuoted(b, s), nil
	}
	return append(b, s...), nil
}

// appendQuoted appends s single-quoted, with backslashes and single quotes
// escaped by a backslash.
func appendQuoted(b []byte, s string) []byte {
	b = append(b, '\'')
	for i := range len(s) {
		if s[i] == '\\' || s[i] == '\'' {
			b = append(b, '\\')
		}
		b = append(b, s[i])
	}
	return append(b, '\'')
}

// scalarText renders a scalar value as DuckDB's cast from VARCHAR to t reads
// it.
func scalarText(t logicaltype.Type, v any) (string, error) {
	switch t.ID {
	case logicaltype.IDBoolean:
		if x, ok := v.(bool); ok {
			return strconv.FormatBool(x), nil
		}
	case logicaltype.IDTinyint, logicaltype.IDSmallint, logicaltype.IDInteger, logicaltype.IDBigint,
		logicaltype.IDUTinyint, logicaltype.IDUSmallint, logicaltype.IDUInteger, logicaltype.IDUBigint:
		if s, ok := intText(v); ok {
			return s, nil
		}
	case logicaltype.IDHugeint, logicaltype.IDUHugeint, logicaltype.IDBignum:
		if s, ok := intText(v); ok {
			return s, nil
		}
		if bi, ok := bigInt(v); ok {
			return bi.String(), nil
		}
		if s, ok := v.(string); ok {
			return s, nil
		}
	case logicaltype.IDFloat, logicaltype.IDDouble:
		if f, ok := toFloat64(v); ok {
			if t.ID == logicaltype.IDFloat && !math.IsInf(f, 0) && math.Abs(f) > math.MaxFloat32 {
				return "", fmt.Errorf("FLOAT value %g out of range", f)
			}
			return floatText(f), nil
		}
	case logicaltype.IDDecimal:
		switch x := v.(type) {
		case string:
			return x, nil
		case float32:
			return floatDecimalText(float64(x), t.Info.Scale)
		case float64:
			return floatDecimalText(x, t.Info.Scale)
		}
		if bi, ok := bigInt(v); ok {
			return value.FormatDecimal(bi, int(t.Info.Scale)), nil
		}
		if s, ok := intText(v); ok {
			return s, nil
		}
	case logicaltype.IDVarchar, logicaltype.IDChar:
		switch x := v.(type) {
		case string:
			return x, nil
		case []byte:
			return string(x), nil
		}
	case logicaltype.IDBlob:
		switch x := v.(type) {
		case []byte:
			return blobText(string(x)), nil
		case string:
			return blobText(x), nil
		}
	case logicaltype.IDBit, logicaltype.IDUUID:
		if s, ok := v.(string); ok {
			return s, nil
		}
	case logicaltype.IDDate:
		if x, ok := v.(time.Time); ok {
			return dateText(x), nil
		}
	case logicaltype.IDTime, logicaltype.IDTimeNs:
		switch x := v.(type) {
		case time.Duration:
			if x < 0 || x > 24*time.Hour {
				return "", fmt.Errorf("time of day %s out of range [0, 24h]", x)
			}
			return clockText(x), nil
		case time.Time:
			return clockText(timeOfDay(x.UTC())), nil
		}
	case logicaltype.IDTimeTz:
		switch x := v.(type) {
		case value.TimeTzValue:
			if err := x.Validate(); err != nil {
				return "", err
			}
			return x.String(), nil
		case time.Time:
			_, offset := x.Zone()
			tz, err := value.NewTimeTz(timeOfDay(x), int32(offset))
			if err != nil {
				return "", err
			}
			return tz.String(), nil
		}
	case logicaltype.IDTimestamp, logicaltype.IDTimestampTz:
		if x, ok := v.(time.Time); ok {
			return timestampText(x, 1e6, t.ID == logicaltype.IDTimestampTz)
		}
	case logicaltype.IDTimestampSec:
		if x, ok := v.(time.Time); ok {
			return timestampText(x, 1, false)
		}
	case logicaltype.IDTimestampMs:
		if x, ok := v.(time.Time); ok {
			return timestampText(x, 1e3, false)
		}
	case logicaltype.IDTimestampNs, logicaltype.IDTimestampTzNs:
		if x, ok := v.(time.Time); ok {
			return timestampText(x, 1e9, t.ID == logicaltype.IDTimestampTzNs)
		}
	case logicaltype.IDInterval:
		switch x := v.(type) {
		case value.IntervalValue:
			return sqlfmt.IntervalText(x.Months, x.Days, x.Micros), nil
		case time.Duration:
			return sqlfmt.IntervalText(0, 0, x.Microseconds()), nil
		}
	case logicaltype.IDEnum:
		if s, ok := v.(string); ok {
			return s, nil
		}
		if mag, neg, ok := intParts(v); ok {
			labels := t.Info.EnumValues
			if neg || mag >= uint64(len(labels)) {
				return "", fmt.Errorf("ENUM index %v out of range (dictionary size %d)", v, len(labels))
			}
			return labels[mag], nil
		}
	}
	return "", fmt.Errorf("%s expects %s, got %T", t.Name(), appendGoTypes[t.ID], v)
}

// intText renders a value of a built-in Go integer type.
func intText(v any) (string, bool) {
	mag, neg, ok := intParts(v)
	switch {
	case !ok:
		return "", false
	case neg:
		return strconv.FormatInt(-int64(mag), 10), true // also right for 1<<63
	}
	return strconv.FormatUint(mag, 10), true
}

// bigInt accepts a non-nil *big.Int or a big.Int.
func bigInt(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case *big.Int:
		return x, x != nil
	case big.Int:
		return &x, true
	}
	return nil, false
}

// floatText renders a float that parses back to the same float64.
func floatText(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// doublePowersOfTen mirrors DuckDB's NumericHelper::DOUBLE_POWERS_OF_TEN
// (exact double literals, unlike products computed at run time).
var doublePowersOfTen = [...]float64{
	1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10, 1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19,
	1e20, 1e21, 1e22, 1e23, 1e24, 1e25, 1e26, 1e27, 1e28, 1e29, 1e30, 1e31, 1e32, 1e33, 1e34, 1e35, 1e36, 1e37, 1e38,
}

// floatDecimalText converts a float to a DECIMAL of the given scale exactly
// like DuckDB's DOUBLE -> DECIMAL cast (DoubleToDecimalCast): multiplied by
// 10^scale in double arithmetic and rounded half away from zero. A float
// therefore stores what INSERT ... VALUES (?) stores, where it is a DOUBLE
// literal cast by the server; its decimal text would round differently.
func floatDecimalText(f float64, scale uint8) (string, error) {
	r := math.Round(f * doublePowersOfTen[scale]) // a DECIMAL's scale is at most 38
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return "", fmt.Errorf("DECIMAL cannot hold %g", f)
	}
	bi, _ := new(big.Float).SetFloat64(r).Int(nil) // r is integral: exact
	return value.FormatDecimal(bi, int(scale)), nil
}

// blobText escapes the bytes outside printable ASCII, and the backslash, as
// \xHH, the way DuckDB's cast from VARCHAR to BLOB reads them.
func blobText(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 0x20 && c < 0x7f && c != '\\' {
			b.WriteByte(c)
			continue
		}
		b.WriteString(`\x`)
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// dateText renders t's UTC date as DuckDB reads it: years before 1 with
// DuckDB's " (BC)" suffix (Go's year 0 is 1 BC), years after 9999 with
// every digit.
func dateText(t time.Time) string {
	switch value.Infinity(t) {
	case 1:
		return "infinity"
	case -1:
		return "-infinity"
	}
	t = t.UTC()
	year, month, day := t.Date()
	if year >= 1 && year <= 9999 {
		return t.Format(time.DateOnly)
	}
	bc := ""
	if year <= 0 {
		year, bc = 1-year, " (BC)"
	}
	return fmt.Sprintf("%04d-%02d-%02d%s", year, int(month), day, bc)
}

// timestampText renders t's UTC wall clock for a timestamp type of
// perSecond units per second. t is rounded to the unit first (halves away
// from the epoch), like DuckDB's cast of the TIMESTAMP or TIMESTAMP_NS
// literal a query argument becomes, since its cast from text would truncate
// the digits below microseconds. tz adds the +00 offset of a TIMESTAMPTZ,
// so the instant does not depend on the session's TimeZone.
func timestampText(t time.Time, perSecond int64, tz bool) (string, error) {
	if value.Infinity(t) != 0 {
		return dateText(t), nil
	}
	if perSecond < 1e9 {
		n, ok := value.UnixRounded(t, perSecond)
		if !ok {
			return "", fmt.Errorf("timestamp %s out of range", t.Format(time.RFC3339Nano))
		}
		t = value.TimestampFromUnits(n, perSecond)
	}
	t = t.UTC()
	var buf [48]byte
	var b []byte
	if y := t.Year(); y >= 1 && y <= 9999 {
		b = t.AppendFormat(buf[:0], "2006-01-02 15:04:05.000000000")
	} else {
		b = append(append(buf[:0], dateText(t)...), ' ')
		b = appendClock(b, timeOfDay(t))
	}
	if tz {
		b = append(b, "+00"...)
	}
	return string(b), nil
}

// clockText renders a time of day as HH:MM:SS.nnnnnnnnn.
func clockText(d time.Duration) string {
	var buf [32]byte
	return string(appendClock(buf[:0], d))
}

func appendClock(b []byte, d time.Duration) []byte {
	two := func(b []byte, n time.Duration) []byte { return append(b, byte('0'+n/10), byte('0'+n%10)) }
	if h := d / time.Hour; h < 100 {
		b = two(b, h)
	} else {
		b = strconv.AppendInt(b, int64(h), 10)
	}
	b = two(append(b, ':'), d/time.Minute%60)
	b = two(append(b, ':'), d/time.Second%60)
	ns := strconv.AppendInt(nil, int64(d%time.Second)+1e9, 10) // 1nnnnnnnnn: zero padded
	return append(append(b, '.'), ns[1:]...)
}

// timeOfDay returns t's wall-clock time of day.
func timeOfDay(t time.Time) time.Duration {
	h, m, s := t.Clock()
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second +
		time.Duration(t.Nanosecond())
}

// intParts splits a value of a built-in Go integer type into its magnitude
// and sign; ok is false for any other type.
func intParts(v any) (mag uint64, neg, ok bool) {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int8:
		n = int64(x)
	case int16:
		n = int64(x)
	case int32:
		n = int64(x)
	case int64:
		n = x
	case uint:
		return uint64(x), false, true
	case uint8:
		return uint64(x), false, true
	case uint16:
		return uint64(x), false, true
	case uint32:
		return uint64(x), false, true
	case uint64:
		return x, false, true
	default:
		return 0, false, false
	}
	if n < 0 {
		return uint64(-n), true, true // -MinInt64 wraps to 1<<63, its magnitude
	}
	return uint64(n), false, true
}

// toFloat64 accepts every Go integer and float type.
func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	}
	mag, neg, ok := intParts(v)
	if neg {
		return -float64(mag), ok
	}
	return float64(mag), ok
}
