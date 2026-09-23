// Package parser 把 SQL 字符串变成语法树（DESIGN §6）。
//
// 这一层不认识 catalog，也不认识 kvdb：它只回答"这段文本是否符合我们的子集
// 语法，如果是，树长什么样"。语义检查（列存不存在、类型对不对、聚合用得对不对）
// 全部留给执行层 —— 那才是唯一知道 schema 的地方。分层带来的代价是错误要晚一点
// 才报，换来的是 parser 能脱离存储单独单测。
//
// 组成：
//   - token.go  词法：关键字表、标识符、数字、字符串、运算符、位置
//   - ast.go    语法树节点
//   - parser.go 递归下降 + 优先级爬升（本文件）
//
// 表达式用优先级爬升而不是"一层函数一个优先级"的展开写法：运算符一多，
// 展开写法要靠调用链顺序隐式表达优先级，改一个优先级就得动五个函数；
// 爬升法把优先级摆在一张表里，一眼能对。
package parser

import (
	"fmt"
	"strings"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// ParseError 是词法/语法错误。Msg 是不带位置的描述，位置单独存字段，
// 方便调用方（将来的 REPL）自己排版。
type ParseError struct {
	Pos Pos
	Msg string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("parse error at %s: %s", e.Pos, e.Msg)
}

// Parse 解析一条 SQL 语句。结尾允许一个分号；之后还有 token 就报错
// （DESIGN §6：多余 token 报错，防止 "SELECT 1; DROP TABLE t" 静默丢掉后半句）。
func Parse(sql string) (Stmt, error) {
	toks, err := tokenize(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	if p.at(Semicolon) {
		p.next()
	}
	if !p.at(EOF) {
		return nil, &ParseError{Pos: p.cur().Pos, Msg: fmt.Sprintf("unexpected %s after end of statement", p.cur())}
	}
	return stmt, nil
}

// ── 运算符优先级（低 → 高）──────────────────────────────────────────
//
// NOT 单独占一档，且低于比较运算：`NOT a = b` 是 NOT (a = b)，
// 不是 (NOT a) = b —— 这正是 SQL 与多数编程语言相反的地方。
const (
	precOr  = 1
	precAnd = 2
	precNot = 3
	precCmp = 4 // = != <> < <= > >=，以及 IS / IN / BETWEEN / LIKE
	precAdd = 5
	precMul = 6
)

// parser 持有 token 流和一个游标。语句短，不做回溯之外的优化。
type parser struct {
	toks []Token
	i    int
}

// cur 返回当前 token（末尾之后一律返回 EOF，永不越界）。
func (p *parser) cur() *Token { return &p.toks[p.i] }

// peekN 向前看 n 个 token，越界返回 EOF。
func (p *parser) peekN(n int) *Token {
	if j := p.i + n; j < len(p.toks) {
		return &p.toks[j]
	}
	return &p.toks[len(p.toks)-1]
}

func (p *parser) peek() *Token { return p.peekN(1) }

// next 取走当前 token 并前进（停在 EOF 上）。
func (p *parser) next() Token {
	t := *p.cur()
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) at(k TokenKind) bool     { return p.cur().Kind == k }
func (p *parser) atKeyword(w string) bool { return p.cur().IsKeyword(w) }
func (p *parser) atOperator(op string) bool {
	return p.cur().Kind == Operator && p.cur().Text == op
}

// error 在当前 token 处报错。
func (p *parser) errorf(format string, args ...any) error {
	return &ParseError{Pos: p.cur().Pos, Msg: fmt.Sprintf(format, args...)}
}

// expect 要求当前 token 是指定类别并取走它。
func (p *parser) expect(k TokenKind, what string) error {
	if !p.at(k) {
		return p.errorf("expected %s, got %s", what, p.cur())
	}
	p.next()
	return nil
}

func (p *parser) expectKeyword(w string) error {
	if !p.atKeyword(w) {
		return p.errorf("expected %s, got %s", w, p.cur())
	}
	p.next()
	return nil
}

// parseIdent 取一个标识符。what 只用于错误信息（"table name" / "column alias" ...）。
func (p *parser) parseIdent(what string) (string, error) {
	if !p.at(Ident) {
		return "", p.errorf("expected %s, got %s", what, p.cur())
	}
	return p.next().Text, nil
}

// ── 语句 ──────────────────────────────────────────────────────────

func (p *parser) parseStatement() (Stmt, error) {
	switch {
	case p.atKeyword("CREATE"):
		return p.parseCreateTable()
	case p.atKeyword("DROP"):
		return p.parseDropTable()
	case p.atKeyword("INSERT"):
		return p.parseInsert()
	case p.atKeyword("UPDATE"):
		return p.parseUpdate()
	case p.atKeyword("DELETE"):
		return p.parseDelete()
	case p.atKeyword("SELECT"):
		return p.parseSelect()
	}
	return nil, p.errorf("unexpected %s, expected CREATE / DROP / INSERT / UPDATE / DELETE / SELECT", p.cur())
}

// parseCreateTable: CREATE TABLE [IF NOT EXISTS] t ( col type [约束] {, ...} )
func (p *parser) parseCreateTable() (Stmt, error) {
	p.next() // CREATE
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	stmt := &CreateTableStmt{}
	if p.atKeyword("IF") {
		p.next()
		if err := p.expectKeyword("NOT"); err != nil {
			return nil, err
		}
		if err := p.expectKeyword("EXISTS"); err != nil {
			return nil, err
		}
		stmt.IfNotExists = true
	}
	name, err := p.parseIdent("table name")
	if err != nil {
		return nil, err
	}
	stmt.Table = name
	if err := p.expect(LParen, "'(' after table name"); err != nil {
		return nil, err
	}
	// 空列清单直接在这里拦：`CREATE TABLE t ()` 报"至少一列"比报
	// "缺列名"更贴近用户真正写错的东西（进循环再发现就已经晚了）。
	if p.at(RParen) {
		return nil, &ParseError{Pos: p.cur().Pos, Msg: fmt.Sprintf("table %s must have at least one column", name)}
	}
	for {
		col, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		stmt.Columns = append(stmt.Columns, col)
		if !p.at(Comma) {
			break
		}
		p.next()
	}
	if err := p.expect(RParen, "')' or ',' in column list"); err != nil {
		return nil, err
	}
	if len(stmt.Columns) == 0 {
		return nil, &ParseError{Pos: p.cur().Pos, Msg: fmt.Sprintf("table %s must have at least one column", name)}
	}
	return stmt, nil
}

// parseColumnDef: col 类型 [(长度)] { NOT NULL | NULL | PRIMARY KEY }
//
// 约束顺序不限（两种顺序都是常见写法），重复声明不报错 —— 后面覆盖前面
// 的语义在一个布尔位上是幂等的，没必要为此拒绝合法 SQL。
func (p *parser) parseColumnDef() (ColumnDef, error) {
	name, err := p.parseIdent("column name")
	if err != nil {
		return ColumnDef{}, err
	}
	typePos := p.cur().Pos
	typeName, err := p.parseIdent("column type")
	if err != nil {
		return ColumnDef{}, err
	}
	kind, err := types.ParseKind(typeName)
	if err != nil {
		return ColumnDef{}, &ParseError{Pos: typePos, Msg: fmt.Sprintf("unknown column type %q", typeName)}
	}
	// VARCHAR(20) 这类长度参数：吃掉并丢弃（本版本不实现长度约束）。
	if p.at(LParen) {
		p.next()
		if !p.at(Number) || p.cur().Value.Kind != types.Int {
			return ColumnDef{}, p.errorf("expected integer length for type %s, got %s", strings.ToUpper(typeName), p.cur())
		}
		p.next()
		if err := p.expect(RParen, "')' after type length"); err != nil {
			return ColumnDef{}, err
		}
	}
	def := ColumnDef{Name: name, Type: kind}
	for {
		switch {
		case p.atKeyword("NOT"):
			p.next()
			if err := p.expectKeyword("NULL"); err != nil {
				return ColumnDef{}, err
			}
			def.NotNull = true
		case p.atKeyword("NULL"): // 显式声明可空：与默认一致，保留是为了不拒绝合法写法
			p.next()
		case p.atKeyword("PRIMARY"):
			p.next()
			if err := p.expectKeyword("KEY"); err != nil {
				return ColumnDef{}, err
			}
			def.PrimaryKey = true
		default:
			return def, nil
		}
	}
}

// parseDropTable: DROP TABLE [IF EXISTS] t
func (p *parser) parseDropTable() (Stmt, error) {
	p.next() // DROP
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	stmt := &DropTableStmt{}
	if p.atKeyword("IF") {
		p.next()
		if err := p.expectKeyword("EXISTS"); err != nil {
			return nil, err
		}
		stmt.IfExists = true
	}
	name, err := p.parseIdent("table name")
	if err != nil {
		return nil, err
	}
	stmt.Table = name
	return stmt, nil
}

// parseInsert: INSERT INTO t [(cols)] VALUES (...), (...)
func (p *parser) parseInsert() (Stmt, error) {
	p.next() // INSERT
	if err := p.expectKeyword("INTO"); err != nil {
		return nil, err
	}
	stmt := &InsertStmt{}
	name, err := p.parseIdent("table name")
	if err != nil {
		return nil, err
	}
	stmt.Table = name

	if p.at(LParen) { // 可选列清单
		p.next()
		for {
			col, err := p.parseIdent("column name")
			if err != nil {
				return nil, err
			}
			stmt.Columns = append(stmt.Columns, col)
			if !p.at(Comma) {
				break
			}
			p.next()
		}
		if err := p.expect(RParen, "')' or ',' in column list"); err != nil {
			return nil, err
		}
		if len(stmt.Columns) == 0 {
			return nil, p.errorf("empty column list")
		}
	}

	if err := p.expectKeyword("VALUES"); err != nil {
		return nil, err
	}
	for {
		rowPos := p.cur().Pos
		if err := p.expect(LParen, "'(' to start a VALUES row"); err != nil {
			return nil, err
		}
		row, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		if err := p.expect(RParen, "')' or ',' in VALUES row"); err != nil {
			return nil, err
		}
		if len(stmt.Rows) > 0 && len(row) != len(stmt.Rows[0]) {
			return nil, &ParseError{Pos: rowPos,
				Msg: fmt.Sprintf("VALUES row %d has %d values, expected %d", len(stmt.Rows)+1, len(row), len(stmt.Rows[0]))}
		}
		stmt.Rows = append(stmt.Rows, row)
		if !p.at(Comma) {
			break
		}
		p.next()
	}
	return stmt, nil
}

// parseUpdate: UPDATE t SET col = expr {, col = expr} [WHERE expr]
func (p *parser) parseUpdate() (Stmt, error) {
	p.next() // UPDATE
	stmt := &UpdateStmt{}
	name, err := p.parseIdent("table name")
	if err != nil {
		return nil, err
	}
	stmt.Table = name
	if err := p.expectKeyword("SET"); err != nil {
		return nil, err
	}
	for {
		col, err := p.parseIdent("column name")
		if err != nil {
			return nil, err
		}
		if !p.atOperator("=") {
			return nil, p.errorf("expected '=' after SET column %s, got %s", col, p.cur())
		}
		p.next()
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Set = append(stmt.Set, SetClause{Column: col, Value: val})
		if !p.at(Comma) {
			break
		}
		p.next()
	}
	where, err := p.parseOptionalWhere()
	if err != nil {
		return nil, err
	}
	stmt.Where = where
	return stmt, nil
}

// parseDelete: DELETE FROM t [WHERE expr]
func (p *parser) parseDelete() (Stmt, error) {
	p.next() // DELETE
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	stmt := &DeleteStmt{}
	name, err := p.parseIdent("table name")
	if err != nil {
		return nil, err
	}
	stmt.Table = name
	where, err := p.parseOptionalWhere()
	if err != nil {
		return nil, err
	}
	stmt.Where = where
	return stmt, nil
}

// parseSelect:
//
//	SELECT [DISTINCT] item {, item}
//	FROM t [[AS] alias] { [INNER] JOIN t [[AS] alias] ON expr }
//	[WHERE expr]
//	[GROUP BY expr {, expr} [HAVING expr]]
//	[ORDER BY expr [ASC|DESC] {, ...}]
//	[LIMIT n] [OFFSET m]
func (p *parser) parseSelect() (Stmt, error) {
	p.next() // SELECT
	stmt := &SelectStmt{}
	if p.atKeyword("DISTINCT") {
		p.next()
		stmt.Distinct = true
	}
	items, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	stmt.Items = items

	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	from, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	stmt.From = from

	for p.atKeyword("JOIN") || p.atKeyword("INNER") {
		if p.atKeyword("INNER") {
			p.next()
			if !p.atKeyword("JOIN") {
				return nil, p.errorf("expected JOIN after INNER, got %s", p.cur())
			}
		}
		p.next() // JOIN
		ref, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("ON"); err != nil {
			return nil, err
		}
		on, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Joins = append(stmt.Joins, Join{Table: ref, On: on})
	}

	where, err := p.parseOptionalWhere()
	if err != nil {
		return nil, err
	}
	stmt.Where = where

	if p.atKeyword("GROUP") {
		p.next()
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		groupBy, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		stmt.GroupBy = groupBy
		if p.atKeyword("HAVING") {
			p.next()
			having, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			stmt.Having = having
		}
	}

	if p.atKeyword("ORDER") {
		p.next()
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			item := OrderItem{Expr: e}
			if p.atKeyword("DESC") {
				p.next()
				item.Desc = true
			} else if p.atKeyword("ASC") {
				p.next()
			}
			stmt.OrderBy = append(stmt.OrderBy, item)
			if !p.at(Comma) {
				break
			}
			p.next()
		}
	}

	// LIMIT / OFFSET 顺序不限（SQL 标准与 MySQL/SQLite 的写法不一致，
	// 学习项目不必为此报错），但各自只能出现一次。
	for p.atKeyword("LIMIT") || p.atKeyword("OFFSET") {
		if p.atKeyword("LIMIT") {
			p.next()
			if stmt.Limit != nil {
				return nil, p.errorf("duplicate LIMIT clause")
			}
			n, err := p.parseNonNegativeInt("LIMIT")
			if err != nil {
				return nil, err
			}
			stmt.Limit = &n
		} else {
			p.next()
			if stmt.Offset != nil {
				return nil, p.errorf("duplicate OFFSET clause")
			}
			n, err := p.parseNonNegativeInt("OFFSET")
			if err != nil {
				return nil, err
			}
			stmt.Offset = &n
		}
	}
	return stmt, nil
}

// parseSelectList 解析 SELECT 列表：* / t.* / expr [[AS] alias]。
func (p *parser) parseSelectList() ([]SelectItem, error) {
	var items []SelectItem
	for {
		if p.atOperator("*") {
			p.next()
			items = append(items, SelectItem{Expr: &StarExpr{}})
		} else if p.at(Ident) && p.peek().Kind == Dot && p.peekN(2).IsOperator("*") {
			table := p.next().Text
			p.next() // '.'
			p.next() // '*'
			items = append(items, SelectItem{Expr: &StarExpr{Table: table}})
		} else {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			item := SelectItem{Expr: e}
			if p.atKeyword("AS") {
				p.next()
				alias, err := p.parseIdent("column alias")
				if err != nil {
					return nil, err
				}
				item.Alias = alias
			} else if p.at(Ident) { // 不带 AS 的别名：`SELECT a b FROM t`
				item.Alias = p.next().Text
			}
			items = append(items, item)
		}
		if !p.at(Comma) {
			return items, nil
		}
		p.next()
	}
}

// parseTableRef 解析表名 + 可选别名（AS 可省）。
func (p *parser) parseTableRef() (TableRef, error) {
	name, err := p.parseIdent("table name")
	if err != nil {
		return TableRef{}, err
	}
	ref := TableRef{Name: name}
	if p.atKeyword("AS") {
		p.next()
		alias, err := p.parseIdent("table alias")
		if err != nil {
			return TableRef{}, err
		}
		ref.Alias = alias
	} else if p.at(Ident) {
		ref.Alias = p.next().Text
	}
	return ref, nil
}

func (p *parser) parseOptionalWhere() (Expr, error) {
	if !p.atKeyword("WHERE") {
		return nil, nil
	}
	p.next()
	return p.parseExpr()
}

// parseExprList 解析逗号分隔的表达式列表。收尾的 ')' 由调用方各自检查 ——
// GROUP BY 的列表后面跟的可能是 HAVING / ORDER，不该由这里判断。
func (p *parser) parseExprList() ([]Expr, error) {
	var list []Expr
	for {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		list = append(list, e)
		if !p.at(Comma) {
			break
		}
		p.next()
	}
	return list, nil
}

// parseNonNegativeInt 读 LIMIT / OFFSET 后面的非负整数。
func (p *parser) parseNonNegativeInt(what string) (int64, error) {
	t := p.cur()
	if t.Kind != Number || t.Value.Kind != types.Int || t.Value.I < 0 {
		return 0, p.errorf("%s expects a non-negative integer, got %s", what, t)
	}
	p.next()
	return t.Value.I, nil
}

// ── 表达式：优先级爬升 ─────────────────────────────────────────────

// parseExpr 是表达式的入口。
func (p *parser) parseExpr() (Expr, error) {
	return p.parseBinary(precOr)
}

// parseBinary 解析"优先级不低于 minPrec"的表达式。
//
// 每轮先把左操作数取出来，再看当前运算符的优先级够不够：
// 够就吃掉运算符、用 prec+1 递归解析右操作数（prec+1 保证左结合），
// 不够就把左操作数交回上一层。
func (p *parser) parseBinary(minPrec int) (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		// IS / IN / BETWEEN / LIKE 是"后置"运算符，与比较运算同档：
		// `a + b IS NULL` 必须是 (a + b) IS NULL，所以放在爬升循环里而不是
		// 跟在 primary 后面（跟在 primary 后面会得到 a + (b IS NULL)）。
		if precCmp >= minPrec {
			x, ok, err := p.tryPostfix(left)
			if err != nil {
				return nil, err
			}
			if ok {
				left = x
				continue
			}
		}
		op, prec, ok := binaryOpOf(p.cur())
		if !ok || prec < minPrec {
			return left, nil
		}
		p.next()
		right, err := p.parseBinary(prec + 1)
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: op, L: left, R: right}
	}
}

