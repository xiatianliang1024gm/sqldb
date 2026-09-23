package catalog

// M0 验收单测：catalog 的核心承诺是"建表 → 关库 → 重开，schema 原样可读"。
// 顺带把建表校验和 DROP 的数据清理钉死。

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/xiatianliang1024gm/kvdb"
	"github.com/xiatianliang1024gm/sqldb/internal/encoding"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// openCatalog 打开一个全新的（或已存在的）kvdb 目录并加载 catalog。
func openCatalog(t *testing.T, dir string) *Catalog {
	t.Helper()
	kv, err := kvdb.Open(kvdb.Options{Dir: dir})
	if err != nil {
		t.Fatalf("open kvdb at %s: %v", dir, err)
	}
	t.Cleanup(func() { kv.Close() })
	cat, err := Load(kv)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return cat
}

func intPK(name string) []Column {
	return []Column{{Name: name, Kind: types.Int, PrimaryKey: true}}
}

// TestCreateAndReopen M0 的验收主线：建表、关库、重开，schema 一模一样。
func TestCreateAndReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	cat := openCatalog(t, dir)
	users, err := cat.CreateTable("users", []Column{
		{Name: "id", Kind: types.Int, PrimaryKey: true},
		{Name: "name", Kind: types.Text, NotNull: true},
		{Name: "score", Kind: types.Float},
		{Name: "active", Kind: types.Bool},
	})
	if err != nil {
		t.Fatalf("create users: %v", err)
	}
	events, err := cat.CreateTable("events", []Column{
		{Name: "key", Kind: types.Text, PrimaryKey: true},
		{Name: "payload", Kind: types.Text},
	})
	if err != nil {
		t.Fatalf("create events: %v", err)
	}
	// 无主键表：自动补隐藏 rowid 主键
	logs, err := cat.CreateTable("logs", []Column{{Name: "msg", Kind: types.Text}})
	if err != nil {
		t.Fatalf("create logs: %v", err)
	}
	if !logs.HasRowid {
		t.Fatal("table without primary key must have hidden rowid")
	}
	if pk := logs.PrimaryKeyIndex(); pk != 1 || logs.Columns[pk].Name != "rowid" ||
		logs.Columns[pk].Kind != types.Int || !logs.Columns[pk].NotNull {
		t.Fatalf("hidden rowid column wrong: %+v", logs.Columns)
	}

	if got := len(cat.Tables()); got != 3 {
		t.Fatalf("catalog has %d tables, want 3", got)
	}
	closeKV(t, cat)

	// 重开：schema 必须原样回来
	cat2 := openCatalog(t, dir)
	for _, want := range []*Table{users, events, logs} {
		got, ok := cat2.GetTable(want.Name)
		if !ok {
			t.Fatalf("table %s missing after reopen", want.Name)
		}
		if got.ID != want.ID {
			t.Fatalf("table %s: ID %d after reopen, want %d", want.Name, got.ID, want.ID)
		}
		if got.HasRowid != want.HasRowid || len(got.Columns) != len(want.Columns) {
			t.Fatalf("table %s schema changed after reopen:\n got %+v\nwant %+v", want.Name, got, want)
		}
		for i, col := range want.Columns {
			if got.Columns[i] != col {
				t.Fatalf("table %s column %d changed after reopen: got %+v want %+v",
					want.Name, i, got.Columns[i], col)
			}
		}
	}
}

// closeKV 关闭 catalog 背后的 kvdb（绕开 openCatalog 注册的 cleanup 重复关闭
// 是安全的 —— kvdb 的 Close 重复调用返回错误，这里只调一次）。
func closeKV(t *testing.T, c *Catalog) {
	t.Helper()
	if err := c.kv.Close(); err != nil {
		t.Fatalf("close kvdb: %v", err)
	}
}

