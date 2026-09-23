package exec

// M4 验收单测（DESIGN §11）：聚合、GROUP BY/HAVING、JOIN、排序、去重、
// 截断，以及贯穿其中的 NULL 三值逻辑。下推可观测性在 JOIN 下依旧钉死
// —— RowsScanned 是各表 Scan 之和，下推收窄哪张表一眼可见。

import (
	"strings"
	"testing"

	"github.com/xiatianliang1024gm/kvdb"
	"github.com/xiatianliang1024gm/sqldb/internal/catalog"
	"github.com/xiatianliang1024gm/sqldb/internal/parser"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// trySelect 是 runSelect 的错误透出版：期望失败的用例拿它拿错误。
func trySelect(t *testing.T, kv *kvdb.DB, cat *catalog.Catalog, sql string) ([][]types.Value, error) {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	plan, err := Build(kv, cat, stmt.(*parser.SelectStmt))
	if err != nil {
		return nil, err
	}
	defer plan.Root.Close()
	var rows [][]types.Value
	for {
		row, err := plan.Root.Next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			return rows, nil
		}
		rows = append(rows, row)
	}
}

// empTable：dept TEXT 里有 NULL、salary 可聚合 —— 给 GROUP BY / HAVING /
// 聚合 NULL 语义留料。主键 name TEXT，扫描顺序即字典序。
func empTable(t *testing.T, kv *kvdb.DB, cat *catalog.Catalog) *catalog.Table {
	t.Helper()
	tab := mustCreate(t, cat, "emp",
		catalog.Column{Name: "name", Kind: types.Text, PrimaryKey: true},
		catalog.Column{Name: "dept", Kind: types.Text},
		catalog.Column{Name: "salary", Kind: types.Int},
	)
	putRow(t, kv, tab, types.TextValue("alice"), types.TextValue("sales"), types.IntValue(100))
	putRow(t, kv, tab, types.TextValue("bob"), types.TextValue("sales"), types.IntValue(200))
	putRow(t, kv, tab, types.TextValue("cat"), types.TextValue("dev"), types.IntValue(300))
	putRow(t, kv, tab, types.TextValue("dan"), types.TextValue("dev"), types.IntValue(100))
	putRow(t, kv, tab, types.TextValue("eve"), types.NullValue(), types.IntValue(50))
	return tab
}

// ── ORDER BY ─────────────────────────────────────────────────────

func TestOrderBy(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 5)

	_, rows := runSelect(t, kv, cat, "SELECT id FROM t ORDER BY id DESC")
	wantInts(t, rows, 0, 4, 3, 2, 1, 0)

	// 表达式键：-id 升序 == id 降序
	_, rows = runSelect(t, kv, cat, "SELECT id FROM t ORDER BY -id")
	wantInts(t, rows, 0, 4, 3, 2, 1, 0)

	// 别名（不带 AS 的输出列名同样命中）
	_, rows = runSelect(t, kv, cat, "SELECT id AS k FROM t ORDER BY k DESC")
	wantInts(t, rows, 0, 4, 3, 2, 1, 0)

	// 序号（SQL 标准的 ORDER BY 2）
	_, rows = runSelect(t, kv, cat, "SELECT id, name FROM t ORDER BY 1 DESC")
	wantInts(t, rows, 0, 4, 3, 2, 1, 0)

	// WHERE + ORDER + LIMIT 组合，下推依旧生效
	plan, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE id > 0 ORDER BY id LIMIT 2")
	if got := plan.RowsScanned(); got != 4 {
		t.Fatalf("scanned %d, want 4", got)
	}
	wantInts(t, rows, 0, 1, 2)

	// 排序键不在输出里：照常工作（Sort 在 Project 之下）
	_, rows = runSelect(t, kv, cat, "SELECT id FROM t ORDER BY score DESC")
	wantInts(t, rows, 0, 4, 3, 2, 1, 0)
}

