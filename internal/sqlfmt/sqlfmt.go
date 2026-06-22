// Package sqlfmt substitutes parameters into a SQL query as DuckDB literals.
//
// Quack v1.5.3 does not expose a server-side parameter-bind path, so this
// package is the *only* injection-defense surface. All string-typed arguments
// are quoted with single quotes; embedded single quotes are doubled.
package sqlfmt

import (
	"database/sql/driver"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// FormatQuery rewrites `query` by substituting each `?` placeholder with the
// corresponding NamedValue formatted as a SQL literal. Named parameters are
// not supported (Quack uses positional `?` like SQLite).
func FormatQuery(query string, args []driver.NamedValue) (string, error) {
	for _, a := range args {
		if a.Name != "" {
			return "", fmt.Errorf("sqlfmt: named parameters are not supported (parameter %q)", a.Name)
		}
	}

	var (
		out   strings.Builder
		i     int
		next  int
		quote byte
	)
	out.Grow(len(query))
	for i = 0; i < len(query); i++ {
		c := query[i]
		// Track single/double-quoted string literals so embedded `?` inside
		// them is treated as data, not a placeholder.
		if quote != 0 {
			if c == quote {
				// Doubled quote escape ('' or "") stays inside the literal.
				if i+1 < len(query) && query[i+1] == quote {
					out.WriteByte(c)
					out.WriteByte(c)
					i++
					continue
				}
				quote = 0
			}
			out.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			out.WriteByte(c)
		case '-':
			// Line comment: skip until newline.
			if i+1 < len(query) && query[i+1] == '-' {
				end := strings.IndexByte(query[i:], '\n')
				if end < 0 {
					out.WriteString(query[i:])
					i = len(query)
				} else {
					out.WriteString(query[i : i+end])
					i += end - 1
				}
				continue
			}
			out.WriteByte(c)
		case '/':
			// Block comment: skip until */.
			if i+1 < len(query) && query[i+1] == '*' {
				end := strings.Index(query[i:], "*/")
				if end < 0 {
					out.WriteString(query[i:])
					i = len(query)
				} else {
					out.WriteString(query[i : i+end+2])
					i += end + 1
				}
				continue
			}
			out.WriteByte(c)
		case '?':
			if next >= len(args) {
				return "", fmt.Errorf("sqlfmt: not enough arguments: query has more ? than %d args", len(args))
			}
			lit, err := FormatValue(args[next].Value)
			if err != nil {
				return "", fmt.Errorf("sqlfmt: argument %d: %w", next+1, err)
			}
			out.WriteString(lit)
			next++
		default:
			out.WriteByte(c)
		}
	}
	if next != len(args) {
		return "", fmt.Errorf("sqlfmt: too many arguments: query has %d placeholders, got %d args", next, len(args))
	}
	return out.String(), nil
}

// FormatValue returns the SQL literal form of v.
func FormatValue(v driver.Value) (string, error) {
	if v == nil {
		return "NULL", nil
	}
	switch x := v.(type) {
	case bool:
		if x {
			return "TRUE", nil
		}
		return "FALSE", nil
	case int:
		return strconv.FormatInt(int64(x), 10), nil
	case int8:
		return strconv.FormatInt(int64(x), 10), nil
	case int16:
		return strconv.FormatInt(int64(x), 10), nil
	case int32:
		return strconv.FormatInt(int64(x), 10), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return formatFloat(float64(x)), nil
	case float64:
		return formatFloat(x), nil
	case string:
		return quoteString(x), nil
	case []byte:
		return formatBlob(x), nil
	case time.Time:
		return "TIMESTAMP " + quoteString(x.UTC().Format("2006-01-02 15:04:05.000000")), nil
	case *big.Int:
		if x == nil {
			return "NULL", nil
		}
		return x.String(), nil
	case big.Int:
		return x.String(), nil
	case *big.Rat:
		if x == nil {
			return "NULL", nil
		}
		return x.FloatString(38), nil
	}
	return "", fmt.Errorf("sqlfmt: cannot format value of type %T", v)
}

func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "'nan'::DOUBLE"
	case math.IsInf(f, 1):
		return "'infinity'::DOUBLE"
	case math.IsInf(f, -1):
		return "'-infinity'::DOUBLE"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func quoteString(s string) string {
	// Single-quote double-escape. Note: backslashes are NOT escaped — DuckDB
	// uses standard SQL string literals where backslash is a regular character
	// unless E'\n' style is used. We don't emit the E'' form.
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := range len(s) {
		c := s[i]
		if c == '\'' {
			b.WriteByte('\'')
			b.WriteByte('\'')
			continue
		}
		b.WriteByte(c)
	}
	b.WriteByte('\'')
	return b.String()
}

func formatBlob(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2+len("'\\x")+len("'::BLOB"))
	out = append(out, '\'', '\\', 'x')
	for _, c := range b {
		out = append(out, hex[c>>4], hex[c&0x0f])
	}
	out = append(out, '\'', ':', ':', 'B', 'L', 'O', 'B')
	return string(out)
}
