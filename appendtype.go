package quackdriver

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/zzir/quackdriver/internal/logicaltype"
)

// parseAppendType parses a DuckDB SQL type expression into a LogicalType.
//
// Supported grammar:
//
//	type        := primary suffix*
//	suffix      := '[' ']'              ;; LIST<primary>
//	             | '[' number ']'       ;; ARRAY<primary, N>
//	primary     := ident
//	             | ident '(' params ')'
//	params      := type-or-field (',' type-or-field)*
//	type-or-field := type | ident type | number
//
// Special-cased parameterized names:
//
//	DECIMAL(width, scale)
//	LIST(type)
//	STRUCT(field-name type, ...)
//	MAP(key-type, value-type)
//	UNION(tag type, ...)
func parseAppendType(s string) (logicaltype.Type, error) {
	tokens, err := tokenizeType(s)
	if err != nil {
		return logicaltype.Type{}, fmt.Errorf("tokenize %q: %w", s, err)
	}
	p := &typeParser{src: s, tokens: tokens}
	t, err := p.parseType()
	if err != nil {
		return logicaltype.Type{}, fmt.Errorf("parse %q: %w", s, err)
	}
	if !p.atEnd() {
		return logicaltype.Type{}, fmt.Errorf("parse %q: unexpected trailing token %q", s, p.peek().text)
	}
	return t, nil
}

type tokenKind int

const (
	tEOF tokenKind = iota
	tIdent
	tNumber
	tString // single-quoted SQL string literal; text is the unquoted contents
	tLParen
	tRParen
	tLBracket
	tRBracket
	tComma
)

type token struct {
	kind tokenKind
	text string
	pos  int
}

func tokenizeType(s string) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		if unicode.IsSpace(rune(c)) {
			i++
			continue
		}
		switch c {
		case '(':
			out = append(out, token{tLParen, "(", i})
			i++
			continue
		case ')':
			out = append(out, token{tRParen, ")", i})
			i++
			continue
		case '[':
			out = append(out, token{tLBracket, "[", i})
			i++
			continue
		case ']':
			out = append(out, token{tRBracket, "]", i})
			i++
			continue
		case ',':
			out = append(out, token{tComma, ",", i})
			i++
			continue
		case '\'':
			// SQL single-quoted string literal. Doubled '' escapes a single
			// quote inside the literal.
			start := i
			i++
			var sb []byte
			closed := false
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						sb = append(sb, '\'')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				sb = append(sb, s[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string literal at offset %d", start)
			}
			out = append(out, token{tString, string(sb), start})
			continue
		}
		if c >= '0' && c <= '9' {
			start := i
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			out = append(out, token{tNumber, s[start:i], start})
			continue
		}
		if isIdentStart(c) {
			start := i
			for i < len(s) && isIdentPart(s[i]) {
				i++
			}
			out = append(out, token{tIdent, s[start:i], start})
			continue
		}
		return nil, fmt.Errorf("unexpected character %q at offset %d", c, i)
	}
	out = append(out, token{tEOF, "", len(s)})
	return out, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

type typeParser struct {
	src    string
	tokens []token
	pos    int
}

func (p *typeParser) peek() token { return p.tokens[p.pos] }
func (p *typeParser) atEnd() bool { return p.peek().kind == tEOF }

func (p *typeParser) advance() token {
	t := p.tokens[p.pos]
	if t.kind != tEOF {
		p.pos++
	}
	return t
}

func (p *typeParser) expect(k tokenKind) (token, error) {
	t := p.advance()
	if t.kind != k {
		return t, fmt.Errorf("expected %s at offset %d, got %q", tokenKindName(k), t.pos, t.text)
	}
	return t, nil
}

func tokenKindName(k tokenKind) string {
	switch k {
	case tIdent:
		return "identifier"
	case tNumber:
		return "number"
	case tString:
		return "string literal"
	case tLParen:
		return "("
	case tRParen:
		return ")"
	case tLBracket:
		return "["
	case tRBracket:
		return "]"
	case tComma:
		return ","
	}
	return "EOF"
}