func TestOrderByNullsFirstAscLastDesc(t *testing.T) {
	kv, cat := newEnv(t)
	tab := mustCreate(t, cat, "n",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "v", Kind: types.Int},
	)
	putRow(t, kv, tab, types.IntValue(1), types.IntValue(30))
	putRow(t, kv, tab, types.IntValue(2), types.NullValue())
	putRow(t, kv, tab, types.IntValue(3), types.IntValue(10))
	putRow(t, kv, tab, types.IntValue(4), types.NullValue())
	putRow(t, kv, tab, types.IntValue(5), types.IntValue(20))
	putRow(t, kv, tab, types.IntValue(6), types.IntValue(30))

	// NULL 视为最小：ASC 最前，DESC 最后（PostgreSQL 默认行为）
	_, rows := runSelect(t, kv, cat, "SELECT id FROM n ORDER BY v")
	wantInts(t, rows, 0, 2, 4, 3, 5, 1, 6)

	_, rows = runSelect(t, kv, cat, "SELECT id FROM n ORDER BY v DESC")
	wantInts(t, rows, 0, 1, 6, 5, 3, 2, 4)

	// 多键：v DESC 平局时按 id ASC（次序由第二键决定）
	_, rows = runSelect(t, kv, cat, "SELECT id FROM n ORDER BY v DESC, id ASC")
	wantInts(t, rows, 0, 1, 6, 5, 3, 2, 4)
}

// ── LIMIT / OFFSET ───────────────────────────────────────────────

func TestLimitOffset(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 10)

	cases := []struct {
		sql  string
		want []int64
	}{
		{"SELECT id FROM t ORDER BY id LIMIT 3", []int64{0, 1, 2}},
		{"SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 8", []int64{8, 9}},
		{"SELECT id FROM t ORDER BY id OFFSET 8", []int64{8, 9}},
		{"SELECT id FROM t ORDER BY id LIMIT 0", nil},
		{"SELECT id FROM t ORDER BY id OFFSET 100", nil},
		{"SELECT id FROM t ORDER BY id LIMIT 100", seq(0, 10)},
	}
	for _, c := range cases {
		_, rows := runSelect(t, kv, cat, c.sql)
		wantInts(t, rows, 0, c.want...)
	}
}

// ── DISTINCT ─────────────────────────────────────────────────────

func distinctTable(t *testing.T, kv *kvdb.DB, cat *catalog.Catalog) *catalog.Table {
	t.Helper()
	tab := mustCreate(t, cat, "d",
		catalog.Column{Name: "a", Kind: types.Int},
		catalog.Column{Name: "b", Kind: types.Text},
	)
	// rowid 表：隐藏主键在最后一位
	putRow(t, kv, tab, types.IntValue(1), types.TextValue("x"), types.IntValue(1))
	putRow(t, kv, tab, types.IntValue(2), types.TextValue("y"), types.IntValue(2))
	putRow(t, kv, tab, types.IntValue(1), types.TextValue("x"), types.IntValue(3))
	putRow(t, kv, tab, types.IntValue(1), types.NullValue(), types.IntValue(4))
	putRow(t, kv, tab, types.IntValue(2), types.TextValue("y"), types.IntValue(5))
	putRow(t, kv, tab, types.NullValue(), types.TextValue("z"), types.IntValue(6))
	return tab
}

func TestDistinct(t *testing.T) {
	kv, cat := newEnv(t)
	distinctTable(t, kv, cat)

	// NULL 与 NULL 去重成一行；去重保留首次出现顺序，排序键为输出列
	_, rows := runSelect(t, kv, cat, "SELECT DISTINCT a FROM d ORDER BY a")
	if len(rows) != 3 || !rows[0][0].IsNull() || rows[1][0].I != 1 || rows[2][0].I != 2 {
		t.Fatalf("distinct a: %v", rows)
	}

	// 多列整行去重：(1,'x') 出现两次算一行，NULL 参与 identity
	_, rows = runSelect(t, kv, cat, "SELECT DISTINCT a, b FROM d")
	if len(rows) != 4 {
		t.Fatalf("distinct a,b: %d rows, want 4: %v", len(rows), rows)
	}
	// 首次出现顺序：1/x, 2/y, 1/NULL, NULL/z
	if rows[0][0].I != 1 || rows[0][1].S != "x" ||
		rows[1][0].I != 2 || rows[1][1].S != "y" ||
		rows[2][0].I != 1 || !rows[2][1].IsNull() ||
		!rows[3][0].IsNull() || rows[3][1].S != "z" {
		t.Fatalf("distinct order: %v", rows)
	}

	// DISTINCT * 全行去重（rowid 各不相同 → 6 行）
	_, rows = runSelect(t, kv, cat, "SELECT DISTINCT * FROM d")
	if len(rows) != 6 {
		t.Fatalf("distinct *: %d rows, want 6", len(rows))
	}
}

