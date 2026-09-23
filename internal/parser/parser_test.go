package parser

// M1 验收单测（二）：语法。
// 组织方式按 DESIGN §2 的分层：这一层不认识 catalog、不认识存储，
// 所以断言只对 AST 结构，不对语义（列存不存在、类型对不对都是执行层的事）。
//
// 三类断言：
//  1. 范围内每一条语句都能解析成预期的树（§1 的能力表逐条覆盖）；
//  2. 表达式优先级与结合性（爬升法最容易写反的地方）；
//  3. 错误都带行列号 —— M1 验收标准里明确要求的那一条。

import (
	"reflect"
	"strings"
	"testing"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// ── 构造 AST 的小工具：让测试里的期望树读起来像 SQL ─────────────────

func lit(v types.Value) Expr          { return &Literal{Value: v} }
func i64(v int64) Expr                { return lit(types.IntValue(v)) }
func str(v string) Expr               { return lit(types.TextValue(v)) }
func col(name string) Expr            { return &ColumnRef{Name: name} }
func tcol(t, name string) Expr        { return &ColumnRef{Table: t, Name: name} }
func bin(op BinaryOp, l, r Expr) Expr { return &BinaryExpr{Op: op, L: l, R: r} }

func mustParse(t *testing.T, sql string) Stmt {
	t.Helper()
	stmt, err := Parse(sql)
	if err != nil {
		t.Fatalf("Parse(%q): %v", sql, err)
	}
	return stmt
}

func assertTree(t *testing.T, sql string, want Stmt) {
	t.Helper()
	got := mustParse(t, sql)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse(%q) 结构不符\n got: %s\nwant: %s", sql, dump(got), dump(want))
	}
}

// dump 是测试专用的极简树形打印，失败时比 %+v 好看一点。
func dump(s Stmt) string {
	switch v := s.(type) {
	case *SelectStmt:
		var b strings.Builder
		b.WriteString("SELECT ")
		for i, it := range v.Items {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(dumpExpr(it.Expr))
			if it.Alias != "" {
				b.WriteString(" AS " + it.Alias)
			}
		}
		b.WriteString(" FROM " + v.From.Name)
		if v.From.Alias != "" {
			b.WriteString(" " + v.From.Alias)
		}
		return b.String()
	}
	return "<?stmt>"
}

func dumpExpr(e Expr) string {
	switch v := e.(type) {
	case *Literal:
		return v.Value.String()
	case *ColumnRef:
		if v.Table != "" {
			return v.Table + "." + v.Name
		}
		return v.Name
	case *StarExpr:
		if v.Table != "" {
			return v.Table + ".*"
		}
		return "*"
	case *UnaryExpr:
		return "(" + v.Op.String() + " " + dumpExpr(v.X) + ")"
	case *BinaryExpr:
		return "(" + dumpExpr(v.L) + " " + v.Op.String() + " " + dumpExpr(v.R) + ")"
	case *IsNullExpr:
		return "(" + dumpExpr(v.X) + " IS " + notStr(v.Not) + "NULL)"
	case *InExpr:
		return "(" + dumpExpr(v.X) + " " + notStr(v.Not) + "IN " + dumpList(v.List) + ")"
	case *BetweenExpr:
		return "(" + dumpExpr(v.X) + " " + notStr(v.Not) + "BETWEEN " + dumpExpr(v.Lo) + " AND " + dumpExpr(v.Hi) + ")"
	case *LikeExpr:
		return "(" + dumpExpr(v.X) + " " + notStr(v.Not) + "LIKE " + dumpExpr(v.Pattern) + ")"
	case *FuncExpr:
		if v.Star {
			return v.Name + "(*)"
		}
		return v.Name + dumpList(v.Args)
	}
	return "<?expr>"
}

func dumpList(list []Expr) string {
	var b strings.Builder
	b.WriteString("(")
	for i, e := range list {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(dumpExpr(e))
	}
	b.WriteString(")")
	return b.String()
}

