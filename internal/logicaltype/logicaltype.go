// Package logicaltype implements DuckDB's logical-type system as serialized
// over the Quack wire protocol. Field IDs and the ExtraTypeInfo discriminator
// values mirror DuckDB's own BinarySerializer layout.
package logicaltype

import (
	"errors"
	"fmt"
	"math"

	"github.com/zzir/quackdriver/internal/codec"
)

// ID enumerates DuckDB logical type ids serialized in Quack result schemas.
type ID uint32

// Logical type ids. Values match DuckDB's `LogicalTypeId` enum (DuckDB
// v2.0.0, src/include/duckdb/common/types.hpp).
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
	IDTimestampTzNs  ID = 33 // TIMESTAMP_TZ_NS: INT64 nanoseconds since the epoch, UTC
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
	// IDAggregateState is LEGACY_AGGREGATE_STATE in DuckDB 2.0. DuckDB's reader
	// turns it into BLOB; 2.0 itself represents aggregate states as ordinary
	// types carrying an AGGREGATE_STATE alias, so a 2.0 server does not send it.
	IDAggregateState ID = 105
	IDLambda         ID = 106
	IDUnion          ID = 107
	IDArray          ID = 108
	IDVariant        ID = 109
	// IDTuple is an unnamed STRUCT: STRUCT_TYPE_INFO whose children all have
	// empty names, physically a STRUCT. row(...) and (a, b) produce it.
	IDTuple ID = 110
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

// ExtraTypeInfo discriminator values. Values match DuckDB's `LogicalTypeInfoType`
// (called ExtraTypeInfoType before 2.0; the numbering did not change).
const (
	ExtraInvalid        ExtraInfoType = 0
	ExtraGeneric        ExtraInfoType = 1
	ExtraDecimal        ExtraInfoType = 2
	ExtraString         ExtraInfoType = 3
	ExtraList           ExtraInfoType = 4
	ExtraStruct         ExtraInfoType = 5
	ExtraEnum           ExtraInfoType = 6
	ExtraUnbound        ExtraInfoType = 7
	ExtraAggregateState ExtraInfoType = 8 // LEGACY_AGGREGATE_STATE_TYPE_INFO in 2.0
	ExtraArray          ExtraInfoType = 9
	ExtraAny            ExtraInfoType = 10
	ExtraIntegerLiteral ExtraInfoType = 11
	ExtraTemplate       ExtraInfoType = 12
	ExtraGeo            ExtraInfoType = 13
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

	// LEGACY_AGGREGATE_STATE (decoded when present; DuckDB 2.0 writes none of these)
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

	// ExtensionInfo is the serialized ExtensionTypeInfo object (field 103),
	// kept verbatim; nil when the type has none. See extension.go.
	ExtensionInfo []byte
}

// Type is a DuckDB logical type plus optional ExtraInfo metadata.
type Type struct {
	ID   ID
	Info *ExtraInfo
}

