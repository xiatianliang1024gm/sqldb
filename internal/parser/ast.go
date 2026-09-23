package parser

// 语法树节点（DESIGN §6 的 ast.go）。
//
// 这是纯语法结构：节点里没有列偏移、没有类型推导、没有 catalog 信息 ——
// 那些都要等执行层把 AST 绑定到 schema 之后才存在（DESIGN §2 的分层）。
// 代价是"列名写错了"这种错误只能到执行期才报，这是刻意的。

import "github.com/xiatianliang1024gm/sqldb/internal/types"

// ── 语句 ──────────────────────────────────────────────────────────

// Stmt 是所有语句的公共接口。
type Stmt interface{ stmt() }

// CreateTableStmt 是 CREATE TABLE。
type CreateTableStmt struct {
	IfNotExists bool
	Table       string
	Columns     []ColumnDef
}

// ColumnDef 是一列的定义。类型已在解析期用 types.ParseKind 落定；
// 长度参数（VARCHAR(n)）本版本不实现约束，解析即丢弃。
type ColumnDef struct {
	Name       string
	Type       types.Kind
	NotNull    bool
	PrimaryKey bool
}

// DropTableStmt 是 DROP TABLE。
type DropTableStmt struct {
	IfExists bool
	Table    string
}

// CreateIndexStmt 是 CREATE [UNIQUE] INDEX。单列索引（DESIGN §7.5）；
// 列是否存在、是不是主键，都是执行层（catalog）的检查。
type CreateIndexStmt struct {
	Unique      bool
	IfNotExists bool
	Name        string
	Table       string
	Column      string
}

// DropIndexStmt 是 DROP INDEX。
type DropIndexStmt struct {
	IfExists bool
	Name     string
}

// InsertStmt 是 INSERT INTO。Columns 为 nil 表示按 schema 顺序给全列；
// Rows 的每一行长度必须与 Columns（或全列数）一致，长度在解析期就校验。
type InsertStmt struct {
	Table   string
	Columns []string
	Rows    [][]Expr
}

// UpdateStmt 是 UPDATE。主键列不允许出现在 Set 里 —— 那是执行层的检查
// （DESIGN 取舍表 #10），解析器不知道哪列是主键。
type UpdateStmt struct {
	Table string
	Set   []SetClause
	Where Expr
}

// SetClause 是 SET 里的一项赋值。
type SetClause struct {
	Column string
	Value  Expr
}

// DeleteStmt 是 DELETE FROM。Where 为 nil 表示全表删。
type DeleteStmt struct {
	Table string
	Where Expr
}

// SelectStmt 是 SELECT。各子句缺省为零值/nil，执行层按 nil 判断有无。
type SelectStmt struct {
	Distinct bool
	Items    []SelectItem
	From     TableRef
	Joins    []Join
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderItem
	Limit    *int64
	Offset   *int64
}

// TableRef 是 FROM / JOIN 里的一个表引用，含可选别名。
type TableRef struct {
	Name  string
	Alias string
}

// Join 是一级 INNER JOIN。本版本只有这一种（DESIGN §1 范围外），
// 用列表而不是单字段，是为了让三表连接不用改结构。
type Join struct {
	Table TableRef
	On    Expr
}

// SelectItem 是 SELECT 列表的一项。Alias 为空表示没有别名，
// 输出列名由执行层从表达式推导。
type SelectItem struct {
	Expr  Expr
	Alias string
}

// OrderItem 是 ORDER BY 的一项。Desc 为 false 即 ASC。
type OrderItem struct {
	Expr Expr
	Desc bool
}

func (*CreateTableStmt) stmt() {}
func (*DropTableStmt) stmt()   {}
func (*CreateIndexStmt) stmt() {}
func (*DropIndexStmt) stmt()   {}
func (*InsertStmt) stmt()      {}
func (*UpdateStmt) stmt()      {}
func (*DeleteStmt) stmt()      {}
func (*SelectStmt) stmt()      {}

