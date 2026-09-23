package exec

// 聚合（DESIGN §7.2 HashAgg / §7.4）：GROUP BY 哈希聚合；无 GROUP BY 时
// 单组，空输入也产一行（COUNT(*)=0、SUM=NULL 的 SQL 语义，DESIGN §7.2）。
//
// 执行形态：子节点整个物化，按 GROUP BY 表达式的值哈希分组，组内聚合
// 一遍扫描增量累积。每个组的输出是一行"伪行"：
//
//	伪行 = 组内第一行的全部基表列 ++ 各聚合的最终值
//
// 之后 SELECT 输出项 / HAVING / ORDER BY 都绑定在伪行上求值：列引用取
// 代表行（组内第一行 —— 非聚合列引用必须被 GROUP BY 覆盖才合法，见
// binder 的 covered 检查），聚合引用取伪行后缀的槽位（aggRefExpr）。

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// aggSpec 是一个已绑定的聚合调用。
type aggSpec struct {
	fn   string // COUNT / SUM / AVG / MIN / MAX（parser 已归一成大写）
	arg  boundExpr
	star bool // COUNT(*)
}

// aggRefExpr 引用伪行里某个聚合槽位的值。
type aggRefExpr struct{ offset int }

func (e aggRefExpr) eval(row []types.Value) (types.Value, error) { return row[e.offset], nil }

// aggContext 是绑定聚合查询表达式时的共享上下文（挂在 binder.ab 上）。
// collect 为 nil 表示当前位置不允许出现聚合调用（GROUP BY 表达式）；
// covered 是允许的非聚合列偏移集合（GROUP BY 表达式引用到的列）。
type aggContext struct {
	collect  *[]aggSpec
	aggIndex map[string]int // describeExpr → 槽位（同一聚合写多处时去重）
	covered  map[int]bool
	width    int  // 基表列总宽（伪行前缀长度）
	inAgg    bool // 正在绑定聚合参数：参数里的列引用豁免 covered 检查
}

// ── 增量聚合状态 ─────────────────────────────────────────────────

type aggState struct {
	n       int64       // 参与聚合的非 NULL 值个数（COUNT(*) 时即行数）
	isFloat bool        // SUM 见过 FLOAT 输入 → 结果升格 FLOAT
	iSum    int64       // INT 累加（isFloat=false 时的最终结果）
	fSum    float64     // FLOAT 累加（含 INT 升格；AVG 永远走这里）
	best    types.Value // MIN/MAX 当前极值
	hasBest bool
}

type aggGroup struct {
	rep    []types.Value // 组内第一行（伪行前缀）
	states []aggState
}

// HashAggNode 分组聚合节点。
type HashAggNode struct {
	child      Operator
	groupExprs []boundExpr // 绑定在基表行上
	aggs       []aggSpec
	width      int // 基表列总宽

	groups  []*aggGroup
	drained bool
	pos     int
}

func (h *HashAggNode) Next() ([]types.Value, error) {
	if !h.drained {
		if err := h.drain(); err != nil {
			return nil, err
		}
		h.drained = true
	}
	if h.pos >= len(h.groups) {
		return nil, nil
	}
	g := h.groups[h.pos]
	h.pos++
	row := make([]types.Value, 0, h.width+len(h.aggs))
	row = append(row, g.rep...)
	for i := range h.aggs {
		v, err := finalizeAgg(&h.aggs[i], &g.states[i])
		if err != nil {
			return nil, err
		}
		row = append(row, v)
	}
	return row, nil
}

func (h *HashAggNode) drain() error {
	index := make(map[string]*aggGroup)
	for {
		row, err := h.child.Next()
		if err != nil {
			return err
		}
		if row == nil {
			break
		}
		key := ""
		if len(h.groupExprs) > 0 {
			kv := make([]types.Value, len(h.groupExprs))
			for i, ge := range h.groupExprs {
				v, err := ge.eval(row)
				if err != nil {
					return fmt.Errorf("evaluating GROUP BY key %d: %w", i+1, err)
				}
				kv[i] = v
			}
			key = rowIdentity(kv) // NULL 与 NULL 同组（SQL 分组语义）
		}
		g := index[key]
		if g == nil {
			g = &aggGroup{
				rep:    append([]types.Value(nil), row...),
				states: make([]aggState, len(h.aggs)),
			}
			index[key] = g
			h.groups = append(h.groups, g)
		}
		for i := range h.aggs {
			if err := accumulate(&h.aggs[i], &g.states[i], row); err != nil {
				return err
			}
		}
	}
	// 无 GROUP BY 的空输入也产一行单组（COUNT(*)=0 语义）；有 GROUP BY
	// 的空输入就是零组零行 —— 组必须真的存在才有一行。
	if len(h.groups) == 0 && len(h.groupExprs) == 0 {
		rep := make([]types.Value, h.width)
		for i := range rep {
			rep[i] = types.NullValue()
		}
		h.groups = append(h.groups, &aggGroup{rep: rep, states: make([]aggState, len(h.aggs))})
	}
	return nil
}

