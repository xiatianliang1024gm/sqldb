package exec

// M2 验收单测（DESIGN §11）：`SELECT * FROM t WHERE pk > 10` 结果正确，
// 且范围下推可观测 —— RowsScanned 必须随下推收窄，而不是永远等于全表行数。
//
// M3 之前没有 INSERT，测试数据直接用 encoding 层摆进 kvdb：
// 行键 = RowKey(表ID, EncodeKey(pk))，行值 = EncodeRow(全列)。
// 这本身就是对"M2 只依赖五个原语"的一次体感校验。

import (
	"path/filepath"
	"testing"

	"github.com/xiatianliang1024gm/kvdb"
	"github.com/xiatianliang1024gm/sqldb/internal/catalog"
	"github.com/xiatianliang1024gm/sqldb/internal/encoding"
	"github.com/xiatianliang1024gm/sqldb/internal/parser"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

func newEnv(t *testing.T) (*kvdb.DB, *catalog.Catalog) {
	t.Helper()
	kv, err := kvdb.Open(kvdb.Options{Dir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatalf("open kvdb: %v", err)
	}
	t.Cleanup(func() { kv.Close() })
	cat, err := catalog.Load(kv)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return kv, cat
}

func mustCreate(t *testing.T, cat *catalog.Catalog, name string, cols ...catalog.Column) *catalog.Table {
	t.Helper()
	tab, err := cat.CreateTable(name, cols)
	if err != nil {
		t.Fatalf("create table %s: %v", name, err)
	}
	return tab
}

func putRow(t *testing.T, kv *kvdb.DB, tab *catalog.Table, vals ...types.Value) {
	t.Helper()
	pkIdx := tab.PrimaryKeyIndex()
	enc, err := encoding.EncodeKey(vals[pkIdx])
	if err != nil {
		t.Fatalf("encode pk %v: %v", vals[pkIdx], err)
	}
	if err := kv.Put(encoding.RowKey(tab.ID, enc), encoding.EncodeRow(vals)); err != nil {
		t.Fatalf("put row: %v", err)
	}
}

// runSelect 解析、构建计划、拉干整个 Volcano 树，返回计划与结果行。
func runSelect(t *testing.T, kv *kvdb.DB, cat *catalog.Catalog, sql string) (*Plan, [][]types.Value) {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	sel, ok := stmt.(*parser.SelectStmt)
	if !ok {
		t.Fatalf("%q is not a SELECT", sql)
	}
	plan, err := Build(kv, cat, sel)
	if err != nil {
		t.Fatalf("build %q: %v", sql, err)
	}
	defer plan.Root.Close()

	var rows [][]types.Value
	for {
		row, err := plan.Root.Next()
		if err != nil {
			t.Fatalf("next on %q: %v", sql, err)
		}
		if row == nil {
			break
		}
		rows = append(rows, row)
	}
	return plan, rows
}

func wantInts(t *testing.T, got [][]types.Value, col int, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
	}
	for i, row := range got {
		if row[col].Kind != types.Int || row[col].I != want[i] {
			t.Fatalf("row %d: got %v, want %d", i, row[col], want[i])
		}
	}
}

// intTable 建一张 id INT 主键 + name TEXT + score FLOAT 的表并灌 n 行
// （id 0..n-1，偶数行 name 为 NULL、score 为 NULL —— 给三值逻辑测试留料）。
func intTable(t *testing.T, kv *kvdb.DB, cat *catalog.Catalog, n int) *catalog.Table {
	t.Helper()
	tab := mustCreate(t, cat, "t",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "name", Kind: types.Text},
		catalog.Column{Name: "score", Kind: types.Float},
	)
	for i := 0; i < n; i++ {
		var name types.Value
		if i%2 == 1 {
			name = types.TextValue("name" + string(rune('a'+i%26)))
		} else {
			name = types.NullValue()
		}
		putRow(t, kv, tab, types.IntValue(int64(i)), name, types.FloatValue(float64(i)/2))
	}
	return tab
}

// ── Scan 本体 ────────────────────────────────────────────────────