// TestTableIDMonotonicAcrossReopen ID 分配计数器也持久化：重开后再建表，
// 新 ID 必须接着旧的最大值走，不能从 0 重来 —— 否则新表的行键会砸进旧表区间。
func TestTableIDMonotonicAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	cat := openCatalog(t, dir)
	first, err := cat.CreateTable("a", intPK("id"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateTable("b", intPK("id")); err != nil {
		t.Fatal(err)
	}
	closeKV(t, cat)

	cat2 := openCatalog(t, dir)
	third, err := cat2.CreateTable("c", intPK("id"))
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != first.ID+2 {
		t.Fatalf("new table ID %d, want %d (must continue after reopen)", third.ID, first.ID+2)
	}
}

// TestCreateTableValidation 建表校验：重复表名、重复列名、双主键、保留名 rowid。
func TestCreateTableValidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cat := openCatalog(t, dir)

	if _, err := cat.CreateTable("t", intPK("id")); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateTable("t", intPK("id")); !errors.Is(err, ErrTableExists) {
		t.Fatalf("duplicate table: got %v, want ErrTableExists", err)
	}
	if _, err := cat.CreateTable("t2", []Column{
		{Name: "a", Kind: types.Int}, {Name: "a", Kind: types.Text},
	}); err == nil {
		t.Fatal("duplicate column names must fail")
	}
	if _, err := cat.CreateTable("t2", []Column{
		{Name: "a", Kind: types.Int, PrimaryKey: true}, {Name: "b", Kind: types.Int, PrimaryKey: true},
	}); err == nil {
		t.Fatal("two primary key columns must fail")
	}
	if _, err := cat.CreateTable("t2", []Column{{Name: "ROWID", Kind: types.Int}}); !errors.Is(err, ErrReservedColumn) {
		t.Fatalf("column named ROWID: got %v, want ErrReservedColumn", err)
	}
	if _, err := cat.CreateTable("t2", nil); err == nil {
		t.Fatal("zero columns must fail")
	}
	// 失败的建表不能留下半截状态
	if _, ok := cat.GetTable("t2"); ok {
		t.Fatal("failed CreateTable must not register the table")
	}
}

// TestPrimaryKeyImpliesNotNull 主键列隐式 NOT NULL（DESIGN §4.4）。
func TestPrimaryKeyImpliesNotNull(t *testing.T) {
	tbl, err := buildTable("t", []Column{{Name: "id", Kind: types.Int, PrimaryKey: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !tbl.Columns[0].NotNull {
		t.Fatal("primary key column must be implicitly NOT NULL")
	}
}

// TestDropTableRemovesEverything DROP 后：内存没了、重开也没了、
// 数据前缀干干净净、rowseq 计数器一并清掉。
func TestDropTableRemovesEverything(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cat := openCatalog(t, dir)

	tbl, err := cat.CreateTable("t", intPK("id"))
	if err != nil {
		t.Fatal(err)
	}
	// 直接借 kvdb 模拟执行器写入 2500 行 —— 超过 batchDeleteLimit(1000)，
	// 顺便验证分批删除真的会循环。
	for i := 0; i < 2500; i++ {
		pkEnc, err := encoding.EncodeKey(types.IntValue(int64(i)))
		if err != nil {
			t.Fatal(err)
		}
		row := encoding.EncodeRow([]types.Value{types.IntValue(int64(i))})
		if err := cat.kv.Put(encoding.RowKey(tbl.ID, pkEnc), row); err != nil {
			t.Fatal(err)
		}
	}
	prefix := encoding.RowKeyPrefix(tbl.ID)
	if n := countKeys(t, cat, prefix); n != 2500 {
		t.Fatalf("wrote 2500 rows, prefix scan sees %d", n)
	}

	if err := cat.DropTable("no-such-table"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("drop unknown table: got %v, want ErrTableNotFound", err)
	}
	if err := cat.DropTable("t"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, ok := cat.GetTable("t"); ok {
		t.Fatal("dropped table still in memory")
	}
	if n := countKeys(t, cat, prefix); n != 0 {
		t.Fatalf("%d row keys left after DROP", n)
	}
	closeKV(t, cat)

	cat2 := openCatalog(t, dir)
	if _, ok := cat2.GetTable("t"); ok {
		t.Fatal("dropped table resurrected after reopen")
	}
	if n := countKeys(t, cat2, prefix); n != 0 {
		t.Fatalf("%d row keys left after reopen", n)
	}
}

func countKeys(t *testing.T, c *Catalog, prefix []byte) int {
	t.Helper()
	it := c.kv.NewIterator(&kvdb.IteratorOptions{Prefix: prefix})
	defer it.Close()
	n := 0
	for it.SeekToFirst(); it.Valid(); it.Next() {
		n++
	}
	if err := it.Error(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return n
}