func (p *typeParser) parseType() (logicaltype.Type, error) {
	t, err := p.parsePrimary()
	if err != nil {
		return logicaltype.Type{}, err
	}
	for p.peek().kind == tLBracket {
		p.advance() // [
		switch p.peek().kind {
		case tRBracket:
			p.advance()
			t = makeList(t)
		case tNumber:
			n, err := strconv.Atoi(p.advance().text)
			if err != nil || n <= 0 {
				return logicaltype.Type{}, fmt.Errorf("invalid array size at offset %d", p.peek().pos)
			}
			if _, err := p.expect(tRBracket); err != nil {
				return logicaltype.Type{}, err
			}
			t = makeArray(t, uint32(n))
		default:
			return logicaltype.Type{}, fmt.Errorf("expected number or ']' at offset %d, got %q", p.peek().pos, p.peek().text)
		}
	}
	return t, nil
}

func (p *typeParser) parsePrimary() (logicaltype.Type, error) {
	tok, err := p.expect(tIdent)
	if err != nil {
		return logicaltype.Type{}, err
	}
	name := strings.ToUpper(tok.text)
	if p.peek().kind != tLParen {
		return simpleType(name)
	}
	p.advance() // (
	switch name {
	case "DECIMAL":
		w, err := p.expectNumber()
		if err != nil {
			return logicaltype.Type{}, err
		}
		if _, err := p.expect(tComma); err != nil {
			return logicaltype.Type{}, err
		}
		sc, err := p.expectNumber()
		if err != nil {
			return logicaltype.Type{}, err
		}
		if _, err := p.expect(tRParen); err != nil {
			return logicaltype.Type{}, err
		}
		if w < 1 || w > 38 {
			return logicaltype.Type{}, fmt.Errorf("DECIMAL width %d out of range (1-38)", w)
		}
		if sc < 0 || sc > w {
			return logicaltype.Type{}, fmt.Errorf("DECIMAL scale %d out of range (0..%d)", sc, w)
		}
		return logicaltype.Type{
			ID:   logicaltype.IDDecimal,
			Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraDecimal, Width: uint8(w), Scale: uint8(sc)},
		}, nil
	case "LIST":
		inner, err := p.parseType()
		if err != nil {
			return logicaltype.Type{}, err
		}
		if _, err := p.expect(tRParen); err != nil {
			return logicaltype.Type{}, err
		}
		return makeList(inner), nil
	case "STRUCT":
		children, err := p.parseStructFields()
		if err != nil {
			return logicaltype.Type{}, err
		}
		if _, err := p.expect(tRParen); err != nil {
			return logicaltype.Type{}, err
		}
		return logicaltype.Type{
			ID:   logicaltype.IDStruct,
			Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraStruct, Children: children},
		}, nil
	case "MAP":
		kt, err := p.parseType()
		if err != nil {
			return logicaltype.Type{}, err
		}
		if _, err := p.expect(tComma); err != nil {
			return logicaltype.Type{}, err
		}
		vt, err := p.parseType()
		if err != nil {
			return logicaltype.Type{}, err
		}
		if _, err := p.expect(tRParen); err != nil {
			return logicaltype.Type{}, err
		}
		// DuckDB internally represents MAP as LIST<STRUCT<key, value>>.
		entry := logicaltype.Type{
			ID: logicaltype.IDStruct,
			Info: &logicaltype.ExtraInfo{
				Kind: logicaltype.ExtraStruct,
				Children: []logicaltype.ChildType{
					{Name: "key", Type: kt},
					{Name: "value", Type: vt},
				},
			},
		}
		return logicaltype.Type{
			ID:   logicaltype.IDMap,
			Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraList, Child: &entry},
		}, nil
	case "ENUM":
		// ENUM('label1', 'label2', ...) — string literals are required by
		// DuckDB syntax. The dictionary's index width is determined later by
		// PhysicalType (UTINYINT / USMALLINT / UINTEGER depending on size).
		var values []string
		for {
			tok, err := p.expect(tString)
			if err != nil {
				return logicaltype.Type{}, err
			}
			values = append(values, tok.text)
			if p.peek().kind != tComma {
				break
			}
			p.advance()
		}
		if _, err := p.expect(tRParen); err != nil {
			return logicaltype.Type{}, err
		}
		if len(values) == 0 {
			return logicaltype.Type{}, fmt.Errorf("ENUM requires at least one label")
		}
		return logicaltype.Type{
			ID:   logicaltype.IDEnum,
			Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraEnum, EnumValues: values},
		}, nil
	case "UNION":
		variants, err := p.parseStructFields()
		if err != nil {
			return logicaltype.Type{}, err
		}
		if _, err := p.expect(tRParen); err != nil {
			return logicaltype.Type{}, err
		}
		// UNION shares the STRUCT physical layout but with a hidden tag field
		// at index 0 (unnamed, UTINYINT) — the wire decode against DuckDB
		// v1.5.3 returns {"": tag, <variant>: value, <other>: nil, ...}.
		children := make([]logicaltype.ChildType, 0, len(variants)+1)
		children = append(children, logicaltype.ChildType{Name: "", Type: logicaltype.Type{ID: logicaltype.IDUTinyint}})
		children = append(children, variants...)
		return logicaltype.Type{
			ID:   logicaltype.IDUnion,
			Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraStruct, Children: children},
		}, nil
	}
	return logicaltype.Type{}, fmt.Errorf("unknown parameterized type %s", name)
}