func (h *HashAggNode) Close() error { return h.child.Close() }

// accumulate 把一行喂进一个聚合的增量状态。类型检查从宽到执行期
// （DESIGN §7.4）：SUM(text_col) 在第一个非 NULL 值上报错，而不是计划期。
func accumulate(spec *aggSpec, st *aggState, row []types.Value) error {
	if spec.star {
		st.n++ // COUNT(*)：数行，不碰任何列
		return nil
	}
	v, err := spec.arg.eval(row)
	if err != nil {
		return err
	}
	if v.IsNull() {
		return nil // 除 COUNT(*) 外，所有聚合都跳过 NULL
	}
	switch spec.fn {
	case "COUNT":
		st.n++
	case "SUM", "AVG":
		switch v.Kind {
		case types.Int:
			st.iSum += v.I
			st.fSum += float64(v.I) // AVG 永远按浮点算
		case types.Float:
			st.isFloat = true
			st.fSum += v.F
		default:
			return fmt.Errorf("%s requires numeric input, got %s", spec.fn, v.Kind)
		}
		st.n++
	case "MIN", "MAX":
		if !st.hasBest {
			st.best, st.hasBest = v, true
			break
		}
		c, err := types.Compare(v, st.best)
		if err != nil {
			return fmt.Errorf("%s: %w", spec.fn, err)
		}
		if (spec.fn == "MIN" && c < 0) || (spec.fn == "MAX" && c > 0) {
			st.best = v
		}
		st.n++
	default:
		return fmt.Errorf("unknown aggregate %s", spec.fn)
	}
	return nil
}

// finalizeAgg 结算一个聚合。空组（无输入行或全 NULL）的约定：
// COUNT = 0，SUM/AVG/MIN/MAX = NULL —— 与 PostgreSQL 一致。
func finalizeAgg(spec *aggSpec, st *aggState) (types.Value, error) {
	switch spec.fn {
	case "COUNT":
		return types.IntValue(st.n), nil
	case "SUM":
		if st.n == 0 {
			return types.NullValue(), nil
		}
		if st.isFloat {
			return types.FloatValue(st.fSum), nil
		}
		return types.IntValue(st.iSum), nil // INT 求和保持 INT，不悄悄升格
	case "AVG":
		if st.n == 0 {
			return types.NullValue(), nil
		}
		return types.FloatValue(st.fSum / float64(st.n)), nil // AVG 永远是 FLOAT
	case "MIN", "MAX":
		if !st.hasBest {
			return types.NullValue(), nil
		}
		return st.best, nil
	}
	return types.Value{}, fmt.Errorf("unknown aggregate %s", spec.fn)
}

// ── 值的规范化身份 ───────────────────────────────────────────────

// valueIdentity 把一个 SQL 值规范化成可比较身份的字符串 —— GROUP BY 的
// 哈希键和 DISTINCT 的去重键共用。约定（DESIGN §12 M4 部分）：
//   - NULL 与 NULL 同一身份（SQL 分组 / 去重语义）；
//   - ±0.0 归一化成同一身份（types.Compare 判定它们相等，身份必须跟上）；
//   - INT 5 与 FLOAT 5.0 是不同身份：不同 Kind 不进同一等价类，
//     与 types.Compare 拒绝跨族比较的立场一致。
func valueIdentity(v types.Value) string {
	switch v.Kind {
	case types.Null:
		return "\x00"
	case types.Int:
		b := binary.BigEndian.AppendUint64(nil, uint64(v.I))
		return "\x01" + string(b)
	case types.Float:
		f := v.F
		if f == 0 {
			f = 0 // 把 -0.0 归一化成 +0.0
		}
		b := binary.BigEndian.AppendUint64(nil, math.Float64bits(f))
		return "\x02" + string(b)
	case types.Text:
		b := binary.AppendUvarint(nil, uint64(len(v.S)))
		return "\x03" + string(b) + v.S
	case types.Bool:
		if v.B {
			return "\x04\x01"
		}
		return "\x04\x00"
	}
	return "\xFF" // 不可达（五种 Kind 之外没有值）
}

// rowIdentity 拼整行身份。各列的 Kind 由 schema 固定，按位拼接无歧义。
func rowIdentity(row []types.Value) string {
	s := make([]byte, 0, len(row)*9)
	for _, v := range row {
		s = append(s, valueIdentity(v)...)
	}
	return string(s)
}
