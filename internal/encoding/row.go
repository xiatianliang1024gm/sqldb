package encoding

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// 行值编码走的是另一条路：**不需要保序**（行的物理顺序完全由主键 key 决定），
// 只需要紧凑和可还原。选了最直白的 TLV：
//
//	每个列值 = null标记(1B) | 载荷
//	  0x00          （无载荷）
//	  0x01 | 载荷
//
//	载荷按类型：INT/FLOAT 定长 8 字节大端；TEXT uvarint 长度 + 字节；BOOL 1 字节。
//	多列按 schema 顺序首尾相接，没有列与列之间的分隔符 —— 解码靠 schema 顺序。
//
// 对比一下主键编码：那边花大力气做保序和前缀无歧义，这边只求编解码对称。
// "一个值两套编码"不是浪费 —— 两处诉求本来不同，硬用同一套只会两头受气。

// EncodeRow 按顺序编码一行的所有列值。
func EncodeRow(vals []types.Value) []byte {
	buf := make([]byte, 0, 32*len(vals))
	for _, v := range vals {
		if v.IsNull() {
			buf = append(buf, 0x00)
			continue
		}
		buf = append(buf, 0x01)
		switch v.Kind {
		case types.Int:
			var tmp [8]byte
			binary.BigEndian.PutUint64(tmp[:], uint64(v.I))
			buf = append(buf, tmp[:]...)
		case types.Float:
			var tmp [8]byte
			binary.BigEndian.PutUint64(tmp[:], math.Float64bits(v.F))
			buf = append(buf, tmp[:]...)
		case types.Text:
			var tmp [binary.MaxVarintLen64]byte
			n := binary.PutUvarint(tmp[:], uint64(len(v.S)))
			buf = append(buf, tmp[:n]...)
			buf = append(buf, v.S...)
		case types.Bool:
			if v.B {
				buf = append(buf, 0x01)
			} else {
				buf = append(buf, 0x00)
			}
		default:
			// 不可能到达：EncodeRow 的入参来自执行器，类型在 schema 校验时就拦住了
			buf = append(buf, 0x00)
		}
	}
	return buf
}

// DecodeRow 按列类型依次解码一行。需要 schema 是因为 TLV 里没带类型 ——
// 这是"空间换灵活性"：带着类型解码器可以自描述，但每行都付 5% 的元数据开销。
func DecodeRow(b []byte, kinds []types.Kind) ([]types.Value, error) {
	vals := make([]types.Value, 0, len(kinds))
	pos := 0
	for _, k := range kinds {
		if pos >= len(b) {
			return nil, fmt.Errorf("row truncated: expect column %d", len(vals))
		}
		if b[pos] == 0x00 {
			vals = append(vals, types.NullValue())
			pos++
			continue
		}
		if b[pos] != 0x01 {
			return nil, fmt.Errorf("corrupt row: bad null tag 0x%02x at %d", b[pos], pos)
		}
		pos++
		switch k {
		case types.Int, types.Float:
			if pos+8 > len(b) {
				return nil, fmt.Errorf("row truncated: expect 8-byte number")
			}
			u := binary.BigEndian.Uint64(b[pos:])
			pos += 8
			if k == types.Int {
				vals = append(vals, types.IntValue(int64(u)))
			} else {
				vals = append(vals, types.FloatValue(math.Float64frombits(u)))
			}
		case types.Text:
			n, m := binary.Uvarint(b[pos:])
			if m <= 0 || pos+m+int(n) > len(b) {
				return nil, fmt.Errorf("row truncated: bad text length")
			}
			pos += m
			vals = append(vals, types.TextValue(string(b[pos:pos+int(n)])))
			pos += int(n)
		case types.Bool:
			if pos+1 > len(b) {
				return nil, fmt.Errorf("row truncated: expect bool")
			}
			vals = append(vals, types.BoolValue(b[pos] == 0x01))
			pos++
		default:
			return nil, fmt.Errorf("unknown column kind %s", k)
		}
	}
	return vals, nil
}
