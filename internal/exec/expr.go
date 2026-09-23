package exec

// 表达式的"绑定"与"求值"（DESIGN §2：exec 把 AST 绑定到 catalog，名字 → 列偏移）。
//
// 绑定（bind）把 parser 的 AST 翻译成内部表达式树：列名在这里落成列偏移，
// LIKE 的正则在这里编译一次、缓存于计划内（DESIGN §6）。翻译一次、逐行求值 ——
// 每行都拿 AST 重新解析列名是最亏的做法。
//
// 求值（eval）实现 SQL 的 NULL 语义：比较和算术遇 NULL 得 NULL（传染），
// AND/OR/NOT 用三值逻辑（types 包），WHERE 子句里 NULL 一律视为不通过
// —— 那条规则由 Filter 节点执行，这里只负责把 NULL 如实往上送。

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/xiatianliang1024gm/sqldb/internal/catalog"
	"github.com/xiatianliang1024gm/sqldb/internal/parser"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// boundExpr 是求值期的表达式节点。eval 的入参是一行的全部列值
// （按 schema 顺序），出参是 SQL 值 —— Bool 或 NULL 表示谓词结果。
type boundExpr interface {
	eval(row []types.Value) (types.Value, error)
}

// ── 各节点：与 AST 一一对应，只是列名换成了偏移 ────────────────────

type litExpr struct{ v types.Value }

func (e litExpr) eval([]types.Value) (types.Value, error) { return e.v, nil }

type colExpr struct{ idx int }

func (e colExpr) eval(row []types.Value) (types.Value, error) { return row[e.idx], nil }

type unaryExpr struct {
	op parser.UnaryOp
	x  boundExpr
}

func (e *unaryExpr) eval(row []types.Value) (types.Value, error) {
	v, err := e.x.eval(row)
	if err != nil {
		return types.Value{}, err
	}
	switch e.op {
	case parser.OpNot:
		return types.LogicNot(v), nil
	case parser.OpNeg:
		if v.IsNull() {
			return types.NullValue(), nil
		}
		switch v.Kind {
		case types.Int:
			return types.IntValue(-v.I), nil
		case types.Float:
			return types.FloatValue(-v.F), nil
		}
		return types.Value{}, fmt.Errorf("cannot negate %s", v.Kind)
	case parser.OpPos: // 一元正号是恒等
		return v, nil
	}
	return types.Value{}, fmt.Errorf("unknown unary op %s", e.op)
}

type binaryExpr struct {
	op   parser.BinaryOp
	l, r boundExpr
}

func (e *binaryExpr) eval(row []types.Value) (types.Value, error) {
	lv, err := e.l.eval(row)
	if err != nil {
		return types.Value{}, err
	}
	rv, err := e.r.eval(row)
	if err != nil {
		return types.Value{}, err
	}
	return evalBinary(e.op, lv, rv)
}

// evalBinary 分派二元运算。AND/OR 不短路：两个操作数都求值
// （短路只省性能，不改变三值逻辑的结果；不做短路是为了数据流一目了然）。
func evalBinary(op parser.BinaryOp, l, r types.Value) (types.Value, error) {
	switch op {
	case parser.OpAnd, parser.OpOr:
		if err := requireLogic(l, op.String()); err != nil {
			return types.Value{}, err
		}
		if err := requireLogic(r, op.String()); err != nil {
			return types.Value{}, err
		}
		if op == parser.OpAnd {
			return types.LogicAnd(l, r), nil
		}
		return types.LogicOr(l, r), nil
	case parser.OpEq, parser.OpNe, parser.OpLt, parser.OpLe, parser.OpGt, parser.OpGe:
		return compareOp(op, l, r)
	case parser.OpPlus, parser.OpMinus, parser.OpMul, parser.OpDiv, parser.OpMod:
		return arith(op, l, r)
	}
	return types.Value{}, fmt.Errorf("unknown binary op %s", op)
}

func requireLogic(v types.Value, op string) error {
	if v.Kind == types.Bool || v.Kind == types.Null {
		return nil
	}
	return fmt.Errorf("%s requires boolean operands, got %s", op, v.Kind)
}

// compareOp 实现六个比较运算符：NULL 传染，其余交给 types.Compare。
func compareOp(op parser.BinaryOp, l, r types.Value) (types.Value, error) {
	if l.IsNull() || r.IsNull() {
		return types.NullValue(), nil
	}
	c, err := types.Compare(l, r)
	if err != nil {
		return types.Value{}, err
	}
	var res bool
	switch op {
	case parser.OpEq:
		res = c == 0
	case parser.OpNe:
		res = c != 0
	case parser.OpLt:
		res = c < 0
	case parser.OpLe:
		res = c <= 0
	case parser.OpGt:
		res = c > 0
	case parser.OpGe:
		res = c >= 0
	}
	return types.BoolValue(res), nil
}