func TestDistinctOrderByConstraint(t *testing.T) {
	kv, cat := newEnv(t)
	distinctTable(t, kv, cat)

	// DISTINCT 下排序键必须是输出列 —— 否则去重后无处求值
	for _, sql := range []string{
		"SELECT DISTINCT a FROM d ORDER BY b",
		"SELECT DISTINCT a FROM d ORDER BY a + 1",
	} {
		if _, err := trySelect(t, kv, cat, sql); err == nil {
			t.Errorf("%q: expected error", sql)
		}
	}

	// 表达式出现在输出里就允许（输出列描述一致即可）
	if _, err := trySelect(t, kv, cat, "SELECT DISTINCT a + 1 FROM d ORDER BY a + 1"); err != nil {
		t.Fatalf("distinct + order by same expr: %v", err)
	}
}

// ── 聚合（无 GROUP BY）──────────────────────────────────────────

func TestAggregatesNoGroup(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 10)

	eqI := func(n int64) func(types.Value) bool {
		return func(v types.Value) bool { return v.Kind == types.Int && v.I == n }
	}
	eqF := func(f float64) func(types.Value) bool {
		return func(v types.Value) bool { return v.Kind == types.Float && v.F == f }
	}
	eqS := func(s string) func(types.Value) bool {
		return func(v types.Value) bool { return v.Kind == types.Text && v.S == s }
	}
	cases := []struct {
		sql string
		ck  func(types.Value) bool
	}{
		{"SELECT COUNT(*) FROM t", eqI(10)},
		{"SELECT COUNT(name) FROM t", eqI(5)},   // 偶数行 name 为 NULL
		{"SELECT COUNT(score) FROM t", eqI(10)}, // COUNT(col) 只数非 NULL
		{"SELECT SUM(id) FROM t", eqI(45)},      // INT 求和保持 INT
		{"SELECT SUM(score) FROM t", eqF(22.5)}, // 见过 FLOAT → FLOAT
		{"SELECT AVG(id) FROM t", eqF(4.5)},     // AVG 永远 FLOAT
		{"SELECT AVG(score) FROM t", eqF(2.25)},
		{"SELECT MIN(id) FROM t", eqI(0)},
		{"SELECT MAX(id) FROM t", eqI(9)},
		{"SELECT MIN(name) FROM t", eqS("nameb")}, // NULL 跳过
		{"SELECT MAX(name) FROM t", eqS("namej")},
	}
	for _, c := range cases {
		_, rows := runSelect(t, kv, cat, c.sql)
		if len(rows) != 1 || !c.ck(rows[0][0]) {
			t.Fatalf("%s: got %v", c.sql, rows)
		}
	}

	// 空输入（无 GROUP BY）：单组一行，COUNT=0、其余 NULL
	_, rows := runSelect(t, kv, cat,
		"SELECT COUNT(*) AS c, SUM(id) AS s, MIN(name) AS m, AVG(id) AS a FROM t WHERE id > 100")
	if len(rows) != 1 {
		t.Fatalf("empty aggregate: %d rows", len(rows))
	}
	r := rows[0]
	if r[0].Kind != types.Int || r[0].I != 0 ||
		!r[1].IsNull() || !r[2].IsNull() || !r[3].IsNull() {
		t.Fatalf("empty aggregate row: %v", r)
	}

	// SUM(text)：执行到第一个非 NULL 值报错（DESIGN §7.4 从宽的方向）
	if _, err := trySelect(t, kv, cat, "SELECT SUM(name) FROM t"); err == nil ||
		!strings.Contains(err.Error(), "requires numeric") {
		t.Fatalf("SUM(text): %v", err)
	}

	// 全 NULL 输入的 MIN：NULL，不报错
	_, rows = runSelect(t, kv, cat, "SELECT MIN(name) FROM t WHERE id IN (2, 4)")
	if len(rows) != 1 || !rows[0][0].IsNull() {
		t.Fatalf("MIN over all-NULL: %v", rows)
	}
}

