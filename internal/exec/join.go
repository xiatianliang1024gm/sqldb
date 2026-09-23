package exec

// NLJoin：INNER JOIN 的嵌套循环实现（DESIGN §7.2）。外表逐行，内表全量
// 物化缓存 —— 内表每轮重建迭代器是最亏的读法，物化一次是 Volcano 框架里
// 最朴素也最常见的做法；哈希连接 / 归并连接是这条基线之后的课。
//
// 多表 JOIN 在 select.go 里装成左深树：
//
//	NLJoin(NLJoin(scan1, scan2, on12), scan3, on(123))
//
// 连接行 = 各表行按 FROM 出现顺序首尾相接，binder 的列偏移基址按同一
// 顺序分配 —— 输出表达式里的列偏移因此与表在 FROM 里的位置一一对应。

import (
	"fmt"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

type NLJoinNode struct {
	outer      Operator
	inner      Operator  // 内表扫描（ScanNode / IndexScanNode）；首轮 Next 时整表物化，之后不再碰迭代器
	on         boundExpr // 绑定在"外表 ++ 内表"的合并行上（作用域只含已出现的表）
	outerWidth int       // 外表行的列数 = 合并行的前缀长度

	innerRows [][]types.Value
	loaded    bool

	cur  []types.Value // 当前外表行；nil 表示该取下一行
	i    int           // 在 innerRows 里的游标（当前外表行已配到哪）
	done bool
}

func (n *NLJoinNode) Next() ([]types.Value, error) {
	if n.done {
		return nil, nil
	}
	if !n.loaded {
		for {
			row, err := n.inner.Next()
			if err != nil {
				return nil, err
			}
			if row == nil {
				break
			}
			n.innerRows = append(n.innerRows, row)
		}
		n.loaded = true
	}
	for {
		if n.cur == nil {
			row, err := n.outer.Next()
			if err != nil || row == nil {
				n.done = row == nil
				return row, err
			}
			n.cur = row
			n.i = 0
		}
		for ; n.i < len(n.innerRows); n.i++ {
			combined := make([]types.Value, 0, n.outerWidth+len(n.innerRows[n.i]))
			combined = append(combined, n.cur...)
			combined = append(combined, n.innerRows[n.i]...)
			v, err := n.on.eval(combined)
			if err != nil {
				return nil, fmt.Errorf("evaluating JOIN ON: %w", err)
			}
			if v.IsNull() {
				continue // 三值逻辑：ON 结果为 NULL 视为不匹配
			}
			if v.Kind != types.Bool {
				return nil, fmt.Errorf("JOIN ON must evaluate to boolean, got %s", v.Kind)
			}
			if v.B {
				n.i++ // 下次从内表的下一行继续配对
				return combined, nil
			}
		}
		n.cur = nil // 内表耗尽，换下一行外表
	}
}

func (n *NLJoinNode) Close() error {
	err := n.outer.Close()
	if err2 := n.inner.Close(); err == nil {
		err = err2
	}
	return err
}
