package parser

// M1 验收单测（一）：词法。
// 关键字大小写、数字/字符串的字面量解析、位置（行列号）、以及四类词法错误。

import (
	"strings"
	"testing"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// tok 是测试里期望的最小 token 描述。
type tok struct {
	kind TokenKind
	text string
}

func TestTokenizeKinds(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []tok
	}{
		{"关键字与标识符", "SELECT id FROM t", []tok{
			{Keyword, "SELECT"}, {Ident, "id"}, {Keyword, "FROM"}, {Ident, "t"}, {EOF, ""},
		}},
		{"关键字大小写不敏感但保留原文", "SeLeCt Id", []tok{
			{Keyword, "SeLeCt"}, {Ident, "Id"}, {EOF, ""},
		}},
		{"数字", "1 2.5 .5 1e3 1.5e-3", []tok{
			{Number, "1"}, {Number, "2.5"}, {Number, ".5"}, {Number, "1e3"}, {Number, "1.5e-3"}, {EOF, ""},
		}},
		{"字符串与引号转义", "'a''b' ''", []tok{
			{String, "a'b"}, {String, ""}, {EOF, ""},
		}},
		{"比较运算符", "= <> != < <= > >=", []tok{
			{Operator, "="}, {Operator, "<>"}, {Operator, "!="}, {Operator, "<"},
			{Operator, "<="}, {Operator, ">"}, {Operator, ">="}, {EOF, ""},
		}},
		{"算术运算符", "+ - * / %", []tok{
			{Operator, "+"}, {Operator, "-"}, {Operator, "*"}, {Operator, "/"}, {Operator, "%"}, {EOF, ""},
		}},
		{"标点", "(),;.", []tok{
			{LParen, "("}, {RParen, ")"}, {Comma, ","}, {Semicolon, ";"}, {Dot, "."}, {EOF, ""},
		}},
		{"中文标识符", "姓名 学生表", []tok{
			{Ident, "姓名"}, {Ident, "学生表"}, {EOF, ""},
		}},
		{"两条语句", "SELECT 1;SELECT 2", []tok{
			{Keyword, "SELECT"}, {Number, "1"}, {Semicolon, ";"},
			{Keyword, "SELECT"}, {Number, "2"}, {EOF, ""},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := tokenize(c.sql)
			if err != nil {
				t.Fatalf("tokenize(%q): %v", c.sql, err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("tokenize(%q) 得到 %d 个 token，期望 %d：%v", c.sql, len(got), len(c.want), got)
			}
			for i, w := range c.want {
				if got[i].Kind != w.kind || got[i].Text != w.text {
					t.Errorf("token[%d]: got (%s, %q), want (%s, %q)",
						i, got[i].Kind, got[i].Text, w.kind, w.text)
				}
			}
		})
	}
}

// TestTokenizeLiteralValues 字面量的值在词法层就定好类型：
// 有没有小数点/指数决定 INT 还是 FLOAT —— 执行层的类型检查全靠这个初始 Kind。
func TestTokenizeLiteralValues(t *testing.T) {
	toks, err := tokenize("1 2.5 1e3 'hi'")
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	want := []types.Value{
		types.IntValue(1),
		types.FloatValue(2.5),
		types.FloatValue(1000),
		types.TextValue("hi"),
	}
	for i, w := range want {
		if got := toks[i].Value; got != w {
			t.Errorf("token[%d] value: got %v (%s), want %v (%s)", i, got, got.Kind, w, w.Kind)
		}
	}
}

// TestTokenizePositions 位置从 1 开始，遇换行重置列号，列按 rune 计。
func TestTokenizePositions(t *testing.T) {
	toks, err := tokenize("SELECT\n  姓名\nFROM t")
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	want := map[int]Pos{0: {1, 1}, 1: {2, 3}, 2: {3, 1}, 3: {3, 6}}
	for i, w := range want {
		if toks[i].Pos != w {
			t.Errorf("token[%d] (%s) pos: got %s, want %s", i, toks[i].Text, toks[i].Pos, w)
		}
	}
}

// TestTokenizeErrors 四类词法错误都必须带位置 —— 报错没有行列号等于没报错。
func TestTokenizeErrors(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"字符串没闭合", "SELECT 'abc", "line 1, column 8: unterminated string literal"},
		{"非法字符", "SELECT #1", "line 1, column 8: unexpected character '#'"},
		{"指数没有数字", "SELECT 1e", "line 1, column 10: malformed number literal"},
		{"整数越界", "SELECT 99999999999999999999", "out of range"},
		{"非法字符在变量表里", "\"a\"", "unexpected character '\"'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := tokenize(c.sql)
			if err == nil {
				t.Fatalf("tokenize(%q): 期望报错，得到 nil", c.sql)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("tokenize(%q): got %q, want contains %q", c.sql, err.Error(), c.want)
			}
		})
	}
}

// TestTokenString Token 的 String 是错误信息的一部分，顺手钉住它的形状。
func TestTokenString(t *testing.T) {
	toks, err := tokenize("select 1 'a'")
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	want := []string{`keyword "SELECT"`, `number 1`, `string "a"`, "end of input"}
	for i, w := range want {
		if got := toks[i].String(); got != w {
			t.Errorf("token[%d].String(): got %q, want %q", i, got, w)
		}
	}
}