func (p *typeParser) parseStructFields() ([]logicaltype.ChildType, error) {
	var out []logicaltype.ChildType
	for {
		nameTok, err := p.expect(tIdent)
		if err != nil {
			return nil, err
		}
		ftype, err := p.parseType()
		if err != nil {
			return nil, err
		}
		out = append(out, logicaltype.ChildType{Name: nameTok.text, Type: ftype})
		if p.peek().kind != tComma {
			return out, nil
		}
		p.advance() // ,
	}
}

func (p *typeParser) expectNumber() (int, error) {
	tok, err := p.expect(tNumber)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(tok.text)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", tok.text)
	}
	return n, nil
}

func makeList(elem logicaltype.Type) logicaltype.Type {
	return logicaltype.Type{
		ID:   logicaltype.IDList,
		Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraList, Child: &elem},
	}
}

func makeArray(elem logicaltype.Type, size uint32) logicaltype.Type {
	return logicaltype.Type{
		ID:   logicaltype.IDArray,
		Info: &logicaltype.ExtraInfo{Kind: logicaltype.ExtraArray, Child: &elem, ArraySize: size},
	}
}

func simpleType(upper string) (logicaltype.Type, error) {
	switch upper {
	case "BOOLEAN", "BOOL":
		return logicaltype.Type{ID: logicaltype.IDBoolean}, nil
	case "TINYINT":
		return logicaltype.Type{ID: logicaltype.IDTinyint}, nil
	case "SMALLINT":
		return logicaltype.Type{ID: logicaltype.IDSmallint}, nil
	case "INTEGER", "INT":
		return logicaltype.Type{ID: logicaltype.IDInteger}, nil
	case "BIGINT":
		return logicaltype.Type{ID: logicaltype.IDBigint}, nil
	case "UTINYINT":
		return logicaltype.Type{ID: logicaltype.IDUTinyint}, nil
	case "USMALLINT":
		return logicaltype.Type{ID: logicaltype.IDUSmallint}, nil
	case "UINTEGER":
		return logicaltype.Type{ID: logicaltype.IDUInteger}, nil
	case "UBIGINT":
		return logicaltype.Type{ID: logicaltype.IDUBigint}, nil
	case "HUGEINT":
		return logicaltype.Type{ID: logicaltype.IDHugeint}, nil
	case "UHUGEINT":
		return logicaltype.Type{ID: logicaltype.IDUHugeint}, nil
	case "FLOAT", "REAL":
		return logicaltype.Type{ID: logicaltype.IDFloat}, nil
	case "DOUBLE":
		return logicaltype.Type{ID: logicaltype.IDDouble}, nil
	case "VARCHAR", "TEXT", "STRING":
		return logicaltype.Type{ID: logicaltype.IDVarchar}, nil
	case "BLOB", "BYTEA":
		return logicaltype.Type{ID: logicaltype.IDBlob}, nil
	case "UUID":
		return logicaltype.Type{ID: logicaltype.IDUUID}, nil
	case "DATE":
		return logicaltype.Type{ID: logicaltype.IDDate}, nil
	case "TIMESTAMP":
		return logicaltype.Type{ID: logicaltype.IDTimestamp}, nil
	case "TIMESTAMP_S":
		return logicaltype.Type{ID: logicaltype.IDTimestampSec}, nil
	case "TIMESTAMP_MS":
		return logicaltype.Type{ID: logicaltype.IDTimestampMs}, nil
	case "TIMESTAMP_NS":
		return logicaltype.Type{ID: logicaltype.IDTimestampNs}, nil
	case "INTERVAL":
		return logicaltype.Type{ID: logicaltype.IDInterval}, nil
	}
	return logicaltype.Type{}, fmt.Errorf("unknown type %s", upper)
}