// arith 实现四则运算与取模。整数/浮点混比升格为浮点（与 Compare 一致）；
// 整数溢出按 Go 语义回绕 —— 教学项目如实接受，不装作处理了。
func arith(op parser.BinaryOp, l, r types.Value) (types.Value, error) {
	if l.IsNull() || r.IsNull() {
		return types.NullValue(), nil
	}
	numeric := func(v types.Value) bool { return v.Kind == types.Int || v.Kind == types.Float }
	if !numeric(l) || !numeric(r) {
		return types.Value{}, fmt.Errorf("cannot apply %s to %s and %s", op, l.Kind, r.Kind)
	}
	if l.Kind == types.Float || r.Kind == types.Float {
		lf, rf := toFloat(l), toFloat(r)
		switch op {
		case parser.OpPlus:
			return types.FloatValue(lf + rf), nil
		case parser.OpMinus:
			return types.FloatValue(lf - rf), nil
		case parser.OpMul:
			return types.FloatValue(lf * rf), nil
		case parser.OpDiv:
			if rf == 0 {
				return types.Value{}, errors.New("division by zero")
			}
			return types.FloatValue(lf / rf), nil
		case parser.OpMod:
			return types.Value{}, fmt.Errorf("MOD requires integer operands, got %s and %s", l.Kind, r.Kind)
		}
	}
	switch op {
	case parser.OpPlus:
		return types.IntValue(l.I + r.I), nil
	case parser.OpMinus:
		return types.IntValue(l.I - r.I), nil
	case parser.OpMul:
		return types.IntValue(l.I * r.I), nil
	case parser.OpDiv:
		if r.I == 0 {
			return types.Value{}, errors.New("division by zero")
		}
		return types.IntValue(l.I / r.I), nil
	case parser.OpMod:
		if r.I == 0 {
			return types.Value{}, errors.New("division by zero")
		}
		return types.IntValue(l.I % r.I), nil
	}
	return types.Value{}, fmt.Errorf("unknown arithmetic op %s", op)
}

type isNullExpr struct {
	x   boundExpr
	not bool
}

func (e *isNullExpr) eval(row []types.Value) (types.Value, error) {
	v, err := e.x.eval(row)
	if err != nil {
		return types.Value{}, err
	}
	res := v.IsNull()
	if e.not {
		res = !res
	}
	return types.BoolValue(res), nil // IS [NOT] NULL 永远是 Bool，从不传染
}

type inExpr struct {
	x    boundExpr
	list []boundExpr
	not  bool
}

func (e *inExpr) eval(row []types.Value) (types.Value, error) {
	x, err := e.x.eval(row)
	if err != nil {
		return types.Value{}, err
	}
	if x.IsNull() {
		return types.NullValue(), nil
	}
	// SQL 的 IN 三值语义：命中 → true；没命中但列表里有 NULL 或 x 本身是
	// NULL → NULL；否则 false。NOT IN 是对整个结果做三值取反。
	inner := types.BoolValue(false)
	sawNull := false
	for _, item := range e.list {
		v, err := item.eval(row)
		if err != nil {
			return types.Value{}, fmt.Errorf("IN: %w", err)
		}
		if v.IsNull() {
			sawNull = true
			continue
		}
		c, err := types.Compare(x, v)
		if err != nil {
			return types.Value{}, fmt.Errorf("IN: %w", err)
		}
		if c == 0 {
			inner = types.BoolValue(true)
			break
		}
	}
	if !inner.B && sawNull {
		inner = types.NullValue()
	}
	if e.not {
		return types.LogicNot(inner), nil
	}
	return inner, nil
}

type betweenExpr struct {
	x, lo, hi boundExpr
	not       bool
}

func (e *betweenExpr) eval(row []types.Value) (types.Value, error) {
	x, err := e.x.eval(row)
	if err != nil {
		return types.Value{}, err
	}
	lo, err := e.lo.eval(row)
	if err != nil {
		return types.Value{}, err
	}
	hi, err := e.hi.eval(row)
	if err != nil {
		return types.Value{}, err
	}
	ge, err := compareOp(parser.OpGe, x, lo) // x >= lo
	if err != nil {
		return types.Value{}, fmt.Errorf("BETWEEN: %w", err)
	}
	le, err := compareOp(parser.OpLe, x, hi) // x <= hi
	if err != nil {
		return types.Value{}, fmt.Errorf("BETWEEN: %w", err)
	}
	inner := types.LogicAnd(ge, le) // 闭区间，NULL 交给三值逻辑
	if e.not {
		return types.LogicNot(inner), nil
	}
	return inner, nil
}