// Contains reports whether t, or a type nested in it (a LIST, ARRAY or MAP
// child, a STRUCT, TUPLE or UNION member), has the given id.
func (t Type) Contains(id ID) bool {
	if t.ID == id {
		return true
	}
	if t.Info == nil {
		return false
	}
	if t.Info.Child != nil && t.Info.Child.Contains(id) {
		return true
	}
	for _, c := range t.Info.Children {
		if c.Type.Contains(id) {
			return true
		}
	}
	return false
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
	case IDBigint, IDTime, IDTimeNs, IDTimestamp, IDTimestampSec, IDTimestampNs, IDTimestampMs, IDTimeTz,
		IDTimestampTz, IDTimestampTzNs:
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
	case IDUnion, IDVariant, IDStruct, IDTuple:
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
	_, err := FixedSize(p)
	return err == nil
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

// maxDepth bounds type nesting on decode so a hostile payload cannot exhaust
// the goroutine stack. Real schemas nest a handful of levels.
const maxDepth = 256

// Encode writes a LogicalType to w the way DuckDB 2.0's LogicalType::Serialize
// does at StorageVersion::V2_0_0: field 100 id, then field 101 nullable type
// info only when Info is non-nil. Properties DuckDB writes "with default" are
// omitted when they hold their default value, so the bytes match DuckDB's.
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

// Decode reads a LogicalType from r. Like DuckDB's reader it rejects type
// info that does not belong to the type id and DECIMAL widths/scales DuckDB
// considers invalid.
func Decode(r *codec.Reader) (Type, error) {
	return decodeType(r, 0)
}

func decodeType(r *codec.Reader, depth int) (Type, error) {
	if depth > maxDepth {
		return Type{}, fmt.Errorf("logicaltype: type nesting exceeds %d levels", maxDepth)
	}
	var out Type
	err := r.ReadObject(func(r *codec.Reader) error {
		var id uint64
		if err := r.ReadRequiredField(100, func(r *codec.Reader) (err error) {
			id, err = r.ReadULEB128()
			return err
		}); err != nil {
			return err
		}
		if id > math.MaxUint8 {
			return fmt.Errorf("logicaltype: type id %d out of range", id)
		}
		out.ID = ID(id)
		_, err := r.ReadOptionalField(101, func(r *codec.Reader) error {
			present, err := r.ReadBool()
			if err != nil || !present {
				return err
			}
			out.Info, err = decodeExtra(r, depth)
			return err
		})
		return err
	})
	if err == nil {
		err = validate(out)
	}
	if err != nil {
		return Type{}, err
	}
	return out, nil
}

// validate mirrors the checks in DuckDB 2.0's LogicalType::Deserialize.
func validate(t Type) error {
	if t.Info == nil {
		return nil
	}
	kind := t.Info.Kind
	if kind == ExtraInvalid || kind == ExtraGeneric {
		return nil
	}
	if t.ID == IDAggregateState {
		// DuckDB converts LEGACY_AGGREGATE_STATE to BLOB before checking.
		return nil
	}
	if want, ok := infoKindFor(t.ID); !ok || want != kind {
		return fmt.Errorf("logicaltype: type info kind %d does not match type %s", kind, t.Name())
	}
	if kind == ExtraDecimal && (t.Info.Width < 1 || t.Info.Width > MaxDecimalWidth || t.Info.Scale > t.Info.Width) {
		return fmt.Errorf("logicaltype: invalid DECIMAL(%d, %d)", t.Info.Width, t.Info.Scale)
	}
	return nil
}

// MaxDecimalWidth is DuckDB's Decimal::MAX_WIDTH_DECIMAL.
const MaxDecimalWidth = 38

// infoKindFor returns the only non-generic type info kind a type id accepts
// (DuckDB's LogicalType::HasParameters).
func infoKindFor(id ID) (ExtraInfoType, bool) {
	switch id {
	case IDList, IDMap:
		return ExtraList, true
	case IDStruct, IDTuple, IDUnion, IDVariant:
		return ExtraStruct, true
	case IDArray:
		return ExtraArray, true
	case IDDecimal:
		return ExtraDecimal, true
	case IDEnum:
		return ExtraEnum, true
	case IDVarchar:
		return ExtraString, true
	case IDAny:
		return ExtraAny, true
	case IDIntegerLiteral:
		return ExtraIntegerLiteral, true
	case IDTemplate:
		return ExtraTemplate, true
	case IDGeometry:
		return ExtraGeo, true
	case IDUnbound:
		return ExtraUnbound, true
	}
	return 0, false
}

// encodeExtra writes a LogicalTypeInfo object following DuckDB 2.0's
// generated serialize_types.cpp (R = always written, D = written only when
// not the default).
func encodeExtra(w *codec.Writer, info *ExtraInfo) {
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(100, func(w *codec.Writer) { w.WriteULEB128(uint64(info.Kind)) }) // R
		w.WriteStringWithDefault(101, info.Alias)                                      // D
		if info.ExtensionInfo != nil {                                                 // D (nullable)
			w.WriteField(103, func(w *codec.Writer) {
				w.WriteBool(true)
				w.WriteBytes(info.ExtensionInfo)
			})
		}
		switch info.Kind {
		case ExtraInvalid, ExtraGeneric:
		case ExtraDecimal:
			w.WriteUintWithDefault(200, uint64(info.Width)) // D
			w.WriteUintWithDefault(201, uint64(info.Scale)) // D
		case ExtraString:
			w.WriteStringWithDefault(200, info.Collation) // D
		case ExtraList:
			if info.Child != nil { // R in DuckDB; a nil child cannot be represented
				w.WriteField(200, func(w *codec.Writer) { Encode(w, *info.Child) })
			}
		case ExtraStruct:
			if len(info.Children) > 0 { // D
				w.WriteField(200, func(w *codec.Writer) { encodeChildren(w, info.Children) })
			}
		case ExtraEnum:
			w.WriteField(200, func(w *codec.Writer) { w.WriteULEB128(uint64(len(info.EnumValues))) }) // R
			w.WriteField(201, func(w *codec.Writer) {                                                 // R
				w.WriteULEB128(uint64(len(info.EnumValues)))
				for _, v := range info.EnumValues {
					w.WriteString(v)
				}
			})
		case ExtraAggregateState:
			// LEGACY_AGGREGATE_STATE_TYPE_INFO: all of its properties were deleted in 2.0.
		case ExtraArray:
			if info.Child != nil { // R
				w.WriteField(200, func(w *codec.Writer) { Encode(w, *info.Child) })
			}
			w.WriteUintWithDefault(201, uint64(info.ArraySize)) // D
		case ExtraAny:
			if info.TargetType != nil { // R
				w.WriteField(200, func(w *codec.Writer) { Encode(w, *info.TargetType) })
			}
			w.WriteUintWithDefault(201, info.CastScore) // D
		case ExtraTemplate:
			w.WriteStringWithDefault(200, info.TemplateName) // D
		case ExtraGeo:
			w.WriteField(200, func(w *codec.Writer) { // R: CoordinateReferenceSystem object
				w.WriteObject(func(w *codec.Writer) {
					w.WriteStringWithDefault(100, info.CRSDefinition) // D
				})
			})
		case ExtraUnbound:
			w.WriteStringWithDefault(200, info.UnboundName)
			w.WriteStringWithDefault(201, info.UnboundCatalog)
			w.WriteStringWithDefault(202, info.UnboundSchema)
		}
	})
}

