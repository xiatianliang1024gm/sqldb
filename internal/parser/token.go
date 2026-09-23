package parser

// 词法层：把 SQL 文本切成一串 token（DESIGN §6 的 token.go）。
//
// 三件事值得单说：
//
//  1. 关键字大小写不敏感 —— SELECT / select / SeLeCt 是同一个 token，
//     标识符则保留原文（SQLite 的列名折叠策略留给执行层决定）；
//  2. 位置按 rune 计列 —— 中文表名不会让报错的列号翻倍；
//  3. 数字和字符串在词法层就把值算好（types.Value），语法层不再碰字面量解析。

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// Pos 是源码里的一个位置。行、列都从 1 开始。
type Pos struct {
	Line int
	Col  int
}

func (p Pos) String() string {
	return fmt.Sprintf("line %d, column %d", p.Line, p.Col)
}

// TokenKind 是 token 的大类。
type TokenKind uint8

const (
	Illegal TokenKind = iota // 零值；正常 tokenize 不会产出它
	EOF
	Ident
	Keyword
	Number
	String
	Operator
	LParen
	RParen
	Comma
	Semicolon
	Dot
)

func (k TokenKind) String() string {
	switch k {
	case Illegal:
		return "illegal"
	case EOF:
		return "EOF"
	case Ident:
		return "identifier"
	case Keyword:
		return "keyword"
	case Number:
		return "number"
	case String:
		return "string"
	case Operator:
		return "operator"
	case LParen:
		return "'('"
	case RParen:
		return "')'"
	case Comma:
		return "','"
	case Semicolon:
		return "';'"
	case Dot:
		return "'.'"
	}
	return "unknown"
}

// Token 是一个词法单元。
type Token struct {
	Kind  TokenKind
	Text  string      // 标识符/关键字的原文、数字的字面文本、字符串的内容（已去引号与转义）
	Value types.Value // 仅 Number / String 两类：词法层就解析好的字面量值
	Pos   Pos
}

// IsKeyword 判断 token 是不是指定关键字（大小写不敏感）。
func (t Token) IsKeyword(word string) bool {
	return t.Kind == Keyword && strings.EqualFold(t.Text, word)
}

// IsOperator 判断 token 是不是指定运算符（精确匹配，运算符没有大小写问题）。
func (t Token) IsOperator(op string) bool {
	return t.Kind == Operator && t.Text == op
}

func (t Token) String() string {
	switch t.Kind {
	case EOF:
		return "end of input"
	case Ident:
		return fmt.Sprintf("identifier %q", t.Text)
	case Keyword:
		return fmt.Sprintf("keyword %q", strings.ToUpper(t.Text))
	case Number:
		return fmt.Sprintf("number %s", t.Text)
	case String:
		return fmt.Sprintf("string %q", t.Text)
	case Operator:
		return fmt.Sprintf("operator %q", t.Text)
	case LParen, RParen, Comma, Semicolon, Dot:
		return t.Kind.String()
	}
	return "unknown token"
}

// keywords 是保留字表（全大写比较）。
//
// 刻意不含类型名（INT/TEXT/...）：类型名只在列定义那个位置有意义，
// 放进保留字会让 `CREATE TABLE t (text TEXT)` 这类合法语句被误判为语法错误。
var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "AS": true, "DISTINCT": true,
	"ORDER": true, "BY": true, "GROUP": true, "HAVING": true,
	"LIMIT": true, "OFFSET": true, "ASC": true, "DESC": true,
	"INSERT": true, "INTO": true, "VALUES": true,
	"UPDATE": true, "SET": true, "DELETE": true,
	"CREATE": true, "TABLE": true, "DROP": true, "IF": true, "EXISTS": true,
	"NOT": true, "NULL": true, "PRIMARY": true, "KEY": true,
	"JOIN": true, "INNER": true, "ON": true,
	"AND": true, "OR": true, "IS": true, "IN": true, "BETWEEN": true, "LIKE": true,
	"TRUE": true, "FALSE": true,
}

// lexer 是一次性扫描器：语句都很短，先把整串切完再交给语法层，
// 语法层就能毫无顾忌地向前看两个 token。
type lexer struct {
	src  string
	off  int
	line int
	col  int
}

func newLexer(src string) *lexer {
	return &lexer{src: src, line: 1, col: 1}
}

func (l *lexer) pos() Pos { return Pos{Line: l.line, Col: l.col} }

func (l *lexer) done() bool { return l.off >= len(l.src) }