type likeExpr struct {
	x   boundExpr
	re  *regexp.Regexp
	not bool
}

func (e *likeExpr) eval(row []types.Value) (types.Value, error) {
	x, err := e.x.eval(row)
	if err != nil {
		return types.Value{}, err
	}
	if x.IsNull() {
		return types.NullValue(), nil
	}
	if x.Kind != types.Text {
		return types.Value{}, fmt.Errorf("LIKE requires TEXT operand, got %s", x.Kind)
	}
	m := e.re.MatchString(x.S)
	if e.not {
		m = !m
	}
	return types.BoolValue(m), nil
}

// toFloat 把数值类型的值升格为 float64（与 types.Compare 的混比规则一致）。
// 调用方保证 v 是 Int/Float。
func toFloat(v types.Value) float64 {
	if v.Kind == types.Int {
		return float64(v.I)
	}
	return v.F
}

// likeToRegex 把 LIKE 模式（% 任意串、_ 单字符）编译成锚定正则。
// 其余字符 QuoteMeta 转义 —— 用户数据里出现 . * ? 不会被当成正则元字符。
// (?s) 让 _ 也能匹配换行符。
func likeToRegex(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString("(?s)^")
	for _, r := range pattern {
		switch r {
		case '%':
			sb.WriteString(".*")
		case '_':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

// ── 绑定：AST → boundExpr ────────────────────────────────────────

// scopeEntry 是 FROM/JOIN 里的一个作用域：一张表、它的限定名（别名优先，
// 否则表名）、以及它的第一列在连接行里的偏移基址。单表语句就是只有一个
// 条目的特例 —— 写路径（INSERT/UPDATE/DELETE）继续走这个特例。
type scopeEntry struct {
	alias string
	table *catalog.Table
	base  int // 该表第一列在（可能的）连接行里的全局偏移
}

// binder 持有当前语句的作用域列表。JOIN 时各表按 FROM 出现顺序排列，
// 连接行 = 各表行首尾相接，列偏移基址按同一顺序分配（DESIGN §7.2）。
type binder struct {
	scopes []scopeEntry

	// M4：聚合绑定上下文。非 nil 表示正在绑定聚合查询的 GROUP BY /
	// SELECT / HAVING / ORDER BY 表达式，语义见 aggregate.go。
	ab *aggContext
}

func newBinder(t *catalog.Table, scope string) *binder {
	return &binder{scopes: []scopeEntry{{alias: scope, table: t}}}
}

// resolve 把（可选限定符 + 列名）解析成连接行里的全局列偏移。
// 有限定符：精确匹配某个作用域的限定名；无限定符：跨全部作用域查找，
// 命中多个报 ambiguous（JOIN 下未限定列的歧义必须显式报错，不能猜）。
func (b *binder) resolve(qualifier, name string) (int, error) {
	if qualifier != "" {
		for _, s := range b.scopes {
			if s.alias == qualifier {
				idx := s.table.ColumnIndex(name)
				if idx < 0 {
					return -1, fmt.Errorf("no such column: %s.%s", qualifier, name)
				}
				return s.base + idx, nil
			}
		}
		return -1, fmt.Errorf("no such table %q in FROM", qualifier)
	}
	found, hits := -1, 0
	for _, s := range b.scopes {
		if idx := s.table.ColumnIndex(name); idx >= 0 {
			found, hits = s.base+idx, hits+1
		}
	}
	switch {
	case hits == 0:
		return -1, fmt.Errorf("no such column: %s", name)
	case hits > 1:
		return -1, fmt.Errorf("ambiguous column %q: qualify it with a table name", name)
	}
	return found, nil
}

// scopeForOffset 返回全局列偏移落在哪个作用域条目里（主键下推用：
// 要知道这列是哪张表的主键才能折算那张表的扫描区间）。
func (b *binder) scopeForOffset(offset int) int {
	for i := range b.scopes {
		s := &b.scopes[i]
		if offset >= s.base && offset < s.base+len(s.table.Columns) {
			return i
		}
	}
	return -1
}

func (b *binder) bind(e parser.Expr) (boundExpr, error) {
	switch e := e.(type) {
	case *parser.Literal:
		return litExpr{v: e.Value}, nil
	case *parser.ColumnRef:
		idx, err := b.resolve(e.Table, e.Name)
		if err != nil {
			return nil, err
		}
		// 聚合查询的输出表达式里，非聚合列引用必须被 GROUP BY 覆盖 ——
		// 否则它在组内不是常量，取代表行的值没有语义（见 select.go 的
		// covered 集合构造）。聚合参数不受此限制（inAgg 期间豁免）。
		if b.ab != nil && b.ab.collect != nil && !b.ab.inAgg && !b.ab.covered[idx] {
			return nil, fmt.Errorf("column %s must appear in the GROUP BY clause or be used in an aggregate function", e.Name)
		}
		return colExpr{idx: idx}, nil
	case *parser.StarExpr:
		return nil, errors.New("unexpected * in expression")
	case *parser.UnaryExpr:
		x, err := b.bind(e.X)
		if err != nil {
			return nil, err
		}
		return &unaryExpr{op: e.Op, x: x}, nil
	case *parser.BinaryExpr:
		l, err := b.bind(e.L)
		if err != nil {
			return nil, err
		}
		r, err := b.bind(e.R)
		if err != nil {
			return nil, err
		}
		return &binaryExpr{op: e.Op, l: l, r: r}, nil
	case *parser.IsNullExpr:
		x, err := b.bind(e.X)
		if err != nil {
			return nil, err
		}
		return &isNullExpr{x: x, not: e.Not}, nil
	case *parser.InExpr:
		x, err := b.bind(e.X)
		if err != nil {
			return nil, err
		}
		list := make([]boundExpr, len(e.List))
		for i, item := range e.List {
			if list[i], err = b.bind(item); err != nil {
				return nil, err
			}
		}
		return &inExpr{x: x, list: list, not: e.Not}, nil
	case *parser.BetweenExpr:
		x, err := b.bind(e.X)
		if err != nil {
			return nil, err
		}
		lo, err := b.bind(e.Lo)
		if err != nil {
			return nil, err
		}
		hi, err := b.bind(e.Hi)
		if err != nil {
			return nil, err
		}
		return &betweenExpr{x: x, lo: lo, hi: hi, not: e.Not}, nil
	case *parser.LikeExpr:
		x, err := b.bind(e.X)
		if err != nil {
			return nil, err
		}
		lit, ok := e.Pattern.(*parser.Literal)
		if !ok || lit.Value.Kind != types.Text {
			// 模式必须是常量：正则编译一次、缓存于计划内（DESIGN §6），
			// 逐行动态编译正则的代价和复杂性不值得。
			return nil, errors.New("LIKE pattern must be a string literal")
		}
		re, err := likeToRegex(lit.Value.S)
		if err != nil {
			return nil, fmt.Errorf("bad LIKE pattern: %w", err)
		}
		return &likeExpr{x: x, re: re, not: e.Not}, nil
	case *parser.FuncExpr:
		if b.ab == nil || b.ab.collect == nil {
			return nil, fmt.Errorf("aggregate function %s is not allowed here", e.Name)
		}
		return b.bindAgg(e)
	}
	return nil, fmt.Errorf("unsupported expression %T", e)
}

// bindAgg 绑定一个聚合调用：参数按普通表达式绑定（列引用不受覆盖限制），
// 聚合本身登记进 aggContext 的收集表并替换成 aggRefExpr —— 求值时从
// HashAgg 产出的伪行里取现成的聚合值（见 aggregate.go）。同一个聚合
// 在 SELECT 和 ORDER BY 里各写一遍时去重共享，只累积一份。
func (b *binder) bindAgg(e *parser.FuncExpr) (boundExpr, error) {
	ab := b.ab
	if e.Star && e.Name != "COUNT" {
		return nil, fmt.Errorf("%s(*) is not supported: only COUNT(*)", e.Name)
	}
	if !e.Star && len(e.Args) != 1 {
		return nil, fmt.Errorf("%s takes exactly one argument", e.Name)
	}
	if ab.inAgg {
		return nil, errors.New("aggregate call nested inside another aggregate")
	}
	key := describeExpr(e)
	if slot, ok := ab.aggIndex[key]; ok {
		return aggRefExpr{offset: ab.width + slot}, nil
	}
	var arg boundExpr
	if !e.Star {
		ab.inAgg = true
		a, err := b.bind(e.Args[0])
		ab.inAgg = false
		if err != nil {
			return nil, err
		}
		arg = a
	}
	slot := len(*ab.collect)
	*ab.collect = append(*ab.collect, aggSpec{fn: e.Name, arg: arg, star: e.Star})
	ab.aggIndex[key] = slot
	return aggRefExpr{offset: ab.width + slot}, nil
}
