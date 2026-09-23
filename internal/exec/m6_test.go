package exec

// M6 验收单测（DESIGN §11）：二级索引的扫描正确性、下推可观测性、
// 唯一约束（含 NULL 豁免与同批重复）、写路径维护、既有数据回填、
// 重开持久、DROP TABLE 清理。

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/xiatianliang1024gm/kvdb"
	"github.com/xiatianliang1024gm/sqldb/internal/catalog"
	"github.com/xiatianliang1024gm/sqldb/internal/encoding"
	"github.com/xiatianliang1024gm/sqldb/internal/parser"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// ── 帮手：写语句直通执行器 ────────────────────────────────────────

func execWrite(t *testing.T, kv *kvdb.DB, cat *catalog.Catalog, sql string) int64 {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	switch s := stmt.(type) {
	case *parser.InsertStmt:
		n, err := ExecInsert(kv, cat, s)
		if err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
		return n
	case *parser.UpdateStmt:
		n, err := ExecUpdate(kv, cat, s)
		if err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
		return n
	case *parser.DeleteStmt:
		n, err := ExecDelete(kv, cat, s)
		if err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
		return n
	case *parser.CreateIndexStmt:
		if err := ExecCreateIndex(kv, cat, s); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
		return 0
	case *parser.DropIndexStmt:
		if err := ExecDropIndex(kv, cat, s); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
		return 0
	case *parser.DropTableStmt:
		if err := cat.DropTable(s.Table); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
		return 0
	}
	t.Fatalf("unsupported statement %T", stmt)
	return 0
}

// tryWrite 是 execWrite 的错误透出版：期望失败的用例拿它拿错误。
func tryWrite(t *testing.T, kv *kvdb.DB, cat *catalog.Catalog, sql string) error {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		return err
	}
	switch s := stmt.(type) {
	case *parser.InsertStmt:
		_, err := ExecInsert(kv, cat, s)
		return err
	case *parser.UpdateStmt:
		_, err := ExecUpdate(kv, cat, s)
		return err
	case *parser.CreateIndexStmt:
		return ExecCreateIndex(kv, cat, s)
	case *parser.DropIndexStmt:
		return ExecDropIndex(kv, cat, s)
	}
	t.Fatalf("unsupported statement %T", stmt)
	return nil
}

// indexedTable：id INT 主键 + email TEXT + score FLOAT，灌 n 行
// （email = "u<i>"，score = i/2），建 email 非唯一索引 + score 唯一索引。
// 全部行走 INSERT 进去 —— 写路径的索引维护顺便被持续验证。
func indexedTable(t *testing.T, kv *kvdb.DB, cat *catalog.Catalog, n int) *catalog.Table {
	t.Helper()
	mustCreate(t, cat, "t",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "email", Kind: types.Text},
		catalog.Column{Name: "score", Kind: types.Float},
	)
	execWrite(t, kv, cat, "CREATE INDEX ix_email ON t (email)")
	execWrite(t, kv, cat, "CREATE UNIQUE INDEX ux_score ON t (score)")
	for i := 0; i < n; i++ {
		execWrite(t, kv, cat,
			"INSERT INTO t VALUES ("+itoa(i)+", 'u"+itoa(i)+"', "+ftoa(float64(i)/2)+")")
	}
	return mustGetTable(t, cat, "t")
}

func mustGetTable(t *testing.T, cat *catalog.Catalog, name string) *catalog.Table {
	t.Helper()
	tab, ok := cat.GetTable(name)
	if !ok {
		t.Fatalf("table %s not found", name)
	}
	return tab
}

func itoa(i int) string { return strconv.Itoa(i) }

