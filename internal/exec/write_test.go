package exec

// M3 验收单测（DESIGN §11）：INSERT/UPDATE/DELETE + 约束 —— 主键冲突、
// NOT NULL、类型检查、受影响行数、rowid 分配；写路径的范围下推可观测。

import (
	"strings"
	"testing"

	"github.com/xiatianliang1024gm/kvdb"
	"github.com/xiatianliang1024gm/sqldb/internal/catalog"
	"github.com/xiatianliang1024gm/sqldb/internal/parser"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// runWrite 解析并执行一条写语句，返回受影响行数。
func runWrite(t *testing.T, kv *kvdb.DB, cat *catalog.Catalog, sql string) int64 {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	var n int64
	switch s := stmt.(type) {
	case *parser.InsertStmt:
		n, err = ExecInsert(kv, cat, s)
	case *parser.UpdateStmt:
		n, err = ExecUpdate(kv, cat, s)
	case *parser.DeleteStmt:
		n, err = ExecDelete(kv, cat, s)
	default:
		t.Fatalf("%q is not a write statement", sql)
	}
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return n
}

func wantWriteErr(t *testing.T, kv *kvdb.DB, cat *catalog.Catalog, sql, wantSub string) {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		if strings.Contains(err.Error(), wantSub) {
			return // 语法层就报了，也算数
		}
		t.Fatalf("%q: parse error %q does not mention %q", sql, err, wantSub)
	}
	var execErr error
	switch s := stmt.(type) {
	case *parser.InsertStmt:
		_, execErr = ExecInsert(kv, cat, s)
	case *parser.UpdateStmt:
		_, execErr = ExecUpdate(kv, cat, s)
	case *parser.DeleteStmt:
		_, execErr = ExecDelete(kv, cat, s)
	}
	if execErr == nil || !strings.Contains(execErr.Error(), wantSub) {
		t.Fatalf("%q: error %v does not mention %q", sql, execErr, wantSub)
	}
}

// ── INSERT ───────────────────────────────────────────────────────

func TestInsertMultiRowAtomic(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 0)

	n := runWrite(t, kv, cat,
		"INSERT INTO t (id, name, score) VALUES (1, 'ann', 1.5), (2, 'bob', 2.5), (3, NULL, 3.5)")
	if n != 3 {
		t.Fatalf("RowsAffected = %d, want 3", n)
	}
	_, rows := runSelect(t, kv, cat, "SELECT id, name, score FROM t WHERE id <= 3")
	wantInts(t, rows, 0, 1, 2, 3)
	if !rows[2][1].IsNull() {
		t.Fatalf("row 3 name should be NULL, got %v", rows[2][1])
	}
}

// 主键冲突：报错且原行不受影响；同一语句里后一行冲突时，整条语句一行不落。
func TestInsertDuplicatePK(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 2) // 已有 id 0、1

	wantWriteErr(t, kv, cat, "INSERT INTO t (id, name) VALUES (1, 'x')", "duplicate primary key")
	plan, rows := runSelect(t, kv, cat, "SELECT id FROM t")
	if len(rows) != 2 || plan.RowsScanned() != 2 {
		t.Fatalf("table changed by failed insert: %d rows", len(rows))
	}

	// 第 2 行冲突 → 第 1 行也不能进来（错误发生在 Write 之前）
	wantWriteErr(t, kv, cat, "INSERT INTO t (id, name) VALUES (7, 'x'), (0, 'y')", "duplicate primary key")
	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE id = 7")
	if len(rows) != 0 {
		t.Fatalf("partial insert leaked row 7: %v", rows)
	}
}

func TestInsertConstraints(t *testing.T) {
	kv, cat := newEnv(t)
	mustCreate(t, cat, "c",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "name", Kind: types.Text, NotNull: true},
	)

	// NOT NULL：显式 NULL / 缺省列都不行
	wantWriteErr(t, kv, cat, "INSERT INTO c (id, name) VALUES (1, NULL)", "NOT NULL")
	wantWriteErr(t, kv, cat, "INSERT INTO c (id) VALUES (1)", "NOT NULL")

	// 类型检查：TEXT 进 INT 列、FLOAT 进 INT 列都拒绝
	wantWriteErr(t, kv, cat, "INSERT INTO c (id, name) VALUES ('x', 'y')", "is INT")
	wantWriteErr(t, kv, cat, "INSERT INTO c (id, name) VALUES (1.5, 'y')", "is INT")

	// INT 字面量进 FLOAT 列允许（升格）：单独一张表验证
	mustCreate(t, cat, "f",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "x", Kind: types.Float},
	)
	runWrite(t, kv, cat, "INSERT INTO f VALUES (1, 3)")
	_, rows := runSelect(t, kv, cat, "SELECT x FROM f")
	if len(rows) != 1 || rows[0][0].Kind != types.Float || rows[0][0].F != 3 {
		t.Fatalf("int literal into float column: %v", rows)
	}

	// 列清单错误：未知列 / 重复列 / rowid 是隐藏列
	wantWriteErr(t, kv, cat, "INSERT INTO c (id, nope) VALUES (1, 'y')", "no such column")
	wantWriteErr(t, kv, cat, "INSERT INTO c (id, id) VALUES (1, 2)", "duplicate column")

	// VALUES 里引用列没有意义
	wantWriteErr(t, kv, cat, "INSERT INTO c (id, name) VALUES (id, 'y')", "VALUES")

	// 表达式值：运算结果可以进列
	runWrite(t, kv, cat, "INSERT INTO c (id, name) VALUES (1 + 1, 'ann')")
	_, rows = runSelect(t, kv, cat, "SELECT id FROM c WHERE id = 2")
	if len(rows) != 1 {
		t.Fatalf("expression value insert failed: %v", rows)
	}
}

