// Package logicaltype implements DuckDB's logical-type system as serialized
// over the Quack wire protocol. Field IDs and the ExtraTypeInfo discriminator
// values mirror DuckDB's own BinarySerializer layout.
package logicaltype

import (
	"errors"
	"fmt"

	"github.com/zzir/quackdriver/internal/codec"
)

// ID enumerates DuckDB logical type ids serialized in Quack result schemas.
type ID uint32

// Logical type ids. Values match DuckDB's `LogicalTypeId` enum.
const (
	IDInvalid        ID = 0
	IDSQLNull        ID = 1
	IDUnknown        ID = 2
	IDAny            ID = 3
	IDUnbound        ID = 4
	IDTemplate       ID = 5
	IDType           ID = 6
	IDBoolean        ID = 10
	IDTinyint        ID = 11
	IDSmallint       ID = 12
	IDInteger        ID = 13
	IDBigint         ID = 14
	IDDate           ID = 15
	IDTime           ID = 16
	IDTimestampSec   ID = 17
	IDTimestampMs    ID = 18
	IDTimestamp      ID = 19
	IDTimestampNs    ID = 20
	IDDecimal        ID = 21
	IDFloat          ID = 22
	IDDouble         ID = 23
	IDChar           ID = 24
	IDVarchar        ID = 25
	IDBlob           ID = 26
	IDInterval       ID = 27
	IDUTinyint       ID = 28
	IDUSmallint      ID = 29
	IDUInteger       ID = 30
	IDUBigint        ID = 31
	IDTimestampTz    ID = 32
	IDTimeTz         ID = 34
	IDTimeNs         ID = 35
	IDBit            ID = 36
	IDStringLiteral  ID = 37
	IDIntegerLiteral ID = 38
	IDBignum         ID = 39
	IDUHugeint       ID = 49
	IDHugeint        ID = 50
	IDPointer        ID = 51
	IDValidity       ID = 53
	IDUUID           ID = 54
	IDGeometry       ID = 60
	IDStruct         ID = 100
	IDList           ID = 101
	IDMap            ID = 102
	IDTable          ID = 103
	IDEnum           ID = 104
	IDAggregateState ID = 105
	IDLambda         ID = 106
	IDUnion          ID = 107
	IDArray          ID = 108
	IDVariant        ID = 109
)

// PhysicalType enumerates DuckDB's physical storage tags.
type PhysicalType uint8

// Physical type tags. Values match DuckDB's `PhysicalType` enum.
const (
	PhysicalInvalid  PhysicalType = 0
	PhysicalBool     PhysicalType = 1
	PhysicalUint8    PhysicalType = 2
	PhysicalInt8     PhysicalType = 3
	PhysicalUint16   PhysicalType = 4
	PhysicalInt16    PhysicalType = 5
	PhysicalUint32   PhysicalType = 6
	PhysicalInt32    PhysicalType = 7
	PhysicalUint64   PhysicalType = 8
	PhysicalInt64    PhysicalType = 9
	PhysicalFloat    PhysicalType = 11
	PhysicalDouble   PhysicalType = 12
	PhysicalInterval PhysicalType = 21
	PhysicalList     PhysicalType = 23
	PhysicalStruct   PhysicalType = 24
	PhysicalArray    PhysicalType = 29
	PhysicalVarchar  PhysicalType = 200
	PhysicalUint128  PhysicalType = 203
	PhysicalInt128   PhysicalType = 204
	PhysicalUnknown  PhysicalType = 205
	PhysicalBit      PhysicalType = 206
)

// ExtraInfoType discriminates ExtraTypeInfo variants.
type ExtraInfoType uint32

// ExtraTypeInfo discriminator values. Values match DuckDB's `ExtraTypeInfoType`.
const (
	ExtraInvalid         ExtraInfoType = 0
	ExtraGeneric         ExtraInfoType = 1
	ExtraDecimal         ExtraInfoType = 2
	ExtraString          ExtraInfoType = 3
	ExtraList            ExtraInfoType = 4
	ExtraStruct          ExtraInfoType = 5
	ExtraEnum            ExtraInfoType = 6
	ExtraUnbound         ExtraInfoType = 7
	ExtraAggregateState  ExtraInfoType = 8
	ExtraArray           ExtraInfoType = 9
	ExtraAny             ExtraInfoType = 10
	ExtraIntegerLiteral  ExtraInfoType = 11
	ExtraTemplate        ExtraInfoType = 12
	ExtraGeo             ExtraInfoType = 13
)

