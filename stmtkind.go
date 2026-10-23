package quackdriver

import (
	"strings"

	"github.com/zzir/quackdriver/internal/sqlfmt"
)

// stmtInfo is what the driver needs to know about a statement, judged by
// its leading keyword (for a WITH statement, the keyword of the main
// statement after the CTE list, when it can be found).
type stmtInfo struct {
	// readOnly is set for statements whose early termination cannot lose a
	// write: Rows.Close cancels them instead of draining the rest.
	readOnly bool
	// dml is set for statements whose single "Count" result is a row count
	// (INSERT, UPDATE, DELETE, MERGE, COPY). For INSERT, UPDATE, DELETE and
	// MERGE with RETURNING, each result row is a changed row: returning is
	// set for those statement kinds, hasReturning when the statement has a
	// top-level RETURNING clause.
	dml          bool
	returning    bool
	hasReturning bool
	// create is set for CREATE statements: CREATE TABLE ... AS reports the
	// rows it wrote as a "Count" row (DuckDB's CHANGED_ROWS result); other
	// CREATE statements return no row.
	create bool
}

var readOnlyKeywords = map[string]bool{
	"SELECT": true, "FROM": true, "VALUES": true, "TABLE": true, "SHOW": true,
	"DESCRIBE": true, "SUMMARIZE": true, "EXPLAIN": true, "PRAGMA": true,
}

var dmlKeywords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "COPY": true,
}

var returningKeywords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true,
}

// classifyStatement inspects the leading keyword of sql, skipping whitespace,
// comments and opening parentheses. The tokens come from sqlfmt.Tokenize, the
// scanner FormatQuery uses, so literals and comments are delimited exactly as
// the server sees them. Unknown or unparsable text is neither read-only nor
// DML, which makes the driver take the conservative path (drain instead of
// cancel, RowsAffected 0).
func classifyStatement(sql string) stmtInfo {
	return classifyTokens(sqlfmt.Tokenize(sql))
}

func classifyTokens(toks []sqlfmt.Token) stmtInfo {
	sc := &tokenCursor{toks: toks}
	kw := sc.firstWord()
	switch kw {
	case "EXPLAIN":
		// EXPLAIN ANALYZE runs the statement it explains.
		if next := sc.firstWord(); next == "ANALYZE" || next == "ANALYSE" {
			return stmtInfo{readOnly: classifyTokens(toks[sc.i:]).readOnly}
		}
		return stmtInfo{readOnly: true}
	case "CREATE":
		return stmtInfo{create: true}
	case "WITH":
		kw = sc.withMainKeyword()
	}
	info := stmtInfo{readOnly: readOnlyKeywords[kw], dml: dmlKeywords[kw], returning: returningKeywords[kw]}
	info.hasReturning = info.returning && sc.hasTopLevelWord("RETURNING")
	return info
}

// tokenCursor walks the tokens of a statement.
type tokenCursor struct {
	toks []sqlfmt.Token
	i    int
}

// next returns the next token; the zero Token (TokenEOF) at the end.
func (sc *tokenCursor) next() sqlfmt.Token {
	if sc.i >= len(sc.toks) {
		return sqlfmt.Token{}
	}
	t := sc.toks[sc.i]
	sc.i++
	return t
}

// firstWord returns the next word, uppercased, skipping opening
// parentheses; "" if anything else comes first.
func (sc *tokenCursor) firstWord() string {
	for {
		t := sc.next()
		switch t.Kind {
		case sqlfmt.TokenLParen:
			continue
		case sqlfmt.TokenWord:
			return strings.ToUpper(t.Text)
		default:
			return ""
		}
	}
}

// withMainKeyword skips the CTE list of a WITH statement (the cursor is
// positioned after WITH) and returns the main statement's keyword. A CTE is
// `name [(columns)] [USING KEY (...)] AS [[NOT] MATERIALIZED] (body)`; the main
// statement starts at the first word that follows a closed top-level
// parenthesis and is neither AS nor USING.
func (sc *tokenCursor) withMainKeyword() string {
	depth := 0
	afterClose := false
	for {
		t := sc.next()
		switch t.Kind {
		case sqlfmt.TokenEOF:
			return ""
		case sqlfmt.TokenLParen:
			if afterClose && depth == 0 {
				// the main statement itself is parenthesized
				return sc.firstWord()
			}
			depth++
		case sqlfmt.TokenRParen:
			depth--
			if depth < 0 {
				return ""
			}
			afterClose = depth == 0
		case sqlfmt.TokenComma:
			if depth == 0 {
				afterClose = false
			}
		case sqlfmt.TokenWord:
			if depth != 0 {
				continue
			}
			w := strings.ToUpper(t.Text)
			if afterClose {
				if w == "AS" || w == "USING" {
					afterClose = false
					continue
				}
				return w
			}
		default:
			if depth == 0 && afterClose {
				return ""
			}
		}
	}
}

// hasTopLevelWord reports whether word (uppercase) follows, outside
// parentheses, before the end of the current statement (a top-level ';').
func (sc *tokenCursor) hasTopLevelWord(word string) bool {
	depth := 0
	for {
		t := sc.next()
		switch t.Kind {
		case sqlfmt.TokenEOF:
			return false
		case sqlfmt.TokenLParen:
			depth++
		case sqlfmt.TokenRParen:
			depth--
		case sqlfmt.TokenWord:
			if depth <= 0 && strings.EqualFold(t.Text, word) {
				return true
			}
		case sqlfmt.TokenSemicolon:
			if depth <= 0 {
				return false
			}
		}
	}
}