func TestScanFullTableInPKOrder(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 50)

	plan, rows := runSelect(t, kv, cat, "SELECT id, name, score FROM t")
	if got := plan.RowsScanned(); got != 50 {
		t.Fatalf("scanned %d rows, want 50", got)
	}
	// 无 WHERE：全表按主键升序回来
	wantInts(t, rows, 0, seq(0, 50)...)
	if len(plan.Columns) != 3 || plan.Columns[0] != "id" {
		t.Fatalf("columns wrong: %v", plan.Columns)
	}
	// 偶数行 name 为 NULL，奇数行按构造校验一个样本
	if rows[3][1].Kind != types.Text || rows[3][1].S != "named" {
		t.Fatalf("row 3 name = %v, want named", rows[3][1])
	}
	if !rows[2][1].IsNull() {
		t.Fatalf("row 2 name should be NULL, got %v", rows[2][1])
	}
}

func seq(from, to int64) []int64 {
	out := make([]int64, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

func TestScanEmptyTable(t *testing.T) {
	kv, cat := newEnv(t)
	mustCreate(t, cat, "empty",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true})
	plan, rows := runSelect(t, kv, cat, "SELECT id FROM empty")
	if len(rows) != 0 || plan.RowsScanned() != 0 {
		t.Fatalf("empty table returned %d rows, scanned %d", len(rows), plan.RowsScanned())
	}
}

// ── 范围下推（M2 的主角）─────────────────────────────────────────

func TestPushdownRangeObservable(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 100)

	// WHERE id > 90：下推后只该读到 9 行，返回 91..99。
	plan, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE id > 90")
	if got := plan.RowsScanned(); got != 9 {
		t.Fatalf("pushed-down scan read %d rows, want 9", got)
	}
	wantInts(t, rows, 0, seq(91, 100)...)

	// 对照组：非主键谓词无法下推，必须读全表（可观测的差异）。
	plan2, rows2 := runSelect(t, kv, cat, "SELECT id FROM t WHERE score > 24.5")
	if got := plan2.RowsScanned(); got != 100 {
		t.Fatalf("non-pushed scan read %d rows, want 100", got)
	}
	if len(rows2) != 50 {
		t.Fatalf("score > 24.5 returned %d rows, want 50", len(rows2))
	}
}

func TestPushdownAllOperators(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 100)

	cases := []struct {
		sql       string
		scanned   int64
		firstVals []int64
	}{
		{"SELECT id FROM t WHERE id = 42", 1, []int64{42}},
		{"SELECT id FROM t WHERE id >= 97", 3, []int64{97, 98, 99}},
		{"SELECT id FROM t WHERE id < 3", 3, []int64{0, 1, 2}},
		{"SELECT id FROM t WHERE id <= 2", 3, []int64{0, 1, 2}},
		{"SELECT id FROM t WHERE id > 97 AND id < 99", 1, []int64{98}},
		{"SELECT id FROM t WHERE id >= 10 AND id <= 12", 3, []int64{10, 11, 12}},
		// 字面量在左边的写法同样下推
		{"SELECT id FROM t WHERE 42 = id", 1, []int64{42}},
		{"SELECT id FROM t WHERE 90 < id", 9, seq(91, 100)},
	}
	for _, c := range cases {
		plan, rows := runSelect(t, kv, cat, c.sql)
		if got := plan.RowsScanned(); got != c.scanned {
			t.Fatalf("%s: scanned %d, want %d", c.sql, got, c.scanned)
		}
		wantInts(t, rows, 0, c.firstVals...)
	}
}

// 矛盾条件（=5 且 >5）折算出空区间，不报错、零行。
func TestPushdownContradictionIsEmpty(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 10)
	plan, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE id = 5 AND id > 5")
	if len(rows) != 0 || plan.RowsScanned() != 0 {
		t.Fatalf("contradiction returned %d rows (scanned %d), want none", len(rows), plan.RowsScanned())
	}
}

func TestPushdownNegativeInts(t *testing.T) {
	kv, cat := newEnv(t)
	tab := mustCreate(t, cat, "neg",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true})
	for i := int64(-5); i <= 5; i++ {
		putRow(t, kv, tab, types.IntValue(i))
	}
	// 负数编码排在前面的性质，正好被符号位翻转 + 范围下推一起检验
	_, rows := runSelect(t, kv, cat, "SELECT id FROM neg WHERE id < 0")
	wantInts(t, rows, 0, -5, -4, -3, -2, -1)
}

