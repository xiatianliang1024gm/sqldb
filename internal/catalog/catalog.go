// Package catalog 维护"数据库里有哪些表"，是唯一知道 schema 的层（DESIGN §5）。
//
// 两个关键决定：
//
//  1. schema 不是独立文件，就存在同一个 KV 键空间的 meta 区 —— schema 和数据
//     在同一条 WAL 的保护下，崩溃一致性免费；一个目录即整库。
//  2. 打开时全量加载进内存（表数量级小），之后读走内存、写同时落 KV 和内存。
//
// 执行器永远面对"每张表都有主键"这一个世界（DESIGN §5）：无主键的表在
// Create 时自动补一个隐藏的 rowid INT 主键，值来自 rowseq 计数器（M3 的
// INSERT 路径才会用到计数器，这里只负责把计数器键摆好）。
package catalog

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/xiatianliang1024gm/kvdb"
	"github.com/xiatianliang1024gm/sqldb/internal/encoding"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// ── meta 区键布局（DESIGN §3）────────────────────────────────────
//
//	m\x00schema:<表名>    → JSON（表 ID、列定义、主键位）
//	m\x00index:<索引名>   → JSON（索引名、表名、列名、唯一位）（M6）
//	m\x00nextid           → 8B 大端：下一个可分配的表 ID
//	m\x00rowseq:<表ID>    → 8B 大端：该表 rowid 计数器（隐藏主键的分配游标）
//
// 前缀全部是固定文本，8B 二进制 ID 只出现在前缀之后，四个区域互不越界。
var (
	metaNextIDKey    = []byte("m\x00nextid")
	metaSchemaPrefix = []byte("m\x00schema:")
	metaIndexPrefix  = []byte("m\x00index:")
	metaRowSeqPrefix = []byte("m\x00rowseq:")
)

func schemaKey(name string) []byte {
	k := make([]byte, 0, len(metaSchemaPrefix)+len(name))
	k = append(k, metaSchemaPrefix...)
	return append(k, name...)
}

func rowSeqKey(tableID uint64) []byte {
	k := append([]byte{}, metaRowSeqPrefix...)
	return binary.BigEndian.AppendUint64(k, tableID)
}

func indexKey(name string) []byte {
	k := make([]byte, 0, len(metaIndexPrefix)+len(name))
	k = append(k, metaIndexPrefix...)
	return append(k, name...)
}

// batchDeleteLimit 是 DROP TABLE 单批删除的行数上限。WriteBatch 编码后有
// 64MB 上限而行键长度不定，按行数切是教学项目够用的保守做法（DESIGN §5）。
const batchDeleteLimit = 1000

var (
	ErrTableExists    = errors.New("catalog: table already exists")
	ErrTableNotFound  = errors.New("catalog: table not found")
	ErrReservedColumn = errors.New("catalog: column name is reserved")
	ErrIndexExists    = errors.New("catalog: index already exists")
	ErrIndexNotFound  = errors.New("catalog: index not found")
)

// ── schema 的内存表示 ────────────────────────────────────────────

// Column 是一列的定义。
type Column struct {
	Name       string
	Kind       types.Kind
	NotNull    bool
	PrimaryKey bool
}

// Table 是一张表的 schema。建表之后不可变（本版本没有 ALTER TABLE），
// 因此并发读不需要每列加锁，整表当只读数据用。
type Table struct {
	Name     string
	ID       uint64
	Columns  []Column
	HasRowid bool // 无主键表自动补的隐藏 rowid 主键
}

// PrimaryKeyIndex 返回主键列下标。建表路径保证主键必然存在
// （无主键表补 rowid），返回 -1 只可能是 schema 损坏，调用方当错误处理。
func (t *Table) PrimaryKeyIndex() int {
	for i, col := range t.Columns {
		if col.PrimaryKey {
			return i
		}
	}
	return -1
}

// IndexDef 是一个二级索引的定义（M6）。单列索引；Name 全局唯一
// （SQLite 风格，meta 区按名字平铺）。
type IndexDef struct {
	Name   string
	Table  string
	Column string
	Unique bool
}

// indexJSON 是 IndexDef 的持久化表示（取舍表 #12：可读性优先）。
type indexJSON struct {
	Name   string `json:"name"`
	Table  string `json:"table"`
	Column string `json:"column"`
	Unique bool   `json:"unique,omitempty"`
}

