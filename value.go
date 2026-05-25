package quackdriver

import (
	"database/sql/driver"
	"math"
	"math/big"
	"time"
)

// CheckNamedValue accepts any Go scalar plus a few common stdlib types. It is
// used by database/sql to validate that a value is convertible to driver.Value
// before the driver attempts to format it.
func (c *Conn) CheckNamedValue(nv *driver.NamedValue) error {
	v, err := convertValue(nv.Value)
	if err != nil {
		return err
	}
	nv.Value = v
	return nil
}

var _ driver.NamedValueChecker = (*Conn)(nil)

func convertValue(v any) (driver.Value, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case bool, int64, float64, string, []byte, time.Time:
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
	}
	// Fall back to database/sql's default conversion, which understands the
	// driver.Valuer interface and a handful of additional types.
	return driver.DefaultParameterConverter.ConvertValue(v)
}
