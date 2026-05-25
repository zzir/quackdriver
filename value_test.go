package quackdriver

import (
	"math"
	"testing"
)

// Regression: converting a uint that doesn't fit in int64 used to silently
// wrap to a negative int64, which the sqlfmt layer would then render as a
// negative SQL literal — silent data corruption.
func TestConvertValueLargeUintPassesThrough(t *testing.T) {
	v, err := convertValue(uint(math.MaxUint64))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v.(uint64); !ok || got != math.MaxUint64 {
		t.Fatalf("uint(MaxUint64) → uint64(MaxUint64), got %T %v", v, v)
	}
}

func TestConvertValueUintMaxInt64Boundary(t *testing.T) {
	// uint(MaxInt64) still fits in int64; should normalize to int64.
	v, err := convertValue(uint(math.MaxInt64))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v.(int64); !ok || got != math.MaxInt64 {
		t.Fatalf("uint(MaxInt64) → int64(MaxInt64), got %T %v", v, v)
	}
	// uint(MaxInt64+1) is the smallest uint that does NOT fit in int64.
	v, err = convertValue(uint(math.MaxInt64) + 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v.(uint64); !ok || got != uint64(math.MaxInt64)+1 {
		t.Fatalf("uint(MaxInt64+1) → uint64(MaxInt64+1), got %T %v", v, v)
	}
}