// ftoa 渲染测试用的 FLOAT 字面量（%g 足够覆盖 0.5 / 7 / 49 这类数据）。
func ftoa(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// ── 下推可观测性（DESIGN §7.5 的验收线）──────────────────────────

func TestIndexScanObservable(t *testing.T) {
	kv, cat := newEnv(t)
	indexedTable(t, kv, cat, 100)

	// 不可下推的谓词（LIKE 不是比较运算）：全表扫 100 行，索引条目 0。
	plan, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE email LIKE 'u7'")
	if plan.RowsScanned() != 100 || plan.IndexEntriesScanned() != 0 {
		t.Fatalf("full scan: rows=%d entries=%d, want 100/0", plan.RowsScanned(), plan.IndexEntriesScanned())
	}
	if len(rows) != 1 || rows[0][0].I != 7 {
		t.Fatalf("wrong result: %v", rows)
	}

	// 有索引列等值谓词：走 IndexScan，数据区 0 行，条目+回表 = 命中数。
	plan, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE email = 'u7'")
	if plan.RowsScanned() != 0 {
		t.Fatalf("index scan should not touch data region, rows=%d", plan.RowsScanned())
	}
	if got := plan.IndexEntriesScanned(); got != 2 { // 1 条目 + 1 回表
		t.Fatalf("index entries = %d, want 2", got)
	}
	if len(rows) != 1 || rows[0][0].I != 7 {
		t.Fatalf("wrong result: %v", rows)
	}

	// 范围谓词同样走索引：u90..u99 共 10 行。
	plan, _ = runSelect(t, kv, cat, "SELECT id FROM t WHERE email >= 'u90'")
	if plan.RowsScanned() != 0 || plan.IndexEntriesScanned() != 20 {
		t.Fatalf("range: rows=%d entries=%d, want 0/20", plan.RowsScanned(), plan.IndexEntriesScanned())
	}

	// 主键优先：id 谓词永远走主键下推，不碰索引。
	plan, _ = runSelect(t, kv, cat, "SELECT id FROM t WHERE id > 90 AND email = 'u7'")
	if plan.RowsScanned() != 9 || plan.IndexEntriesScanned() != 0 {
		t.Fatalf("pk priority: rows=%d entries=%d, want 9/0", plan.RowsScanned(), plan.IndexEntriesScanned())
	}

	// 类型不匹配（FLOAT 列配 INT 字面量）不折算区间，退回全表 + Filter。
	plan, _ = runSelect(t, kv, cat, "SELECT id FROM t WHERE score = 7")
	if plan.RowsScanned() != 100 || plan.IndexEntriesScanned() != 0 {
		t.Fatalf("type mismatch: rows=%d entries=%d, want 100/0", plan.RowsScanned(), plan.IndexEntriesScanned())
	}

	// 唯一索引列的谓词同样可走索引（等值优先已验证；这里钉范围）。
	plan, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE score > 49.0")
	if plan.RowsScanned() != 0 {
		t.Fatalf("unique range should use index, rows=%d", plan.RowsScanned())
	}
	if len(rows) != 1 || rows[0][0].I != 99 {
		t.Fatalf("wrong range result: %v", rows)
	}
}

// ── 结果正确性：索引路径与全表路径给出同样的答案 ─────────────────

func TestIndexScanCorrectness(t *testing.T) {
	kv, cat := newEnv(t)
	indexedTable(t, kv, cat, 50)

	for _, sql := range []string{
		"SELECT id, email, score FROM t WHERE email = 'u25' ORDER BY id",
		"SELECT id FROM t WHERE email < 'u05' ORDER BY id",
		"SELECT id FROM t WHERE email >= 'u40' ORDER BY id",
		"SELECT id FROM t WHERE email > 'u10' AND email <= 'u15' ORDER BY id",
		"SELECT id FROM t WHERE score = 12.5 ORDER BY id",
	} {
		_, viaIndex := runSelect(t, kv, cat, sql)
		// 同一谓词换写成类型一致的另一个入口不太现实 —— 对照组用
		// OR 展开不了的补救：把索引删掉再跑一遍，结果必须逐行一致。
		execWrite(t, kv, cat, "DROP INDEX ix_email")
		execWrite(t, kv, cat, "DROP INDEX ux_score")
		_, viaTable := runSelect(t, kv, cat, sql)
		execWrite(t, kv, cat, "CREATE INDEX ix_email ON t (email)")
		execWrite(t, kv, cat, "CREATE UNIQUE INDEX ux_score ON t (score)")

		if len(viaIndex) != len(viaTable) {
			t.Fatalf("%s: %d rows via index, %d via table", sql, len(viaIndex), len(viaTable))
		}
		for i := range viaIndex {
			for j := range viaIndex[i] {
				if viaIndex[i][j].String() != viaTable[i][j].String() {
					t.Fatalf("%s: row %d col %d: %v via index, %v via table",
						sql, i, j, viaIndex[i][j], viaTable[i][j])
				}
			}
		}
	}
}

// ── 唯一约束 ─────────────────────────────────────────────────────

func TestUniqueIndex(t *testing.T) {
	kv, cat := newEnv(t)
	mustCreate(t, cat, "t",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "email", Kind: types.Text},
	)
	execWrite(t, kv, cat, "CREATE UNIQUE INDEX ux_email ON t (email)")
	execWrite(t, kv, cat, "INSERT INTO t VALUES (1, 'a@x'), (2, 'b@x')")

	// 已提交值冲突。
	if err := tryWrite(t, kv, cat, "INSERT INTO t VALUES (3, 'a@x')"); err == nil ||
		!strings.Contains(err.Error(), "duplicate value") {
		t.Fatalf("committed duplicate: %v", err)
	}
	// 同批两行同值（Get 查重看不见，必须由语句内查重兜住）。
	if err := tryWrite(t, kv, cat, "INSERT INTO t VALUES (3, 'c@x'), (4, 'c@x')"); err == nil ||
		!strings.Contains(err.Error(), "duplicate value") {
		t.Fatalf("in-batch duplicate: %v", err)
	}
	// 失败一行不落：上一条语句的 (3,'c@x') 不能残留。
	_, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE email = 'c@x'")
	if len(rows) != 0 {
		t.Fatalf("failed statement left rows: %v", rows)
	}
	// 主键同批重复也顺带被兜住（M3 的洞，M6 补上）。
	if err := tryWrite(t, kv, cat, "INSERT INTO t VALUES (5, 'e@x'), (5, 'f@x')"); err == nil ||
		!strings.Contains(err.Error(), "duplicate primary key") {
		t.Fatalf("in-batch duplicate pk: %v", err)
	}
	// UPDATE 撞唯一索引。
	if err := tryWrite(t, kv, cat, "UPDATE t SET email = 'a@x' WHERE id = 2"); err == nil ||
		!strings.Contains(err.Error(), "duplicate value") {
		t.Fatalf("update into duplicate: %v", err)
	}
	// UPDATE 同值改写放行（比较对象是"条目里的主键 ≠ 本行主键"）。
	execWrite(t, kv, cat, "UPDATE t SET email = 'b@x' WHERE id = 2")
	// UPDATE 成功改值后索引反映新值。
	execWrite(t, kv, cat, "UPDATE t SET email = 'bb@x' WHERE id = 2")
	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE email = 'bb@x'")
	if len(rows) != 1 || rows[0][0].I != 2 {
		t.Fatalf("index not maintained by UPDATE: %v", rows)
	}
}

