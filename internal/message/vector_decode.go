package message

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/value"
)

// ErrVariant is returned for a vector of type VARIANT (or with VARIANT
// nested in it): VARIANT values are not decoded.
var ErrVariant = errors.New("message: VARIANT values are not supported")

// The vector decoder follows DuckDB v2.0.0 Vector::Deserialize
// (src/common/types/vector.cpp) and the buffer deserializers of
// src/common/vector/{dictionary,sequence}_vector.cpp:
//
//	90 vector_type (default FLAT)
//	FLAT:       [99 geometry_format] 100 has_validity_mask [101 validity]
//	            fixed size:   102 data (count * width bytes)
//	            GEOMETRY:     102 list<WKB blob>
//	            VARCHAR-like: 107 byte_data_length, 108 length_data (count * u32),
//	                          109 byte_data; or the legacy 102 list<blob>
//	            STRUCT:       103 list<child vector object>
//	            LIST:         104 list_size, 105 list<{100 offset, 101 length}>,
//	                          106 child vector object
//	            ARRAY:        103 array_size, 104 child vector object
//	CONSTANT:   a nested vector body of one row, in the same object
//	DICTIONARY: 91 sel_vector (count * u32), 92 dict_count, then the
//	            dictionary's vector body in the same object
//	SEQUENCE:   91 seq_start, 92 seq_increment (signed varints)
//
// SHREDDED vectors (type 5) only hold VARIANT, which is rejected first.
//
// Every length is validated before anything is allocated for it; columns
// reference the input bytes instead of copying them.

const (
	// maxVectorDepth bounds the recursion through nested vector formats and
	// nested types, so hostile input cannot exhaust the stack.
	maxVectorDepth = 512
	// maxVectorCount is DuckDB's DConstants::MAX_VECTOR_SIZE (2^37), the
	// largest vector capacity DuckDB itself will allocate.
	maxVectorCount = 1 << 37
)

// Cheaply encoded vectors (CONSTANT, SEQUENCE and DICTIONARY children,
// overlapping LIST entries) can describe far more values than the bytes they
// take, and materializing a LIST, ARRAY or STRUCT row allocates all its values
// at once; an out-of-memory error cannot be recovered in Go. So the number of
// values one row of a vector materializes (cost.maxRow) is bounded by
// materializeRatio values per byte of the vector, or materializeFloor
// values, whichever is larger. Data DuckDB sends in FLAT form takes at least
// one byte per value.
const (
	materializeRatio = 8
	materializeFloor = 1 << 20
)

// checkRowCost rejects a vector (or chunk) of the given type name whose rows
// would materialize up to maxRow values each, when the used bytes do not
// justify that.
func checkRowCost(name string, maxRow uint64, used int) error {
	limit := max(uint64(materializeFloor), satMul(materializeRatio, uint64(max(used, 0))))
	if maxRow > limit {
		return fmt.Errorf("message: a %s row of %d bytes would materialize %d values (limit %d)", name, used, maxRow, limit)
	}
	return nil
}

// geometryStorageWKB is GeometryStorageType::WKB; DuckDB writes it as the
// GEOMETRY vector's field 99 at every storage version that knows GEOMETRY.
const geometryStorageWKB = 1

// decodeVector reads one vector object holding count rows of type t.
func decodeVector(r *codec.Reader, t logicaltype.Type, count, depth int) (out Column, err error) {
	err = r.ReadObject(func(r *codec.Reader) error {
		out, err = decodeVectorBody(r, t, count, depth)
		return err
	})
	return out, err
}

func decodeVectorBody(r *codec.Reader, t logicaltype.Type, count, depth int) (Column, error) {
	if depth > maxVectorDepth {
		return nil, fmt.Errorf("message: vector nesting exceeds %d levels", maxVectorDepth)
	}
	if count < 0 || count > maxVectorCount {
		return nil, fmt.Errorf("message: vector size %d out of range", count)
	}
	if t.ID == logicaltype.IDVariant {
		return nil, ErrVariant
	}
	var format uint64 // VectorFlat when absent
	if err := r.ReadUintWithDefault(90, &format); err != nil {
		return nil, err
	}
	switch VectorFormat(format) {
	case VectorFlat:
		return decodeFlatVector(r, t, count, depth)
	case VectorConstant:
		// Vector::Deserialize reads a one-row vector whatever count is.
		inner, err := decodeVectorBody(r, t, 1, depth+1)
		if err != nil {
			return nil, err
		}
		return &constantColumn{n: count, inner: inner}, nil
	case VectorDictionary:
		return decodeDictionaryVector(r, t, count, depth)
	case VectorSequence:
		return decodeSequenceVector(r, t, count)
	}
	return nil, fmt.Errorf("message: unsupported vector format %d", format)
}

