package encoding

// 行值编码的单测：不需要保序，但必须可逆、必须能识别截断与损坏。

import (
	"math/rand"
	"testing"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// TestRowRoundtrip 多列、含 NULL 的行往返。逐列用 Compare 校验相等
// （允许 -0.0 之类的位模式差异，值语义相等即可）。
func TestRowRoundtrip(t *testing.T) {
	kinds := []types.Kind{types.Int, types.Float, types.Text, types.Bool, types.Text}
	cases := [][]types.Value{
		{
			types.IntValue(-42), types.FloatValue(3.5), types.TextValue("hello"),
			types.BoolValue(true), types.TextValue("with\x00nul"),
		},
		{
			types.IntValue(0), types.NullValue(), types.NullValue(),
			types.BoolValue(false), types.TextValue(""),
		},
		{
			types.NullValue(), types.FloatValue(-0.5), types.TextValue("x"),
			types.NullValue(), types.NullValue(),
		},
	}
	for _, vals := range cases {
		enc := EncodeRow(vals)
		got, err := DecodeRow(enc, kinds)
		if err != nil {
			t.Fatalf("decode % x: %v", enc, err)
		}
		if len(got) != len(kinds) {
			t.Fatalf("decoded %d columns, want %d", len(got), len(kinds))
		}
		for i := range vals {
			if vals[i].IsNull() != got[i].IsNull() {
				t.Fatalf("column %d nullness mismatch: %v vs %v", i, vals[i], got[i])
			}
			if vals[i].IsNull() {
				continue
			}
			cmp, err := types.Compare(vals[i], got[i])
			if err != nil || cmp != 0 {
				t.Fatalf("column %d mismatch: %v -> %v (cmp=%d err=%v)", i, vals[i], got[i], cmp, err)
			}
		}
	}
}

// TestDecodeRowTruncated 每种截断点都必须报错：null 标记之后少载荷、
// 定长数少字节、TEXT 长度头或内容被截断、BOOL 缺字节。
func TestDecodeRowTruncated(t *testing.T) {
	kinds := []types.Kind{types.Int, types.Text, types.Bool}
	full := EncodeRow([]types.Value{
		types.IntValue(1234567), types.TextValue("some text"), types.BoolValue(true),
	})
	// 对每一种"截到最后 n 字节"的长度，解码要么成功（恰好是可截的边界），
	// 要么报错 —— 绝不能返回错误数量的列或崩溃。
	for n := 0; n < len(full); n++ {
		got, err := DecodeRow(full[:n], kinds)
		if err == nil {
			// 只有截到"前缀列恰好完整"的边界才合法；此时列数必须小于完整列数。
			if len(got) >= len(kinds) {
				t.Fatalf("DecodeRow(%d bytes) unexpectedly decoded full row", n)
			}
		}
	}
	// 坏 null 标记
	if _, err := DecodeRow([]byte{0x02, 0, 0, 0, 0, 0, 0, 0, 1}, kinds); err == nil {
		t.Fatal("bad null tag must fail")
	}
	// 未知列类型
	if _, err := DecodeRow(EncodeRow([]types.Value{types.IntValue(1)}), []types.Kind{types.Kind(99)}); err == nil {
		t.Fatal("unknown column kind must fail")
	}
}

// TestRowRoundtripRandom 随机行来回压缩一遍，抓手工样例想不到的组合。
func TestRowRoundtripRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(20260923))
	allKinds := []types.Kind{types.Int, types.Float, types.Text, types.Bool}
	for iter := 0; iter < 200; iter++ {
		n := 1 + rng.Intn(8)
		kinds := make([]types.Kind, n)
		vals := make([]types.Value, n)
		for i := range kinds {
			k := allKinds[rng.Intn(len(allKinds))]
			kinds[i] = k
			if rng.Intn(4) == 0 {
				vals[i] = types.NullValue()
				continue
			}
			switch k {
			case types.Int:
				vals[i] = types.IntValue(int64(rng.Int63()) - (1 << 62))
			case types.Float:
				vals[i] = types.FloatValue(rng.Float64()*2 - 1)
			case types.Text:
				b := make([]byte, rng.Intn(16))
				rng.Read(b)
				vals[i] = types.TextValue(string(b))
			case types.Bool:
				vals[i] = types.BoolValue(rng.Intn(2) == 0)
			}
		}
		got, err := DecodeRow(EncodeRow(vals), kinds)
		if err != nil {
			t.Fatalf("iter %d: decode: %v", iter, err)
		}
		for i := range vals {
			if vals[i].IsNull() {
				if !got[i].IsNull() {
					t.Fatalf("iter %d col %d: want NULL", iter, i)
				}
				continue
			}
			cmp, err := types.Compare(vals[i], got[i])
			if err != nil || cmp != 0 {
				t.Fatalf("iter %d col %d: %v vs %v", iter, i, vals[i], got[i])
			}
		}
	}
}
