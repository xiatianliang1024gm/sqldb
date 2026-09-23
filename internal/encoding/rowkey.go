// 行键：数据区的键布局（DESIGN §3）。
//
//	d \x00 [tableID: 8B 大端] r [PK 保序编码]
//	└区┘└──┘└────固定头────┘└标记┘└─────行定位─────┘
//
// tableID 用定长 8B 而不是表名：表名长度可变，tab1 和 tab10 会前缀撞车，
// 定长 ID 不会。"r" 这个单字节标记是命名空间预留 —— 将来二级索引放
// d\x00<tableID>i...，与行数据互不干扰。
//
// 放在 encoding 包而不是 catalog：这里只做"字节怎么摆"，catalog 只借走
// 这三个函数，自己不拼任何键（分层约定见 DESIGN §2）。
package encoding

import (
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