// readRawBlob reads a length-prefixed blob without copying it.
func readRawBlob(r *codec.Reader) ([]byte, error) {
	n, err := r.ReadCount()
	if err != nil {
		return nil, err
	}
	return r.ReadBytes(n)
}

// readSizedBlob reads a required blob property that must hold exactly size
// bytes (DuckDB's ReadProperty(id, ptr, size) checks the same).
func readSizedBlob(r *codec.Reader, id uint16, what string, size uint64) (out []byte, err error) {
	err = r.ReadRequiredField(id, func(r *codec.Reader) error {
		if out, err = readRawBlob(r); err == nil && uint64(len(out)) != size {
			err = fmt.Errorf("message: %s has %d bytes, expected %d", what, len(out), size)
		}
		return err
	})
	return out, err
}

func readRequiredULEB(r *codec.Reader, id uint16) (v uint64, err error) {
	err = r.ReadRequiredField(id, func(r *codec.Reader) error { v, err = r.ReadULEB128(); return err })
	return v, err
}

func readRequiredSLEB(r *codec.Reader, id uint16) (v int64, err error) {
	err = r.ReadRequiredField(id, func(r *codec.Reader) error { v, err = r.ReadSLEB128(); return err })
	return v, err
}

func decodeDictionaryVector(r *codec.Reader, t logicaltype.Type, count, depth int) (Column, error) {
	sel, err := readSizedBlob(r, 91, "dictionary selection vector", uint64(count)*4)
	if err != nil {
		return nil, err
	}
	dictCount, err := readRequiredULEB(r, 92)
	if err != nil {
		return nil, err
	}
	if dictCount > maxVectorCount {
		return nil, fmt.Errorf("message: dictionary size %d out of range", dictCount)
	}
	for i := range count {
		if idx := binary.LittleEndian.Uint32(sel[i*4:]); uint64(idx) >= dictCount {
			return nil, fmt.Errorf("message: dictionary selection %d out of range (size %d)", idx, dictCount)
		}
	}
	dict, err := decodeVectorBody(r, t, int(dictCount), depth+1)
	if err != nil {
		return nil, err
	}
	return &dictColumn{sel: sel, dict: dict}, nil
}

func decodeSequenceVector(r *codec.Reader, t logicaltype.Type, count int) (Column, error) {
	start, err := readRequiredSLEB(r, 91)
	if err != nil {
		return nil, err
	}
	increment, err := readRequiredSLEB(r, 92)
	if err != nil {
		return nil, err
	}
	// GenerateSequence only supports INT8..INT64 and rejects a start or
	// increment above the type's maximum.
	var maxValue int64
	switch phys := t.PhysicalType(); phys {
	case logicaltype.PhysicalInt8:
		maxValue = math.MaxInt8
	case logicaltype.PhysicalInt16:
		maxValue = math.MaxInt16
	case logicaltype.PhysicalInt32:
		maxValue = math.MaxInt32
	case logicaltype.PhysicalInt64:
		maxValue = math.MaxInt64
	default:
		return nil, fmt.Errorf("message: SEQUENCE vector of type %s is not supported", t.Name())
	}
	if start > maxValue || increment > maxValue {
		return nil, fmt.Errorf("message: SEQUENCE start %d or increment %d out of range for %s", start, increment, t.Name())
	}
	width, box, err := fixedBoxer(t)
	if err != nil {
		return nil, err
	}
	return &sequenceColumn{n: count, start: uint64(start), inc: uint64(increment), width: width, box: box}, nil
}