// 聚合下的下推可观测：COUNT(*) + WHERE id > 90 只该读 9 行。
func TestAggregatePushdownObservable(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 100)
	plan, rows := runSelect(t, kv, cat, "SELECT COUNT(*) FROM t WHERE id > 90")
	if got := plan.RowsScanned(); got != 9 {
		t.Fatalf("scanned %d, want 9", got)
	}
	if len(rows) != 1 || rows[0][0].I != 9 {
		t.Fatalf("count: %v", rows)
	}
}

// ── GROUP BY / HAVING ────────────────────────────────────────────

func TestGroupByHaving(t *testing.T) {
	kv, cat := newEnv(t)
	empTable(t, kv, cat)

	// 列分组：NULL 部门自成一组（NULL 与 NULL 同组），输出按 dept 排序
	_, rows := runSelect(t, kv, cat,
		"SELECT dept, COUNT(*) AS n, SUM(salary) AS total FROM emp GROUP BY dept ORDER BY dept")
	if len(rows) != 3 {
		t.Fatalf("group rows: %d", len(rows))
	}
	// dept 排序 ASC：NULL 最前，然后 dev、sales
	if !rows[0][0].IsNull() || rows[1][0].S != "dev" || rows[2][0].S != "sales" {
		t.Fatalf("group order: %v", rows)
	}
	if rows[0][1].I != 1 || rows[1][1].I != 2 || rows[2][1].I != 2 {
		t.Fatalf("counts: %v", rows)
	}
	if rows[0][2].I != 50 || rows[1][2].I != 400 || rows[2][2].I != 300 {
		t.Fatalf("sums: %v", rows)
	}

	// HAVING 过滤组（求值环境：组内聚合值 + 分组键），键在 ORDER BY 里用别名
	_, rows = runSelect(t, kv, cat,
		"SELECT dept, SUM(salary) AS total FROM emp GROUP BY dept HAVING COUNT(*) > 1 ORDER BY total DESC")
	if len(rows) != 2 || rows[0][0].S != "dev" || rows[0][1].I != 400 ||
		rows[1][0].S != "sales" || rows[1][1].I != 300 {
		t.Fatalf("having: %v", rows)
	}

	// 分组键表达式 + 聚合混合投影（键在代表行上求值）
	intTable(t, kv, cat, 10)
	_, rows = runSelect(t, kv, cat,
		"SELECT id % 3 AS m, COUNT(*) AS c FROM t GROUP BY id % 3 ORDER BY m")
	if len(rows) != 3 ||
		rows[0][0].I != 0 || rows[0][1].I != 4 ||
		rows[1][0].I != 1 || rows[1][1].I != 3 ||
		rows[2][0].I != 2 || rows[2][1].I != 3 {
		t.Fatalf("group by expression: %v", rows)
	}

	// HAVING 不带 GROUP BY 是 M1 文法之外的写法（DESIGN §6），解析期就拦下
	if _, err := trySelect(t, kv, cat, "SELECT COUNT(*) FROM emp HAVING COUNT(*) > 2"); err == nil {
		t.Fatal("standalone HAVING should be a parse error")
	}

	// AVG 每组：永远 FLOAT
	_, rows = runSelect(t, kv, cat,
		"SELECT dept, AVG(salary) AS a FROM emp WHERE dept = 'sales' GROUP BY dept")
	if len(rows) != 1 || rows[0][1].Kind != types.Float || rows[0][1].F != 150 {
		t.Fatalf("avg per group: %v", rows)
	}

	// 有 GROUP BY 的空输入：零行（组必须真的存在才有一行）
	_, rows = runSelect(t, kv, cat, "SELECT dept, COUNT(*) FROM emp WHERE salary > 999 GROUP BY dept")
	if len(rows) != 0 {
		t.Fatalf("empty grouped: %v", rows)
	}
}

