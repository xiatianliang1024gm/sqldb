// 行键：数据区的键布局（DESIGN §3）。
//
//	d \x00 [tableID: 8B 大端] r [PK 保序编码]
//	└区┘└──┘└────固定头────┘└标记┘└─────行定位─────┘
//
// tableID 用定长 8B 而不是表名：表名长度可变，tab1 和 tab10 会前缀撞车，
// 定长 ID 不会。"r" 是行数据的命名空间标记，二级索引条目放
// d\x00<tableID>i...（M6 起，见下方索引条目键一节）。
//
// 放在 encoding 包而不是 catalog：这里只做"字节怎么摆"，catalog 只借走
// 这些函数，自己不拼任何键（分层约定见 DESIGN §2）。
package encoding

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// dataRegionPrefix 是数据区的两字节前缀，与 meta 区的 "m\x00" 对应。
const dataRegionPrefix = "d\x00"

// rowKeyHeadLen 是行键的固定头长度：区前缀(2) + 表 ID(8) + 'r'(1)。
const rowKeyHeadLen = len(dataRegionPrefix) + 8 + 1

// RowKeyPrefix 返回一张表全部行键的公共前缀。表内行按键中 PK 编码有序，
// 因此一次前缀扫描就是一次按主键值序的全表扫。
func RowKeyPrefix(tableID uint64) []byte {
	buf := make([]byte, 0, rowKeyHeadLen)
	buf = append(buf, dataRegionPrefix...)
	buf = binary.BigEndian.AppendUint64(buf, tableID)
	return append(buf, 'r')
}

// RowKey 拼出一行的完整键：表前缀 + 已保序编码的主键。
// pkEncoded 必须来自 EncodeKey —— 这里不重复校验，调用方（执行器）负责。
func RowKey(tableID uint64, pkEncoded []byte) []byte {
	buf := RowKeyPrefix(tableID)
	return append(buf, pkEncoded...)
}

// SplitRowKey 把一行键拆回表 ID 和主键编码，是 RowKey 的逆操作。
// DROP TABLE 按前缀扫出的键、以及从扫描结果反查行归属时用得到。
func SplitRowKey(k []byte) (tableID uint64, pkEncoded []byte, err error) {
	if len(k) < rowKeyHeadLen || string(k[:2]) != dataRegionPrefix || k[10] != 'r' {
		return 0, nil, fmt.Errorf("not a row key (len %d): % x", len(k), k)
	}
	return binary.BigEndian.Uint64(k[2:10]), k[11:], nil
}

// ── 索引条目键（M6，DESIGN §7.5）────────────────────────────────
//
//	d \x00 [tableID: 8B] i [索引名] \x00 [索引列保序编码] [主键保序编码]   值 = 空
//	└区┘└──┘└────固定头────┘└标记┘└──区域──┘└────定位────────└────行定位────┘
//
// 唯一/非唯一索引同构：主键编码接在键尾，非唯一索引靠它区分同值行，
// 唯一索引靠"对索引值前缀 seek、比较尾部主键"查重（DESIGN §7.5）。
//
// 索引名是区域分隔：一张表可以有多张索引，它们的条目混在同一个 i 前缀
// 里，键本身不含"属于哪张索引"——不隔开的话，DROP INDEX 无法只删自己
// 的条目，同类型列的两张索引还会在区间扫描里互相串行。用名字而不是
// 数字 ID：索引名是标识符，词法上不可能含 0x00，单字节终止符无歧义，
// 不需要额外的 ID 分配计数器，代价只是键略长（取舍表 #17）。

// indexKeyHeadLen 是索引条目键的数字固定头长度：区前缀(2) + 表 ID(8) + 'i'(1)。
const indexKeyHeadLen = len(dataRegionPrefix) + 8 + 1

// IndexKeyPrefix 返回一张表全部索引条目键的公共前缀（跨该表所有索引）。
func IndexKeyPrefix(tableID uint64) []byte {
	buf := make([]byte, 0, indexKeyHeadLen)
	buf = append(buf, dataRegionPrefix...)
	buf = binary.BigEndian.AppendUint64(buf, tableID)
	return append(buf, 'i')
}