func TestUniqueIndexNullExempt(t *testing.T) {
	kv, cat := newEnv(t)
	mustCreate(t, cat, "t",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "email", Kind: types.Text},
	)
	execWrite(t, kv, cat, "CREATE UNIQUE INDEX ux_email ON t (email)")
	// 多个 NULL 共存（SQL 标准语义），NULL 之外照常查重。
	execWrite(t, kv, cat, "INSERT INTO t VALUES (1, NULL), (2, NULL), (3, 'a@x')")
	if err := tryWrite(t, kv, cat, "INSERT INTO t VALUES (4, 'a@x')"); err == nil {
		t.Fatal("non-NULL duplicate accepted")
	}
	_, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE email IS NULL")
	if len(rows) != 2 {
		t.Fatalf("NULL rows: %v", rows)
	}
}

// ── 写路径维护 ───────────────────────────────────────────────────

func TestIndexMaintenanceByDML(t *testing.T) {
	kv, cat := newEnv(t)
	tab := indexedTable(t, kv, cat, 10)

	// DELETE 后索引条目同步消失：查不到、且索引侧计数为 0 行命中。
	execWrite(t, kv, cat, "DELETE FROM t WHERE id = 5")
	_, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE email = 'u5'")
	if len(rows) != 0 {
		t.Fatalf("deleted row still visible via index: %v", rows)
	}
	// DELETE 索引列没改 —— 但行没了条目必须没。表上有两张索引，
	// 剩 9 行 → 每张索引 9 条 = 18 条。
	if got := countIndexEntries(t, kv, tab); got != 18 {
		t.Fatalf("index entries after DELETE = %d, want 18", got)
	}

	// UPDATE 索引列：旧条目消失、新条目出现。
	execWrite(t, kv, cat, "UPDATE t SET email = 'renamed' WHERE id = 3")
	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE email = 'u3'")
	if len(rows) != 0 {
		t.Fatalf("old email still visible via index: %v", rows)
	}
	_, rows = runSelect(t, kv, cat, "SELECT id FROM t WHERE email = 'renamed'")
	if len(rows) != 1 || rows[0][0].I != 3 {
		t.Fatalf("new email not visible via index: %v", rows)
	}

	// UPDATE 不涉及索引列（同值更新唯一索引列）：条目数不变、查询照常。
	execWrite(t, kv, cat, "UPDATE t SET score = score + 0 WHERE id = 1")
	if got := countIndexEntries(t, kv, tab); got != 18 {
		t.Fatalf("index entries after no-op UPDATE = %d, want 18", got)
	}
}

