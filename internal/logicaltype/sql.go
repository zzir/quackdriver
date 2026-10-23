package logicaltype

import (
	"strconv"
	"strings"
)

// SQL returns the type as DuckDB SQL text that DuckDB 2.0 parses back to an
// equal LogicalType: same id and the same type info (STRUCT/UNION member
// names, ENUM labels, DECIMAL width/scale, ARRAY size, MAP key/value).
// The spelling follows DuckDB's own LogicalType::ToString, except that
// STRUCT and UNION member names are always double-quoted (`"a""b"`) so any
// name round-trips; ENUM labels are single-quoted with embedded single
// quotes doubled.
//
// Examples: INTEGER, DECIMAL(18,4), VARCHAR[], INTEGER[3],
// STRUCT("a" INTEGER, "b" VARCHAR), TUPLE(INTEGER, VARCHAR),
// MAP(VARCHAR, INTEGER), UNION("num" INTEGER, "err" VARCHAR), ENUM('a', 'b'),
// TIMESTAMP WITH TIME ZONE, TIMESTAMPTZ_NS, TIME WITH TIME ZONE.
//
// Limits, all shared with DuckDB's ToString: a type carrying an alias (for
// example JSON) renders as the alias and needs that type to exist on the
// server; a VARCHAR collation is not rendered; CHAR renders as "CHAR", which
// DuckDB parses as VARCHAR; ids that have no SQL spelling (ANY, POINTER, ...)
// render as their Name().
func (t Type) SQL() string {
	var b strings.Builder
	writeSQL(&b, t, 0)
	return b.String()
}

func writeSQL(b *strings.Builder, t Type, depth int) {
	if depth > maxDepth {
		// Unreachable for decoded types (Decode enforces the same bound);
		// a hand-built cyclic or absurdly deep type must not recurse forever.
		b.WriteString(t.Name())
		return
	}
	info := t.Info
	if info != nil && info.Alias != "" {
		writeTypeName(b, info.Alias)
		return
	}
	switch t.ID {
	case IDStruct, IDTuple:
		name := t.Name()
		if info == nil || info.Kind != ExtraStruct || len(info.Children) == 0 {
			b.WriteString(name)
			return
		}
		b.WriteString(name)
		b.WriteByte('(')
		for i, child := range info.Children {
			if i > 0 {
				b.WriteString(", ")
			}
			if t.ID == IDStruct {
				b.WriteString(QuoteIdentifier(child.Name))
				b.WriteByte(' ')
			}
			writeSQL(b, child.Type, depth+1)
		}
		b.WriteByte(')')
	case IDUnion:
		// Children[0] is the hidden UTINYINT tag; the members follow it.
		if info == nil || info.Kind != ExtraStruct || len(info.Children) < 2 {
			b.WriteString("UNION")
			return
		}
		b.WriteString("UNION(")
		for i, member := range info.Children[1:] {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(QuoteIdentifier(member.Name))
			b.WriteByte(' ')
			writeSQL(b, member.Type, depth+1)
		}
		b.WriteByte(')')
	case IDList:
		if info == nil || info.Kind != ExtraList || info.Child == nil {
			b.WriteString("LIST")
			return
		}
		writeSQL(b, *info.Child, depth+1)
		b.WriteString("[]")
	case IDArray:
		if info == nil || info.Kind != ExtraArray || info.Child == nil {
			b.WriteString("ARRAY")
			return
		}
		writeSQL(b, *info.Child, depth+1)
		if info.ArraySize == 0 {
			b.WriteString("[ANY]")
		} else {
			b.WriteByte('[')
			b.WriteString(strconv.FormatUint(uint64(info.ArraySize), 10))
			b.WriteByte(']')
		}
	case IDMap:
		// MAP is LIST(STRUCT(key K, value V)).
		if info == nil || info.Kind != ExtraList || info.Child == nil || info.Child.Info == nil ||
			len(info.Child.Info.Children) != 2 {
			b.WriteString("MAP")
			return
		}
		entry := info.Child.Info.Children
		b.WriteString("MAP(")
		writeSQL(b, entry[0].Type, depth+1)
		b.WriteString(", ")
		writeSQL(b, entry[1].Type, depth+1)
		b.WriteByte(')')
	case IDDecimal:
		if info == nil || info.Kind != ExtraDecimal || info.Width == 0 {
			b.WriteString("DECIMAL")
			return
		}
		b.WriteString("DECIMAL(")
		b.WriteString(strconv.Itoa(int(info.Width)))
		b.WriteByte(',')
		b.WriteString(strconv.Itoa(int(info.Scale)))
		b.WriteByte(')')
	case IDEnum:
		b.WriteString("ENUM(")
		if info != nil {
			for i, label := range info.EnumValues {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(QuoteString(label))
			}
		}
		b.WriteByte(')')
	case IDSQLNull:
		b.WriteString(`"NULL"`)
	case IDGeometry:
		if info == nil || info.Kind != ExtraGeo || info.CRSDefinition == "" {
			b.WriteString("GEOMETRY")
			return
		}
		b.WriteString("GEOMETRY(")
		b.WriteString(QuoteString(info.CRSDefinition))
		b.WriteByte(')')
	default:
		b.WriteString(t.Name())
	}
}

// writeTypeName writes a type name (an alias such as JSON) bare when it is a
// plain identifier, otherwise double-quoted.
func writeTypeName(b *strings.Builder, name string) {
	if isPlainIdentifier(name) {
		b.WriteString(name)
		return
	}
	b.WriteString(QuoteIdentifier(name))
}

func isPlainIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// QuoteIdentifier double-quotes s as a SQL identifier, doubling embedded
// double quotes: a"b becomes "a""b".
func QuoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// QuoteString single-quotes s as a standard SQL string literal, doubling
// embedded single quotes, so that any string round-trips. It is the one
// string-literal quoter of the driver (sqlfmt renders string arguments with
// it): DuckDB's standard strings give no meaning to backslashes, and every
// other byte, NUL and invalid UTF-8 included, is copied verbatim.
func QuoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
