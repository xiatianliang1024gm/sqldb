package exec

// 二级索引（M6，DESIGN §7.5 + §8）：
//
//   - 条目键构造与唯一查重（写路径三条语句 + 回填共用）；
//   - ExecCreateIndex / ExecDropIndex（含既有数据的回填）；
//   - IndexScanNode：索引条目区间扫 → 拆出主键 → 点查回表 → 解码。
//
// 写路径的分工约定（DESIGN §8）：行与它的索引条目编进同一个 WriteBatch，
// 同生同死；唯一查重发生在提交之前，任何一行冲突整条语句一行不落。
// 本语句多行之间的重复靠 Get 看不见（都没提交），由 writeScratch 补上
// —— 它同时修掉了 M3 遗留的"同批两条 INSERT 同主键"漏检。

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/xiatianliang1024gm/kvdb"
	"github.com/xiatianliang1024gm/sqldb/internal/catalog"
	"github.com/xiatianliang1024gm/sqldb/internal/encoding"
	"github.com/xiatianliang1024gm/sqldb/internal/parser"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// indexBatchLimit 是回填 / 条目清理单批的条目数上限（与 catalog 的
// batchDeleteLimit 同一套保守做法：WriteBatch 有 64MB 上限，按条数切）。
const indexBatchLimit = 1000

// ── 条目键与查重 ─────────────────────────────────────────────────

// indexEntryKey 按索引定义从一行算出条目键。索引列在 schema 里必然
// 存在（catalog 写入时校验过），防御分支只为损坏的内存态报错。
func indexEntryKey(t *catalog.Table, d *catalog.IndexDef, row []types.Value) ([]byte, error) {
	ci := t.ColumnIndex(d.Column)
	if ci < 0 {
		return nil, fmt.Errorf("index %s references missing column %s", d.Name, d.Column)
	}
	idxEnc, err := encoding.EncodeIndexValue(row[ci])
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", d.Name, err)
	}
	pkEnc, err := encoding.EncodeKey(row[t.PrimaryKeyIndex()])
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", d.Name, err)
	}
	return encoding.IndexEntryKey(t.ID, d.Name, idxEnc, pkEnc), nil
}

// writeScratch 收集"本语句已决定写入 batch 的键"。同批多行互相看不见
// —— Get 查重只对已提交数据有效，语句内部的重复必须在这里兜住。
type writeScratch struct {
	rows  map[string]bool            // 行键（主键查重，M6 顺手补 M3 的洞）
	uvals map[string]map[string]bool // 唯一索引：值前缀 → 已占用主键集合
}

func newWriteScratch() *writeScratch {
	return &writeScratch{rows: make(map[string]bool), uvals: make(map[string]map[string]bool)}
}

// checkUnique 对唯一索引 d 检查"把值 v、主键 pkEnc 写进去"是否冲突。
// 两道关：已提交数据（对索引值前缀 seek，读到的条目主键不等于自己就是
// 冲突）+ 本语句先前写下的同值行（scratch）。NULL 豁免（SQL 标准语义，
// DESIGN 取舍表 #14）。
func checkUnique(kv kvdb.Reader, t *catalog.Table, d *catalog.IndexDef, v types.Value, pkEnc []byte, s *writeScratch) error {
	if v.IsNull() {
		return nil
	}
	enc, err := encoding.EncodeKey(v)
	if err != nil {
		return fmt.Errorf("index %s: %w", d.Name, err)
	}
	prefix := string(encoding.IndexValuePrefix(t.ID, d.Name, enc))
	pk := string(pkEnc)

	// 关 1：本语句先前写下的同值主键。
	if seen := s.uvals[prefix]; seen != nil {
		for other := range seen {
			if other != pk {
				return fmt.Errorf("duplicate value %v for unique index %s", v, d.Name)
			}
		}
	}

	// 关 2：已提交数据。seek 到值前缀的第一个条目，比较尾部主键 ——
	// 前缀无歧义保证这第一个就是"全部同值条目"中的任意代表。
	upper := prefixSuccessor([]byte(prefix))
	it := kv.NewIterator(&kvdb.IteratorOptions{LowerBound: []byte(prefix), UpperBound: upper})
	it.Seek([]byte(prefix))
	// 迭代器本身就是空的（该值没有条目）＝ 无冲突。
	if !it.Valid() {
		err := it.Error()
		it.Close()
		if err != nil {
			return fmt.Errorf("check unique index %s: %w", d.Name, err)
		}
	} else {
		_, _, _, entryPK, err := encoding.SplitIndexEntryKey(it.Key())
		it.Close()
		if err != nil {
			return fmt.Errorf("check unique index %s: %w", d.Name, err)
		}
		if !bytes.Equal(entryPK, pkEnc) {
			return fmt.Errorf("duplicate value %v for unique index %s", v, d.Name)
		}
	}

	// 记账：这个值现在归本行占用（对本语句的后续行可见）。
	if s.uvals[prefix] == nil {
		s.uvals[prefix] = make(map[string]bool)
	}
	s.uvals[prefix][pk] = true
	return nil
}