func decodeExtra(r *codec.Reader, depth int) (*ExtraInfo, error) {
	info := &ExtraInfo{}
	// typeField reads a LogicalType property into *dst.
	typeField := func(dst **Type) func(*codec.Reader) error {
		return func(r *codec.Reader) error {
			t, err := decodeType(r, depth+1)
			*dst = &t
			return err
		}
	}
	err := r.ReadObject(func(r *codec.Reader) error {
		var kind uint64
		if err := r.ReadRequiredField(100, func(r *codec.Reader) (err error) {
			kind, err = r.ReadULEB128()
			return err
		}); err != nil {
			return err
		}
		if kind > math.MaxUint8 {
			return fmt.Errorf("logicaltype: type info kind %d out of range", kind)
		}
		info.Kind = ExtraInfoType(kind)
		if err := r.ReadStringWithDefault(101, &info.Alias); err != nil {
			return err
		}
		// 102 (modifiers) is a deleted property holding Values; DuckDB 2.0
		// never writes it.
		if id, err := r.PeekFieldID(); err == nil && id == 102 {
			return fmt.Errorf("logicaltype: legacy type modifiers: %w", ErrUnsupported)
		}
		if _, err := r.ReadOptionalField(103, func(r *codec.Reader) error {
			present, err := r.ReadBool()
			if err != nil || !present {
				return err
			}
			info.ExtensionInfo, err = readExtensionInfo(r, depth)
			return err
		}); err != nil {
			return err
		}

		switch info.Kind {
		case ExtraInvalid, ExtraGeneric:
			return nil
		case ExtraDecimal:
			var width, scale uint64
			if err := r.ReadUintWithDefault(200, &width); err != nil {
				return err
			}
			if err := r.ReadUintWithDefault(201, &scale); err != nil {
				return err
			}
			if width > math.MaxUint8 || scale > math.MaxUint8 {
				return fmt.Errorf("logicaltype: DECIMAL(%d, %d) out of range", width, scale)
			}
			info.Width, info.Scale = uint8(width), uint8(scale)
			return nil
		case ExtraString:
			return r.ReadStringWithDefault(200, &info.Collation)
		case ExtraList:
			return r.ReadRequiredField(200, typeField(&info.Child))
		case ExtraStruct:
			_, err := r.ReadOptionalField(200, func(r *codec.Reader) (err error) {
				info.Children, err = decodeChildren(r, depth)
				return err
			})
			return err
		case ExtraEnum:
			var count uint64
			if err := r.ReadRequiredField(200, func(r *codec.Reader) (err error) {
				count, err = r.ReadULEB128()
				return err
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(201, func(r *codec.Reader) error {
				n, err := r.ReadCount()
				if err != nil {
					return err
				}
				if uint64(n) != count {
					return fmt.Errorf("logicaltype: enum declares %d values but lists %d", count, n)
				}
				info.EnumValues = make([]string, 0, min(n, codec.MaxPrealloc))
				for range n {
					s, err := r.ReadString()
					if err != nil {
						return err
					}
					info.EnumValues = append(info.EnumValues, s)
				}
				return nil
			})
		case ExtraAggregateState:
			// Deleted properties in 2.0; read them when an older writer sent them.
			if err := r.ReadStringWithDefault(200, &info.FunctionName); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(201, typeField(&info.ReturnType)); err != nil {
				return err
			}
			_, err := r.ReadOptionalField(202, func(r *codec.Reader) error {
				n, err := r.ReadCount()
				if err != nil {
					return err
				}
				for range n {
					t, err := decodeType(r, depth+1)
					if err != nil {
						return err
					}
					info.BoundArgumentTypes = append(info.BoundArgumentTypes, t)
				}
				return nil
			})
			return err
		case ExtraArray:
			if err := r.ReadRequiredField(200, typeField(&info.Child)); err != nil {
				return err
			}
			var size uint64
			if err := r.ReadUintWithDefault(201, &size); err != nil {
				return err
			}
			if size > math.MaxUint32 {
				return fmt.Errorf("logicaltype: ARRAY size %d out of range", size)
			}
			info.ArraySize = uint32(size)
			return nil
		case ExtraAny:
			if err := r.ReadRequiredField(200, typeField(&info.TargetType)); err != nil {
				return err
			}
			return r.ReadUintWithDefault(201, &info.CastScore)
		case ExtraIntegerLiteral:
			if id, err := r.PeekFieldID(); err == nil && id == 200 {
				return fmt.Errorf("logicaltype: INTEGER_LITERAL with bound value: %w", ErrUnsupported)
			}
			return nil
		case ExtraTemplate:
			return r.ReadStringWithDefault(200, &info.TemplateName)
		case ExtraGeo:
			return r.ReadRequiredField(200, func(r *codec.Reader) error {
				return r.ReadObject(func(r *codec.Reader) error {
					return r.ReadStringWithDefault(100, &info.CRSDefinition)
				})
			})
		case ExtraUnbound:
			if id, err := r.PeekFieldID(); err == nil && id == 204 {
				return fmt.Errorf("logicaltype: UNBOUND with parsed expression: %w", ErrUnsupported)
			}
			if err := r.ReadStringWithDefault(200, &info.UnboundName); err != nil {
				return err
			}
			if err := r.ReadStringWithDefault(201, &info.UnboundCatalog); err != nil {
				return err
			}
			return r.ReadStringWithDefault(202, &info.UnboundSchema)
		}
		return fmt.Errorf("logicaltype: unknown ExtraInfoType %d", info.Kind)
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// encodeChildren writes a child_list_t: a list of pair objects
// {0: name, 1: LogicalType}. Both pair members are always written.
func encodeChildren(w *codec.Writer, children []ChildType) {
	w.WriteULEB128(uint64(len(children)))
	for _, child := range children {
		w.WriteObject(func(w *codec.Writer) {
			w.WriteField(0, func(w *codec.Writer) { w.WriteString(child.Name) })
			w.WriteField(1, func(w *codec.Writer) { Encode(w, child.Type) })
		})
	}
}

func decodeChildren(r *codec.Reader, depth int) ([]ChildType, error) {
	n, err := r.ReadCount()
	if err != nil {
		return nil, err
	}
	out := make([]ChildType, 0, min(n, codec.MaxPrealloc))
	for range n {
		var c ChildType
		if err := r.ReadObject(func(r *codec.Reader) error {
			if err := r.ReadRequiredField(0, func(r *codec.Reader) (err error) {
				c.Name, err = r.ReadString()
				return err
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(1, func(r *codec.Reader) (err error) {
				c.Type, err = decodeType(r, depth+1)
				return err
			})
		}); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Name returns DuckDB's display name for the type id (EnumUtil::ToString of
// LogicalTypeId), e.g. "TIMESTAMP WITH TIME ZONE" or "TIMESTAMPTZ_NS".
// Unknown ids render as "ID(n)".
func (t Type) Name() string {
	if name, ok := idNames[t.ID]; ok {
		return name
	}
	return fmt.Sprintf("ID(%d)", t.ID)
}

// idNames mirrors GetLogicalTypeIdValues() in DuckDB v2.0.0 enum_util.cpp.
var idNames = map[ID]string{
	IDInvalid: "INVALID", IDSQLNull: "NULL", IDUnknown: "UNKNOWN", IDAny: "ANY",
	IDUnbound: "UNBOUND", IDTemplate: "TEMPLATE", IDType: "TYPE",
	IDBoolean: "BOOLEAN", IDTinyint: "TINYINT", IDSmallint: "SMALLINT", IDInteger: "INTEGER", IDBigint: "BIGINT",
	IDDate: "DATE", IDTime: "TIME", IDTimestampSec: "TIMESTAMP_S", IDTimestampMs: "TIMESTAMP_MS",
	IDTimestamp: "TIMESTAMP", IDTimestampNs: "TIMESTAMP_NS", IDDecimal: "DECIMAL",
	IDFloat: "FLOAT", IDDouble: "DOUBLE", IDChar: "CHAR", IDVarchar: "VARCHAR", IDBlob: "BLOB",
	IDInterval: "INTERVAL", IDUTinyint: "UTINYINT", IDUSmallint: "USMALLINT", IDUInteger: "UINTEGER",
	IDUBigint: "UBIGINT", IDTimestampTz: "TIMESTAMP WITH TIME ZONE", IDTimestampTzNs: "TIMESTAMPTZ_NS",
	IDTimeTz: "TIME WITH TIME ZONE", IDTimeNs: "TIME_NS", IDBit: "BIT",
	IDStringLiteral: "STRING_LITERAL", IDIntegerLiteral: "INTEGER_LITERAL", IDBignum: "BIGNUM",
	IDUHugeint: "UHUGEINT", IDHugeint: "HUGEINT", IDPointer: "POINTER", IDValidity: "VALIDITY",
	IDUUID: "UUID", IDGeometry: "GEOMETRY",
	IDStruct: "STRUCT", IDList: "LIST", IDMap: "MAP", IDTable: "TABLE", IDEnum: "ENUM",
	IDAggregateState: "LEGACY_AGGREGATE_STATE", IDLambda: "LAMBDA", IDUnion: "UNION",
	IDArray: "ARRAY", IDVariant: "VARIANT", IDTuple: "TUPLE",
}
