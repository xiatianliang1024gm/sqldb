// Package encoding 负责 SQL 值与字节之间的双向翻译。
//
// 这是"在 KV 上做 SQL"的第一个核心决定：**主键必须保序编码**。
// KV 引擎唯一提供的能力是"按字节序扫描"，而 SQL 语义要的是"按值序扫描"
// （WHERE id > 10 要能转化成一次 seek）。两者对得上，范围下推才成立；
// 对不上，就只能全表捞回内存再排序 —— 等于把 KV 退化成一个大 map。
//
// 因此主键编码有三条铁律：
//  1. 任意两个值的编码按字节序比较的结果 == 值按 SQL 语义比较的结果；
//  2. 解码必须能完整还原（编码是双射）；
//  3. 前缀不能有歧义：两个不同值的编码不能互为前缀。
package encoding

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// 标签字节：null(0x00) < int(0x01) < float(0x02) < text(0x03) < bool(0x04)。
// 类型本身有序这一点顺带保证了"跨类型排序"有确定结果（实际 SQL 不允许跨族比较，
// 但主键列只可能是一种类型，这个性质只是防御性的）。
const (
	tagNull  byte = 0x00
	tagInt   byte = 0x01
	tagFloat byte = 0x02
	tagText  byte = 0x03
	tagBool  byte = 0x04
)

// EncodeKey 把一个非 NULL 主键值编码成保序字节串。NULL 主键不允许。
func EncodeKey(v types.Value) ([]byte, error) {
	if v.IsNull() {
		return nil, fmt.Errorf("primary key cannot be NULL")
	}
	switch v.Kind {
	case types.Int:
		// 8 字节大端 + 符号翻转：把 int64 的符号位取反后按无符号解释，
		// 负数（高位为 1 的补码）落到 0x00.. 区间，正数落到 0x80.. 区间，
		// 于是"数值升序"恰好等于"字节升序"。这是 UUID/时间戳有序化同款技巧。
		buf := make([]byte, 9)
		buf[0] = tagInt
		binary.BigEndian.PutUint64(buf[1:], uint64(v.I)^(1<<63))
		return buf, nil
	case types.Float:
		// IEEE 754 的排序怪癖：正浮点按位无符号比较恰好有序，负浮点恰好反序，
		// 0.0 与 -0.0 位模式不同但值相等。经典修法：负数全部按位取反，
		// 再翻一次符号位归一化。
		f := v.F
		if f == 0 {
			f = 0 // 把 -0.0 归一化成 +0.0：值相等，编码必须一致，否则解码会还原出 NaN
		}
		bits := math.Float64bits(f)
		if f < 0 {
			bits = ^bits
		} else {
			bits ^= 1 << 63
		}
		buf := make([]byte, 9)
		buf[0] = tagFloat
		binary.BigEndian.PutUint64(buf[1:], bits)
		return buf, nil
	case types.Text:
		// 文本不能直接接在后面：终止符方案里文本内容可能含 0x00，
		// 与"下一个标签字节"产生歧义。做法是转义 —— 内容里的 0x00 写成
		// 0x00 0xFF，真正的结尾用 0x00 0x00。因为转义序列严格大于任何
		// "前缀 + 提前终止"，字符串序 == 编码序，且两个编码互不为前缀。
		buf := make([]byte, 0, len(v.S)+3)
		buf = append(buf, tagText)
		for i := 0; i < len(v.S); i++ {
			buf = append(buf, v.S[i])
			if v.S[i] == 0x00 {
				buf = append(buf, 0xFF)
			}
		}
		return append(buf, 0x00, 0x00), nil
	case types.Bool:
		if v.B {
			return []byte{tagBool, 0x01}, nil
		}
		return []byte{tagBool, 0x00}, nil
	}
	return nil, fmt.Errorf("cannot encode key of kind %s", v.Kind)
}

// EncodeIndexValue 编码二级索引的索引列值（DESIGN §7.5）。与 EncodeKey
// 唯一的区别是允许 NULL：索引列可以为空，NULL 行用标签 0x00 占位
// （唯一约束豁免 NULL，但条目本身要进索引）。
func EncodeIndexValue(v types.Value) ([]byte, error) {
	if v.IsNull() {
		return []byte{tagNull}, nil
	}
	return EncodeKey(v)
}

// DecodeKey 是 EncodeKey 的逆操作（tagNull 分支服务于索引列值，M6）。
func DecodeKey(b []byte) (types.Value, error) {
	if len(b) == 0 {
		return types.Value{}, fmt.Errorf("empty key")
	}
	switch b[0] {
	case tagNull:
		if len(b) != 1 {
			return types.Value{}, fmt.Errorf("bad null key length %d", len(b))
		}
		return types.NullValue(), nil
	case tagInt:
		if len(b) != 9 {
			return types.Value{}, fmt.Errorf("bad int key length %d", len(b))
		}
		u := binary.BigEndian.Uint64(b[1:]) ^ (1 << 63)
		return types.IntValue(int64(u)), nil
	case tagFloat:
		if len(b) != 9 {
			return types.Value{}, fmt.Errorf("bad float key length %d", len(b))
		}
		bits := binary.BigEndian.Uint64(b[1:])
		if bits&(1<<63) == 0 {
			// 编码时负数被全部取反（符号位为 0），解码要先取反回去
			bits = ^bits
		} else {
			bits ^= 1 << 63
		}
		return types.FloatValue(math.Float64frombits(bits)), nil
	case tagText:
		out := make([]byte, 0, len(b))
		for i := 1; i < len(b); i++ {
			if b[i] == 0x00 {
				// 0x00 0xFF 是转义的 0x00；0x00 0x00 是结尾
				if i+1 < len(b) && b[i+1] == 0xFF {
					out = append(out, 0x00)
					i++
					continue
				}
				return types.TextValue(string(out)), nil
			}
			out = append(out, b[i])
		}
		return types.Value{}, fmt.Errorf("unterminated text key")
	case tagBool:
		if len(b) != 2 {
			return types.Value{}, fmt.Errorf("bad bool key length %d", len(b))
		}
		return types.BoolValue(b[1] == 0x01), nil
	}
	return types.Value{}, fmt.Errorf("unknown key tag 0x%02x", b[0])
}