// ErrUnsupported indicates a logical-type variant the driver does not implement yet.
var ErrUnsupported = errors.New("logicaltype: unsupported")

// ChildType is a named child used by STRUCT/UNION/MAP.
type ChildType struct {
	Name string
	Type Type
}

// ExtraInfo carries per-variant metadata. Only the fields named in Kind are
// meaningful for that variant.
type ExtraInfo struct {
	Kind  ExtraInfoType
	Alias string

	// DECIMAL
	Width uint8
	Scale uint8

	// STRING / CHAR / VARCHAR
	Collation string

	// LIST / MAP / ARRAY (single child)
	Child *Type

	// STRUCT / UNION
	Children []ChildType

	// ARRAY
	ArraySize uint32

	// ENUM
	EnumValues []string

	// AGGREGATE_STATE
	FunctionName       string
	ReturnType         *Type
	BoundArgumentTypes []Type

	// ANY
	TargetType *Type
	CastScore  uint64

	// TEMPLATE
	TemplateName string

	// UNBOUND
	UnboundName    string
	UnboundCatalog string
	UnboundSchema  string

	// GEO
	CRSDefinition string
}

// Type is a DuckDB logical type plus optional ExtraInfo metadata.
type Type struct {
	ID    ID
	Info  *ExtraInfo
}

// PhysicalType returns the storage tag used by Vector encodings for this type.
func (t Type) PhysicalType() PhysicalType {
	switch t.ID {
	case IDBoolean:
		return PhysicalBool
	case IDTinyint:
		return PhysicalInt8
	case IDUTinyint:
		return PhysicalUint8
	case IDSmallint:
		return PhysicalInt16
	case IDUSmallint:
		return PhysicalUint16
	case IDSQLNull, IDDate, IDInteger:
		return PhysicalInt32
	case IDUInteger:
		return PhysicalUint32
	case IDBigint, IDTime, IDTimeNs, IDTimestamp, IDTimestampSec, IDTimestampNs, IDTimestampMs, IDTimeTz, IDTimestampTz:
		return PhysicalInt64
	case IDUBigint:
		return PhysicalUint64
	case IDUHugeint:
		return PhysicalUint128
	case IDHugeint, IDUUID:
		return PhysicalInt128
	case IDFloat:
		return PhysicalFloat
	case IDDouble:
		return PhysicalDouble
	case IDDecimal:
		return decimalPhysical(t)
	case IDBignum, IDVarchar, IDChar, IDBlob, IDBit, IDType, IDAggregateState, IDGeometry:
		return PhysicalVarchar
	case IDInterval:
		return PhysicalInterval
	case IDUnion, IDVariant, IDStruct:
		return PhysicalStruct
	case IDList, IDMap:
		return PhysicalList
	case IDArray:
		return PhysicalArray
	case IDPointer:
		return PhysicalUint64
	case IDValidity:
		return PhysicalBit
	case IDEnum:
		return enumPhysical(t)
	}
	return PhysicalInvalid
}

// IsFixedSize reports whether a physical type has fixed-width values.
func IsFixedSize(p PhysicalType) bool {
	switch p {
	case PhysicalBool, PhysicalUint8, PhysicalInt8, PhysicalUint16, PhysicalInt16,
		PhysicalUint32, PhysicalInt32, PhysicalUint64, PhysicalInt64,
		PhysicalFloat, PhysicalDouble, PhysicalInterval, PhysicalUint128, PhysicalInt128:
		return true
	}
	return false
}

// FixedSize returns the byte width for a fixed-size physical type.
func FixedSize(p PhysicalType) (int, error) {
	switch p {
	case PhysicalBool, PhysicalUint8, PhysicalInt8:
		return 1, nil
	case PhysicalUint16, PhysicalInt16:
		return 2, nil
	case PhysicalUint32, PhysicalInt32, PhysicalFloat:
		return 4, nil
	case PhysicalUint64, PhysicalInt64, PhysicalDouble:
		return 8, nil
	case PhysicalInterval, PhysicalUint128, PhysicalInt128:
		return 16, nil
	}
	return 0, fmt.Errorf("logicaltype: physical type %d is not fixed-size", p)
}

func decimalPhysical(t Type) PhysicalType {
	if t.Info == nil || t.Info.Kind != ExtraDecimal {
		return PhysicalInvalid
	}
	switch {
	case t.Info.Width <= 4:
		return PhysicalInt16
	case t.Info.Width <= 9:
		return PhysicalInt32
	case t.Info.Width <= 18:
		return PhysicalInt64
	case t.Info.Width <= 38:
		return PhysicalInt128
	}
	return PhysicalInvalid
}

