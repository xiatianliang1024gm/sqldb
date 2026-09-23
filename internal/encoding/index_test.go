package encoding

// M6 索引条目键的单测（DESIGN §7.5）：往返、切段、前缀无歧义、区域隔离。
// 保序性质本身由 key_test.go 钉死 —— 条目键只是把索引名和两段保序编码
// 接起来，这里验证的是"接起来之后还能完整拆回去、不同索引值不互为前缀、
// 不同索引的区域互不重叠"。

import (
	"bytes"
	"testing"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

func TestIndexEntryKeyRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		idx  types.Value // 索引列值（含 NULL）
		pk   types.Value
	}{
		{"int", types.IntValue(-5), types.IntValue(42)},
		{"float", types.FloatValue(2.5), types.IntValue(7)},
		{"text", types.TextValue("hello"), types.TextValue("pk1")},
		{"text-with-nul", types.TextValue("a\x00b"), types.IntValue(1)},
		{"null-index-value", types.NullValue(), types.IntValue(3)},
		{"bool", types.BoolValue(true), types.TextValue("z")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idxEnc, err := EncodeIndexValue(tc.idx)
			if err != nil {
				t.Fatalf("encode index value: %v", err)
			}
			pkEnc, err := EncodeKey(tc.pk)
			if err != nil {
				t.Fatalf("encode pk: %v", err)
			}
			key := IndexEntryKey(9, "ix_name", idxEnc, pkEnc)

			tableID, indexName, gotIdx, gotPK, err := SplitIndexEntryKey(key)
			if err != nil {
				t.Fatalf("split: %v", err)
			}
			if tableID != 9 || indexName != "ix_name" {
				t.Fatalf("tableID=%d indexName=%q", tableID, indexName)
			}
			if !bytes.Equal(gotIdx, idxEnc) {
				t.Fatalf("index encoding mismatch: got % x, want % x", gotIdx, idxEnc)
			}
			if !bytes.Equal(gotPK, pkEnc) {
				t.Fatalf("pk encoding mismatch: got % x, want % x", gotPK, pkEnc)
			}
			// 拆出来的索引值必须能解码回原值（NULL 也一样）。
			gotIdxVal, err := DecodeKey(gotIdx)
			if err != nil {
				t.Fatalf("decode index value: %v", err)
			}
			if tc.idx.IsNull() != gotIdxVal.IsNull() {
				t.Fatalf("index value null-ness: got %v", gotIdxVal)
			}
		})
	}
}

func TestIndexValuePrefixUnambiguous(t *testing.T) {
	// 不同索引值的编码互不为前缀 —— 等值下推的区间几何依赖这一点。
	vals := []types.Value{
		types.IntValue(1), types.IntValue(12),
		types.FloatValue(1), types.FloatValue(1.5),
		types.TextValue("ab"), types.TextValue("abc"), types.TextValue("a\x00"),
		types.NullValue(), types.BoolValue(false),
	}
	for _, v := range vals {
		enc, err := EncodeIndexValue(v)
		if err != nil {
			t.Fatalf("encode %v: %v", v, err)
		}
		for _, w := range vals {
			wenc, err := EncodeIndexValue(w)
			if err != nil {
				t.Fatalf("encode %v: %v", w, err)
			}
			if !bytes.Equal(enc, wenc) && bytes.HasPrefix(wenc, enc) {
				t.Fatalf("encoding of %v is a prefix of encoding of %v", v, w)
			}
		}
	}
}

func TestIndexRegionIsolation(t *testing.T) {
	// 同一张表的两张索引，区域前缀必须互不重叠（同名不可能——catalog
	// 保证名字全局唯一；不同名则 0x00 终止符保证一个不是另一个的前缀）。
	a := IndexRegionPrefix(7, "ix_a")
	b := IndexRegionPrefix(7, "ix_ab")
	if bytes.HasPrefix(b, a) || bytes.HasPrefix(a, b) {
		t.Fatalf("regions overlap: % x vs % x", a, b)
	}
	// 跨表的区域天然隔离（tableID 定长）。
	if bytes.HasPrefix(IndexRegionPrefix(8, "ix_a"), a) {
		t.Fatal("different tables share region")
	}
}

func TestSplitIndexEntryKeyErrors(t *testing.T) {
	if _, _, _, _, err := SplitIndexEntryKey(RowKey(1, []byte{tagInt, 0, 0, 0, 0, 0, 0, 0, 1})); err == nil {
		t.Fatal("row key accepted as index entry key")
	}
	// 缺索引名终止符必须报错。
	bad := append(IndexKeyPrefix(1), 'i', 'x')
	if _, _, _, _, err := SplitIndexEntryKey(bad); err == nil {
		t.Fatal("unterminated index name accepted")
	}
	// 截断的 TEXT 编码（没有 0x00 0x00 结尾）必须报错，不能误切。
	bad2 := append(IndexRegionPrefix(1, "ix"), tagText, 'x')
	if _, _, _, _, err := SplitIndexEntryKey(bad2); err == nil {
		t.Fatal("unterminated text encoding accepted")
	}
}