func TestGroupByErrors(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 5)

	for _, sql := range []string{
		"SELECT name FROM t GROUP BY id",                 // 非聚合列未被 GROUP BY 覆盖
		"SELECT id FROM t GROUP BY COUNT(id)",            // GROUP BY 里不允许聚合
		"SELECT SUM(COUNT(*)) FROM t",                    // 聚合嵌套聚合
		"SELECT SUM(*) FROM t",                           // 只有 COUNT(*) 允许 star
		"SELECT id FROM t WHERE COUNT(*) > 1",            // WHERE 里没有聚合的位置
		"SELECT id FROM t GROUP BY id HAVING name = 'x'", // HAVING 列未覆盖
	} {
		if _, err := trySelect(t, kv, cat, sql); err == nil {
			t.Errorf("%q: expected error", sql)
		}
	}
}

// ── JOIN ─────────────────────────────────────────────────────────

func joinTables(t *testing.T, kv *kvdb.DB, cat *catalog.Catalog) {
	t.Helper()
	users := mustCreate(t, cat, "users",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "name", Kind: types.Text},
	)
	putRow(t, kv, users, types.IntValue(1), types.TextValue("ann"))
	putRow(t, kv, users, types.IntValue(2), types.TextValue("bob"))
	putRow(t, kv, users, types.IntValue(3), types.TextValue("cat"))

	orders := mustCreate(t, cat, "orders",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "uid", Kind: types.Int},
		catalog.Column{Name: "amt", Kind: types.Float},
	)
	putRow(t, kv, orders, types.IntValue(10), types.IntValue(1), types.FloatValue(5.0))
	putRow(t, kv, orders, types.IntValue(11), types.IntValue(1), types.FloatValue(7.5))
	putRow(t, kv, orders, types.IntValue(12), types.IntValue(2), types.FloatValue(1.0))
	putRow(t, kv, orders, types.IntValue(13), types.IntValue(9), types.FloatValue(9.9)) // uid 无匹配
	putRow(t, kv, orders, types.IntValue(14), types.NullValue(), types.FloatValue(2.0)) // NULL 不匹配
}

func TestInnerJoin(t *testing.T) {
	kv, cat := newEnv(t)
	joinTables(t, kv, cat)

	// 基本内连接 + 排序：uid 无匹配的行、uid 为 NULL 的行都被丢掉
	_, rows := runSelect(t, kv, cat,
		"SELECT u.name, o.amt FROM users u JOIN orders o ON o.uid = u.id ORDER BY o.id")
	if len(rows) != 3 ||
		rows[0][0].S != "ann" || rows[0][1].F != 5 ||
		rows[1][0].S != "ann" || rows[1][1].F != 7.5 ||
		rows[2][0].S != "bob" || rows[2][1].F != 1 {
		t.Fatalf("join rows: %v", rows)
	}

	// 全表扫描可观测：无下推时 users 3 行 + orders 5 行
	plan, _ := runSelect(t, kv, cat,
		"SELECT u.name, o.amt FROM users u JOIN orders o ON o.uid = u.id ORDER BY o.id")
	if got := plan.RowsScanned(); got != 8 {
		t.Fatalf("scanned %d, want 8", got)
	}

	// 下推作用在 orders 上：orders 读 3 行（id >= 12），users 全读 3 行
	plan, rows = runSelect(t, kv, cat,
		"SELECT o.id FROM users u JOIN orders o ON o.uid = u.id WHERE o.id >= 12")
	if got := plan.RowsScanned(); got != 6 {
		t.Fatalf("scanned %d, want 6", got)
	}
	if len(rows) != 1 || rows[0][0].I != 12 { // 13 的 uid=9、14 的 uid=NULL 都不匹配
		t.Fatalf("join + pushdown rows: %v", rows)
	}

	// 下推作用在 users 上
	plan, rows = runSelect(t, kv, cat,
		"SELECT o.id FROM users u JOIN orders o ON o.uid = u.id WHERE u.id = 2")
	if got := plan.RowsScanned(); got != 6 { // users 1 行 + orders 5 行
		t.Fatalf("scanned %d, want 6", got)
	}
	if len(rows) != 1 || rows[0][0].I != 12 {
		t.Fatalf("user-side pushdown rows: %v", rows)
	}

	// 限定 star
	_, rows = runSelect(t, kv, cat,
		"SELECT o.* FROM users u JOIN orders o ON o.uid = u.id WHERE u.id = 1")
	if len(rows) != 2 || rows[0][0].I != 10 || rows[1][0].I != 11 {
		t.Fatalf("qualified star: %v", rows)
	}
}