func enumPhysical(t Type) PhysicalType {
	if t.Info == nil || t.Info.Kind != ExtraEnum {
		return PhysicalInvalid
	}
	n := len(t.Info.EnumValues)
	switch {
	case n <= 0xff:
		return PhysicalUint8
	case n <= 0xffff:
		return PhysicalUint16
	}
	return PhysicalUint32
}

// Encode writes a LogicalType to w.
func Encode(w *codec.Writer, t Type) {
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(100, func(w *codec.Writer) { w.WriteULEB128(uint64(t.ID)) })
		if t.Info != nil {
			w.WriteField(101, func(w *codec.Writer) {
				w.WriteBool(true)
				encodeExtra(w, t.Info)
			})
		}
	})
}

// Decode reads a LogicalType from r.
func Decode(r *codec.Reader) (Type, error) {
	var out Type
	err := r.ReadObject(func(r *codec.Reader) error {
		if err := r.ReadRequiredField(100, func(r *codec.Reader) error {
			id, err := r.ReadULEB128()
			if err != nil {
				return err
			}
			out.ID = ID(id)
			return nil
		}); err != nil {
			return err
		}
		_, err := r.ReadOptionalField(101, func(r *codec.Reader) error {
			present, err := r.ReadBool()
			if err != nil {
				return err
			}
			if !present {
				return nil
			}
			info, err := decodeExtra(r)
			if err != nil {
				return err
			}
			out.Info = info
			return nil
		})
		return err
	})
	return out, err
}

func encodeExtra(w *codec.Writer, info *ExtraInfo) {
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(100, func(w *codec.Writer) { w.WriteULEB128(uint64(info.Kind)) })
		if info.Alias != "" {
			w.WriteField(101, func(w *codec.Writer) { w.WriteString(info.Alias) })
		}
		switch info.Kind {
		case ExtraInvalid, ExtraGeneric:
		case ExtraDecimal:
			w.WriteField(200, func(w *codec.Writer) { w.WriteULEB128(uint64(info.Width)) })
			w.WriteField(201, func(w *codec.Writer) { w.WriteULEB128(uint64(info.Scale)) })
		case ExtraString:
			w.WriteField(200, func(w *codec.Writer) { w.WriteString(info.Collation) })
		case ExtraList:
			if info.Child != nil {
				w.WriteField(200, func(w *codec.Writer) { Encode(w, *info.Child) })
			}
		case ExtraStruct:
			w.WriteField(200, func(w *codec.Writer) { encodeChildren(w, info.Children) })
		case ExtraEnum:
			w.WriteField(200, func(w *codec.Writer) { w.WriteULEB128(uint64(len(info.EnumValues))) })
			w.WriteField(201, func(w *codec.Writer) {
				w.WriteULEB128(uint64(len(info.EnumValues)))
				for _, v := range info.EnumValues {
					w.WriteString(v)
				}
			})
		case ExtraAggregateState:
			w.WriteField(200, func(w *codec.Writer) { w.WriteString(info.FunctionName) })
			if info.ReturnType != nil {
				w.WriteField(201, func(w *codec.Writer) { Encode(w, *info.ReturnType) })
			}
			w.WriteField(202, func(w *codec.Writer) {
				w.WriteULEB128(uint64(len(info.BoundArgumentTypes)))
				for _, t := range info.BoundArgumentTypes {
					Encode(w, t)
				}
			})
		case ExtraArray:
			if info.Child != nil {
				w.WriteField(200, func(w *codec.Writer) { Encode(w, *info.Child) })
			}
			w.WriteField(201, func(w *codec.Writer) { w.WriteULEB128(uint64(info.ArraySize)) })
		case ExtraAny:
			if info.TargetType != nil {
				w.WriteField(200, func(w *codec.Writer) { Encode(w, *info.TargetType) })
			}
			w.WriteField(201, func(w *codec.Writer) { w.WriteULEB128(info.CastScore) })
		case ExtraTemplate:
			w.WriteField(200, func(w *codec.Writer) { w.WriteString(info.TemplateName) })
		case ExtraGeo:
			w.WriteField(200, func(w *codec.Writer) {
				w.WriteObject(func(w *codec.Writer) {
					if info.CRSDefinition != "" {
						w.WriteField(100, func(w *codec.Writer) { w.WriteString(info.CRSDefinition) })
					}
				})
			})
		case ExtraUnbound:
			if info.UnboundName != "" {
				w.WriteField(200, func(w *codec.Writer) { w.WriteString(info.UnboundName) })
			}
			if info.UnboundCatalog != "" {
				w.WriteField(201, func(w *codec.Writer) { w.WriteString(info.UnboundCatalog) })
			}
			if info.UnboundSchema != "" {
				w.WriteField(202, func(w *codec.Writer) { w.WriteString(info.UnboundSchema) })
			}
		}
	})
}