func (d *IndexDef) marshal() ([]byte, error) { return json.Marshal(indexJSON(*d)) }

func decodeIndex(b []byte) (*IndexDef, error) {
	var ij indexJSON
	if err := json.Unmarshal(b, &ij); err != nil {
		return nil, fmt.Errorf("catalog: corrupt index JSON: %w", err)
	}
	if ij.Name == "" || ij.Table == "" || ij.Column == "" {
		return nil, fmt.Errorf("catalog: corrupt index JSON: missing name/table/column")
	}
	d := IndexDef(ij)
	return &d, nil
}

// ColumnIndex 按列名查下标，不存在返回 -1。名字精确匹配 ——
// 大小写折叠留给解析器层统一决定。
func (t *Table) ColumnIndex(name string) int {
	for i, col := range t.Columns {
		if col.Name == name {
			return i
		}
	}
	return -1
}

// Kinds 返回按 schema 顺序的列类型序列，行解码（encoding.DecodeRow）必需。
func (t *Table) Kinds() []types.Kind {
	ks := make([]types.Kind, len(t.Columns))
	for i, col := range t.Columns {
		ks[i] = col.Kind
	}
	return ks
}

// ── schema 的持久化表示：JSON ────────────────────────────────────
//
// schema 变更频率极低、数量级小，可读性 > 紧凑性（DESIGN 取舍表 #12）。
// Kind 存成 "INT"/"TEXT" 这类字符串而不是裸 uint8，打开 meta 键能直接读懂。

type tableJSON struct {
	Name     string       `json:"name"`
	ID       uint64       `json:"id"`
	HasRowid bool         `json:"has_rowid,omitempty"`
	Columns  []columnJSON `json:"columns"`
}

type columnJSON struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	NotNull    bool   `json:"not_null,omitempty"`
	PrimaryKey bool   `json:"primary_key,omitempty"`
}

func (t *Table) marshal() ([]byte, error) {
	tj := tableJSON{Name: t.Name, ID: t.ID, HasRowid: t.HasRowid}
	for _, col := range t.Columns {
		tj.Columns = append(tj.Columns, columnJSON{
			Name:       col.Name,
			Kind:       col.Kind.String(),
			NotNull:    col.NotNull,
			PrimaryKey: col.PrimaryKey,
		})
	}
	return json.Marshal(tj)
}

func decodeTable(b []byte) (*Table, error) {
	var tj tableJSON
	if err := json.Unmarshal(b, &tj); err != nil {
		return nil, fmt.Errorf("catalog: corrupt schema JSON: %w", err)
	}
	t := &Table{Name: tj.Name, ID: tj.ID, HasRowid: tj.HasRowid}
	for _, cj := range tj.Columns {
		kind, err := types.ParseKind(cj.Kind)
		if err != nil {
			return nil, fmt.Errorf("catalog: table %s column %s: %w", tj.Name, cj.Name, err)
		}
		t.Columns = append(t.Columns, Column{
			Name:       cj.Name,
			Kind:       kind,
			NotNull:    cj.NotNull,
			PrimaryKey: cj.PrimaryKey,
		})
	}
	if t.Name == "" || t.PrimaryKeyIndex() < 0 {
		return nil, fmt.Errorf("catalog: corrupt schema for table %q: no name or no primary key", t.Name)
	}
	return t, nil
}

// ── Catalog 本体 ─────────────────────────────────────────────────

// Catalog 是 schema 的内存缓存 + KV 持久化。按"单写者、多读者"假设设计
// （DESIGN §1 范围外：不做并发 DDL）。
type Catalog struct {
	mu     sync.RWMutex
	kv     *kvdb.DB
	tables map[string]*Table
	index  map[string]*IndexDef
}

