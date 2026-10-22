package quackdriver

import (
	"database/sql/driver"
	"math"
	"math/big"
	"reflect"
	"time"
)

// CheckNamedValue accepts Go scalars, the standard time types, big numbers
// and the driver's own value types. The values are later rendered as SQL
// literals (Quack carries only SQL text); see the README for the literal
// each type becomes.
func (c *Conn) CheckNamedValue(nv *driver.NamedValue) error {
	v, err := convertValue(nv.Value)
	if err != nil {
		return err
	}
	nv.Value = v
	return nil
}

var _ driver.NamedValueChecker = (*Conn)(nil)

// maxValuerDepth bounds driver.Valuer chains (a Valuer returning a Valuer).
const maxValuerDepth = 8

func convertValue(v any) (driver.Value, error) {
	return convertValueDepth(v, 0)
}

func convertValueDepth(v any, depth int) (driver.Value, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case bool, int64, float64, string, []byte, time.Time:
		return x, nil
	case time.Duration, IntervalValue, TimeTzValue:
		return x, nil
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case uint:
		// uint is 64-bit on most platforms; values above MaxInt64 would
		// silently wrap to a negative int64. Pass through as uint64 so the
		// sqlfmt layer renders the actual magnitude.
		if uint64(x) > math.MaxInt64 {
			return uint64(x), nil
		}
		return int64(x), nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		// Pass through as-is so the sqlfmt layer can render it without
		// truncating to int64.
		return x, nil
	case float32:
		return float64(x), nil
	case *big.Int, big.Int, *big.Rat:
		return x, nil
	case driver.Valuer:
		// Resolve Valuers here: the default converter below would reject
		// the driver's own types (IntervalValue, ...) a Valuer may return.
		if depth >= maxValuerDepth {
			break
		}
		if isNilPointer(x) {
			return nil, nil
		}
		inner, err := x.Value()
		if err != nil {
			return nil, err
		}
		return convertValueDepth(inner, depth+1)
	}
	// Fall back to database/sql's default conversion, which handles
	// pointers and named types with a scalar underlying type.
	return driver.DefaultParameterConverter.ConvertValue(v)
}

// isNilPointer reports a nil pointer stored in an interface, e.g. a nil
// *MyValuer: database/sql's default converter treats those as NULL too.
func isNilPointer(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}