func notStr(not bool) string {
	if not {
		return "NOT "
	}
	return ""
}

func intp(v int64) *int64 { return &v }

// ── DDL ───────────────────────────────────────────────────────────

func TestParseCreateTable(t *testing.T) {
	assertTree(t, `CREATE TABLE users (
		id INT PRIMARY KEY,
		name TEXT NOT NULL,
		score FLOAT,
		vip BOOL
	)`, &CreateTableStmt{
		Table: "users",
		Columns: []ColumnDef{
			// 解析器只记下"声明了 PRIMARY KEY"，主键列隐式 NOT NULL 是 catalog 的事
			// （DESIGN §4.4），语法层不该替语义层做这个推导。
			{Name: "id", Type: types.Int, PrimaryKey: true},
			{Name: "name", Type: types.Text, NotNull: true},
			{Name: "score", Type: types.Float},
			{Name: "vip", Type: types.Bool},
		},
	})
}

func TestParseCreateTableIfNotExistsAndTypeLength(t *testing.T) {
	assertTree(t, "CREATE TABLE IF NOT EXISTS t (a VARCHAR(20) NOT NULL PRIMARY KEY, b BIGINT)",
		&CreateTableStmt{
			IfNotExists: true,
			Table:       "t",
			Columns: []ColumnDef{
				// VARCHAR(n) 的 n 解析即丢弃：本版本不实现长度约束
				{Name: "a", Type: types.Text, NotNull: true, PrimaryKey: true},
				{Name: "b", Type: types.Int},
			},
		})
}

func TestParseDropTable(t *testing.T) {
	assertTree(t, "DROP TABLE users", &DropTableStmt{Table: "users"})
	assertTree(t, "drop table if exists users", &DropTableStmt{IfExists: true, Table: "users"})
}

// ── DML ───────────────────────────────────────────────────────────

func TestParseInsert(t *testing.T) {
	assertTree(t, "INSERT INTO users (id, name, age) VALUES (1, 'a', 20), (2, 'b', NULL)",
		&InsertStmt{
			Table:   "users",
			Columns: []string{"id", "name", "age"},
			Rows: [][]Expr{
				{i64(1), str("a"), i64(20)},
				{i64(2), str("b"), lit(types.NullValue())},
			},
		})
}

func TestParseInsertWithoutColumnList(t *testing.T) {
	assertTree(t, "INSERT INTO t VALUES (1)", &InsertStmt{
		Table: "t",
		Rows:  [][]Expr{{i64(1)}},
	})
}

func TestParseUpdate(t *testing.T) {
	assertTree(t, "UPDATE users SET name = 'x', age = age + 1 WHERE id = 1",
		&UpdateStmt{
			Table: "users",
			Set: []SetClause{
				{Column: "name", Value: str("x")},
				{Column: "age", Value: bin(OpPlus, col("age"), i64(1))},
			},
			Where: bin(OpEq, col("id"), i64(1)),
		})
}

func TestParseDelete(t *testing.T) {
	assertTree(t, "DELETE FROM users", &DeleteStmt{Table: "users"})
	assertTree(t, "DELETE FROM users WHERE id IN (1, 2)",
		&DeleteStmt{
			Table: "users",
			Where: &InExpr{X: col("id"), List: []Expr{i64(1), i64(2)}},
		})
}

// ── SELECT：把 §1 能力表里的查询子句一次串起来 ──────────────────────