func decodeExtra(r *codec.Reader) (*ExtraInfo, error) {
	info := &ExtraInfo{}
	err := r.ReadObject(func(r *codec.Reader) error {
		if err := r.ReadRequiredField(100, func(r *codec.Reader) error {
			v, err := r.ReadULEB128()
			if err != nil {
				return err
			}
			info.Kind = ExtraInfoType(v)
			return nil
		}); err != nil {
			return err
		}
		if _, err := r.ReadOptionalField(101, func(r *codec.Reader) error {
			v, err := r.ReadString()
			info.Alias = v
			return err
		}); err != nil {
			return err
		}
		// Skip future extension-marker field at 103 if present.
		if _, err := r.ReadOptionalField(103, func(r *codec.Reader) error {
			present, err := r.ReadBool()
			if err != nil {
				return err
			}
			if present {
				return fmt.Errorf("logicaltype: extension type metadata: %w", ErrUnsupported)
			}
			return nil
		}); err != nil {
			return err
		}

		switch info.Kind {
		case ExtraInvalid, ExtraGeneric:
			return nil
		case ExtraDecimal:
			if _, err := r.ReadOptionalField(200, func(r *codec.Reader) error {
				v, err := r.ReadULEB128()
				info.Width = uint8(v)
				return err
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(201, func(r *codec.Reader) error {
				v, err := r.ReadULEB128()
				info.Scale = uint8(v)
				return err
			}); err != nil {
				return err
			}
			return nil
		case ExtraString:
			if _, err := r.ReadOptionalField(200, func(r *codec.Reader) error {
				v, err := r.ReadString()
				info.Collation = v
				return err
			}); err != nil {
				return err
			}
			return nil
		case ExtraList:
			return r.ReadRequiredField(200, func(r *codec.Reader) error {
				child, err := Decode(r)
				if err != nil {
					return err
				}
				info.Child = &child
				return nil
			})
		case ExtraStruct:
			if _, err := r.ReadOptionalField(200, func(r *codec.Reader) error {
				children, err := decodeChildren(r)
				if err != nil {
					return err
				}
				info.Children = children
				return nil
			}); err != nil {
				return err
			}
			return nil
		case ExtraEnum:
			if err := r.ReadRequiredField(200, func(r *codec.Reader) error {
				_, err := r.ReadULEB128()
				return err
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(201, func(r *codec.Reader) error {
				n, err := r.ReadULEB128()
				if err != nil {
					return err
				}
				values := make([]string, int(n))
				for i := range values {
					s, err := r.ReadString()
					if err != nil {
						return err
					}
					values[i] = s
				}
				info.EnumValues = values
				return nil
			})
		case ExtraAggregateState:
			if err := r.ReadRequiredField(200, func(r *codec.Reader) error {
				v, err := r.ReadString()
				info.FunctionName = v
				return err
			}); err != nil {
				return err
			}
			if err := r.ReadRequiredField(201, func(r *codec.Reader) error {
				t, err := Decode(r)
				if err != nil {
					return err
				}
				info.ReturnType = &t
				return nil
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(202, func(r *codec.Reader) error {
				n, err := r.ReadULEB128()
				if err != nil {
					return err
				}
				args := make([]Type, int(n))
				for i := range args {
					t, err := Decode(r)
					if err != nil {
						return err
					}
					args[i] = t
				}
				info.BoundArgumentTypes = args
				return nil
			})
		case ExtraArray:
			if err := r.ReadRequiredField(200, func(r *codec.Reader) error {
				t, err := Decode(r)
				if err != nil {
					return err
				}
				info.Child = &t
				return nil
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(201, func(r *codec.Reader) error {
				v, err := r.ReadULEB128()
				info.ArraySize = uint32(v)
				return err
			})
		case ExtraAny:
			if err := r.ReadRequiredField(200, func(r *codec.Reader) error {
				t, err := Decode(r)
				if err != nil {
					return err
				}
				info.TargetType = &t
				return nil
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(201, func(r *codec.Reader) error {
				v, err := r.ReadULEB128()
				info.CastScore = v
				return err
			})
		case ExtraIntegerLiteral:
			peeked, err := r.PeekFieldID()
			if err == nil && peeked == 200 {
				return fmt.Errorf("logicaltype: INTEGER_LITERAL with bound value: %w", ErrUnsupported)
			}
			return nil
		case ExtraTemplate:
			if _, err := r.ReadOptionalField(200, func(r *codec.Reader) error {
				v, err := r.ReadString()
				info.TemplateName = v
				return err
			}); err != nil {
				return err
			}
			return nil
		case ExtraGeo:
			if _, err := r.ReadOptionalField(200, func(r *codec.Reader) error {
				return r.ReadObject(func(r *codec.Reader) error {
					_, err := r.ReadOptionalField(100, func(r *codec.Reader) error {
						v, err := r.ReadString()
						info.CRSDefinition = v
						return err
					})
					return err
				})
			}); err != nil {
				return err
			}
			return nil
		case ExtraUnbound:
			peeked, err := r.PeekFieldID()
			if err == nil && peeked == 204 {
				return fmt.Errorf("logicaltype: UNBOUND with parsed expression: %w", ErrUnsupported)
			}
			if _, err := r.ReadOptionalField(200, func(r *codec.Reader) error {
				v, err := r.ReadString()
				info.UnboundName = v
				return err
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(201, func(r *codec.Reader) error {
				v, err := r.ReadString()
				info.UnboundCatalog = v
				return err
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(202, func(r *codec.Reader) error {
				v, err := r.ReadString()
				info.UnboundSchema = v
				return err
			}); err != nil {
				return err
			}
			return nil
		}
		return fmt.Errorf("logicaltype: unknown ExtraInfoType %d", info.Kind)
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

func encodeChildren(w *codec.Writer, children []ChildType) {
	w.WriteULEB128(uint64(len(children)))
	for _, child := range children {
		w.WriteObject(func(w *codec.Writer) {
			w.WriteField(0, func(w *codec.Writer) { w.WriteString(child.Name) })
			w.WriteField(1, func(w *codec.Writer) { Encode(w, child.Type) })
		})
	}
}

func decodeChildren(r *codec.Reader) ([]ChildType, error) {
	n, err := r.ReadULEB128()
	if err != nil {
		return nil, err
	}
	out := make([]ChildType, int(n))
	for i := range out {
		var pair ChildType
		err := r.ReadObject(func(r *codec.Reader) error {
			if err := r.ReadRequiredField(0, func(r *codec.Reader) error {
				v, err := r.ReadString()
				pair.Name = v
				return err
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(1, func(r *codec.Reader) error {
				t, err := Decode(r)
				if err != nil {
					return err
				}
				pair.Type = t
				return nil
			})
		})
		if err != nil {
			return nil, err
		}
		out[i] = pair
	}
	return out, nil
}

// Name returns a human-readable label for a logical type id.
func (t Type) Name() string {
	if name, ok := idNames[t.ID]; ok {
		return name
	}
	return fmt.Sprintf("ID(%d)", t.ID)
}

var idNames = map[ID]string{
	IDSQLNull: "NULL", IDBoolean: "BOOLEAN",
	IDTinyint: "TINYINT", IDSmallint: "SMALLINT", IDInteger: "INTEGER", IDBigint: "BIGINT",
	IDUTinyint: "UTINYINT", IDUSmallint: "USMALLINT", IDUInteger: "UINTEGER", IDUBigint: "UBIGINT",
	IDHugeint: "HUGEINT", IDUHugeint: "UHUGEINT", IDFloat: "FLOAT", IDDouble: "DOUBLE",
	IDDecimal: "DECIMAL", IDChar: "CHAR", IDVarchar: "VARCHAR", IDBlob: "BLOB", IDBit: "BIT",
	IDDate: "DATE", IDTime: "TIME", IDTimeNs: "TIME_NS", IDTimeTz: "TIME WITH TIME ZONE",
	IDTimestamp: "TIMESTAMP", IDTimestampSec: "TIMESTAMP_S", IDTimestampMs: "TIMESTAMP_MS",
	IDTimestampNs: "TIMESTAMP_NS", IDTimestampTz: "TIMESTAMP WITH TIME ZONE",
	IDInterval: "INTERVAL", IDUUID: "UUID", IDGeometry: "GEOMETRY",
	IDStruct: "STRUCT", IDList: "LIST", IDMap: "MAP", IDArray: "ARRAY",
	IDEnum: "ENUM", IDUnion: "UNION", IDVariant: "VARIANT",
}
