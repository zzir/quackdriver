package message

import (
	"encoding/binary"
	"math"
	"math/bits"
	"strconv"

	"github.com/zzir/quackdriver/internal/value"
)

// This file holds the lazily boxing Column implementations produced by the
// vector decoder. Every bound that Value relies on (data lengths, list
// entries, dictionary indices) is validated when the vector is decoded, so
// Value cannot fail for an in-range row index.

// validityBit reports whether row i is valid in a DuckDB validity mask
// (64-bit little-endian words, bit i%64 of word i/64, i.e. bit i%8 of byte
// i/8). A nil mask means every row is valid.
func validityBit(mask []byte, i int) bool {
	return mask == nil || mask[i>>3]&(1<<(uint(i)&7)) != 0
}

// fixedColumn is a FLAT vector of a fixed-size physical type: raw holds
// n*width little-endian bytes and box converts one element.
type fixedColumn struct {
	n        int
	width    int
	raw      []byte
	validity []byte
	box      func(b []byte) any
}

func (c *fixedColumn) Len() int { return c.n }

func (c *fixedColumn) Value(i int) any {
	checkIndex(i, c.n)
	if !validityBit(c.validity, i) {
		return nil
	}
	off := i * c.width
	return c.box(c.raw[off : off+c.width])
}

// stringKind selects how a stringColumn boxes its bytes.
type stringKind uint8

const (
	stringText   stringKind = iota // string
	stringBlob                     // fresh []byte copy
	stringBit                      // string of '0'/'1' (framing validated on decode)
	stringBignum                   // *big.Int (framing validated on decode)
)

// stringColumn is a FLAT vector of a VARCHAR-physical type. Row i is
// data[offs[i]:offs[i+1]]; NULL rows are empty.
type stringColumn struct {
	validity []byte
	data     []byte
	offs     []int
	kind     stringKind
}

func (c *stringColumn) Len() int { return len(c.offs) - 1 }

func (c *stringColumn) Value(i int) any {
	if !validityBit(c.validity, i) {
		return nil
	}
	b := c.data[c.offs[i]:c.offs[i+1]]
	switch c.kind {
	case stringBlob:
		out := make([]byte, len(b))
		copy(out, b)
		return out
	case stringBit:
		s, _ := value.BitString(b) // validated on decode
		return s
	case stringBignum:
		v, _ := value.Bignum(b) // validated on decode
		return v
	}
	return string(b)
}

// structColumn is a FLAT STRUCT, UNION or TUPLE vector. A row is a
// map[string]any keyed by child name, or a positional []any for TUPLE.
type structColumn struct {
	n        int
	validity []byte
	names    []string
	children []Column
	tuple    bool
	cost     cost
}

func (c *structColumn) Len() int { return c.n }

func (c *structColumn) Value(i int) any {
	checkIndex(i, c.n)
	if !validityBit(c.validity, i) {
		return nil
	}
	if c.tuple {
		out := make([]any, len(c.children))
		for k, child := range c.children {
			out[k] = child.Value(i)
		}
		return out
	}
	out := make(map[string]any, len(c.children))
	for k, child := range c.children {
		out[c.names[k]] = child.Value(i)
	}
	return out
}

// listEntry is a list_entry_t: the child range of one LIST row.
type listEntry struct {
	offset, length int
}

// listColumn is a FLAT LIST or MAP vector; row i is
// child[entries[i].offset : offset+length] as []any.
type listColumn struct {
	validity []byte
	entries  []listEntry
	child    Column
	cost     cost
}

func (c *listColumn) Len() int { return len(c.entries) }

func (c *listColumn) Value(i int) any {
	e := c.entries[i]
	if !validityBit(c.validity, i) {
		return nil
	}
	out := make([]any, e.length)
	for j := range out {
		out[j] = c.child.Value(e.offset + j)
	}
	return out
}

// arrayColumn is a FLAT ARRAY vector; row i is child[i*size : (i+1)*size].
type arrayColumn struct {
	n        int
	size     int
	validity []byte
	child    Column
	cost     cost
}

func (c *arrayColumn) Len() int { return c.n }

func (c *arrayColumn) Value(i int) any {
	checkIndex(i, c.n)
	if !validityBit(c.validity, i) {
		return nil
	}
	out := make([]any, c.size)
	base := i * c.size
	for j := range out {
		out[j] = c.child.Value(base + j)
	}
	return out
}

// constantColumn is a CONSTANT vector: every row is row 0 of inner. The value
// is boxed again on every call, so callers never share a []byte or map.
type constantColumn struct {
	n     int
	inner Column
}

func (c *constantColumn) Len() int { return c.n }

func (c *constantColumn) Value(i int) any {
	checkIndex(i, c.n)
	return c.inner.Value(0)
}

// dictColumn is a DICTIONARY vector: row i is dict row sel[i], where sel is
// the raw little-endian uint32 selection vector.
type dictColumn struct {
	sel  []byte
	dict Column
}

func (c *dictColumn) Len() int { return len(c.sel) / 4 }

func (c *dictColumn) Value(i int) any {
	return c.dict.Value(int(binary.LittleEndian.Uint32(c.sel[i*4:])))
}

// sequenceColumn is a SEQUENCE vector: row i is start + increment*i,
// computed with uint64 wrap-around and truncated to the physical width like
// DuckDB's GenerateSequence, then boxed like a FLAT value of the same type.
type sequenceColumn struct {
	n          int
	start, inc uint64
	width      int
	box        func(b []byte) any
}

func (c *sequenceColumn) Len() int { return c.n }

func (c *sequenceColumn) Value(i int) any {
	checkIndex(i, c.n)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], c.start+c.inc*uint64(i))
	return c.box(buf[:c.width])
}

// nullColumn is a column of the SQLNULL type: every row is NULL.
type nullColumn struct{ n int }

func (c nullColumn) Len() int { return c.n }

func (c nullColumn) Value(i int) any {
	checkIndex(i, c.n)
	return nil
}

// cost is how many Go values materializing a column produces (every value,
// list and map counts one): at most maxRow for one row (one Value call), at
// most total for all rows together. The decoder bounds both against the
// bytes it received, because cheaply encoded vectors (CONSTANT, SEQUENCE,
// DICTIONARY, overlapping LIST entries) can describe far more values than
// their size.
type cost struct{ maxRow, total uint64 }

// costOf returns the cost of a decoded column.
func costOf(c Column) cost {
	var k cost
	switch x := c.(type) {
	case *structColumn:
		k = x.cost
	case *listColumn:
		k = x.cost
	case *arrayColumn:
		k = x.cost
	case *constantColumn:
		in := costOf(x.inner)
		return cost{maxRow: in.maxRow, total: satMul(uint64(x.n), in.maxRow)}
	case *dictColumn:
		in := costOf(x.dict)
		return cost{maxRow: in.maxRow, total: satMul(uint64(x.Len()), in.maxRow)}
	}
	if k.maxRow == 0 { // a scalar column, or one built without a cost
		return cost{maxRow: 1, total: uint64(c.Len())}
	}
	return k
}

// satAdd and satMul are uint64 arithmetic saturating at math.MaxUint64.
func satAdd(a, b uint64) uint64 {
	s, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return math.MaxUint64
	}
	return s
}

func satMul(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		return math.MaxUint64
	}
	return lo
}

// checkIndex panics like a slice index expression when i is outside [0, n).
func checkIndex(i, n int) {
	if uint(i) >= uint(n) {
		panic("message: column index " + strconv.Itoa(i) + " out of range [0, " + strconv.Itoa(n) + ")")
	}
}