// ── 表达式 ────────────────────────────────────────────────────────

// Expr 是所有表达式的公共接口。
type Expr interface{ expr() }

// Literal 是字面量。NULL / TRUE / FALSE 也走这里，只是 Value 的 Kind 不同。
type Literal struct {
	Value types.Value
}

// ColumnRef 是列引用。Table 为空表示未限定（执行层按当前作用域解析）。
type ColumnRef struct {
	Table string
	Name  string
}

// StarExpr 是 "*" 或 "t.*"。只出现在 SELECT 列表里；
// COUNT(*) 是 FuncExpr 的 Star 字段，不是这个节点。
type StarExpr struct {
	Table string
}

// UnaryOp 是一元运算符。
type UnaryOp uint8

const (
	OpNot UnaryOp = iota // NOT
	OpNeg                // 一元负号
	OpPos                // 一元正号（恒等，保留是为了不丢掉源码信息）
)

func (op UnaryOp) String() string {
	switch op {
	case OpNot:
		return "NOT"
	case OpNeg:
		return "-"
	case OpPos:
		return "+"
	}
	return "?"
}

// UnaryExpr 是一元运算。
type UnaryExpr struct {
	Op UnaryOp
	X  Expr
}

// BinaryOp 是二元运算符。
type BinaryOp uint8

const (
	OpAnd   BinaryOp = iota // AND
	OpOr                    // OR
	OpEq                    // =
	OpNe                    // != / <>
	OpLt                    // <
	OpLe                    // <=
	OpGt                    // >
	OpGe                    // >=
	OpPlus                  // +
	OpMinus                 // -
	OpMul                   // *
	OpDiv                   // /
	OpMod                   // %
)

func (op BinaryOp) String() string {
	switch op {
	case OpAnd:
		return "AND"
	case OpOr:
		return "OR"
	case OpEq:
		return "="
	case OpNe:
		return "!="
	case OpLt:
		return "<"
	case OpLe:
		return "<="
	case OpGt:
		return ">"
	case OpGe:
		return ">="
	case OpPlus:
		return "+"
	case OpMinus:
		return "-"
	case OpMul:
		return "*"
	case OpDiv:
		return "/"
	case OpMod:
		return "%"
	}
	return "?"
}

// BinaryExpr 是二元运算。
type BinaryExpr struct {
	Op BinaryOp
	L  Expr
	R  Expr
}

// IsNullExpr 是 IS NULL / IS NOT NULL（DESIGN §4.4 的 NULL 语义入口）。
type IsNullExpr struct {
	X   Expr
	Not bool
}

// InExpr 是 IN (...) / NOT IN (...)。
type InExpr struct {
	X    Expr
	List []Expr
	Not  bool
}

// BetweenExpr 是 BETWEEN lo AND hi / NOT BETWEEN。闭区间。
type BetweenExpr struct {
	X   Expr
	Lo  Expr
	Hi  Expr
	Not bool
}

// LikeExpr 是 LIKE / NOT LIKE。Pattern 是表达式（通常是字符串字面量），
// 编译成正则在执行层做（DESIGN §6：一次编译、计划内缓存）。
type LikeExpr struct {
	X       Expr
	Pattern Expr
	Not     bool
}

// FuncExpr 是函数调用。本版本只有聚合函数 COUNT/SUM/AVG/MIN/MAX，
// 名字大小写不敏感、存成大写；Star 为 true 表示 COUNT(*) 这种写法。
type FuncExpr struct {
	Name string
	Args []Expr
	Star bool
}

func (*Literal) expr()     {}
func (*ColumnRef) expr()   {}
func (*StarExpr) expr()    {}
func (*UnaryExpr) expr()   {}
func (*BinaryExpr) expr()  {}
func (*IsNullExpr) expr()  {}
func (*InExpr) expr()      {}
func (*BetweenExpr) expr() {}
func (*LikeExpr) expr()    {}
func (*FuncExpr) expr()    {}