func TestThreeWayJoin(t *testing.T) {
	kv, cat := newEnv(t)
	joinTables(t, kv, cat)
	ships := mustCreate(t, cat, "shipments",
		catalog.Column{Name: "oid", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "city", Kind: types.Text},
	)
	putRow(t, kv, ships, types.IntValue(10), types.TextValue("sh"))
	putRow(t, kv, ships, types.IntValue(12), types.TextValue("bj"))

	// 左深树：users ⋈ orders ⋈ shipments
	_, rows := runSelect(t, kv, cat,
		"SELECT u.name, o.amt, s.city FROM users u JOIN orders o ON o.uid = u.id "+
			"JOIN shipments s ON s.oid = o.id ORDER BY o.id")
	if len(rows) != 2 ||
		rows[0][0].S != "ann" || rows[0][2].S != "sh" ||
		rows[1][0].S != "bob" || rows[1][2].S != "bj" {
		t.Fatalf("three-way join: %v", rows)
	}
}

func TestJoinErrors(t *testing.T) {
	kv, cat := newEnv(t)
	joinTables(t, kv, cat)

	for _, sql := range []string{
		"SELECT id FROM users u JOIN orders o ON o.uid = u.id",                                      // 两表都有 id，歧义
		"SELECT * FROM users u JOIN orders o ON u.nope = o.uid",                                     // 列不存在
		"SELECT * FROM users JOIN orders ON id = uid",                                               // id 歧义（前一个）
		"SELECT u.name FROM users u JOIN orders o ON u.id = s.uid JOIN shipments s ON s.oid = o.id", // ON 引用了后面才出现的表
		"SELECT * FROM users JOIN users ON users.id = users.id",                                     // 不带别名的自连接：作用域名重复
	} {
		if _, err := trySelect(t, kv, cat, sql); err == nil {
			t.Errorf("%q: expected error", sql)
		}
	}

	// ON 结果不是布尔：绑定期合法（无类型推导），执行期报错
	if _, err := trySelect(t, kv, cat, "SELECT u.name FROM users u JOIN orders o ON o.id"); err == nil ||
		!strings.Contains(err.Error(), "JOIN ON") {
		t.Fatalf("non-bool ON: %v", err)
	}

	// 正面用例：起了别名的自连接合法
	if _, err := trySelect(t, kv, cat, "SELECT b.id FROM users a JOIN users b ON a.id = b.id WHERE a.id = 1"); err != nil {
		t.Fatalf("aliased self join: %v", err)
	}
}

// ── 组合拳 ───────────────────────────────────────────────────────

func TestM4Combined(t *testing.T) {
	kv, cat := newEnv(t)
	joinTables(t, kv, cat)

	// JOIN + GROUP BY + HAVING + ORDER BY 别名 + LIMIT
	_, rows := runSelect(t, kv, cat,
		"SELECT u.name, COUNT(*) AS c, SUM(o.amt) AS total "+
			"FROM users u JOIN orders o ON o.uid = u.id "+
			"GROUP BY u.name HAVING COUNT(*) >= 1 ORDER BY total DESC LIMIT 2")
	if len(rows) != 2 ||
		rows[0][0].S != "ann" || rows[0][1].I != 2 || rows[0][2].F != 12.5 ||
		rows[1][0].S != "bob" || rows[1][1].I != 1 || rows[1][2].F != 1 {
		t.Fatalf("combined: %v", rows)
	}

	// DISTINCT + ORDER BY + LIMIT：NULL 参与去重与排序
	_, rows = runSelect(t, kv, cat, "SELECT DISTINCT uid FROM orders ORDER BY uid LIMIT 2")
	// uid: 1,1,2,9,NULL → DISTINCT 保序去重 → ASC 排序：NULL 最前
	if len(rows) != 2 || !rows[0][0].IsNull() || rows[1][0].I != 1 {
		t.Fatalf("distinct+order+limit: %v", rows)
	}
}