func TestPushdownTextPK(t *testing.T) {
	kv, cat := newEnv(t)
	tab := mustCreate(t, cat, "words",
		catalog.Column{Name: "k", Kind: types.Text, PrimaryKey: true},
		catalog.Column{Name: "n", Kind: types.Int},
	)
	for _, w := range []string{"apple", "banana", "cherry", "date", "fig"} {
		putRow(t, kv, tab, types.TextValue(w), types.IntValue(int64(len(w))))
	}
	// 文本主键的字节序 == 值序，闭开区间语义一致
	plan, rows := runSelect(t, kv, cat, "SELECT k FROM words WHERE k >= 'banana' AND k < 'date'")
	if got := plan.RowsScanned(); got != 2 {
		t.Fatalf("scanned %d, want 2", got)
	}
	if len(rows) != 2 || rows[0][0].S != "banana" || rows[1][0].S != "cherry" {
		t.Fatalf("rows wrong: %v", rows)
	}
}

func TestPushdownFloatPK(t *testing.T) {
	kv, cat := newEnv(t)
	tab := mustCreate(t, cat, "f",
		catalog.Column{Name: "x", Kind: types.Float, PrimaryKey: true})
	for _, v := range []float64{-1.5, -0.5, 0.5, 1.5, 2.5} {
		putRow(t, kv, tab, types.FloatValue(v))
	}
	_, rows := runSelect(t, kv, cat, "SELECT x FROM f WHERE x > -0.5 AND x <= 1.5")
	if len(rows) != 2 || rows[0][0].F != 0.5 || rows[1][0].F != 1.5 {
		t.Fatalf("float range wrong: %v", rows)
	}
}

// 数值混比（INT 列 vs FLOAT 字面量）不下推 —— 留在 Filter 里由求值器升格。
// 结果必须仍然正确，只是读了全表。
func TestMixedNumericCompareStaysInFilter(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 100)
	plan, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE id > 90.5")
	if got := plan.RowsScanned(); got != 100 {
		t.Fatalf("mixed-type predicate should not push down, scanned %d", got)
	}
	wantInts(t, rows, 0, seq(91, 100)...)
}

// ── Filter：三值逻辑与谓词语义 ───────────────────────────────────

func TestFilterThreeValuedLogic(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 6) // id 0..5，偶数行 name NULL

	// NULL = 任何值 → NULL → 不通过
	_, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE name = 'named'")
	wantInts(t, rows, 0, 3)

	// IS NULL 抓回 NULL 行
	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE name IS NULL")
	wantInts(t, rows, 0, 0, 2, 4)

	// IS NOT NULL
	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE name IS NOT NULL")
	wantInts(t, rows, 0, 1, 3, 5)

	// OR 的三值逻辑：false OR NULL = NULL → 不通过；true OR NULL = true → 通过
	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE id = 3 OR id = 4 OR name = 'named'")
	wantInts(t, rows, 0, 3, 4)

	// NOT 的三值逻辑：NOT NULL = NULL → 不通过（所以只回了奇数行）
	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE NOT name = 'named'")
	wantInts(t, rows, 0, 1, 5)
}

func TestFilterPredicates(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 10)

	_, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE id IN (1, 3, 99)")
	wantInts(t, rows, 0, 1, 3)

	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE id BETWEEN 2 AND 4")
	wantInts(t, rows, 0, 2, 3, 4)

	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE id + 1 = 5")
	wantInts(t, rows, 0, 4)

	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE score * 2 = 7")
	wantInts(t, rows, 0, 7) // score = 3.5
}

func TestFilterLike(t *testing.T) {
	kv, cat := newEnv(t)
	tab := mustCreate(t, cat, "s",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "txt", Kind: types.Text},
	)
	putRow(t, kv, tab, types.IntValue(1), types.TextValue("hello"))
	putRow(t, kv, tab, types.IntValue(2), types.TextValue("help"))
	putRow(t, kv, tab, types.IntValue(3), types.TextValue("world"))
	putRow(t, kv, tab, types.IntValue(4), types.TextValue("he%llo"))

	_, rows := runSelect(t, kv, cat, "SELECT id FROM s WHERE txt LIKE 'hel%'")
	wantInts(t, rows, 0, 1, 2)

	// 'hel_o' 匹配 5 字符的 hello，不匹配 4 字符的 help —— _ 严格占一个字符
	_, rows = runSelect(t, kv, cat, "SELECT id FROM s WHERE txt LIKE 'hel_o'")
	wantInts(t, rows, 0, 1)

	// % 是通配符不是正则元字符：字面 % 也按通配符处理，he%llo 匹配 hel%...
	// 这里验证 QuoteMeta：'he%llo' 里的 % 在模式中仍是通配符
	_, rows = runSelect(t, kv, cat, "SELECT id FROM s WHERE txt LIKE 'w%'")
	wantInts(t, rows, 0, 3)

	_, rows = runSelect(t, kv, cat, "SELECT id FROM s WHERE txt NOT LIKE 'hel%'")
	wantInts(t, rows, 0, 3, 4)
}

