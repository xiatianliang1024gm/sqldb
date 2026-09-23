package exec

// 写路径（DESIGN §8）：INSERT / UPDATE / DELETE。
//
// 三条路径共享同一个骨架：定位行（INSERT 按主键点查，UPDATE/DELETE 按
// WHERE 扫描并享受主键范围下推）→ 校验约束 → 编进一个 WriteBatch →
// 一次 Write 提交。语句级原子性完全靠 kvdb 的 batch —— 这是对存储原语
// 承诺过的依赖面（DESIGN §1），多一个都不用。
//
// 如实记录的无事务代价（DESIGN §8"读检查非原子"）：INSERT 的主键查重、
// rowid 计数器读取与最终提交之间没有隔离，并发写理论上可产生重复主键。
// 单机嵌入式、单写者假设下不会发生，不为此加锁。

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

// ── INSERT ───────────────────────────────────────────────────────

// ExecInsert 执行一条 INSERT，返回受影响行数。多行编进一个 WriteBatch
// 原子提交；任何一行的约束检查失败都发生在提交之前，整条语句不落一行。
// 参数是 kvdb.Store（点查 + 扫描 + batch 写）—— 恰好是 §1 承诺的原语集合。
func ExecInsert(kv kvdb.Store, cat *catalog.Catalog, stmt *parser.InsertStmt) (int64, error) {
	t, ok := cat.GetTable(stmt.Table)
	if !ok {
		return 0, fmt.Errorf("no such table: %s", stmt.Table)
	}

	// 1. 定位列（DESIGN §8 第 1 步）：把 VALUES 的第 j 个表达式映射到
	//    schema 列偏移。没写列清单 = 按用户可见列顺序给全列（隐藏 rowid
	//    不在"用户可见列"里，它永远来自计数器）。
	targets, err := insertTargets(t, stmt.Columns)
	if err != nil {
		return 0, err
	}

	// 2. 绑定 + 常量检查。VALUES 里引用列在 SQL 里没有意义 —— 那一行
	//    还不存在，无处取值，直接拒绝而不是等 eval 拿到空行再炸。
	//    聚合函数也在这里被拒：VALUES 没有聚合的位置。
	b := newBinder(t, stmt.Table)
	bound := make([][]boundExpr, len(stmt.Rows))
	for i, row := range stmt.Rows {
		bound[i] = make([]boundExpr, len(row))
		for j, e := range row {
			if err := checkConstantExpr(e); err != nil {
				return 0, fmt.Errorf("VALUES row %d: %w", i+1, err)
			}
			be, err := b.bind(e)
			if err != nil {
				return 0, fmt.Errorf("VALUES row %d: %w", i+1, err)
			}
			bound[i][j] = be
		}
	}

	// 3. rowid 表先预留 ID；计数器更新与行数据同批落盘（DESIGN §8 第 6 步）。
	firstID, seqKey, seqVal, err := cat.NextRowIDs(t, len(stmt.Rows))
	if err != nil {
		return 0, err
	}

	// 4. 逐行求值、约束检查、查重，全部通过后编 batch。任何失败都在
	//    Write 之前返回 —— 整条语句一行不落（语句级原子）。
	pkIdx := t.PrimaryKeyIndex()
	batch := kvdb.NewWriteBatch()
	scratch := newWriteScratch() // 同批多行的查重洞（含主键 + 唯一索引）
	for i := range stmt.Rows {
		row := make([]types.Value, len(t.Columns))
		for ci := range row {
			row[ci] = types.NullValue() // 未指定的列默认 NULL（NOT NULL 检查兜底）
		}
		if t.HasRowid {
			row[pkIdx] = types.IntValue(int64(firstID) + int64(i))
		}
		for j, be := range bound[i] {
			v, err := be.eval(nil)
			if err != nil {
				return 0, fmt.Errorf("VALUES row %d: %w", i+1, err)
			}
			col := &t.Columns[targets[j]]
			v, err = coerceValue(v, col, t.Name)
			if err != nil {
				return 0, fmt.Errorf("VALUES row %d: %w", i+1, err)
			}
			row[targets[j]] = v
		}
		// NOT NULL 对整行生效 —— 省略的列默认 NULL，同样不能混过检查。
		for ci := range row {
			if t.Columns[ci].NotNull && row[ci].IsNull() {
				return 0, fmt.Errorf("VALUES row %d: column %s of %s is NOT NULL", i+1, t.Columns[ci].Name, t.Name)
			}
		}

		enc, err := encoding.EncodeKey(row[pkIdx])
		if err != nil {
			return 0, fmt.Errorf("VALUES row %d: %w", i+1, err)
		}
		key := encoding.RowKey(t.ID, enc)
		// 查重两道关：本语句先前编进 batch 的行（Get 看不见），以及已提交
		// 的数据。第二关与提交之间无隔离（DESIGN §8"读检查非原子"）。
		if scratch.rows[string(key)] {
			return 0, fmt.Errorf("VALUES row %d: duplicate primary key %v in %s", i+1, row[pkIdx], t.Name)
		}
		if _, err := kv.Get(key); err == nil {
			return 0, fmt.Errorf("VALUES row %d: duplicate primary key %v in %s", i+1, row[pkIdx], t.Name)
		} else if !errors.Is(err, kvdb.ErrNotFound) {
			return 0, fmt.Errorf("insert into %s: %w", t.Name, err)
		}
		scratch.rows[string(key)] = true
		// 唯一索引查重（M6，DESIGN §8）：NULL 豁免在 checkUnique 里。
		for _, d := range cat.IndexesOf(t) {
			if !d.Unique {
				continue
			}
			v := row[t.ColumnIndex(d.Column)]
			if err := checkUnique(kv, t, d, v, enc, scratch); err != nil {
				return 0, fmt.Errorf("VALUES row %d: %w", i+1, err)
			}
		}
		if err := batch.Put(key, encoding.EncodeRow(row)); err != nil {
			return 0, err
		}
		// 索引条目与行编进同一个 batch —— 同生同死（DESIGN §8 索引维护）。
		if err := putIndexEntries(batch, cat, t, row); err != nil {
			return 0, err
		}
	}
	if seqKey != nil {
		if err := batch.Put(seqKey, seqVal); err != nil {
			return 0, err
		}
	}

	// 5. 一个 batch，一次 Write（DESIGN §8 第 5 步）。行数超过 kvdb 的
	//    MaxBatchBytes 会在这里报错 —— 教学项目的诚实上限，不藏着掖着。
	if err := kv.Write(batch); err != nil {
		return 0, fmt.Errorf("insert into %s: %w", t.Name, err)
	}
	return int64(len(stmt.Rows)), nil
}