// 缺省列补 NULL：允许 NULL 的列省略后读回来就是 NULL。
func TestInsertDefaultNull(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 0)
	runWrite(t, kv, cat, "INSERT INTO t (id) VALUES (9)")
	_, rows := runSelect(t, kv, cat, "SELECT name, score FROM t WHERE id = 9")
	if len(rows) != 1 || !rows[0][0].IsNull() || !rows[0][1].IsNull() {
		t.Fatalf("omitted columns should be NULL: %v", rows)
	}
}

// rowid 表：隐藏主键从 0 连续分配，多行一次 INSERT 不会重复分配。
func TestInsertRowidTable(t *testing.T) {
	kv, cat := newEnv(t)
	mustCreate(t, cat, "r",
		catalog.Column{Name: "a", Kind: types.Text, NotNull: true},
		catalog.Column{Name: "b", Kind: types.Int},
	)

	n := runWrite(t, kv, cat, "INSERT INTO r (a, b) VALUES ('x', 1), ('y', 2)")
	if n != 2 {
		t.Fatalf("RowsAffected = %d", n)
	}
	// 不写列清单 = 全部用户可见列
	runWrite(t, kv, cat, "INSERT INTO r VALUES ('z', 3)")
	// rowid 本身可以被查询（它就是主键）
	_, rows := runSelect(t, kv, cat, "SELECT rowid, a FROM r")
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	for i, want := range []int64{0, 1, 2} {
		if rows[i][0].I != want {
			t.Fatalf("row %d rowid = %d, want %d", i, rows[i][0].I, want)
		}
	}
	// 显式给 rowid 拒绝 —— 计数器是唯一分配来源
	wantWriteErr(t, kv, cat, "INSERT INTO r (rowid, a) VALUES (99, 'w')", "implicit")
	// 分配出去的 rowid 与计数器一致：下一行拿 3
	runWrite(t, kv, cat, "INSERT INTO r (a) VALUES ('w')")
	_, rows = runSelect(t, kv, cat, "SELECT rowid FROM r WHERE a = 'w'")
	if len(rows) != 1 || rows[0][0].I != 3 {
		t.Fatalf("next rowid = %v, want 3", rows)
	}
}

// ── UPDATE ───────────────────────────────────────────────────────

func TestUpdateBasics(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 10)

	n := runWrite(t, kv, cat, "UPDATE t SET score = score + 100, name = 'u' WHERE id >= 5")
	if n != 5 {
		t.Fatalf("RowsAffected = %d, want 5", n)
	}
	_, rows := runSelect(t, kv, cat, "SELECT id, name, score FROM t WHERE id = 6")
	if rows[0][1].S != "u" || rows[0][2].F != 103 {
		t.Fatalf("updated row: %v", rows)
	}
	// 范围外的行不动
	_, rows = runSelect(t, kv, cat, "SELECT name FROM t WHERE id = 4")
	if rows[0][0].Kind == types.Text && rows[0][0].S == "u" {
		t.Fatalf("row 4 should be untouched: %v", rows)
	}

	// 无 WHERE = 全表
	n = runWrite(t, kv, cat, "UPDATE t SET name = NULL")
	if n != 10 {
		t.Fatalf("full update affected %d, want 10", n)
	}
	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE name IS NULL")
	if len(rows) != 10 {
		t.Fatalf("after full update: %d NULL rows", len(rows))
	}
}

func TestUpdateConstraints(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 3) // name 列无 NOT NULL；建一张有的
	mustCreate(t, cat, "c",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "name", Kind: types.Text, NotNull: true},
	)

	// 主键不允许 UPDATE
	wantWriteErr(t, kv, cat, "UPDATE t SET id = 9 WHERE id = 1", "primary key")
	// NOT NULL 列不能被改成 NULL
	runWrite(t, kv, cat, "INSERT INTO c (id, name) VALUES (1, 'x')")
	wantWriteErr(t, kv, cat, "UPDATE c SET name = NULL", "NOT NULL")
	// 类型不匹配拒绝；UPDATE 失败的行保持原值
	wantWriteErr(t, kv, cat, "UPDATE c SET name = 5", "is TEXT")
	_, rows := runSelect(t, kv, cat, "SELECT name FROM c")
	if rows[0][0].S != "x" {
		t.Fatalf("failed update changed data: %v", rows)
	}
}