// IndexRegionPrefix 返回某个索引的条目区域前缀：表内 i 前缀 + 索引名 +
// 0x00 终止符。该索引的全部条目恰好都以它为前缀 —— DROP INDEX / DROP
// TABLE 的区域清理、计划里的兜底区间都用它。
func IndexRegionPrefix(tableID uint64, indexName string) []byte {
	buf := make([]byte, 0, indexKeyHeadLen+len(indexName)+1)
	buf = append(buf, IndexKeyPrefix(tableID)...)
	buf = append(buf, indexName...)
	return append(buf, 0x00)
}

// IndexEntryKey 拼出一条索引条目键：索引区域前缀 + 已保序编码的索引列值 +
// 已保序编码的主键。两段编码都必须来自 EncodeKey，这里不重复校验。
func IndexEntryKey(tableID uint64, indexName string, idxEncoded, pkEncoded []byte) []byte {
	buf := make([]byte, 0, indexKeyHeadLen+len(indexName)+1+len(idxEncoded)+len(pkEncoded))
	buf = append(buf, IndexRegionPrefix(tableID, indexName)...)
	buf = append(buf, idxEncoded...)
	return append(buf, pkEncoded...)
}

// IndexValuePrefix 返回"某索引中索引值等于给定编码"的条目前缀 —— 唯一
// 查重与等值区间下推都用它。
func IndexValuePrefix(tableID uint64, indexName string, idxEncoded []byte) []byte {
	buf := make([]byte, 0, indexKeyHeadLen+len(indexName)+1+len(idxEncoded))
	buf = append(buf, IndexRegionPrefix(tableID, indexName)...)
	return append(buf, idxEncoded...)
}

// SplitIndexEntryKey 把索引条目键拆回表 ID、索引名、索引列编码和主键
// 编码，是 IndexEntryKey 的逆操作。索引名靠 0x00 终止符切段；索引列
// 编码是变长的，按类型标签定长（或自终止）切段 —— encodedKeyLen 与
// EncodeKey 的格式一一对应。
func SplitIndexEntryKey(k []byte) (tableID uint64, indexName string, idxEncoded, pkEncoded []byte, err error) {
	if len(k) < indexKeyHeadLen || string(k[:2]) != dataRegionPrefix || k[10] != 'i' {
		return 0, "", nil, nil, fmt.Errorf("not an index entry key (len %d): % x", len(k), k)
	}
	z := bytes.IndexByte(k[11:], 0x00)
	if z < 0 {
		return 0, "", nil, nil, fmt.Errorf("index entry key % x: unterminated index name", k)
	}
	name := string(k[11 : 11+z])
	rest := k[12+z:]
	n, err := encodedKeyLen(rest)
	if err != nil {
		return 0, "", nil, nil, fmt.Errorf("index entry key % x: %w", k, err)
	}
	if len(rest) <= n {
		return 0, "", nil, nil, fmt.Errorf("index entry key % x: missing primary key suffix", k)
	}
	return binary.BigEndian.Uint64(k[2:10]), name, rest[:n], rest[n:], nil
}

// encodedKeyLen 返回 b 开头第一个保序编码的字节长度。TEXT 靠 0x00 0x00
// 结尾自终止，其余按标签定长。DecodeKey 只认完整编码，切段必须自己数。
func encodedKeyLen(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, fmt.Errorf("empty encoding")
	}
	switch b[0] {
	case tagNull:
		return 1, nil
	case tagInt, tagFloat:
		return 9, nil
	case tagBool:
		return 2, nil
	case tagText:
		for i := 1; i < len(b); i++ {
			if b[i] != 0x00 {
				continue
			}
			if i+1 < len(b) && b[i+1] == 0xFF {
				i++ // 转义的 0x00，跳过补字节
				continue
			}
			// 结尾 0x00 0x00：EncodeKey 恒以这两个字节收尾
			if i+1 >= len(b) {
				return 0, fmt.Errorf("truncated text encoding")
			}
			return i + 2, nil
		}
		return 0, fmt.Errorf("unterminated text encoding")
	}
	return 0, fmt.Errorf("unknown key tag 0x%02x", b[0])
}