// insertTargets 解析 INSERT 的目标列：返回每个 VALUES 位置对应的 schema
// 列偏移。列清单省略 = 全部用户可见列；列清单里出现隐藏 rowid 直接拒绝
// —— 它是系统分配的，用户显式给值会和计数器打架。
func insertTargets(t *catalog.Table, cols []string) ([]int, error) {
	if cols == nil {
		n := len(t.Columns)
		if t.HasRowid {
			n--
		}
		targets := make([]int, n)
		for i := range targets {
			targets[i] = i
		}
		return targets, nil
	}
	seen := make(map[string]bool, len(cols))
	targets := make([]int, len(cols))
	for i, name := range cols {
		if t.HasRowid && name == "rowid" {
			return nil, fmt.Errorf("column rowid is implicit and cannot be inserted into")
		}
		idx := t.ColumnIndex(name)
		if idx < 0 {
			return nil, fmt.Errorf("no such column: %s", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate column %s in INSERT", name)
		}
		seen[name] = true
		targets[i] = idx
	}
	return targets, nil
}

// checkConstantExpr 拒绝 VALUES 里的列引用和 *。聚合函数由 bind 拒绝
// （VALUES 里没有聚合的位置）。INSERT VALUES 只允许常量表达式
// （字面量 + 运算的组合）。
func checkConstantExpr(e parser.Expr) error {
	switch x := e.(type) {
	case *parser.Literal, *parser.UnaryExpr, *parser.BinaryExpr,
		*parser.IsNullExpr, *parser.InExpr, *parser.BetweenExpr, *parser.LikeExpr:
		return nil
	case *parser.ColumnRef:
		return fmt.Errorf("cannot reference column %s in VALUES (the row does not exist yet)", x.Name)
	case *parser.StarExpr:
		return errors.New("unexpected * in VALUES")
	}
	return nil
}

// coerceValue 把待写入的值对齐到列类型。规则从宽但方向固定（DESIGN 取舍
// 表 #11"类型检查执行期从宽"的落地方向）：
//   - INT 字面量进 FLOAT 列：允许，升格为浮点（数值语义，无损）；
//   - FLOAT 进 INT 列：拒绝（截断有损，SQL 的隐式降格是坑，不做）；
//   - 其余跨族一律拒绝。
func coerceValue(v types.Value, col *catalog.Column, table string) (types.Value, error) {
	if v.IsNull() {
		return v, nil // NOT NULL 由调用方检查（错误信息里带上语句上下文）
	}
	switch {
	case v.Kind == col.Kind:
		return v, nil
	case col.Kind == types.Float && v.Kind == types.Int:
		return types.FloatValue(float64(v.I)), nil
	}
	return types.Value{}, fmt.Errorf("column %s of %s is %s, got %s value %v",
		col.Name, table, col.Kind, v.Kind, v)
}

// ── UPDATE / DELETE 共用的写扫描 ─────────────────────────────────

// rowRef 是写路径眼里的"一行"：定位它的键 + 解码后的全列值。
// UPDATE 原地改值、DELETE 删键，都以 key 为操作对象。
type rowRef struct {
	key []byte
	row []types.Value
}

// scanTableRows 物化一张表（按 WHERE 下推后的范围 + 残留谓词过滤）的全部
// 命中行。UPDATE/DELETE 需要行键，而 Volcano 的 Next 只回值 —— 与其在
// Operator 接口上凿一个"顺手把键也给我"的口子，不如给写路径一个诚实的
// 物化扫描：结果本来就要整个编进 batch，拉平成切片没有额外代价。
// 返回实际读过的行数，让写路径的下推效果和 M2 的查询一样可观测。
func scanTableRows(kv kvdb.Reader, t *catalog.Table, lower, upper []byte, residual []boundExpr) ([]rowRef, int64, error) {
	prefix := encoding.RowKeyPrefix(t.ID)
	opt := &kvdb.IteratorOptions{}
	if lower != nil || upper != nil {
		// 与 ScanNode 相同的封口规则：共享键空间里，区间必须关在自己
		// 区域内，否则无上界扫描会越进 meta 区（M2 的教训，DESIGN §12）。
		opt.LowerBound, opt.UpperBound = lower, upper
		if opt.UpperBound == nil {
			opt.UpperBound = prefixSuccessor(prefix)
		}
		opt.Prefix = nil
	} else {
		opt.Prefix = prefix
	}

	it := kv.NewIterator(opt)
	defer it.Close()
	if lower != nil {
		it.Seek(lower)
	} else {
		it.SeekToFirst()
	}

	var pred boundExpr
	for _, be := range residual {
		if pred == nil {
			pred = be
			continue
		}
		pred = &binaryExpr{op: parser.OpAnd, l: pred, r: be}
	}

	var out []rowRef
	var scanned int64
	for ; it.Valid(); it.Next() {
		key := append([]byte(nil), it.Key()...) // Next 后失效，必须复制
		row, err := encoding.DecodeRow(it.Value(), t.Kinds())
		if err != nil {
			return nil, 0, fmt.Errorf("scan %s: corrupt row at key % x: %w", t.Name, key, err)
		}
		scanned++
		if pred != nil {
			v, err := pred.eval(row)
			if err != nil {
				return nil, 0, fmt.Errorf("evaluating WHERE: %w", err)
			}
			if v.IsNull() {
				continue // 与 FilterNode 一致：NULL 视为不通过
			}
			if v.Kind != types.Bool {
				return nil, 0, fmt.Errorf("WHERE must evaluate to boolean, got %s", v.Kind)
			}
			if !v.B {
				continue
			}
		}
		out = append(out, rowRef{key: key, row: row})
	}
	if err := it.Error(); err != nil {
		return nil, 0, fmt.Errorf("scan %s: %w", t.Name, err)
	}
	return out, scanned, nil
}

// bindWhere 为 UPDATE / DELETE 绑定 WHERE。这两个语句的表名没有别名语法，
// scope 就是表名本身。返回该表（作用域条目 0）的下推区间与残留谓词。
// cat 传 nil：写路径不做索引下推，只维护索引（取舍表 #16）—— 扫描仍走
// 主键快路，索引条目由 ExecUpdate/ExecDelete 负责跟着行改。
func bindWhere(t *catalog.Table, tableName string, where parser.Expr) (lower, upper []byte, residual []boundExpr, err error) {
	b := newBinder(t, tableName)
	bounds, res, _, err := pushdownWhere(b, nil, where)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, e := range res {
		be, err := b.bind(e)
		if err != nil {
			return nil, nil, nil, err
		}
		residual = append(residual, be)
	}
	return bounds[0].lower, bounds[0].upper, residual, nil
}

// ── UPDATE ───────────────────────────────────────────────────────

// ExecUpdate 执行 UPDATE：下推主键范围 → 扫描命中行 → 逐行求值 SET →
// 编进一个 WriteBatch（键不变，Put 新值）→ 返回受影响行数。
// 主键列不允许出现在 SET 里（DESIGN 取舍表 #10）：改主键 = 删旧行 +
// 插新行，把它留给用户显式的 DELETE + INSERT，语义更诚实。
func ExecUpdate(kv kvdb.Store, cat *catalog.Catalog, stmt *parser.UpdateStmt) (int64, error) {
	t, ok := cat.GetTable(stmt.Table)
	if !ok {
		return 0, fmt.Errorf("no such table: %s", stmt.Table)
	}
	pkIdx := t.PrimaryKeyIndex()

	b := newBinder(t, stmt.Table)
	type setItem struct {
		idx  int
		name string
		expr boundExpr
	}
	sets := make([]setItem, len(stmt.Set))
	for i, sc := range stmt.Set {
		idx, err := b.resolve("", sc.Column)
		if err != nil {
			return 0, err
		}
		if idx == pkIdx {
			return 0, fmt.Errorf("cannot update primary key column %s (DELETE + INSERT instead)", sc.Column)
		}
		be, err := b.bind(sc.Value)
		if err != nil {
			return 0, fmt.Errorf("SET %s: %w", sc.Column, err)
		}
		sets[i] = setItem{idx: idx, name: sc.Column, expr: be}
	}

	lower, upper, residual, err := bindWhere(t, stmt.Table, stmt.Where)
	if err != nil {
		return 0, err
	}
	hits, _, err := scanTableRows(kv, t, lower, upper, residual)
	if err != nil {
		return 0, err
	}

	// 只改到某索引列才动那张索引（DESIGN §8 索引维护）：先收集被 SET 的列。
	setCols := make(map[int]bool, len(stmt.Set))
	for _, s := range sets {
		setCols[s.idx] = true
	}
	scratch := newWriteScratch()
	batch := kvdb.NewWriteBatch()
	for _, h := range hits {
		pkEnc, err := encoding.EncodeKey(h.row[pkIdx]) // 主键不可改，新旧行共用
		if err != nil {
			return 0, err
		}
		newRow := append([]types.Value(nil), h.row...)
		for _, s := range sets {
			v, err := s.expr.eval(h.row) // SET 的求值环境是更新前的行
			if err != nil {
				return 0, fmt.Errorf("SET %s: %w", s.name, err)
			}
			col := &t.Columns[s.idx]
			v, err = coerceValue(v, col, t.Name)
			if err != nil {
				return 0, fmt.Errorf("UPDATE %s: %w", t.Name, err)
			}
			if col.NotNull && v.IsNull() {
				return 0, fmt.Errorf("UPDATE %s: column %s is NOT NULL", t.Name, col.Name)
			}
			newRow[s.idx] = v
		}
		// 索引维护（M6）：删旧条目、唯一查重、写新条目，全部编进同一个
		// batch —— 行与条目同生同死。唯一查重的冲突必须发生在 Write 之前，
		// 任何一行冲突整条语句一行不落（与 INSERT 同一纪律）。
		for _, d := range cat.IndexesOf(t) {
			ci := t.ColumnIndex(d.Column)
			if !setCols[ci] {
				continue // 索引列没被 SET，条目原样有效
			}
			oldKey, err := indexEntryKey(t, d, h.row)
			if err != nil {
				return 0, err
			}
			newKey, err := indexEntryKey(t, d, newRow)
			if err != nil {
				return 0, err
			}
			if bytes.Equal(oldKey, newKey) {
				continue // 值没变（或变化不改变编码），条目原样有效
			}
			if d.Unique {
				if err := checkUnique(kv, t, d, newRow[ci], pkEnc, scratch); err != nil {
					return 0, fmt.Errorf("UPDATE %s: %w", t.Name, err)
				}
			}
			if err := batch.Delete(oldKey); err != nil {
				return 0, err
			}
			if err := batch.Put(newKey, nil); err != nil {
				return 0, err
			}
		}
		if err := batch.Put(h.key, encoding.EncodeRow(newRow)); err != nil {
			return 0, err
		}
	}
	if err := kv.Write(batch); err != nil {
		return 0, fmt.Errorf("update %s: %w", t.Name, err)
	}
	return int64(len(hits)), nil
}

// ── DELETE ───────────────────────────────────────────────────────

// ExecDelete 执行 DELETE：同一套扫描（享受下推）→ 命中行进 batch（Delete）→
// 返回行数。不带 WHERE = 全表删，同样走扫描路径 —— 逐行判断谓词的能力
// 与全删共用一条路，键区间直删的优化留给将来（DESIGN §8）。
func ExecDelete(kv kvdb.Store, cat *catalog.Catalog, stmt *parser.DeleteStmt) (int64, error) {
	t, ok := cat.GetTable(stmt.Table)
	if !ok {
		return 0, fmt.Errorf("no such table: %s", stmt.Table)
	}
	lower, upper, residual, err := bindWhere(t, stmt.Table, stmt.Where)
	if err != nil {
		return 0, err
	}
	hits, _, err := scanTableRows(kv, t, lower, upper, residual)
	if err != nil {
		return 0, err
	}

	batch := kvdb.NewWriteBatch()
	for _, h := range hits {
		if err := batch.Delete(h.key); err != nil {
			return 0, err
		}
		// 索引条目跟着行一起删，同一个 batch（DESIGN §8 索引维护）。
		if err := deleteIndexEntries(batch, cat, t, h.row); err != nil {
			return 0, err
		}
	}
	if err := kv.Write(batch); err != nil {
		return 0, fmt.Errorf("delete from %s: %w", t.Name, err)
	}
	return int64(len(hits)), nil
}