func TestParseSelectAllClauses(t *testing.T) {
	assertTree(t, `SELECT DISTINCT u.id, u.name AS n, COUNT(*) AS c
		FROM users u INNER JOIN orders o ON o.uid = u.id
		WHERE u.age >= 18 AND u.name LIKE 'a%' OR u.city IN ('bj', 'sh')
		GROUP BY u.id, u.name HAVING COUNT(*) > 1
		ORDER BY u.name DESC, u.id
		LIMIT 10 OFFSET 5`,
		&SelectStmt{
			Distinct: true,
			Items: []SelectItem{
				{Expr: tcol("u", "id")},
				{Expr: tcol("u", "name"), Alias: "n"},
				{Expr: &FuncExpr{Name: "COUNT", Star: true}, Alias: "c"},
			},
			From: TableRef{Name: "users", Alias: "u"},
			Joins: []Join{{
				Table: TableRef{Name: "orders", Alias: "o"},
				On:    bin(OpEq, tcol("o", "uid"), tcol("u", "id")),
			}},
			Where: bin(OpOr,
				bin(OpAnd, bin(OpGe, tcol("u", "age"), i64(18)),
					&LikeExpr{X: tcol("u", "name"), Pattern: str("a%")}),
				&InExpr{X: tcol("u", "city"), List: []Expr{str("bj"), str("sh")}},
			),
			GroupBy: []Expr{tcol("u", "id"), tcol("u", "name")},
			Having:  bin(OpGt, &FuncExpr{Name: "COUNT", Star: true}, i64(1)),
			OrderBy: []OrderItem{
				{Expr: tcol("u", "name"), Desc: true},
				{Expr: tcol("u", "id")},
			},
			Limit:  intp(10),
			Offset: intp(5),
		})
}

func TestParseSelectStarAndAliasForms(t *testing.T) {
	s := mustParse(t, "SELECT *, t.*, a b, c AS d FROM t").(*SelectStmt)
	want := []SelectItem{
		{Expr: &StarExpr{}},
		{Expr: &StarExpr{Table: "t"}},
		{Expr: col("a"), Alias: "b"}, // 不带 AS 的别名
		{Expr: col("c"), Alias: "d"},
	}
	if !reflect.DeepEqual(s.Items, want) {
		t.Fatalf("items: got %+v, want %+v", s.Items, want)
	}
}

func TestParseSelectOffsetBeforeLimit(t *testing.T) {
	// LIMIT / OFFSET 两种顺序都收：SQL 标准与各家实现不一致，
	// 学习项目不该在这上面拒绝合法写法。
	s := mustParse(t, "SELECT * FROM t OFFSET 5 LIMIT 10").(*SelectStmt)
	if s.Limit == nil || *s.Limit != 10 || s.Offset == nil || *s.Offset != 5 {
		t.Fatalf("got limit=%v offset=%v", s.Limit, s.Offset)
	}
}

func TestParseSelectJoinWithoutInnerKeyword(t *testing.T) {
	s := mustParse(t, "SELECT * FROM a JOIN b ON a.id = b.id").(*SelectStmt)
	if len(s.Joins) != 1 || s.Joins[0].Table.Name != "b" {
		t.Fatalf("joins: %+v", s.Joins)
	}
}

func TestParseSelectOrderByAsc(t *testing.T) {
	s := mustParse(t, "SELECT * FROM t ORDER BY a ASC, b DESC, c").(*SelectStmt)
	want := []OrderItem{{Expr: col("a")}, {Expr: col("b"), Desc: true}, {Expr: col("c")}}
	if !reflect.DeepEqual(s.OrderBy, want) {
		t.Fatalf("order by: got %+v, want %+v", s.OrderBy, want)
	}
}

// TestParseChineseIdentifiers 标识符走 unicode 判断，中文表名/列名不是特例，
// 报错的列号也按 rune 计（位置断言见 token_test）。
func TestParseChineseIdentifiers(t *testing.T) {
	assertTree(t, "SELECT 姓名 FROM 学生表 WHERE 年龄 > 18", &SelectStmt{
		Items: []SelectItem{{Expr: &ColumnRef{Name: "姓名"}}},
		From:  TableRef{Name: "学生表"},
		Where: &BinaryExpr{Op: OpGt, L: &ColumnRef{Name: "年龄"}, R: &Literal{Value: types.IntValue(18)}},
	})
}