func countIndexEntries(t *testing.T, kv *kvdb.DB, tab *catalog.Table) int {
	t.Helper()
	it := kv.NewIterator(&kvdb.IteratorOptions{Prefix: encoding.IndexKeyPrefix(tab.ID)})
	defer it.Close()
	n := 0
	for it.SeekToFirst(); it.Valid(); it.Next() {
		n++
	}
	if err := it.Error(); err != nil {
		t.Fatalf("iterate index entries: %v", err)
	}
	return n
}

// ── 回填 / 重开 / DROP 清理 ──────────────────────────────────────

func TestCreateIndexBackfill(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	kv := openKV(t, dir)
	cat := loadCatalog(t, kv)
	// 先灌数据（直写，不经过索引维护），再建索引 —— 回填必须把既有行
	// 全部补进条目。
	tab := mustCreate(t, cat, "t",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "email", Kind: types.Text},
	)
	for i := 0; i < 30; i++ {
		putRow(t, kv, tab, types.IntValue(int64(i)), types.TextValue("u"+itoa(i)))
	}
	execWrite(t, kv, cat, "CREATE INDEX ix_email ON t (email)")
	if got := countIndexEntries(t, kv, tab); got != 30 {
		t.Fatalf("backfill entries = %d, want 30", got)
	}
	_, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE email = 'u17'")
	if len(rows) != 1 || rows[0][0].I != 17 {
		t.Fatalf("backfilled index wrong result: %v", rows)
	}

	// 关库重开持久：索引还在、还能走。
	kv.Close()
	kv = openKV(t, dir)
	cat = loadCatalog(t, kv)
	plan, rows := runSelect(t, kv, cat, "SELECT id FROM t WHERE email = 'u17'")
	if len(rows) != 1 || plan.RowsScanned() != 0 {
		t.Fatalf("after reopen: rows=%v rowsScanned=%d", rows, plan.RowsScanned())
	}
}