func decodeFlatVector(r *codec.Reader, t logicaltype.Type, count, depth int) (Column, error) {
	if t.ID == logicaltype.IDGeometry {
		// Absent means the pre-1.5 SPATIAL format (ReadPropertyWithExplicitDefault).
		var format uint64
		if err := r.ReadUintWithDefault(99, &format); err != nil {
			return nil, err
		}
		if format != geometryStorageWKB {
			return nil, fmt.Errorf("message: GEOMETRY storage format %d is not supported (want WKB)", format)
		}
	}

	var hasValidity bool
	if err := r.ReadRequiredField(100, func(r *codec.Reader) (err error) {
		hasValidity, err = r.ReadBool()
		return err
	}); err != nil {
		return nil, err
	}
	var validity []byte // nil: every row is valid
	if hasValidity {
		var err error
		if validity, err = readSizedBlob(r, 101, "validity mask", validityMaskSize(count)); err != nil {
			return nil, err
		}
	}

	phys := t.PhysicalType()
	switch {
	case t.ID == logicaltype.IDSQLNull:
		// SQLNULL is physically INT32; every row is NULL.
		if _, err := readSizedBlob(r, 102, "vector data", uint64(count)*4); err != nil {
			return nil, err
		}
		return nullColumn{n: count}, nil
	case logicaltype.IsFixedSize(phys):
		width, box, err := fixedBoxer(t)
		if err != nil {
			return nil, err
		}
		raw, err := readSizedBlob(r, 102, "vector data", uint64(count)*uint64(width))
		if err != nil {
			return nil, err
		}
		if t.ID == logicaltype.IDEnum {
			if err := checkEnumIndexes(t, raw, width, count, validity); err != nil {
				return nil, err
			}
		}
		return &fixedColumn{n: count, width: width, raw: raw, validity: validity, box: box}, nil
	case t.ID == logicaltype.IDGeometry:
		col, err := readLegacyStrings(r, count, validity)
		if err != nil {
			return nil, err
		}
		col.kind = stringBlob
		return col, nil
	case phys == logicaltype.PhysicalVarchar:
		return decodeStringVector(r, t, count, validity)
	case phys == logicaltype.PhysicalStruct:
		return decodeStructVector(r, t, count, validity, depth)
	case phys == logicaltype.PhysicalList:
		return decodeListVector(r, t, count, validity, depth)
	case phys == logicaltype.PhysicalArray:
		return decodeArrayVector(r, t, count, validity, depth)
	}
	return nil, fmt.Errorf("message: cannot decode vector of type %s", t.Name())
}

// validityMaskSize is ValidityMask::ValidityMaskSize: 64-row words.
func validityMaskSize(count int) uint64 {
	return (uint64(count) + 63) / 64 * 8
}

func checkEnumIndexes(t logicaltype.Type, raw []byte, width, count int, validity []byte) error {
	labels := uint32(len(t.Info.EnumValues))
	for i := range count {
		if idx := enumIndexAt(raw[i*width:], width); validityBit(validity, i) && idx >= labels {
			return fmt.Errorf("message: ENUM index %d out of range (%d labels)", idx, labels)
		}
	}
	return nil
}

// decodeStringVector reads a VARCHAR-physical FLAT vector (VARCHAR, CHAR,
// BLOB, BIT, BIGNUM, TYPE, aliases such as JSON, LEGACY_AGGREGATE_STATE).
func decodeStringVector(r *codec.Reader, t logicaltype.Type, count int, validity []byte) (Column, error) {
	var byteDataLength codec.OptionalIdx
	if _, err := r.ReadOptionalField(107, func(r *codec.Reader) (err error) {
		byteDataLength, err = r.ReadOptionalIdx()
		return err
	}); err != nil {
		return nil, err
	}
	var col *stringColumn
	if byteDataLength.Valid {
		lengths, err := readSizedBlob(r, 108, "string length data", uint64(count)*4)
		if err != nil {
			return nil, err
		}
		data, err := readSizedBlob(r, 109, "string byte data", byteDataLength.Value)
		if err != nil {
			return nil, err
		}
		offs := make([]int, count+1)
		pos := 0
		for i := range count {
			if validityBit(validity, i) {
				// NULL rows consume no bytes whatever their length says.
				n := int(binary.LittleEndian.Uint32(lengths[i*4:]))
				if n > len(data)-pos {
					return nil, fmt.Errorf("message: string lengths exceed the %d bytes of string data", len(data))
				}
				pos += n
			}
			offs[i+1] = pos
		}
		col = &stringColumn{validity: validity, data: data, offs: offs}
	} else {
		var err error
		if col, err = readLegacyStrings(r, count, validity); err != nil {
			return nil, err
		}
	}

	switch t.ID {
	case logicaltype.IDBlob, logicaltype.IDAggregateState:
		// LEGACY_AGGREGATE_STATE is read as BLOB by DuckDB 2.0.
		col.kind = stringBlob
	case logicaltype.IDBit:
		col.kind = stringBit
		return col, validateStrings(col, func(b []byte) error { _, err := value.BitString(b); return err })
	case logicaltype.IDBignum:
		col.kind = stringBignum
		return col, validateStrings(col, func(b []byte) error { _, err := value.Bignum(b); return err })
	}
	return col, nil
}