func TestParseTrailingSemicolonAndCaseInsensitive(t *testing.T) {
	mustParse(t, "select * from t where id > 1;")
	mustParse(t, "SELECT * FROM t ;")
	mustParse(t, "  \n\tSELECT *\n\tFROM t\n")
}

// ── 表达式：优先级与结合性 ─────────────────────────────────────────

// parseSelectExpr 借 SELECT 列表当表达式的载体（本版本没有裸表达式入口）。
func parseSelectExpr(t *testing.T, expr string) Expr {
	t.Helper()
	s := mustParse(t, "SELECT "+expr+" FROM t").(*SelectStmt)
	return s.Items[0].Expr
}

func TestParseExpressionPrecedence(t *testing.T) {
	cases := []struct {
		expr string
		want Expr
	}{
		// 算术：* / % 高于 + -
		{"1 + 2 * 3", bin(OpPlus, i64(1), bin(OpMul, i64(2), i64(3)))},
		{"1 * 2 + 3", bin(OpPlus, bin(OpMul, i64(1), i64(2)), i64(3))},
		{"10 - 3 - 2", bin(OpMinus, bin(OpMinus, i64(10), i64(3)), i64(2))}, // 左结合
		{"(a + b) * c", bin(OpMul, bin(OpPlus, col("a"), col("b")), col("c"))},
		{"a % 2 = 0", bin(OpEq, bin(OpMod, col("a"), i64(2)), i64(0))},
		// 逻辑：AND 高于 OR
		{"a AND b OR c", bin(OpOr, bin(OpAnd, col("a"), col("b")), col("c"))},
		{"a OR b AND c", bin(OpOr, col("a"), bin(OpAnd, col("b"), col("c")))},
		// NOT 低于比较：`NOT a = b` 是 NOT (a = b)，不是 (NOT a) = b
		{"NOT a = b", &UnaryExpr{Op: OpNot, X: bin(OpEq, col("a"), col("b"))}},
		{"NOT a AND b", bin(OpAnd, &UnaryExpr{Op: OpNot, X: col("a")}, col("b"))},
		// 一元负号比 * 紧
		{"-a * b", bin(OpMul, &UnaryExpr{Op: OpNeg, X: col("a")}, col("b"))},
		// IS / IN / BETWEEN / LIKE 与比较同档，低于算术：
		// `a + b IS NULL` 必须是 (a + b) IS NULL
		{"a + b IS NULL", &IsNullExpr{X: bin(OpPlus, col("a"), col("b"))}},
		{"a IS NULL AND b", bin(OpAnd, &IsNullExpr{X: col("a")}, col("b"))},
		{"a IS NOT NULL", &IsNullExpr{X: col("a"), Not: true}},
		{"x BETWEEN 1 AND 2 AND y", bin(OpAnd, &BetweenExpr{X: col("x"), Lo: i64(1), Hi: i64(2)}, col("y"))},
		{"x NOT BETWEEN 1 AND 2", &BetweenExpr{X: col("x"), Lo: i64(1), Hi: i64(2), Not: true}},
		{"x IN (1, 2)", &InExpr{X: col("x"), List: []Expr{i64(1), i64(2)}}},
		{"x NOT IN (1)", &InExpr{X: col("x"), List: []Expr{i64(1)}, Not: true}},
		{"name LIKE 'a%'", &LikeExpr{X: col("name"), Pattern: str("a%")}},
		{"name NOT LIKE 'a%'", &LikeExpr{X: col("name"), Pattern: str("a%"), Not: true}},
		{"name LIKE 'a%' AND x", bin(OpAnd, &LikeExpr{X: col("name"), Pattern: str("a%")}, col("x"))},
		// 比较运算符的两种"不等于"写法
		{"a <> b", bin(OpNe, col("a"), col("b"))},
		{"a != b", bin(OpNe, col("a"), col("b"))},
		// 字面量
		{"TRUE", lit(types.BoolValue(true))},
		{"FALSE", lit(types.BoolValue(false))},
		{"NULL", lit(types.NullValue())},
		{"1.5e3", lit(types.FloatValue(1500))},
		{"'it''s'", str("it's")},
		// 限定列名与聚合函数
		{"t.a", tcol("t", "a")},
		{"SUM(a + 1)", &FuncExpr{Name: "SUM", Args: []Expr{bin(OpPlus, col("a"), i64(1))}}},
		{"count(*)", &FuncExpr{Name: "COUNT", Star: true}}, // 函数名大小写不敏感，存成大写
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			got := parseSelectExpr(t, c.expr)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Parse(%q)\n got: %s\nwant: %s", c.expr, dumpExpr(got), dumpExpr(c.want))
			}
		})
	}
}