// Load 打开数据库时全量加载 meta 前缀。表与索引的数量级都小，两次前缀
// 扫描即可。索引加载在后并校验引用的表与列存在 —— meta 区内容只应由
// catalog 自己写出，出现悬空引用说明库被外部改坏，宁可打不开也别带病运行。
func Load(kv *kvdb.DB) (*Catalog, error) {
	c := &Catalog{kv: kv, tables: make(map[string]*Table), index: make(map[string]*IndexDef)}
	it := kv.NewIterator(&kvdb.IteratorOptions{Prefix: metaSchemaPrefix})
	defer it.Close()
	for it.SeekToFirst(); it.Valid(); it.Next() {
		t, err := decodeTable(it.Value())
		if err != nil {
			return nil, err
		}
		c.tables[t.Name] = t
	}
	if err := it.Error(); err != nil {
		return nil, fmt.Errorf("catalog: scan meta prefix: %w", err)
	}

	it2 := kv.NewIterator(&kvdb.IteratorOptions{Prefix: metaIndexPrefix})
	defer it2.Close()
	for it2.SeekToFirst(); it2.Valid(); it2.Next() {
		d, err := decodeIndex(it2.Value())
		if err != nil {
			return nil, err
		}
		if err := c.validateIndexDef(d); err != nil {
			return nil, err
		}
		c.index[d.Name] = d
	}
	if err := it2.Error(); err != nil {
		return nil, fmt.Errorf("catalog: scan index prefix: %w", err)
	}
	return c, nil
}

// validateIndexDef 校验索引定义引用的表与列存在、且列不是主键
// （主键本身就是有序入口，再建索引是纯浪费，直接拒绝）。
// Load（构建期无锁）与 CreateIndex（已持写锁）共用。
func (c *Catalog) validateIndexDef(d *IndexDef) error {
	t, ok := c.tables[d.Table]
	if !ok {
		return fmt.Errorf("catalog: index %s references missing table %s", d.Name, d.Table)
	}
	ci := t.ColumnIndex(d.Column)
	if ci < 0 {
		return fmt.Errorf("catalog: index %s references missing column %s.%s", d.Name, d.Table, d.Column)
	}
	if t.Columns[ci].PrimaryKey {
		return fmt.Errorf("catalog: column %s.%s is the primary key and is already indexed", d.Table, d.Column)
	}
	return nil
}

// GetTable 按名字查表（读走内存）。
func (c *Catalog) GetTable(name string) (*Table, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tables[name]
	return t, ok
}

// Tables 返回全部表，按表名排序 —— 给将来的 SHOW TABLES 和遍历用。
func (c *Catalog) Tables() []*Table {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*Table, 0, len(c.tables))
	for _, t := range c.tables {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *Table) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// GetIndex 按名字查索引定义（读走内存）。
func (c *Catalog) GetIndex(name string) (*IndexDef, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.index[name]
	return d, ok
}