// readLegacyStrings reads field 102 as list<blob> with exactly count entries
// (the pre-2.0 string layout, still used for GEOMETRY). NULL entries are
// ignored; the valid ones are packed into one buffer.
func readLegacyStrings(r *codec.Reader, count int, validity []byte) (*stringColumn, error) {
	var col *stringColumn
	err := r.ReadRequiredField(102, func(r *codec.Reader) error {
		n, err := r.ReadCount()
		if err != nil {
			return err
		}
		if n != count {
			return fmt.Errorf("message: string vector has %d values, expected %d", n, count)
		}
		parts := make([][]byte, n)
		total := 0
		for i := range parts {
			b, err := readRawBlob(r)
			if err != nil {
				return err
			}
			if validityBit(validity, i) {
				parts[i] = b
				total += len(b)
			}
		}
		data := make([]byte, 0, total)
		offs := make([]int, n+1)
		for i, b := range parts {
			data = append(data, b...)
			offs[i+1] = len(data)
		}
		col = &stringColumn{validity: validity, data: data, offs: offs}
		return nil
	})
	return col, err
}

// validateStrings checks every valid row with check, for types whose
// conversion in Value must not fail.
func validateStrings(col *stringColumn, check func([]byte) error) error {
	for i := range col.Len() {
		if validityBit(col.validity, i) {
			if err := check(col.data[col.offs[i]:col.offs[i+1]]); err != nil {
				return fmt.Errorf("message: row %d: %w", i, err)
			}
		}
	}
	return nil
}

// structChildren returns the children of a physical-STRUCT type.
func structChildren(t logicaltype.Type) ([]logicaltype.ChildType, error) {
	if t.Info != nil && len(t.Info.Children) > 0 {
		return t.Info.Children, nil
	}
	if t.Info == nil {
		return nil, fmt.Errorf("message: %s vector missing type metadata", t.Name())
	}
	return nil, nil
}

// isTuple reports whether rows of t are positional ([]any): TUPLE, and an
// unnamed STRUCT, which DuckDB reads back as TUPLE.
func isTuple(t logicaltype.Type, children []logicaltype.ChildType) bool {
	if t.ID == logicaltype.IDTuple {
		return true
	}
	if t.ID != logicaltype.IDStruct || len(children) == 0 {
		return false
	}
	for _, c := range children {
		if c.Name != "" {
			return false
		}
	}
	return true
}

