package sqlfmt

import (
	"reflect"
	"testing"
)

func TestTokenize(t *testing.T) {
	w := func(s string) Token { return Token{Kind: TokenWord, Text: s} }
	o := Token{Kind: TokenOther}
	lp, rp, comma, semi := Token{Kind: TokenLParen}, Token{Kind: TokenRParen}, Token{Kind: TokenComma}, Token{Kind: TokenSemicolon}
	cases := []struct {
		sql  string
		want []Token
	}{
		{"", nil},
		{"  -- c\n /* a /* b */ c */ ", nil},
		{"SELECT 1", []Token{w("SELECT"), o}},
		{"select a.b, 'it''s', \"q)\", `b(`", []Token{w("select"), w("a"), o, w("b"), comma, o, comma, o, comma, o}},
		{"WITH x AS (SELECT $$ ) $$, E'\\') ' AS q) (SELECT 1)",
			[]Token{w("WITH"), w("x"), w("AS"), lp, w("SELECT"), o, comma, o, w("AS"), w("q"), rp, lp, w("SELECT"), o, rp}},
		{"INSERT INTO t VALUES (?, $1, $name); SELECT 1.5e3 -- RETURNING\n",
			[]Token{w("INSERT"), w("INTO"), w("t"), w("VALUES"), lp, o, comma, o, comma, o, rp, semi, w("SELECT"), o}},
		{"X'ab' N'n' B'0' E'e' $tag$ ) $tag$ `id` \"id\" 1_000 .5 a::b ${", []Token{o, o, o, o, o, o, o, o, o, w("a"), o, o, w("b"), o, o}},
		{"ünïcode_word 3x", []Token{w("ünïcode_word"), o, w("x")}},
		{"$1.5", nil}, // rejected by the scanner, like FormatQuery would
	}
	for _, c := range cases {
		if got := Tokenize(c.sql); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q:\n got %+v\nwant %+v", c.sql, got, c.want)
		}
	}
}