// IndexesOf 返回一张表的全部索引，按索引名排序（确定性给计划与测试）。
func (c *Catalog) IndexesOf(t *Table) []*IndexDef {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []*IndexDef
	for _, d := range c.index {
		if d.Table == t.Name {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b *IndexDef) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// CreateIndex 写入一个索引定义。只管 schema——条目回填是数据操作，
// 归执行层（DESIGN §7.5：条目分批先写、meta 最后写），因此调用方在
// 回填完成之后才应该调用这里。
func (c *Catalog) CreateIndex(d IndexDef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.index[d.Name]; ok {
		return fmt.Errorf("%w: %s", ErrIndexExists, d.Name)
	}
	if err := c.validateIndexDef(&d); err != nil {
		return err
	}
	b, err := (&IndexDef{Name: d.Name, Table: d.Table, Column: d.Column, Unique: d.Unique}).marshal()
	if err != nil {
		return err
	}
	batch := kvdb.NewWriteBatch()
	if err := batch.Put(indexKey(d.Name), b); err != nil {
		return err
	}
	if err := c.kv.Write(batch); err != nil {
		return fmt.Errorf("catalog: write index %s: %w", d.Name, err)
	}
	def := d
	c.index[d.Name] = &def
	return nil
}

// DropIndex 删除索引定义。条目清理（i 区范围删除）同样归执行层——
// 它需要先读出定义才知道删哪个区间，见 exec.ExecDropIndex。
func (c *Catalog) DropIndex(name string) (*IndexDef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.index[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrIndexNotFound, name)
	}
	batch := kvdb.NewWriteBatch()
	if err := batch.Delete(indexKey(name)); err != nil {
		return nil, err
	}
	if err := c.kv.Write(batch); err != nil {
		return nil, fmt.Errorf("catalog: drop index %s: %w", name, err)
	}
	delete(c.index, name)
	return d, nil
}

// buildTable 校验建表参数并补齐派生字段（主键列隐式 NOT NULL、无主键表补
// 隐藏 rowid）。独立于 KV 操作，方便单测只测校验逻辑。
func buildTable(name string, cols []Column) (*Table, error) {
	if name == "" {
		return nil, errors.New("catalog: empty table name")
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("catalog: table %s must have at least one column", name)
	}
	cols = append([]Column(nil), cols...) // 不动调用方的切片

	seen := make(map[string]bool, len(cols))
	pk := -1
	for i := range cols {
		col := &cols[i]
		if col.Name == "" {
			return nil, fmt.Errorf("catalog: table %s has a nameless column", name)
		}
		// 隐藏主键叫 rowid，用户自己建同名列会撞车，直接拒绝（DESIGN §5）。
		// 保留名检查用大小写折叠（防御性），列查找仍精确匹配。
		if strings.EqualFold(col.Name, "rowid") {
			return nil, fmt.Errorf("%w: rowid is the implicit primary key name", ErrReservedColumn)
		}
		if seen[col.Name] {
			return nil, fmt.Errorf("catalog: duplicate column %s", col.Name)
		}
		seen[col.Name] = true
		if col.PrimaryKey {
			if pk >= 0 {
				return nil, fmt.Errorf("catalog: table %s declares more than one primary key", name)
			}
			pk = i
			col.NotNull = true // 主键列隐式 NOT NULL（DESIGN §4.4）
		}
	}
	hasRowid := false
	if pk < 0 {
		cols = append(cols, Column{Name: "rowid", Kind: types.Int, NotNull: true, PrimaryKey: true})
		hasRowid = true
	}
	return &Table{Name: name, Columns: cols, HasRowid: hasRowid}, nil
}

// CreateTable 建 一张新表：校验 → 分配表 ID → schema + 两个计数器同批落 KV →
// 进内存。nextid 推进、schema 写入、rowseq 清零在同一个 WriteBatch 里，
// 崩溃时要么全有要么全无 —— 这就是"catalog 放在 KV 里"买到的原子性。
func (c *Catalog) CreateTable(name string, cols []Column) (*Table, error) {
	t, err := buildTable(name, cols)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tables[name]; ok {
		return nil, fmt.Errorf("%w: %s", ErrTableExists, name)
	}

	// 分配表 ID。nextid 不存在 = 库里还没有任何表，从 0 开始。
	nextID := uint64(0)
	if v, err := c.kv.Get(metaNextIDKey); err == nil {
		if len(v) != 8 {
			return nil, fmt.Errorf("catalog: corrupt nextid counter (%d bytes)", len(v))
		}
		nextID = binary.BigEndian.Uint64(v)
	} else if !errors.Is(err, kvdb.ErrNotFound) {
		return nil, fmt.Errorf("catalog: read nextid: %w", err)
	}
	t.ID = nextID

	batch := kvdb.NewWriteBatch()
	var nb [8]byte
	binary.BigEndian.PutUint64(nb[:], nextID+1)
	if err := batch.Put(metaNextIDKey, nb[:]); err != nil {
		return nil, err
	}
	sb, err := t.marshal()
	if err != nil {
		return nil, err
	}
	if err := batch.Put(schemaKey(name), sb); err != nil {
		return nil, err
	}
	if err := batch.Put(rowSeqKey(nextID), make([]byte, 8)); err != nil { // 计数器从 0 起步
		return nil, err
	}
	if err := c.kv.Write(batch); err != nil {
		return nil, fmt.Errorf("catalog: write schema for %s: %w", name, err)
	}

	c.tables[name] = t
	return t, nil
}

// NextRowIDs 为一次 INSERT 预留 n 个连续 rowid：返回起始 ID，以及需要编进
// 同一个 WriteBatch 的计数器更新（key → value，调用方 batch.Put 即可）。
//
// 计数器更新跟着行数据同批落盘（DESIGN §8 第 6 步）：崩溃时计数器要么和
// 行一起推进，要么一起没动，不会出现"行写入了、计数器回退、下次 INSERT
// 分配出重复 rowid"。非 rowid 表或 n<=0 直接返回零值，调用方无需特判。
//
// 读取计数器与提交之间没有隔离 —— 与 INSERT 的主键查重是同一种"无事务"
// 代价（DESIGN §8），单写者假设下不会发生，如实记录不改。
func (c *Catalog) NextRowIDs(t *Table, n int) (first uint64, key, value []byte, err error) {
	if n <= 0 || !t.HasRowid {
		return 0, nil, nil, nil
	}
	k := rowSeqKey(t.ID)
	v, err := c.kv.Get(k)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("catalog: read rowseq for %s: %w", t.Name, err)
	}
	if len(v) != 8 {
		return 0, nil, nil, fmt.Errorf("catalog: corrupt rowseq counter for %s (%d bytes)", t.Name, len(v))
	}
	first = binary.BigEndian.Uint64(v)
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, first+uint64(n))
	return first, k, out, nil
}

