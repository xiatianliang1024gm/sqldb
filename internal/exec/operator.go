package exec

// Volcano 拉取模型（DESIGN §7）：每个节点实现 Next()，父节点向子节点逐行拉取，
// 返回 (nil, nil) 表示数据耗尽。它不是最快的模型，但是数据流最清晰的模型 ——
// 这正是学习项目选它的理由（DESIGN 取舍表 #6）。
//
// 约定：
//   - Next 返回的行切片由节点新分配，调用方可以随意持有；
//   - Close 必须调用（Scan 持有 kvdb 迭代器的版本引用，不 Close 会拦住 Compaction）；
//   - 节点不并发安全：一条计划只被一个 goroutine 顺序拉取。

import (
	"fmt"
	"slices"

	"github.com/xiatianliang1024gm/kvdb"
	"github.com/xiatianliang1024gm/sqldb/internal/catalog"
	"github.com/xiatianliang1024gm/sqldb/internal/encoding"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// Operator 是所有执行节点的公共接口。
type Operator interface {
	// Next 返回下一行；(nil, nil) 表示扫描结束。出错时返回 (nil, err)。
	Next() ([]types.Value, error)
	// Close 释放节点持有的资源（主要是 kvdb 迭代器）。重复调用安全。
	Close() error
}

// ── Scan ─────────────────────────────────────────────────────────

// ScanNode 表扫描：kvdb 迭代器按行键序读 → 解码 → []types.Value。
//
// 一张表的所有行键共享前缀 d\x00<tableID>r，且键中主键编码保序，
// 所以前缀扫描天然就是"按主键值序的全表扫"。若上层（select.go）从 WHERE
// 里下推出了主键范围，lower/upper 会替代 prefix 成为扫描区间 —— 那是
// 本设计唯一做的优化（DESIGN §7.3）。
type ScanNode struct {
	kv    kvdb.Reader
	table *catalog.Table

	prefix []byte // 无下推时的全表前缀
	lower  []byte // 下推后的扫描下界（含），nil 表示无
	upper  []byte // 下推后的扫描上界（不含），nil 表示无

	kinds []types.Kind

	it          kvdb.Iterator
	rowsScanned int64 // 可观测的"扫描行数"：下推前后的差异靠它验证
	done        bool
}

// Scanned 返回迭代器实际读过的行数 —— 不是结果行数。
// WHERE 把行滤掉不算本事，少读才算（DESIGN §7.3 的可观测性要求）。
func (s *ScanNode) Scanned() int64 { return s.rowsScanned }

func (s *ScanNode) Next() ([]types.Value, error) {
	if s.done {
		return nil, nil
	}
	if s.it == nil {
		opt := &kvdb.IteratorOptions{}
		if s.lower != nil || s.upper != nil {
			// kvdb 约定 Prefix 与 Lower/UpperBound 互斥：有下推就用区间。
			// 但下界可能没有配套上界（如 WHERE id > 90），而数据区前缀
			// d\x00... 按字节序排在 meta 区 m\x00... 之前 —— 不封口的话，
			// 读完本表的行会一路读进 meta 区，把 schema 的 JSON 当行解码。
			// 所以扫描必须永远关在数据区里：上界为空时用表前缀的后继。
			opt.LowerBound, opt.UpperBound = s.lower, s.upper
			if opt.UpperBound == nil {
				opt.UpperBound = prefixSuccessor(s.prefix)
			}
			opt.Prefix = nil
		} else {
			opt.Prefix = s.prefix // kvdb 自己会把 Prefix 折算成 [P, P后继)
		}
		s.it = s.kv.NewIterator(opt)
		if err := s.it.Error(); err != nil {
			return nil, fmt.Errorf("scan %s: %w", s.table.Name, err)
		}
		if s.lower != nil {
			s.it.Seek(s.lower)
		} else {
			s.it.SeekToFirst()
		}
	}
	if !s.it.Valid() {
		// 正常耗尽（区间为空也算），但迭代器自身报错必须往上送。
		if err := s.it.Error(); err != nil {
			return nil, fmt.Errorf("scan %s: %w", s.table.Name, err)
		}
		s.done = true
		return nil, s.Close()
	}
	// Key/Value 只在下一次 Next 前有效（kvdb 契约），DecodeRow 内部会复制
	// 需要保留的部分，这里无需额外拷贝。
	key, val := s.it.Key(), s.it.Value()
	s.it.Next()

	row, err := encoding.DecodeRow(val, s.kinds)
	if err != nil {
		return nil, fmt.Errorf("scan %s: corrupt row at key % x: %w", s.table.Name, key, err)
	}
	s.rowsScanned++
	return row, nil
}

func (s *ScanNode) Close() error {
	if s.it != nil {
		err := s.it.Close()
		s.it = nil
		return err
	}
	return nil
}

// prefixSuccessor 返回前缀的后继：最后一个非 0xFF 字节 +1、其后截断。
// 与 kvdb 内部给 Prefix 算上界是同一套算法 —— 这里自己算一份，是因为
// 下推扫描必须用 Lower/UpperBound（与 Prefix 互斥），而上界又不能缺。
func prefixSuccessor(p []byte) []byte {
	out := append([]byte(nil), p...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xFF {
			out[i]++
			return out[:i+1]
		}
	}
	return nil // 全 0xFF：不存在后继（本设计的数据区前缀到不了这里）
}

// ── Filter ───────────────────────────────────────────────────────

// FilterNode 谓词过滤。三值逻辑在此收口：求值结果为 NULL 的行一律不通过
// （DESIGN §4.4），非 Bool 的结果说明谓词本身写错了（如 WHERE name），报错。
// label 只用于报错上下文（"WHERE" / "HAVING"），不参与逻辑。
type FilterNode struct {
	child Operator
	pred  boundExpr
	label string
}

func (f *FilterNode) what() string {
	if f.label == "" {
		return "WHERE"
	}
	return f.label
}

func (f *FilterNode) Next() ([]types.Value, error) {
	for {
		row, err := f.child.Next()
		if err != nil || row == nil {
			return row, err
		}
		v, err := f.pred.eval(row)
		if err != nil {
			return nil, fmt.Errorf("evaluating %s: %w", f.what(), err)
		}
		if v.IsNull() {
			continue // NULL 视为不通过
		}
		if v.Kind != types.Bool {
			return nil, fmt.Errorf("%s must evaluate to boolean, got %s", f.what(), v.Kind)
		}
		if v.B {
			return row, nil
		}
	}
}

func (f *FilterNode) Close() error { return f.child.Close() }

// ── Project ──────────────────────────────────────────────────────

// ProjectNode 表达式求值 + 列重排（SELECT 列表）。列名在 select.go 的
// 计划构建期确定（别名 → 列名 → 表达式描述），这里只管逐行算值。
type ProjectNode struct {
	child Operator
	exprs []boundExpr
	Names []string // 输出列名，与 exprs 一一对应
}

func (p *ProjectNode) Next() ([]types.Value, error) {
	row, err := p.child.Next()
	if err != nil || row == nil {
		return row, err
	}
	out := make([]types.Value, len(p.exprs))
	for i, e := range p.exprs {
		v, err := e.eval(row)
		if err != nil {
			return nil, fmt.Errorf("evaluating select item %q: %w", p.Names[i], err)
		}
		out[i] = v
	}
	return out, nil
}

func (p *ProjectNode) Close() error { return p.child.Close() }

// ── Sort ─────────────────────────────────────────────────────────

// SortNode 内存物化 + 多键排序（DESIGN §7.4：单机学习项目不做溢写）。
// NULL 排序约定（DESIGN §4.4）：NULL 视为最小值 —— ASC 排最前
// （与 PostgreSQL 的 NULLS FIRST 默认一致），DESC 自然排最后。
type SortNode struct {
	child Operator
	keys  []boundExpr
	desc  []bool

	rows    []sortRow
	pos     int
	drained bool
}

type sortRow struct {
	row  []types.Value
	keys []types.Value // 逐行预求值的排序键，比较时不再碰表达式
}

func (s *SortNode) Next() ([]types.Value, error) {
	if !s.drained {
		if err := s.drain(); err != nil {
			return nil, err
		}
		s.drained = true
	}
	if s.pos >= len(s.rows) {
		return nil, nil
	}
	r := s.rows[s.pos]
	s.pos++
	return r.row, nil
}

func (s *SortNode) drain() error {
	for {
		row, err := s.child.Next()
		if err != nil {
			return err
		}
		if row == nil {
			break
		}
		sr := sortRow{row: row, keys: make([]types.Value, len(s.keys))}
		for i, k := range s.keys {
			v, err := k.eval(row)
			if err != nil {
				return fmt.Errorf("evaluating ORDER BY key %d: %w", i+1, err)
			}
			sr.keys[i] = v
		}
		s.rows = append(s.rows, sr)
	}
	// 稳定排序：等键行保持原序，结果确定可复现。比较函数无法返回错误，
	// 跨族比较这类坏键先记下来，排完（顺序已无意义）再上报。
	var cmpErr error
	slices.SortStableFunc(s.rows, func(a, b sortRow) int {
		for i := range a.keys {
			c, err := compareForSort(a.keys[i], b.keys[i], s.desc[i])
			if err != nil {
				if cmpErr == nil {
					cmpErr = fmt.Errorf("ORDER BY key %d: %w", i+1, err)
				}
				return 0
			}
			if c != 0 {
				return c
			}
		}
		return 0
	})
	return cmpErr
}

// compareForSort 排序键的全序比较：NULL 视为最小，DESC 时调用方取反
// ——于是 ASC 的 NULLS FIRST 在 DESC 下自然变成 NULLS LAST。
func compareForSort(a, b types.Value, desc bool) (int, error) {
	var c int
	switch {
	case a.IsNull() && b.IsNull():
	case a.IsNull():
		c = -1
	case b.IsNull():
		c = 1
	default:
		var err error
		if c, err = types.Compare(a, b); err != nil {
			return 0, err
		}
	}
	if desc {
		return -c, nil
	}
	return c, nil
}

func (s *SortNode) Close() error { return s.child.Close() }

// ── Limit ────────────────────────────────────────────────────────

// LimitNode OFFSET 先跳过、LIMIT 再截断。limit < 0 表示没有 LIMIT
// （只有 OFFSET 的查询）。解析器保证两个数都是非负整数。
type LimitNode struct {
	child  Operator
	limit  int64 // -1 = 不限
	offset int64

	skipped int64
	sent    int64
	done    bool
}

func (l *LimitNode) Next() ([]types.Value, error) {
	if l.done {
		return nil, nil
	}
	for l.skipped < l.offset {
		row, err := l.child.Next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			l.done = true
			return nil, nil
		}
		l.skipped++
	}
	if l.limit >= 0 && l.sent >= l.limit {
		l.done = true
		return nil, nil
	}
	row, err := l.child.Next()
	if err != nil {
		return nil, err
	}
	if row == nil {
		l.done = true
		return nil, nil
	}
	l.sent++
	return row, nil
}

func (l *LimitNode) Close() error { return l.child.Close() }

// ── Distinct ─────────────────────────────────────────────────────

// DistinctNode 整行去重（SELECT DISTINCT）。用值的规范化身份做键
// （rowIdentity，见 aggregate.go），语义：NULL 与 NULL 算同一个值
// （SQL DISTINCT 的标准语义）；不同 Kind 不进同一等价类。
// 保留首次出现顺序 —— 排序后的输入不会被去重打乱。
type DistinctNode struct {
	child Operator
	seen  map[string]bool
}

func (d *DistinctNode) Next() ([]types.Value, error) {
	if d.seen == nil {
		d.seen = make(map[string]bool)
	}
	for {
		row, err := d.child.Next()
		if err != nil || row == nil {
			return row, err
		}
		key := rowIdentity(row)
		if d.seen[key] {
			continue
		}
		d.seen[key] = true
		return row, nil
	}
}

func (d *DistinctNode) Close() error { return d.child.Close() }
