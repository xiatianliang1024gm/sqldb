package exec

// 计划构建：AST → 绑定 → 算子树（DESIGN §2 的 exec 层入口）。
//
// M4 起覆盖 §7 的完整节点集：Scan/Filter/Project（M2 主链）+ Sort/HashAgg/
// Limit/NLJoin/DISTINCT（M4），树形见 Build 的注释。
//
// 唯一做的优化是主键范围下推（§7.3）：把 WHERE 拆成合取项后，凡是形如
// pk <op> 字面量（op ∈ {=, <, <=, >, >=}）的项直接折算成所属表 Scan 的
// [LowerBound, UpperBound)，其余留在 Filter 里逐行求值。JOIN 时各表各自动作；
// ON 不参与下推，在连接循环里逐行求值（M4 的取舍，见 §12）。

import (
	"bytes"
	"fmt"

	"github.com/xiatianliang1024gm/kvdb"
	"github.com/xiatianliang1024gm/sqldb/internal/catalog"
	"github.com/xiatianliang1024gm/sqldb/internal/encoding"
	"github.com/xiatianliang1024gm/sqldb/internal/parser"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// Plan 是一条构建好的查询计划。
type Plan struct {
	Columns []string // 输出列名（含别名）
	Root    Operator

	scans []*ScanNode // 全部表扫描（JOIN 时含各内表），RowsScanned 可观测
}

// RowsScanned 返回所有 Scan 实际读过的 KV 行数之和。下推是否生效，
// 用这个数字说话：`WHERE id > 90` 在 100 行的表上应读到 9 行，而不是 100 行。
// 单表查询就是 M2/M3 的语义；JOIN 是各表之和。
func (p *Plan) RowsScanned() int64 {
	var n int64
	for _, s := range p.scans {
		n += s.Scanned()
	}
	return n
}

// tableBounds 是一张表经主键下推后的扫描区间（半开）。
type tableBounds struct {
	lower, upper []byte
}

// Build 把一条 SELECT 绑定到 catalog 并生成计划（M4 起是 §7 的完整节点集）。
// 生成的算子树形状：
//
//	Scan{×N} → NLJoin{×N-1} → Filter(WHERE 残留)
//	  → [聚合查询] HashAgg → Filter(HAVING)
//	  → Sort（有 ORDER BY 时） → Project → Distinct → Limit
//
// kv 用 kvdb.Reader 而不是 *kvdb.DB：将来想基于快照构建一致的查询计划，
// 这里的代码一行不用改（kvdb 的 DB 与 Snapshot 本就共享 Reader 契约）。
func Build(kv kvdb.Reader, cat *catalog.Catalog, stmt *parser.SelectStmt) (*Plan, error) {
	// 1. 作用域：FROM 表在前，JOIN 表按出现顺序接在后面（左深树）。
	b := &binder{}
	refs := make([]parser.TableRef, 0, len(stmt.Joins)+1)
	refs = append(refs, stmt.From)
	for _, j := range stmt.Joins {
		refs = append(refs, j.Table)
	}
	var width int // 连接行的总列宽 = 各表列数之和
	for _, r := range refs {
		t, ok := cat.GetTable(r.Name)
		if !ok {
			return nil, fmt.Errorf("no such table: %s", r.Name)
		}
		alias := r.Alias
		if alias == "" {
			alias = r.Name
		}
		for _, s := range b.scopes {
			if s.alias == alias {
				// 自连接必须起别名；别名撞上另一个表名同样在这儿拦下。
				return nil, fmt.Errorf("duplicate table name/alias %q in FROM", alias)
			}
		}
		b.scopes = append(b.scopes, scopeEntry{alias: alias, table: t, base: width})
		width += len(t.Columns)
	}

	// 2. SELECT 列表：展开 * / t.*（JOIN 时按 FROM 顺序展开全部表），
	//    确定输出列名。
	items := make([]parser.Expr, 0, len(stmt.Items))
	names := make([]string, 0, len(stmt.Items))
	for _, item := range stmt.Items {
		if star, ok := item.Expr.(*parser.StarExpr); ok {
			expanded := false
			for _, s := range b.scopes {
				if star.Table != "" && star.Table != s.alias {
					continue
				}
				for _, col := range s.table.Columns {
					items = append(items, &parser.ColumnRef{Table: s.alias, Name: col.Name})
					names = append(names, col.Name)
				}
				expanded = true
			}
			if !expanded {
				return nil, fmt.Errorf("no such table %q for star", star.Table)
			}
			continue
		}
		items = append(items, item.Expr)
		names = append(names, itemName(item))
	}

	// 3. WHERE：拆合取项 → 按所属表各自尝试主键下推 → 残留项合成过滤器。
	//    必须在聚合上下文挂上之前绑定（WHERE 永远作用于基表行）。
	bounds, residual, err := pushdownWhere(b, stmt.Where)
	if err != nil {
		return nil, err
	}

	// 4. 扫描 + 连接树。每张表一个 ScanNode（带上各自的下推区间）；
	//    ON 的绑定作用域只含已出现的表 —— 引用后面才 JOIN 的表是错的。
	plan := &Plan{}
	var root Operator
	for i, s := range b.scopes {
		scan := &ScanNode{
			kv:     kv,
			table:  s.table,
			prefix: encoding.RowKeyPrefix(s.table.ID),
			lower:  bounds[i].lower,
			upper:  bounds[i].upper,
			kinds:  s.table.Kinds(),
		}
		plan.scans = append(plan.scans, scan)
		if i == 0 {
			root = scan
			continue
		}
		onBinder := &binder{scopes: b.scopes[:i+1]}
		on, err := onBinder.bind(stmt.Joins[i-1].On)
		if err != nil {
			return nil, fmt.Errorf("JOIN %s: %w", s.alias, err)
		}
		root = &NLJoinNode{outer: root, inner: scan, on: on, outerWidth: s.base}
	}
	var pred boundExpr
	for _, be := range residual {
		if pred == nil {
			pred = be
			continue
		}
		pred = &binaryExpr{op: parser.OpAnd, l: pred, r: be}
	}
	if pred != nil {
		root = &FilterNode{child: root, pred: pred, label: "WHERE"}
	}

	// 5. ORDER BY 键的语法层解析：别名 / 输出列名 / 序号 先翻译成对应的
	//    SELECT 表达式（它们语义上引用输出列），剩下的按自由表达式处理。
	type orderKey struct {
		expr parser.Expr
		desc bool
	}
	var orderKeys []orderKey
	for _, oi := range stmt.OrderBy {
		expr, fromOutput := oi.Expr, false
		switch e := expr.(type) {
		case *parser.Literal: // ORDER BY 2：输出列序号（SQL 标准语义）
			if e.Value.Kind != types.Int {
				return nil, fmt.Errorf("ORDER BY expects a column, alias, ordinal or expression, got %s literal", e.Value.Kind)
			}
			n := e.Value.I
			if n < 1 || n > int64(len(items)) {
				return nil, fmt.Errorf("ORDER BY ordinal %d out of range (1..%d)", n, len(items))
			}
			expr, fromOutput = items[n-1], true
		case *parser.ColumnRef: // 无限定列名命中输出列名 → 按输出列处理
			if e.Table == "" {
				for i, name := range names {
					if name == e.Name {
						expr, fromOutput = items[i], true
						break
					}
				}
			}
		}
		// DISTINCT 下非输出列的排序键无处求值（去重后基表行已消失），
		// 与 SQL 标准一致：要求 ORDER BY 表达式出现在 SELECT 列表里。
		if stmt.Distinct && !fromOutput {
			de := describeExpr(expr)
			found := false
			for _, name := range names {
				if name == de {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("ORDER BY expression %s must appear in the SELECT list when SELECT DISTINCT", de)
			}
		}
		orderKeys = append(orderKeys, orderKey{expr: expr, desc: oi.Desc})
	}

	// 6. 聚合判定：有 GROUP BY / HAVING，或任何输出、排序项里出现聚合
	//    调用，就是聚合查询（parser 只为聚合函数产 FuncExpr）。
	isAgg := len(stmt.GroupBy) > 0 || stmt.Having != nil
	if !isAgg {
		for _, it := range items {
			if containsAggExpr(it) {
				isAgg = true
				break
			}
		}
	}
	if !isAgg {
		for _, ok := range orderKeys {
			if containsAggExpr(ok.expr) {
				isAgg = true
				break
			}
		}
	}

	// 7. 绑定输出 / HAVING / 排序键 + 组装算子树。
	var itemExprs []boundExpr
	var keyExprs []boundExpr
	var descs []bool
	if isAgg {
		// 7a. GROUP BY 表达式：禁止聚合、不限制列；收集它们引用的列偏移
		//     作为"允许的非聚合列"集合 —— GROUP BY 引用到的列在组内是
		//     常量，这是覆盖校验的依据（不做主键函数依赖推导）。
		covered := make(map[int]bool)
		groupExprs := make([]boundExpr, 0, len(stmt.GroupBy))
		b.ab = &aggContext{collect: nil}
		for i, g := range stmt.GroupBy {
			be, err := b.bind(g)
			if err != nil {
				return nil, fmt.Errorf("GROUP BY item %d: %w", i+1, err)
			}
			groupExprs = append(groupExprs, be)
			collectColumnRefs(b, g, covered)
		}
		b.ab = nil

		// 7b. 输出 / HAVING / ORDER BY 统一在伪行作用域绑定：列引用落在
		//     代表行（组内第一行），聚合调用落在伪行后缀的聚合槽位。
		var aggs []aggSpec
		b.ab = &aggContext{
			collect:  &aggs,
			aggIndex: make(map[string]int),
			covered:  covered,
			width:    width,
		}
		itemExprs = make([]boundExpr, len(items))
		for i, item := range items {
			be, err := b.bind(item)
			if err != nil {
				return nil, fmt.Errorf("select item %q: %w", names[i], err)
			}
			itemExprs[i] = be
		}
		var having boundExpr
		if stmt.Having != nil {
			hb, err := b.bind(stmt.Having)
			if err != nil {
				return nil, fmt.Errorf("HAVING: %w", err)
			}
			having = hb
		}
		keyExprs = make([]boundExpr, len(orderKeys))
		descs = make([]bool, len(orderKeys))
		for i, ok := range orderKeys {
			be, err := b.bind(ok.expr)
			if err != nil {
				return nil, fmt.Errorf("ORDER BY: %w", err)
			}
			keyExprs[i], descs[i] = be, ok.desc
		}
		b.ab = nil

		// 7c. 聚合子树：HashAgg → HAVING。
		root = &HashAggNode{child: root, groupExprs: groupExprs, aggs: aggs, width: width}
		if having != nil {
			root = &FilterNode{child: root, pred: having, label: "HAVING"}
		}
	} else {
		// 非聚合查询：输出与排序键都直接绑在基表（连接）行上。
		itemExprs = make([]boundExpr, len(items))
		for i, item := range items {
			be, err := b.bind(item)
			if err != nil {
				return nil, fmt.Errorf("select item %q: %w", names[i], err)
			}
			itemExprs[i] = be
		}
		keyExprs = make([]boundExpr, len(orderKeys))
		descs = make([]bool, len(orderKeys))
		for i, ok := range orderKeys {
			be, err := b.bind(ok.expr)
			if err != nil {
				return nil, fmt.Errorf("ORDER BY: %w", err)
			}
			keyExprs[i], descs[i] = be, ok.desc
		}
	}

	// 8. Sort → Project → Distinct → Limit。Sort 永远在 Project 之下
	//    （键在基表行 / 伪行上求值）：DISTINCT 保留首次出现顺序，去重
	//    不会打乱排序结果 —— 前提是排序键对应输出列，第 5 步已检查。
	if len(keyExprs) > 0 {
		root = &SortNode{child: root, keys: keyExprs, desc: descs}
	}
	root = &ProjectNode{child: root, exprs: itemExprs, Names: names}
	if stmt.Distinct {
		root = &DistinctNode{child: root}
	}
	if stmt.Limit != nil || stmt.Offset != nil {
		ln := &LimitNode{child: root, limit: -1}
		if stmt.Offset != nil {
			ln.offset = *stmt.Offset
		}
		if stmt.Limit != nil {
			ln.limit = *stmt.Limit
		}
		root = ln
	}

	plan.Columns = names
	plan.Root = root
	return plan, nil
}

// pushdownWhere 处理 WHERE：拆合取项 → 主键谓词按所属表折算成
// [lower, upper)（每个作用域条目一组界）→ 其余绑定成残留谓词（逐行求值）。
// SELECT 的计划构建和 UPDATE/DELETE 的写扫描共用这条路径 —— 写路径必须和
// 查询走同一套下推逻辑（DESIGN §8），否则"改了哪几行"和"看见哪几行"会对不上。
//
// JOIN 时各表各自动作：谓词里的列解析到哪张表，区间就折算到哪张表的
// 扫描上。ON 不参与下推 —— 它在连接循环里逐行求值（M4 的取舍，见 §12）。
func pushdownWhere(b *binder, where parser.Expr) (bounds []tableBounds, residual []boundExpr, err error) {
	bounds = make([]tableBounds, len(b.scopes))
	var conjuncts []parser.Expr
	if where != nil {
		splitConjuncts(where, &conjuncts)
	}

	for _, c := range conjuncts {
		entry, op, lit, ok := matchPushdown(b, c)
		if !ok {
			be, err := b.bind(c)
			if err != nil {
				return nil, nil, err
			}
			residual = append(residual, be)
			continue
		}
		enc, err := encoding.EncodeKey(lit)
		if err != nil {
			return nil, nil, fmt.Errorf("push down primary key bound: %w", err)
		}
		// 行键 = 表前缀 + 保序主键编码。四种比较各自折算成半开区间的
		// 一端；区间推导依赖编码的前缀无歧义（DESIGN §4.2 三铁律），
		// 具体正确性论证见 matchPushdown 下方的注释。
		prefix := encoding.RowKeyPrefix(b.scopes[entry].table.ID)
		k := append(append([]byte{}, prefix...), enc...)
		point := append(append([]byte{}, k...), 0x00) // "第一个大于 k 的键"
		bd := &bounds[entry]
		switch op {
		case parser.OpEq: // 单点：[k, k+00) —— 没有键能落在这两者之间
			bd.lower = maxBytes(bd.lower, k)
			bd.upper = minBytes(bd.upper, point)
		case parser.OpGt: // 严格大于：从 k 的后继起（含端点 k 本身被排除）
			bd.lower = maxBytes(bd.lower, point)
		case parser.OpGe:
			bd.lower = maxBytes(bd.lower, k)
		case parser.OpLt:
			bd.upper = minBytes(bd.upper, k)
		case parser.OpLe: // 含端点：上界取 k 的后继（不含）
			bd.upper = minBytes(bd.upper, point)
		}
	}
	return bounds, residual, nil
}

// splitConjuncts 把 WHERE 树按 AND 拍平。OR 不拆 —— 它不能独立折算成区间。
func splitConjuncts(e parser.Expr, out *[]parser.Expr) {
	if be, ok := e.(*parser.BinaryExpr); ok && be.Op == parser.OpAnd {
		splitConjuncts(be.L, out)
		splitConjuncts(be.R, out)
		return
	}
	*out = append(*out, e)
}

// flipOp 翻转比较方向（字面量在左边时用）。
func flipOp(op parser.BinaryOp) parser.BinaryOp {
	switch op {
	case parser.OpLt:
		return parser.OpGt
	case parser.OpLe:
		return parser.OpGe
	case parser.OpGt:
		return parser.OpLt
	case parser.OpGe:
		return parser.OpLe
	}
	return op // = 和 != 对称
}

// matchPushdown 识别 "pk <op> 字面量"（两种书写顺序都认），返回谓词
// 所属的作用域条目（JOIN 下各表各自动作）。
//
// 两条保守限制，都是为了正确性：
//   - 字面量类型必须与主键列类型**完全一致**。INT 列配 FLOAT 字面量这种
//     数值混比，INT/FLOAT 的标签字节（0x01/0x02）不同族，混进区间会错序 ——
//     这类谓词留在 Filter 里由求值器升格处理（顺便得到该有的类型错误语义）。
//   - != 不下推：它不是区间。
//
// 区间端点的正确性依赖保序编码的前缀无歧义（DESIGN §4.2）：任何其他主键的
// 编码既不是 encode(v) 的前缀、也不以它为前缀，于是 k+00x 恰好是
// "第一个严格大于 k 的可能行键"，点查 [k, k+00) 与 `<=` 的含端点上界都成立。
func matchPushdown(b *binder, e parser.Expr) (entry int, op parser.BinaryOp, lit types.Value, ok bool) {
	be, isBinary := e.(*parser.BinaryExpr)
	if !isBinary || be.Op == parser.OpNe {
		return 0, 0, types.Value{}, false
	}
	op = be.Op
	var col *parser.ColumnRef
	var literal *parser.Literal
	switch l := be.L.(type) {
	case *parser.ColumnRef:
		if r, ok := be.R.(*parser.Literal); ok {
			col, literal = l, r
		}
	case *parser.Literal:
		if r, ok := be.R.(*parser.ColumnRef); ok {
			col, literal = r, l
			op = flipOp(op)
		}
	}
	if col == nil || literal.Value.IsNull() {
		return 0, 0, types.Value{}, false // NULL 不参与比较，语义交给求值器（结果必为 NULL）
	}
	idx, err := b.resolve(col.Table, col.Name)
	if err != nil {
		return 0, 0, types.Value{}, false // 列名错误留给残留绑定期报
	}
	ent := b.scopeForOffset(idx)
	if ent < 0 {
		return 0, 0, types.Value{}, false
	}
	t := b.scopes[ent].table
	pk := t.PrimaryKeyIndex()
	if idx-b.scopes[ent].base != pk || literal.Value.Kind != t.Columns[pk].Kind {
		return 0, 0, types.Value{}, false // 不是主键列，或类型不一致（见上方限制）
	}
	return ent, op, literal.Value, true
}

// containsAggExpr 报告表达式里有没有聚合调用。parser 只为聚合函数产
// FuncExpr（COUNT/SUM/AVG/MIN/MAX），见到 FuncExpr 即可判定。
func containsAggExpr(e parser.Expr) bool {
	switch e := e.(type) {
	case *parser.FuncExpr:
		return true
	case *parser.UnaryExpr:
		return containsAggExpr(e.X)
	case *parser.BinaryExpr:
		return containsAggExpr(e.L) || containsAggExpr(e.R)
	case *parser.IsNullExpr:
		return containsAggExpr(e.X)
	case *parser.InExpr:
		if containsAggExpr(e.X) {
			return true
		}
		for _, item := range e.List {
			if containsAggExpr(item) {
				return true
			}
		}
	case *parser.BetweenExpr:
		return containsAggExpr(e.X) || containsAggExpr(e.Lo) || containsAggExpr(e.Hi)
	case *parser.LikeExpr:
		return containsAggExpr(e.X) || containsAggExpr(e.Pattern)
	}
	return false
}

// collectColumnRefs 把 AST 里出现的列引用（解析成偏移）收进集合。
// GROUP BY 表达式引用到的列在组内是常量，允许出现在非聚合位置 ——
// 这是"非聚合列必须被 GROUP BY 覆盖"校验的依据。解析失败的偏移
// 会在绑定期报同样的错，这里静默跳过。
func collectColumnRefs(b *binder, e parser.Expr, out map[int]bool) {
	switch e := e.(type) {
	case *parser.ColumnRef:
		if idx, err := b.resolve(e.Table, e.Name); err == nil {
			out[idx] = true
		}
	case *parser.UnaryExpr:
		collectColumnRefs(b, e.X, out)
	case *parser.BinaryExpr:
		collectColumnRefs(b, e.L, out)
		collectColumnRefs(b, e.R, out)
	case *parser.IsNullExpr:
		collectColumnRefs(b, e.X, out)
	case *parser.InExpr:
		collectColumnRefs(b, e.X, out)
		for _, item := range e.List {
			collectColumnRefs(b, item, out)
		}
	case *parser.BetweenExpr:
		collectColumnRefs(b, e.X, out)
		collectColumnRefs(b, e.Lo, out)
		collectColumnRefs(b, e.Hi, out)
	case *parser.LikeExpr:
		collectColumnRefs(b, e.X, out)
		collectColumnRefs(b, e.Pattern, out)
	case *parser.FuncExpr:
		if !e.Star {
			for _, a := range e.Args {
				collectColumnRefs(b, a, out)
			}
		}
	}
}

func maxBytes(a, b []byte) []byte {
	if a == nil || bytes.Compare(a, b) < 0 {
		return b
	}
	return a
}

func minBytes(a, b []byte) []byte {
	if a == nil || bytes.Compare(a, b) > 0 {
		return b
	}
	return a
}

// itemName 推导 SELECT 项的输出列名：别名优先；列引用用列名；
// 其余表达式用可读的源码式描述（describeExpr）。
func itemName(item parser.SelectItem) string {
	if item.Alias != "" {
		return item.Alias
	}
	return describeExpr(item.Expr)
}

// describeExpr 把表达式渲染回接近源码的字符串。只用于输出列命名，
// 不追求与原始 SQL 逐字符一致。
func describeExpr(e parser.Expr) string {
	switch e := e.(type) {
	case *parser.Literal:
		if e.Value.Kind == types.Text {
			return "'" + e.Value.S + "'"
		}
		return e.Value.String()
	case *parser.ColumnRef:
		if e.Table != "" {
			return e.Table + "." + e.Name
		}
		return e.Name
	case *parser.StarExpr:
		if e.Table != "" {
			return e.Table + ".*"
		}
		return "*"
	case *parser.UnaryExpr:
		return e.Op.String() + " " + describeExpr(e.X)
	case *parser.BinaryExpr:
		return describeExpr(e.L) + " " + e.Op.String() + " " + describeExpr(e.R)
	case *parser.IsNullExpr:
		if e.Not {
			return describeExpr(e.X) + " IS NOT NULL"
		}
		return describeExpr(e.X) + " IS NULL"
	case *parser.InExpr:
		s := describeExpr(e.X) + " IN ("
		for i, item := range e.List {
			if i > 0 {
				s += ", "
			}
			s += describeExpr(item)
		}
		return s + ")"
	case *parser.BetweenExpr:
		return describeExpr(e.X) + " BETWEEN " + describeExpr(e.Lo) + " AND " + describeExpr(e.Hi)
	case *parser.LikeExpr:
		return describeExpr(e.X) + " LIKE " + describeExpr(e.Pattern)
	case *parser.FuncExpr:
		if e.Star {
			return e.Name + "(*)"
		}
		s := e.Name + "("
		for i, a := range e.Args {
			if i > 0 {
				s += ", "
			}
			s += describeExpr(a)
		}
		return s + ")"
	}
	return "?"
}