// ── Project ──────────────────────────────────────────────────────

func TestProjectStarAndExpressions(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 3)

	plan, rows := runSelect(t, kv, cat, "SELECT * FROM t")
	if len(plan.Columns) != 3 {
		t.Fatalf("star columns: %v", plan.Columns)
	}
	if len(rows[0]) != 3 {
		t.Fatalf("star row width: %v", rows[0])
	}

	plan, rows = runSelect(t, kv, cat,
		"SELECT id, id * 10 AS big, name FROM t WHERE id = 1")
	if len(plan.Columns) != 3 || plan.Columns[0] != "id" || plan.Columns[1] != "big" || plan.Columns[2] != "name" {
		t.Fatalf("columns: %v", plan.Columns)
	}
	if rows[0][1].Kind != types.Int || rows[0][1].I != 10 {
		t.Fatalf("id*10 = %v, want 10", rows[0][1])
	}

	// WHERE 引用 SELECT 里没有的列 —— Filter 在 Project 之下，必须照常工作。
	// score = 0.5 的行是 id=1（奇数行），name = "nameb"（intTable 里 'a'+i 的映射）
	_, rows = runSelect(t, kv, cat, "SELECT name FROM t WHERE score = 0.5")
	if len(rows) != 1 || rows[0][0].S != "nameb" {
		t.Fatalf("filter on unselected column failed: %v", rows)
	}

	// 无别名的表达式列名是表达式描述
	plan, _ = runSelect(t, kv, cat, "SELECT id + 1 FROM t WHERE id = 2")
	if plan.Columns[0] != "id + 1" {
		t.Fatalf("expr column name: %q", plan.Columns[0])
	}
}

// ── 绑定与拒绝路径 ───────────────────────────────────────────────

func TestBindErrors(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 3)

	errCases := []string{
		"SELECT * FROM missing",               // 表不存在
		"SELECT nope FROM t",                  // 列不存在
		"SELECT t.id FROM other",              // 限定符与作用域不符
		"SELECT id FROM t WHERE name LIKE id", // 模式必须是字符串字面量
		"SELECT name, COUNT(*) FROM t",        // 非聚合列未被 GROUP BY 覆盖
		"SELECT id FROM t GROUP BY COUNT(id)", // GROUP BY 里不允许聚合
		"SELECT id FROM t ORDER BY 0",         // 序号越界
		"SELECT id FROM t ORDER BY 3",         // 序号越界
		"SELECT id FROM t ORDER BY 1.5",       // 序号必须是整数字面量
		"SELECT SUM(name) FROM t",             // 聚合参数类型由执行期报（Build 能过）
	}
	for _, sql := range errCases {
		stmt, err := parser.Parse(sql)
		if err != nil {
			continue // 语法错误也行，反正不能成功执行
		}
		plan, err := Build(kv, cat, stmt.(*parser.SelectStmt))
		if err == nil {
			// SUM(name) 这类要到执行期才报错的，拉一行验证
			_, err = plan.Root.Next()
			plan.Root.Close()
		}
		if err == nil {
			t.Errorf("%q: expected error, got plan", sql)
		}
	}
}

// 求值期错误（M2 计划期不做类型推导，DESIGN 取舍表 #11）：
// 执行到第一个坏值才报。跨族比较 / TEXT 算术 / 除零都在这一层。
func TestRuntimeTypeError(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 3)

	for _, sql := range []string{
		"SELECT id FROM t WHERE id / 0 = 1",
		"SELECT id FROM t WHERE name + 1 = 2", // TEXT 不能参与算术
		"SELECT id FROM t WHERE name > 1",     // 跨族比较
	} {
		stmt, _ := parser.Parse(sql)
		plan, err := Build(kv, cat, stmt.(*parser.SelectStmt))
		if err != nil {
			t.Fatalf("%s: build: %v", sql, err)
		}
		_, err = plan.Root.Next()
		if err == nil {
			plan.Root.Close()
			t.Fatalf("%s: expected runtime type error", sql)
		}
		plan.Root.Close()
	}
}
