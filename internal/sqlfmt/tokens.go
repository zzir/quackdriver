package sqlfmt

// TokenKind classifies the tokens Tokenize reports.
type TokenKind uint8

// Token kinds. TokenEOF is the zero Token's kind; Tokenize never produces it,
// so a cursor can return the zero Token at the end of the input.
const (
	TokenEOF       TokenKind = iota
	TokenWord                // an unquoted keyword or identifier
	TokenLParen              // (
	TokenRParen              // )
	TokenComma               // ,
	TokenSemicolon           // ;
	TokenOther               // a literal, quoted identifier, number, operator or placeholder
)

// Token is one token of Tokenize. Text is the word of a TokenWord as written
// (callers fold case), "" for every other kind.
type Token struct {
	Kind TokenKind
	Text string
}

// Tokenize splits query into the tokens that matter for recognizing the shape
// of a statement: words, parentheses, commas and semicolons outside string
// literals, quoted identifiers and comments. Literals, quoted identifiers,
// numbers, operators and placeholders are reported as TokenOther; comments
// are dropped. The regions come from the scanner FormatQuery uses, so both
// agree on DuckDB's lexical rules (quoting forms, nested comments, dollar
// strings). A query the scanner rejects (a malformed numbered placeholder)
// yields no tokens.
func Tokenize(query string) []Token {
	spans, err := scanSQL(query)
	if err != nil {
		return nil
	}
	var out []Token
	pos := 0
	for _, s := range spans {
		out = tokenizePlain(out, query[pos:s.start])
		if s.kind != spanLineComment && s.kind != spanBlockComment {
			out = append(out, Token{Kind: TokenOther})
		}
		pos = s.end
	}
	return tokenizePlain(out, query[pos:])
}

// tokenizePlain tokenizes text between scanned regions, which holds no
// literal, quoted identifier or comment: whitespace, keywords, numbers,
// operators and punctuation.
func tokenizePlain(out []Token, s string) []Token {
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isSpace(c):
			i++
		case c == '(':
			out = append(out, Token{Kind: TokenLParen})
			i++
		case c == ')':
			out = append(out, Token{Kind: TokenRParen})
			i++
		case c == ',':
			out = append(out, Token{Kind: TokenComma})
			i++
		case c == ';':
			out = append(out, Token{Kind: TokenSemicolon})
			i++
		case isInitialNumber(c):
			out = append(out, Token{Kind: TokenOther})
			i = scanNumber(s, i)
		case isSingleByteOperator(c) || isOperatorChar(c):
			out = append(out, Token{Kind: TokenOther})
			i++
		default:
			end := scanKeyword(s, i)
			out = append(out, Token{Kind: TokenWord, Text: s[i:end]})
			i = end
		}
	}
	return out
}