// openKV / loadCatalog 是 newEnv 的拆开版：需要自己控制"关库 → 重开"时用。
func openKV(t *testing.T, dir string) *kvdb.DB {
	t.Helper()
	kv, err := kvdb.Open(kvdb.Options{Dir: dir})
	if err != nil {
		t.Fatalf("open kvdb: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })
	return kv
}

func loadCatalog(t *testing.T, kv *kvdb.DB) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Load(kv)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return cat
}

func TestCreateUniqueIndexBackfillConflict(t *testing.T) {
	kv, cat := newEnv(t)
	tab := mustCreate(t, cat, "t",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "a", Kind: types.Int},
	)
	putRow(t, kv, tab, types.IntValue(1), types.IntValue(7))
	putRow(t, kv, tab, types.IntValue(2), types.IntValue(7))

	if err := tryWrite(t, kv, cat, "CREATE UNIQUE INDEX ux_a ON t (a)"); err == nil ||
		!strings.Contains(err.Error(), "duplicate value") {
		t.Fatalf("backfill conflict: %v", err)
	}
	// 冲突后：索引不存在、条目被回滚（一行索引不剩，DESIGN §7.5）。
	if _, ok := cat.GetIndex("ux_a"); ok {
		t.Fatal("index meta written despite conflict")
	}
	if got := countIndexEntries(t, kv, tab); got != 0 {
		t.Fatalf("orphan entries left after conflict = %d, want 0", got)
	}

	// 非冲突的唯一回填照常。
	execWrite(t, kv, cat, "DELETE FROM t WHERE id = 2")
	execWrite(t, kv, cat, "CREATE UNIQUE INDEX ux_a ON t (a)")
	if err := tryWrite(t, kv, cat, "INSERT INTO t VALUES (2, 7)"); err == nil {
		t.Fatal("unique constraint missing after backfilled create")
	}
}

func TestDropIndexRemovesEntries(t *testing.T) {
	kv, cat := newEnv(t)
	tab := indexedTable(t, kv, cat, 20)

	execWrite(t, kv, cat, "DROP INDEX ix_email")
	if got := countIndexEntries(t, kv, tab); got != 20 { // 只剩 ux_score 的条目
		t.Fatalf("entries after DROP INDEX = %d, want 20", got)
	}
	// 查询退回全表扫。
	plan, _ := runSelect(t, kv, cat, "SELECT id FROM t WHERE email = 'u7'")
	if plan.RowsScanned() != 20 || plan.IndexEntriesScanned() != 0 {
		t.Fatalf("after drop: rows=%d entries=%d, want 20/0", plan.RowsScanned(), plan.IndexEntriesScanned())
	}
	// IF EXISTS 的静默语义。
	execWrite(t, kv, cat, "DROP INDEX IF EXISTS ix_email")
}

func TestDropTableCleansIndexEntries(t *testing.T) {
	kv, cat := newEnv(t)
	tab := indexedTable(t, kv, cat, 15)
	execWrite(t, kv, cat, "DROP TABLE t")
	if got := countIndexEntries(t, kv, tab); got != 0 {
		t.Fatalf("index entries survived DROP TABLE: %d", got)
	}
}

func TestCreateIndexValidations(t *testing.T) {
	kv, cat := newEnv(t)
	mustCreate(t, cat, "t",
		catalog.Column{Name: "id", Kind: types.Int, PrimaryKey: true},
		catalog.Column{Name: "a", Kind: types.Int},
	)
	for _, sql := range []string{
		"CREATE INDEX ix ON ghost (a)", // 表不存在
		"CREATE INDEX ix ON t (ghost)", // 列不存在
		"CREATE INDEX ix ON t (id)",    // 主键列
	} {
		if err := tryWrite(t, kv, cat, sql); err == nil {
			t.Errorf("tryWrite(%q) = nil error, want error", sql)
		}
	}
	execWrite(t, kv, cat, "CREATE INDEX IF NOT EXISTS ix ON t (a)")
	execWrite(t, kv, cat, "CREATE INDEX IF NOT EXISTS ix ON t (a)") // 静默成功
	execWrite(t, kv, cat, "DROP INDEX IF EXISTS nope")              // 静默成功
	if err := tryWrite(t, kv, cat, "CREATE INDEX ix ON t (a)"); err == nil {
		t.Error("duplicate index accepted without IF NOT EXISTS")
	}
}
