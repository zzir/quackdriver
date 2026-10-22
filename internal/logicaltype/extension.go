package logicaltype

import (
	"fmt"

	"github.com/zzir/quackdriver/internal/codec"
)

// DuckDB 2.0 attaches an ExtensionTypeInfo to some types (field 103 of
// LogicalTypeInfo); the aggregate states of "agg(x) EXPORT_STATE", for
// example, record the aggregate's signature there as Values (core
// aggregate_export.cpp):
//
//	ExtensionTypeInfo   100 modifiers  vector<LogicalTypeModifier> (D)
//	                    101 properties unordered_map<string, Value> (D)
//	LogicalTypeModifier 100 value Value, 101 label string (D)
//
// The driver does not interpret it. A serialized Value is not
// self-delimiting, so the object is walked and kept verbatim for Encode.

// readExtensionInfo reads an ExtensionTypeInfo object and returns a copy of
// its bytes.
func readExtensionInfo(r *codec.Reader, depth int) ([]byte, error) {
	start := r.Pos()
	// each reads the list property id, running item on every element object.
	each := func(r *codec.Reader, id uint16, item func(*codec.Reader) error) error {
		_, err := r.ReadOptionalField(id, func(r *codec.Reader) error {
			n, err := r.ReadCount()
			if err != nil {
				return err
			}
			for range n {
				if err := r.ReadObject(item); err != nil {
					return fmt.Errorf("logicaltype: extension type info: %w", err)
				}
			}
			return nil
		})
		return err
	}
	value := func(r *codec.Reader) error { return skipValue(r, nil, depth+1) }
	err := r.ReadObject(func(r *codec.Reader) error {
		if err := each(r, 100, func(r *codec.Reader) error { // modifiers
			if err := r.ReadRequiredField(100, value); err != nil {
				return err
			}
			var label string
			return r.ReadStringWithDefault(101, &label)
		}); err != nil {
			return err
		}
		return each(r, 101, func(r *codec.Reader) error { // properties
			if err := r.ReadRequiredField(0, func(r *codec.Reader) error {
				_, err := r.ReadString()
				return err
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(1, value)
		})
	})
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), r.Since(start)...), nil
}

// skipValue reads one serialized Value object (core value.cpp
// Value::SerializeInternal): 100 type (only when the parent does not imply
// it), 101 is_null, 102 the payload. hint is the type the parent implies,
// nil for a root value.
func skipValue(r *codec.Reader, hint *Type, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("logicaltype: value nesting exceeds %d levels", maxDepth)
	}
	return r.ReadObject(func(r *codec.Reader) error {
		var t Type
		hasType, err := r.ReadOptionalField(100, func(r *codec.Reader) (err error) {
			t, err = decodeType(r, depth+1)
			return err
		})
		if err != nil {
			return err
		}
		if !hasType || t.PhysicalType() == PhysicalInvalid {
			if hint == nil {
				return fmt.Errorf("logicaltype: serialized value has no type")
			}
			t = *hint
		}
		var isNull bool
		if err := r.ReadRequiredField(101, func(r *codec.Reader) (err error) {
			isNull, err = r.ReadBool()
			return err
		}); err != nil || isNull {
			return err
		}
		if err := r.ReadRequiredField(102, func(r *codec.Reader) error {
			return skipValuePayload(r, t, depth)
		}); err != nil {
			return err
		}
		if t.ID == IDGeometry { // 103 geometry_format follows the payload
			var format uint64
			return r.ReadUintWithDefault(103, &format)
		}
		return nil
	})
}

// skipValuePayload reads field 102 of a non-NULL Value of type t.
func skipValuePayload(r *codec.Reader, t Type, depth int) error {
	if t.ID == IDType {
		_, err := decodeType(r, depth+1)
		return err
	}
	var err error
	switch p := t.PhysicalType(); p {
	case PhysicalBool:
		_, err = r.ReadBool()
	case PhysicalInt8, PhysicalInt16, PhysicalInt32, PhysicalInt64:
		_, err = r.ReadSLEB128()
	case PhysicalUint8, PhysicalUint16, PhysicalUint32, PhysicalUint64:
		_, err = r.ReadULEB128()
	case PhysicalInt128:
		_, err = r.ReadHugeInt()
	case PhysicalUint128:
		if _, err = r.ReadULEB128(); err == nil {
			_, err = r.ReadULEB128()
		}
	case PhysicalFloat, PhysicalDouble:
		n, _ := FixedSize(p)
		_, err = r.ReadBytes(n)
	case PhysicalInterval:
		// interval_t: 1 months, 2 days, 3 micros, each written with default.
		err = r.ReadObject(func(r *codec.Reader) error {
			for id := uint16(1); id <= 3; id++ {
				if _, err := r.ReadOptionalField(id, func(r *codec.Reader) error {
					_, err := r.ReadSLEB128()
					return err
				}); err != nil {
					return err
				}
			}
			return nil
		})
	case PhysicalVarchar:
		_, err = r.ReadBlob()
	case PhysicalList, PhysicalStruct, PhysicalArray:
		if t.Info == nil {
			return fmt.Errorf("logicaltype: nested %s value without type info", t.Name())
		}
		err = r.ReadObject(func(r *codec.Reader) error {
			return r.ReadRequiredField(100, func(r *codec.Reader) error {
				n, err := r.ReadCount()
				if err != nil {
					return err
				}
				if p == PhysicalStruct && n > len(t.Info.Children) {
					return fmt.Errorf("logicaltype: %s value has more than %d children", t.Name(), len(t.Info.Children))
				}
				for i := range n {
					// Value::GetChildType; a child type without a physical
					// type (ANY) means the child carries its own.
					child := t.Info.Child
					if p == PhysicalStruct {
						child = &t.Info.Children[i].Type
					}
					if child != nil && child.PhysicalType() == PhysicalInvalid {
						child = nil
					}
					if err := skipValue(r, child, depth+1); err != nil {
						return err
					}
				}
				return nil
			})
		})
	default:
		err = fmt.Errorf("logicaltype: cannot read a serialized %s value: %w", t.Name(), ErrUnsupported)
	}
	return err
}