// peek 返回当前 rune，到末尾返回 0。
func (l *lexer) peek() rune {
	if l.done() {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(l.src[l.off:])
	return r
}

// peekAt 返回往后第 n 个**字节**（只用于 ASCII 运算符/数字的前瞻）。
func (l *lexer) peekAt(n int) rune {
	if l.off+n >= len(l.src) {
		return 0
	}
	return rune(l.src[l.off+n])
}

func (l *lexer) advance() rune {
	r, size := utf8.DecodeRuneInString(l.src[l.off:])
	l.off += size
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func (l *lexer) skipSpace() {
	for !l.done() {
		switch l.peek() {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			l.advance()
		default:
			return
		}
	}
}

// tokenize 切出全部 token，末尾补一个 EOF。词法错误就地返回。
func tokenize(src string) ([]Token, error) {
	l := newLexer(src)
	var toks []Token
	for {
		l.skipSpace()
		if l.done() {
			break
		}
		start := l.pos()
		r := l.peek()
		switch {
		case isIdentStart(r):
			word := l.scanIdent()
			kind := Ident
			if keywords[strings.ToUpper(word)] {
				kind = Keyword
			}
			toks = append(toks, Token{Kind: kind, Text: word, Pos: start})
		case isDigit(r) || (r == '.' && isDigit(l.peekAt(1))):
			tok, err := l.scanNumber(start)
			if err != nil {
				return nil, err
			}
			toks = append(toks, tok)
		case r == '\'':
			tok, err := l.scanString(start)
			if err != nil {
				return nil, err
			}
			toks = append(toks, tok)
		default:
			tok, ok := l.scanPunct(start)
			if !ok {
				return nil, &ParseError{Pos: start, Msg: fmt.Sprintf("unexpected character %q", r)}
			}
			toks = append(toks, tok)
		}
	}
	return append(toks, Token{Kind: EOF, Pos: l.pos()}), nil
}

// scanIdent 读一个标识符：字母/下划线开头，后面接字母、数字、下划线。
// 用 unicode 判断，中文列名也能用。
func (l *lexer) scanIdent() string {
	begin := l.off
	l.advance()
	for !l.done() {
		r := l.peek()
		if !isIdentPart(r) {
			break
		}
		l.advance()
	}
	return l.src[begin:l.off]
}

// scanNumber 读数字字面量：整数、小数、科学计数法。
// 带小数点或指数就是 FLOAT，否则是 INT —— 这个划分决定了字面量的 types.Kind。
func (l *lexer) scanNumber(start Pos) (Token, error) {
	begin := l.off
	isFloat := false
	for isDigit(l.peek()) {
		l.advance()
	}
	if l.peek() == '.' && isDigit(l.peekAt(1)) {
		isFloat = true
		l.advance() // '.'
		for isDigit(l.peek()) {
			l.advance()
		}
	}
	if e := l.peek(); e == 'e' || e == 'E' {
		l.advance()
		if s := l.peek(); s == '+' || s == '-' {
			l.advance()
		}
		if !isDigit(l.peek()) {
			return Token{}, &ParseError{Pos: l.pos(), Msg: "malformed number literal: exponent has no digits"}
		}
		isFloat = true
		for isDigit(l.peek()) {
			l.advance()
		}
	}
	text := l.src[begin:l.off]
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return Token{}, &ParseError{Pos: start, Msg: fmt.Sprintf("invalid float literal %q", text)}
		}
		return Token{Kind: Number, Text: text, Value: types.FloatValue(f), Pos: start}, nil
	}
	i, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return Token{}, &ParseError{Pos: start, Msg: fmt.Sprintf("integer literal %q out of range", text)}
	}
	return Token{Kind: Number, Text: text, Value: types.IntValue(i), Pos: start}, nil
}

// scanString 读单引号字符串。`”` 是引号转义，其余字符原样收下（含换行）。
func (l *lexer) scanString(start Pos) (Token, error) {
	l.advance() // 开引号
	var sb strings.Builder
	for {
		r := l.peek()
		if r == 0 {
			return Token{}, &ParseError{Pos: start, Msg: "unterminated string literal"}
		}
		if r == '\'' {
			l.advance()
			if l.peek() == '\'' { // '' → 一个字面单引号
				sb.WriteRune('\'')
				l.advance()
				continue
			}
			return Token{Kind: String, Text: sb.String(), Value: types.TextValue(sb.String()), Pos: start}, nil
		}
		sb.WriteRune(l.advance())
	}
}

// scanPunct 读运算符和标点。两字符运算符先试（<= / >= / <> / !=），再试单字符。
func (l *lexer) scanPunct(start Pos) (Token, bool) {
	if two := l.src[l.off:min(l.off+2, len(l.src))]; len(two) == 2 {
		switch two {
		case "<=", ">=", "<>", "!=":
			l.advance()
			l.advance()
			return Token{Kind: Operator, Text: two, Pos: start}, true
		}
	}
	switch r := l.peek(); r {
	case '=', '<', '>', '+', '-', '*', '/', '%':
		l.advance()
		return Token{Kind: Operator, Text: string(r), Pos: start}, true
	case '(':
		l.advance()
		return Token{Kind: LParen, Text: "(", Pos: start}, true
	case ')':
		l.advance()
		return Token{Kind: RParen, Text: ")", Pos: start}, true
	case ',':
		l.advance()
		return Token{Kind: Comma, Text: ",", Pos: start}, true
	case ';':
		l.advance()
		return Token{Kind: Semicolon, Text: ";", Pos: start}, true
	case '.':
		l.advance()
		return Token{Kind: Dot, Text: ".", Pos: start}, true
	}
	return Token{}, false
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isIdentStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }

func isIdentPart(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
