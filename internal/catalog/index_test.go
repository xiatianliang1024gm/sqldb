package catalog

// M6 索引元数据的单测：CreateIndex/DropIndex 的校验与持久化，
// 以及 DROP TABLE 对索引 meta 的连带清理（DESIGN §5）。

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

func TestIndexCRUD(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cat := openCatalog(t, dir)
	cat.CreateTable("users", []Column{
		{Name: "id", Kind: types.Int, PrimaryKey: true},
		{Name: "email", Kind: types.Text},
		{Name: "age", Kind: types.Int},
	})

	if err := cat.CreateIndex(IndexDef{Name: "ix_email", Table: "users", Column: "email"}); err != nil {
		t.Fatalf("create index: %v", err)
	}
	d, ok := cat.GetIndex("ix_email")
	if !ok || d.Table != "users" || d.Column != "email" || d.Unique {
		t.Fatalf("got %+v (ok=%v)", d, ok)
	}
	idxs := cat.IndexesOf(mustGetTable(t, cat, "users"))
	if len(idxs) != 1 || idxs[0].Name != "ix_email" {
		t.Fatalf("IndexesOf: %+v", idxs)
	}

	// 重名拒绝。
	if err := cat.CreateIndex(IndexDef{Name: "ix_email", Table: "users", Column: "age"}); !errors.Is(err, ErrIndexExists) {
		t.Fatalf("duplicate index name: %v", err)
	}
	if err := cat.CreateIndex(IndexDef{Name: "ix_u", Table: "users", Column: "age", Unique: true}); err != nil {
		t.Fatalf("create unique index: %v", err)
	}

	// 引用不存在的表 / 列 / 主键列，全部拒绝。
	for _, d := range []IndexDef{
		{Name: "bad1", Table: "ghost", Column: "x"},
		{Name: "bad2", Table: "users", Column: "ghost"},
		{Name: "bad3", Table: "users", Column: "id"}, // 主键列：本身就是有序入口
	} {
		if err := cat.CreateIndex(d); err == nil {
			t.Errorf("CreateIndex(%+v) = nil error, want error", d)
		}
	}

	// 关库重开不丢（含唯一位）。
	closeKV(t, cat)
	cat = openCatalog(t, dir)
	if d, ok := cat.GetIndex("ix_u"); !ok || !d.Unique {
		t.Fatalf("after reopen: got %+v (ok=%v)", d, ok)
	}
	if got := cat.IndexesOf(mustGetTable(t, cat, "users")); len(got) != 2 {
		t.Fatalf("after reopen: IndexesOf = %+v, want 2 entries", got)
	}

	// DropIndex：定义消失，未知的名字报 ErrIndexNotFound。
	if _, err := cat.DropIndex("ix_email"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, ok := cat.GetIndex("ix_email"); ok {
		t.Fatal("ix_email still present after drop")
	}
	if _, err := cat.DropIndex("ix_email"); !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("drop missing index: %v", err)
	}
}

func TestDropTableCleansIndexMeta(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cat := openCatalog(t, dir)
	cat.CreateTable("t", []Column{
		{Name: "id", Kind: types.Int, PrimaryKey: true},
		{Name: "a", Kind: types.Int},
	})
	cat.CreateIndex(IndexDef{Name: "ix_a", Table: "t", Column: "a"})
	cat.CreateTable("keep", []Column{
		{Name: "id", Kind: types.Int, PrimaryKey: true},
		{Name: "a", Kind: types.Int},
	})
	cat.CreateIndex(IndexDef{Name: "ix_keep", Table: "keep", Column: "a"})

	if err := cat.DropTable("t"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, ok := cat.GetIndex("ix_a"); ok {
		t.Fatal("ix_a survived DROP TABLE")
	}
	// 别的表的索引不受牵连。
	if _, ok := cat.GetIndex("ix_keep"); !ok {
		t.Fatal("ix_keep was collateral damage")
	}

	// 关库重开后依旧干净 —— meta 键真的从 KV 里删掉了。
	closeKV(t, cat)
	cat = openCatalog(t, dir)
	if _, ok := cat.GetIndex("ix_a"); ok {
		t.Fatal("ix_a survived reopen after DROP TABLE")
	}
}

// ── 帮手 ─────────────────────────────────────────────────────────

func mustGetTable(t *testing.T, c *Catalog, name string) *Table {
	t.Helper()
	tab, ok := c.GetTable(name)
	if !ok {
		t.Fatalf("table %s not found", name)
	}
	return tab
}