// DropTable 删表：先删每张索引的全部条目（i 区前缀扫描、分批 Delete），
// 再按前缀扫出全部行键分批删（每批不超过 batchDeleteLimit 行），最后一批
// 单独删 schema 键、索引 schema 键和 rowseq 计数器（M6，DESIGN §5）。
//
// 顺序刻意是"索引条目 → 行 → meta"：无事务，中途崩溃的中间态只出现在
// "条目删了一半、行还在"的方向 —— 查询走索引最多多读到几行即将消失的行
// （无隔离本就无害）；反过来"行没了条目还在"会让回表撞 ErrNotFound。
// 崩溃后的最坏结果与 M3 相同（表还在，少了一截数据，再 DROP 一次即可）。
//
// 顺序保证不会出现"schema 没了但行还在"的幽灵数据。
func (c *Catalog) DropTable(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tables[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrTableNotFound, name)
	}
	var indexes []*IndexDef
	for _, d := range c.index {
		if d.Table == name {
			indexes = append(indexes, d)
		}
	}

	// 1. 索引条目分批删（每张索引自己的区域前缀；迭代器的 Key 在 Next
	//    后失效，deleteByPrefix 内部已复制）。
	for _, d := range indexes {
		if err := c.deleteByPrefix(encoding.IndexRegionPrefix(t.ID, d.Name)); err != nil {
			return fmt.Errorf("catalog: delete entries of index %s: %w", d.Name, err)
		}
	}

	// 2. 行数据分批删。
	if err := c.deleteByPrefix(encoding.RowKeyPrefix(t.ID)); err != nil {
		return fmt.Errorf("catalog: delete rows of %s: %w", name, err)
	}

	// 3. 单独一批删 schema + 索引 schema + 计数器。零行的表没有第 2 步，
	//    也必须走到这里。
	batch := kvdb.NewWriteBatch()
	if err := batch.Delete(schemaKey(name)); err != nil {
		return err
	}
	if err := batch.Delete(rowSeqKey(t.ID)); err != nil {
		return err
	}
	for _, d := range indexes {
		if err := batch.Delete(indexKey(d.Name)); err != nil {
			return err
		}
	}
	if err := c.kv.Write(batch); err != nil {
		return fmt.Errorf("catalog: delete schema of %s: %w", name, err)
	}

	delete(c.tables, name)
	for _, d := range indexes {
		delete(c.index, d.Name)
	}
	return nil
}

// deleteByPrefix 按前缀扫出全部键并分批 Delete（每批 ≤ batchDeleteLimit 条）。
// 索引条目与行数据共用 —— DROP TABLE 的两段清理是同一件事。
func (c *Catalog) deleteByPrefix(prefix []byte) error {
	it := c.kv.NewIterator(&kvdb.IteratorOptions{Prefix: prefix})
	var keys [][]byte
	for it.SeekToFirst(); it.Valid(); it.Next() {
		keys = append(keys, append([]byte(nil), it.Key()...))
	}
	err := it.Error()
	it.Close()
	if err != nil {
		return err
	}
	for start := 0; start < len(keys); start += batchDeleteLimit {
		end := min(start+batchDeleteLimit, len(keys))
		batch := kvdb.NewWriteBatch()
		for _, k := range keys[start:end] {
			if err := batch.Delete(k); err != nil {
				return err
			}
		}
		if err := c.kv.Write(batch); err != nil {
			return err
		}
	}
	return nil
}