// putIndexEntries / deleteIndexEntries 把一行的全部索引条目编进 batch。
// 条目值为空：键本身就是全部信息（定位 + 回表），value 无事可做。
func putIndexEntries(batch *kvdb.WriteBatch, cat *catalog.Catalog, t *catalog.Table, row []types.Value) error {
	for _, d := range cat.IndexesOf(t) {
		k, err := indexEntryKey(t, d, row)
		if err != nil {
			return err
		}
		if err := batch.Put(k, nil); err != nil {
			return err
		}
	}
	return nil
}

func deleteIndexEntries(batch *kvdb.WriteBatch, cat *catalog.Catalog, t *catalog.Table, row []types.Value) error {
	for _, d := range cat.IndexesOf(t) {
		k, err := indexEntryKey(t, d, row)
		if err != nil {
			return err
		}
		if err := batch.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// ── CREATE INDEX（含回填）────────────────────────────────────────

// ExecCreateIndex 执行 CREATE [UNIQUE] INDEX：校验 → 扫全表回填条目
// （分批提交，唯一索引逐行查重）→ 最后写 schema meta（DESIGN §7.5：
// meta 最后写，崩溃留下的孤儿条目不可见、幂等、DROP TABLE 带走）。
// IF NOT EXISTS 且索引已存在时按承诺静默成功。
func ExecCreateIndex(kv kvdb.Store, cat *catalog.Catalog, stmt *parser.CreateIndexStmt) error {
	t, ok := cat.GetTable(stmt.Table)
	if !ok {
		return fmt.Errorf("no such table: %s", stmt.Table)
	}
	if _, exists := cat.GetIndex(stmt.Name); exists {
		if stmt.IfNotExists {
			return nil
		}
		return fmt.Errorf("create index %s: %w", stmt.Name, catalog.ErrIndexExists)
	}
	// 廉价预检，别等扫完整表才发现列名写错了（正式校验在 cat.CreateIndex 再做一遍）。
	if ci := t.ColumnIndex(stmt.Column); ci < 0 {
		return fmt.Errorf("no such column: %s.%s", stmt.Table, stmt.Column)
	} else if t.Columns[ci].PrimaryKey {
		return fmt.Errorf("column %s.%s is the primary key and is already indexed", stmt.Table, stmt.Column)
	}

	def := &catalog.IndexDef{Name: stmt.Name, Table: stmt.Table, Column: stmt.Column, Unique: stmt.Unique}
	pkIdx := t.PrimaryKeyIndex()
	scratch := newWriteScratch()
	// 注意：此刻 meta 还没写，cat.IndexesOf(t) 是空的 —— 回填只能用
	// 局部的 def，这也是"meta 最后写"的必然结果（DESIGN §7.5）。
	defs := []*catalog.IndexDef{def}

	// 回填：按主键序扫全表（scanTableRows 无界无谓词），逐行编条目。
	hits, _, err := scanTableRows(kv, t, nil, nil, nil)
	if err != nil {
		return err
	}
	var (
		batch     = kvdb.NewWriteBatch()
		batchKeys [][]byte // 已编进当前 batch 的条目键（冲突回滚用）
		written   [][]byte // 已提交的条目键（冲突回滚用）
	)
	flush := func() error {
		if batch.Len() == 0 {
			return nil
		}
		if err := kv.Write(batch); err != nil {
			return fmt.Errorf("backfill index %s: %w", stmt.Name, err)
		}
		written = append(written, batchKeys...)
		batch = kvdb.NewWriteBatch()
		batchKeys = nil
		return nil
	}
	for _, h := range hits {
		pkEnc, err := encoding.EncodeKey(h.row[pkIdx])
		if err != nil {
			return err
		}
		for _, d := range defs {
			if !d.Unique {
				continue
			}
			v := h.row[t.ColumnIndex(d.Column)]
			if err := checkUnique(kv, t, d, v, pkEnc, scratch); err != nil {
				// 冲突：丢弃编了一半的 batch，回滚已提交的条目，
				// 整条语句一行索引不剩（DESIGN §7.5）。
				if err2 := rollbackEntries(kv, written); err2 != nil {
					return fmt.Errorf("backfill index %s: %w (cleanup also failed: %v)", stmt.Name, err, err2)
				}
				return fmt.Errorf("create index %s: %w", stmt.Name, err)
			}
		}
		for _, d := range defs {
			k, err := indexEntryKey(t, d, h.row)
			if err != nil {
				return err
			}
			if err := batch.Put(k, nil); err != nil {
				return err
			}
			batchKeys = append(batchKeys, k)
		}
		if batch.Len() >= indexBatchLimit {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}

	return cat.CreateIndex(*def)
}

// rollbackEntries 冲突回滚：把已提交的条目分批删掉。此时 meta 还没写，
// 这些条目本就不可见 —— 清理只是为了不留垃圾（DESIGN §7.5）。
func rollbackEntries(kv kvdb.Writer, written [][]byte) error {
	for start := 0; start < len(written); start += indexBatchLimit {
		end := min(start+indexBatchLimit, len(written))
		batch := kvdb.NewWriteBatch()
		for _, k := range written[start:end] {
			if err := batch.Delete(k); err != nil {
				return err
			}
		}
		if err := kv.Write(batch); err != nil {
			return err
		}
	}
	return nil
}

// ── DROP INDEX ───────────────────────────────────────────────────

// ExecDropIndex 执行 DROP INDEX：先删 schema meta（DropIndex），再分批
// 清条目。顺序必须是 meta 在前 —— 条目删一半时 meta 若还在，查询会相信
// 一个缺了条目的索引，结果少行；meta 先走，崩溃留下的只是不可见的孤儿
// 条目（重跑 CREATE INDEX 幂等重写，DROP TABLE 按表前缀带走）。
func ExecDropIndex(kv kvdb.Store, cat *catalog.Catalog, stmt *parser.DropIndexStmt) error {
	d, err := cat.DropIndex(stmt.Name)
	if err != nil {
		if errors.Is(err, catalog.ErrIndexNotFound) && stmt.IfExists {
			return nil
		}
		return fmt.Errorf("drop index %s: %w", stmt.Name, err)
	}
	t, ok := cat.GetTable(d.Table)
	if !ok {
		// meta 说表在、catalog 说不在：只可能是 catalog 之外的改动，防御报错。
		return fmt.Errorf("drop index %s: table %s vanished", stmt.Name, d.Table)
	}
	if err := deleteByPrefixRanges(kv, encoding.IndexRegionPrefix(t.ID, d.Name)); err != nil {
		return fmt.Errorf("drop index %s: %w", stmt.Name, err)
	}
	return nil
}

// deleteByPrefixRanges 按前缀分批删除全部键（exec 侧与 catalog.deleteByPrefix
// 同款，但直接走在五原语上）。
func deleteByPrefixRanges(kv kvdb.Store, prefix []byte) error {
	it := kv.NewIterator(&kvdb.IteratorOptions{Prefix: prefix})
	var keys [][]byte
	for it.SeekToFirst(); it.Valid(); it.Next() {
		keys = append(keys, append([]byte(nil), it.Key()...))
	}
	err := it.Error()
	it.Close()
	if err != nil {
		return err
	}
	for start := 0; start < len(keys); start += indexBatchLimit {
		end := min(start+indexBatchLimit, len(keys))
		batch := kvdb.NewWriteBatch()
		for _, k := range keys[start:end] {
			if err := batch.Delete(k); err != nil {
				return err
			}
		}
		if err := kv.Write(batch); err != nil {
			return err
		}
	}
	return nil
}

// ── IndexScan（M6，DESIGN §7.5）──────────────────────────────────

// IndexScanNode 二级索引扫描：迭代索引条目区间 → 拆出尾部主键编码 →
// 点查回表 → 解码成行。这是"花写放大买第二个有序入口"的兑现处：
// 区间扫省掉了全表，代价是每个命中条目一次点查。
type IndexScanNode struct {
	kv    kvdb.Reader
	table *catalog.Table
	index *catalog.IndexDef

	lower []byte // 条目键下界（含）——计划构建保证非空（i 前缀兜底）
	upper []byte // 条目键上界（不含）——计划构建保证非空（i 前缀后继兜底）

	kinds []types.Kind

	it      kvdb.Iterator
	entries int64 // 读过的索引条目数
	lookups int64 // 回表点查次数
	done    bool
}

// Scanned 返回读过的索引条目数；Lookups 返回回表次数。两者之和是这条
// 扫描从存储引擎读走的 KV 条目总数 —— 与 ScanNode 的 rowsScanned 对偶。
func (s *IndexScanNode) Scanned() int64 { return s.entries }
func (s *IndexScanNode) Lookups() int64 { return s.lookups }

func (s *IndexScanNode) Next() ([]types.Value, error) {
	if s.done {
		return nil, nil
	}
	if s.it == nil {
		s.it = s.kv.NewIterator(&kvdb.IteratorOptions{LowerBound: s.lower, UpperBound: s.upper})
		if err := s.it.Error(); err != nil {
			return nil, fmt.Errorf("index scan %s: %w", s.index.Name, err)
		}
		s.it.Seek(s.lower)
	}
	for {
		if !s.it.Valid() {
			if err := s.it.Error(); err != nil {
				return nil, fmt.Errorf("index scan %s: %w", s.index.Name, err)
			}
			s.done = true
			return nil, s.Close()
		}
		key := append([]byte(nil), s.it.Key()...) // Next 后失效，必须复制
		s.it.Next()
		_, _, _, pkEnc, err := encoding.SplitIndexEntryKey(key)
		if err != nil {
			return nil, fmt.Errorf("index scan %s: %w", s.index.Name, err)
		}
		s.entries++

		// 回表：条目尾部的主键编码就是行定位器（DESIGN §7.5 条目格式）。
		val, err := s.kv.Get(encoding.RowKey(s.table.ID, pkEnc))
		if errors.Is(err, kvdb.ErrNotFound) {
			// 行没了条目还在：只可能来自崩溃中间态（DESIGN §5 把 DROP
			// 的删除顺序压到最小这个窗口）。跳过，不当致命错误。
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("index scan %s: lookup row: %w", s.index.Name, err)
		}
		s.lookups++
		row, err := encoding.DecodeRow(val, s.kinds)
		if err != nil {
			return nil, fmt.Errorf("index scan %s: corrupt row: %w", s.index.Name, err)
		}
		return row, nil
	}
}

func (s *IndexScanNode) Close() error {
	if s.it != nil {
		err := s.it.Close()
		s.it = nil
		return err
	}
	return nil
}