// parseUnary 解析一元运算符和 primary。
func (p *parser) parseUnary() (Expr, error) {
	switch {
	case p.atOperator("-"):
		p.next()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: OpNeg, X: x}, nil
	case p.atOperator("+"):
		p.next()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: OpPos, X: x}, nil
	case p.atKeyword("NOT"):
		p.next()
		// NOT 低于比较：操作数按"比 NOT 高一档"解析，`NOT a = b` → NOT (a = b)。
		x, err := p.parseBinary(precNot + 1)
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: OpNot, X: x}, nil
	}
	return p.parsePrimary()
}

// tryPostfix 尝试解析 IS / IN / BETWEEN / LIKE（可带前缀 NOT）。
// 返回 (新表达式, 是否命中, 错误)。
func (p *parser) tryPostfix(x Expr) (Expr, bool, error) {
	not := false
	t := p.cur()
	if t.IsKeyword("NOT") {
		n := p.peek()
		if !n.IsKeyword("IN") && !n.IsKeyword("BETWEEN") && !n.IsKeyword("LIKE") {
			return nil, false, nil // 是别处的 NOT，交给调用方处理
		}
		not = true
		p.next()
		t = p.cur()
	}
	switch {
	case t.IsKeyword("IS"):
		p.next()
		if p.atKeyword("NOT") {
			p.next()
			not = !not
		}
		if !p.atKeyword("NULL") {
			return nil, false, p.errorf("expected NULL after IS, got %s", p.cur())
		}
		p.next()
		return &IsNullExpr{X: x, Not: not}, true, nil

	case t.IsKeyword("IN"):
		p.next()
		if err := p.expect(LParen, "'(' after IN"); err != nil {
			return nil, false, err
		}
		list, err := p.parseExprList()
		if err != nil {
			return nil, false, err
		}
		if err := p.expect(RParen, "')' or ',' in IN list"); err != nil {
			return nil, false, err
		}
		return &InExpr{X: x, List: list, Not: not}, true, nil

	case t.IsKeyword("BETWEEN"):
		p.next()
		// 上下界按算术档解析，这样 `BETWEEN 1 AND 2` 里的 AND 不会被吃掉。
		lo, err := p.parseBinary(precAdd)
		if err != nil {
			return nil, false, err
		}
		if err := p.expectKeyword("AND"); err != nil {
			return nil, false, err
		}
		hi, err := p.parseBinary(precAdd)
		if err != nil {
			return nil, false, err
		}
		return &BetweenExpr{X: x, Lo: lo, Hi: hi, Not: not}, true, nil

	case t.IsKeyword("LIKE"):
		p.next()
		pat, err := p.parseBinary(precAdd)
		if err != nil {
			return nil, false, err
		}
		return &LikeExpr{X: x, Pattern: pat, Not: not}, true, nil
	}
	return nil, false, nil
}