// ── 错误：每一条都要有行列号 ───────────────────────────────────────

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"空语句", "", "line 1, column 1"},
		{"不是语句开头", "VACUUM", "line 1, column 1: unexpected"},
		{"SELECT 后缺表达式", "SELECT", "line 1, column 7"},
		{"缺 FROM", "SELECT * t", "line 1, column 10: expected FROM"},
		{"FROM 后缺表名", "SELECT * FROM", "line 1, column 14: expected table name"},
		{"WHERE 后缺表达式", "SELECT * FROM t WHERE", "line 1, column 22"},
		{"JOIN 缺 ON", "SELECT * FROM a JOIN b", "line 1, column 23: expected ON"},
		{"LIMIT 不是整数", "SELECT * FROM t LIMIT", "line 1, column 22: LIMIT expects a non-negative integer"},
		{"LIMIT 是负数", "SELECT * FROM t LIMIT -1", "line 1, column 23: LIMIT expects a non-negative integer"},
		{"括号没闭合", "SELECT (a + b FROM t", "expected ')' after parenthesized expression"},
		{"建表没列", "CREATE TABLE t ()", "line 1, column 17: table t must have at least one column"},
		{"未知列类型", "CREATE TABLE t (a FOO)", "line 1, column 19: unknown column type \"FOO\""},
		{"NOT 后面不是 NULL", "CREATE TABLE t (a INT NOT)", "expected NULL"},
		{"VALUES 行数不齐", "INSERT INTO t VALUES (1), (2, 3)", "VALUES row 2 has 2 values, expected 1"},
		{"UPDATE 缺等号", "UPDATE t SET a 1", "expected '=' after SET column a"},
		{"未知函数", "SELECT FOO(a) FROM t", `unknown function "FOO"`},
		{"IS 后面不是 NULL", "SELECT * FROM t WHERE a IS TRUE", "line 1, column 28: expected NULL after IS"},
		{"BETWEEN 缺 AND", "SELECT * FROM t WHERE a BETWEEN 1 2", "expected AND"},
		{"多条语句", "SELECT * FROM t; SELECT * FROM t", "line 1, column 18: unexpected"},
		{"跨行报错", "SELECT *\nFROM t\nWHERE a IS TRUE", "line 3, column 12: expected NULL after IS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.sql)
			if err == nil {
				t.Fatalf("Parse(%q): 期望报错，得到 nil", c.sql)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Parse(%q): got %q, want contains %q", c.sql, err.Error(), c.want)
			}
		})
	}
}

// TestParseErrorIsParseError 调用方（将来的 REPL）要能拿到结构化位置，
// 不能只靠字符串匹配。
func TestParseErrorIsParseError(t *testing.T) {
	_, err := Parse("SELECT * FROM t LIMIT")
	pe, ok := err.(*ParseError)
	if !ok {
		t.Fatalf("err 类型是 %T，期望 *ParseError", err)
	}
	if pe.Pos.Line != 1 || pe.Pos.Col != 22 {
		t.Fatalf("pos: got %s, want line 1, column 22", pe.Pos)
	}
}