func decodeStructVector(r *codec.Reader, t logicaltype.Type, count int, validity []byte, depth int) (Column, error) {
	start := r.Remaining()
	children, err := structChildren(t)
	if err != nil {
		return nil, err
	}
	cols := make([]Column, len(children))
	if err := r.ReadRequiredField(103, func(r *codec.Reader) error {
		n, err := r.ReadULEB128()
		if err != nil {
			return err
		}
		if n != uint64(len(children)) {
			return fmt.Errorf("message: %s has %d children, got %d vectors", t.Name(), len(children), n)
		}
		for i, c := range children {
			if cols[i], err = decodeVector(r, c.Type, count, depth+1); err != nil {
				return fmt.Errorf("child %q: %w", c.Name, err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	names := make([]string, len(children))
	for i, c := range children {
		names[i] = c.Name
	}
	k := cost{maxRow: 1, total: uint64(count)}
	for _, c := range cols {
		ck := costOf(c)
		k.maxRow = satAdd(k.maxRow, ck.maxRow)
		k.total = satAdd(k.total, ck.total)
	}
	if err := checkRowCost(t.Name(), k.maxRow, start-r.Remaining()); err != nil {
		return nil, err
	}
	return &structColumn{n: count, validity: validity, names: names, children: cols, tuple: isTuple(t, children), cost: k}, nil
}

func decodeListVector(r *codec.Reader, t logicaltype.Type, count int, validity []byte, depth int) (Column, error) {
	start := r.Remaining()
	if t.Info == nil || t.Info.Child == nil {
		return nil, fmt.Errorf("message: %s vector missing child type", t.Name())
	}
	listSize, err := readRequiredULEB(r, 104)
	if err != nil {
		return nil, err
	}
	if listSize > maxVectorCount {
		return nil, fmt.Errorf("message: LIST child size %d out of range", listSize)
	}
	var entries []listEntry
	if err := r.ReadRequiredField(105, func(r *codec.Reader) error {
		n, err := r.ReadCount()
		if err != nil {
			return err
		}
		if n != count {
			return fmt.Errorf("message: LIST has %d rows, got %d entries", count, n)
		}
		entries = make([]listEntry, n)
		for i := range entries {
			var offset, length uint64
			if err := r.ReadObject(func(r *codec.Reader) error {
				if offset, err = readRequiredULEB(r, 100); err != nil {
					return err
				}
				length, err = readRequiredULEB(r, 101)
				return err
			}); err != nil {
				return err
			}
			if !validityBit(validity, i) {
				continue // DuckDB resets NULL entries to (0, 0)
			}
			if offset > listSize || length > listSize-offset {
				return fmt.Errorf("message: LIST entry %d (offset=%d length=%d) exceeds child vector size %d",
					i, offset, length, listSize)
			}
			entries[i] = listEntry{offset: int(offset), length: int(length)}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	var child Column
	if err := r.ReadRequiredField(106, func(r *codec.Reader) (err error) {
		child, err = decodeVector(r, *t.Info.Child, int(listSize), depth+1)
		return err
	}); err != nil {
		return nil, err
	}
	// A row materializes the child rows of its entry, each once: at most
	// length times the child's largest row, and at most all child values.
	ck := costOf(child)
	k := cost{maxRow: 1}
	for _, e := range entries {
		row := satAdd(1, min(satMul(uint64(e.length), ck.maxRow), ck.total))
		k.maxRow = max(k.maxRow, row)
		k.total = satAdd(k.total, row)
	}
	if err := checkRowCost(t.Name(), k.maxRow, start-r.Remaining()); err != nil {
		return nil, err
	}
	return &listColumn{validity: validity, entries: entries, child: child, cost: k}, nil
}

func decodeArrayVector(r *codec.Reader, t logicaltype.Type, count int, validity []byte, depth int) (Column, error) {
	start := r.Remaining()
	if t.Info == nil || t.Info.Child == nil {
		return nil, fmt.Errorf("message: %s vector missing child type", t.Name())
	}
	arraySize, err := readRequiredULEB(r, 103)
	if err != nil {
		return nil, err
	}
	if arraySize != uint64(t.Info.ArraySize) {
		return nil, fmt.Errorf("message: ARRAY vector size %d does not match type %d", arraySize, t.Info.ArraySize)
	}
	// arraySize < 2^32 and count <= 2^37, so the product cannot overflow.
	childCount := arraySize * uint64(count)
	if childCount > maxVectorCount {
		return nil, fmt.Errorf("message: ARRAY child size %d out of range", childCount)
	}
	var child Column
	if err := r.ReadRequiredField(104, func(r *codec.Reader) (err error) {
		child, err = decodeVector(r, *t.Info.Child, int(childCount), depth+1)
		return err
	}); err != nil {
		return nil, err
	}
	// Rows cover disjoint child ranges.
	ck := costOf(child)
	k := cost{
		maxRow: satAdd(1, min(satMul(arraySize, ck.maxRow), ck.total)),
		total:  satAdd(uint64(count), ck.total),
	}
	if err := checkRowCost(t.Name(), k.maxRow, start-r.Remaining()); err != nil {
		return nil, err
	}
	return &arrayColumn{n: count, size: int(arraySize), validity: validity, child: child, cost: k}, nil
}