// parsePrimary 解析表达式的最小单元。
func (p *parser) parsePrimary() (Expr, error) {
	t := p.cur()
	switch {
	case t.Kind == Number, t.Kind == String:
		p.next()
		return &Literal{Value: t.Value}, nil
	case t.IsKeyword("NULL"):
		p.next()
		return &Literal{Value: types.NullValue()}, nil
	case t.IsKeyword("TRUE"):
		p.next()
		return &Literal{Value: types.BoolValue(true)}, nil
	case t.IsKeyword("FALSE"):
		p.next()
		return &Literal{Value: types.BoolValue(false)}, nil
	case t.Kind == LParen:
		p.next()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expect(RParen, "')' after parenthesized expression"); err != nil {
			return nil, err
		}
		return e, nil
	case t.Kind == Ident:
		return p.parseIdentOrCall()
	}
	return nil, p.errorf("unexpected %s, expected an expression", t)
}

// parseIdentOrCall 处理 Identifier 开头的三种形状：函数调用、限定列名、列名。
func (p *parser) parseIdentOrCall() (Expr, error) {
	switch p.peek().Kind {
	case LParen: // 函数调用
		return p.parseFuncCall()
	case Dot:
		name := p.next().Text
		p.next() // '.'
		col, err := p.parseIdent("column name")
		if err != nil {
			return nil, err
		}
		return &ColumnRef{Table: name, Name: col}, nil
	}
	name := p.next().Text
	return &ColumnRef{Name: name}, nil
}