// ── DELETE ───────────────────────────────────────────────────────

func TestDeleteBasics(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 10)

	n := runWrite(t, kv, cat, "DELETE FROM t WHERE id < 3")
	if n != 3 {
		t.Fatalf("RowsAffected = %d, want 3", n)
	}
	_, rows := runSelect(t, kv, cat, "SELECT id FROM t")
	wantInts(t, rows, 0, seq(3, 10)...)

	// 删不存在的行：0 行，不报错
	n = runWrite(t, kv, cat, "DELETE FROM t WHERE id > 99")
	if n != 0 {
		t.Fatalf("delete nothing affected %d", n)
	}

	// 无 WHERE = 全表删
	n = runWrite(t, kv, cat, "DELETE FROM t")
	if n != 7 {
		t.Fatalf("full delete affected %d, want 7", n)
	}
	plan, _ := runSelect(t, kv, cat, "SELECT id FROM t")
	if plan.RowsScanned() != 0 {
		t.Fatalf("table not empty after full delete, scanned %d", plan.RowsScanned())
	}
}

// 三值逻辑在写路径同样收口：WHERE 求值为 NULL 的行不被删/改。
// （intTable 里偶数行 name 为 NULL，奇数行是 "nameb"/"named" 这类值。）
func TestWriteWhereNullNotMatched(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 4) // id 0..3，偶数行 name NULL

	n := runWrite(t, kv, cat, "DELETE FROM t WHERE name = 'zzz'")
	if n != 0 { // 没有一行叫 'zzz'，NULL 行的比较结果是 NULL，同样不通过
		t.Fatalf("delete affected %d, want 0", n)
	}
	// name = NULL 的行：比较结果 NULL → 不通过
	n = runWrite(t, kv, cat, "UPDATE t SET name = 'z' WHERE name = NULL")
	if n != 0 {
		t.Fatalf("update matched NULL rows: %d", n)
	}
	// IS NULL 才能抓到它们
	n = runWrite(t, kv, cat, "UPDATE t SET name = 'z' WHERE name IS NULL")
	if n != 2 {
		t.Fatalf("update IS NULL affected %d, want 2", n)
	}
}

// 写路径的下推可观测：直接检验 scanTableRows 的扫描行数。
// WHERE id > 90 在 100 行的表上只该读 9 行 —— 和 M2 的查询同一套下推逻辑。
func TestWriteScanPushdownObservable(t *testing.T) {
	kv, cat := newEnv(t)
	tab := intTable(t, kv, cat, 100)

	lower, upper, residual, err := bindWhere(tab, "t", mustWhere(t, "id > 90"))
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	_, scanned, err := scanTableRows(kv, tab, lower, upper, residual)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 9 {
		t.Fatalf("write scan read %d rows, want 9", scanned)
	}

	// 非主键谓词不下推，读全表
	lower, upper, residual, err = bindWhere(tab, "t", mustWhere(t, "score > 24.5"))
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	_, scanned, err = scanTableRows(kv, tab, lower, upper, residual)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 100 {
		t.Fatalf("non-pushed write scan read %d rows, want 100", scanned)
	}
}

func mustWhere(t *testing.T, expr string) parser.Expr {
	t.Helper()
	// 借 SELECT 的语法解析出 WHERE 表达式
	stmt, err := parser.Parse("SELECT id FROM t WHERE " + expr)
	if err != nil {
		t.Fatalf("parse where %q: %v", expr, err)
	}
	return stmt.(*parser.SelectStmt).Where
}

// UPDATE 主键被禁后，改主键的正当路径是 DELETE + INSERT —— 端到端走一遍。
func TestRekeyViaDeleteInsert(t *testing.T) {
	kv, cat := newEnv(t)
	intTable(t, kv, cat, 3)

	runWrite(t, kv, cat, "DELETE FROM t WHERE id = 1")
	runWrite(t, kv, cat, "INSERT INTO t (id, name, score) VALUES (10, 'moved', 0.5)")
	_, rows := runSelect(t, kv, cat, "SELECT id FROM t")
	wantInts(t, rows, 0, 0, 2, 10)
}

// 约束检查在提交前完成：失败的写语句不留任何痕迹（SELECT 直接验证）。
func TestFailedWriteLeavesNoTrace(t *testing.T) {
	kv, cat := newEnv(t)
	mustCreate(t, cat, "c",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "name", Kind: types.Text, NotNull: true},
	)
	runWrite(t, kv, cat, "INSERT INTO c VALUES (1, 'x')")

	wantWriteErr(t, kv, cat, "UPDATE c SET name = NULL WHERE id = 1", "NOT NULL")
	_, rows := runSelect(t, kv, cat, "SELECT id, name FROM c")
	if len(rows) != 1 || rows[0][1].S != "x" {
		t.Fatalf("failed update changed data: %v", rows)
	}
}