// aggregates 是本版本认识的函数（DESIGN §6：仅聚合函数）。
var aggregates = map[string]bool{
	"COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true,
}

// parseFuncCall 解析 func(args) 或 COUNT(*)。
func (p *parser) parseFuncCall() (Expr, error) {
	nameTok := p.next()
	upper := strings.ToUpper(nameTok.Text)
	if !aggregates[upper] {
		return nil, &ParseError{Pos: nameTok.Pos, Msg: fmt.Sprintf("unknown function %q (only COUNT/SUM/AVG/MIN/MAX are supported)", nameTok.Text)}
	}
	if err := p.expect(LParen, "'(' after function name"); err != nil {
		return nil, err
	}
	fn := &FuncExpr{Name: upper}
	if p.atOperator("*") {
		p.next()
		fn.Star = true
	} else {
		args, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		fn.Args = args
	}
	if err := p.expect(RParen, "')' or ',' in argument list"); err != nil {
		return nil, err
	}
	return fn, nil
}

// binaryOpOf 把 token 映射成二元运算符和它的优先级。
func binaryOpOf(t *Token) (BinaryOp, int, bool) {
	if t.IsKeyword("OR") {
		return OpOr, precOr, true
	}
	if t.IsKeyword("AND") {
		return OpAnd, precAnd, true
	}
	if t.Kind != Operator {
		return 0, 0, false
	}
	switch t.Text {
	case "=":
		return OpEq, precCmp, true
	case "!=", "<>":
		return OpNe, precCmp, true
	case "<":
		return OpLt, precCmp, true
	case "<=":
		return OpLe, precCmp, true
	case ">":
		return OpGt, precCmp, true
	case ">=":
		return OpGe, precCmp, true
	case "+":
		return OpPlus, precAdd, true
	case "-":
		return OpMinus, precAdd, true
	case "*":
		return OpMul, precMul, true
	case "/":
		return OpDiv, precMul, true
	case "%":
		return OpMod, precMul, true
	}
	return 0, 0, false
}
