package encoding

// M0 验收单测：主键保序编码的三条铁律（保序、可逆、前缀无歧义）
// 逐条用数据钉死。以后任何人改 EncodeKey，这里会第一个叫。

import (
	"bytes"
	"math"
	"slices"
	"testing"

	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// 各类型按 SQL 值序升序排列的样例值。测试直接拿这份顺序对照编码顺序。
var orderedSamples = map[types.Kind][]types.Value{
	types.Int: {
		types.IntValue(math.MinInt64), types.IntValue(-1 << 40), types.IntValue(-65536),
		types.IntValue(-1), types.IntValue(0), types.IntValue(1), types.IntValue(255),
		types.IntValue(1 << 40), types.IntValue(math.MaxInt64),
	},
	types.Float: {
		types.FloatValue(-1e308), types.FloatValue(-3.14), types.FloatValue(-1),
		types.FloatValue(-1e-300), types.FloatValue(0), types.FloatValue(1e-300),
		types.FloatValue(1), types.FloatValue(3.14), types.FloatValue(1e308),
	},
	types.Text: {
		types.TextValue(""), types.TextValue("\x00"), types.TextValue("\x00a"),
		types.TextValue("a"), types.TextValue("a\x00b"), types.TextValue("ab"),
		types.TextValue("abc"), types.TextValue("b"), types.TextValue("数据库"),
	},
	types.Bool: {
		types.BoolValue(false), types.BoolValue(true),
	},
}

// TestEncodeKeyOrderedPerKind 铁律一：同一类型内，编码的字典序 == 值序。
// INT 的符号翻转、FLOAT 的位模式修正、TEXT 的 0x00 转义，三套技巧都靠它兜底。
func TestEncodeKeyOrderedPerKind(t *testing.T) {
	for kind, vals := range orderedSamples {
		var prev []byte
		for i, v := range vals {
			enc, err := EncodeKey(v)
			if err != nil {
				t.Fatalf("%s: encode %v: %v", kind, v, err)
			}
			if i > 0 && bytes.Compare(prev, enc) >= 0 {
				t.Fatalf("%s: encoding order broken: %v -> %v\nprev = % x\ncur  = % x",
					kind, vals[i-1], v, prev, enc)
			}
			prev = enc
		}
	}
}

// TestEncodeKeyNegativeIntsBelowPositive 单独验证符号位翻转最容易被写反的地方：
// 负数的编码必须整体落在正数之前。
func TestEncodeKeyNegativeIntsBelowPositive(t *testing.T) {
	neg, _ := EncodeKey(types.IntValue(-1))
	zero, _ := EncodeKey(types.IntValue(0))
	pos, _ := EncodeKey(types.IntValue(1))
	if !(bytes.Compare(neg, zero) < 0 && bytes.Compare(zero, pos) < 0) {
		t.Fatalf("negative ints must sort below positives: % x < % x < % x", neg, zero, pos)
	}
}

// TestFloatNegativeZeroSameEncoding ±0.0 值相同，编码必须逐字节一致，
// 且解码回 +0.0（编码前的实现会在这里还原出 NaN）。
func TestFloatNegativeZeroSameEncoding(t *testing.T) {
	pos, err := EncodeKey(types.FloatValue(0))
	if err != nil {
		t.Fatal(err)
	}
	neg, err := EncodeKey(types.FloatValue(math.Copysign(0, -1)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pos, neg) {
		t.Fatalf("+0.0 and -0.0 must encode identically:\n+0 = % x\n-0 = % x", pos, neg)
	}
	got, err := DecodeKey(neg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != types.Float || math.IsNaN(got.F) || got.F != 0 {
		t.Fatalf("decode of ±0.0 = %v, want +0.0", got)
	}
}

// TestEncodeKeyCrossTypeTagOrder 防御性：标签字节本身有序，跨类型编码有确定结果。
func TestEncodeKeyCrossTypeTagOrder(t *testing.T) {
	intEnc, _ := EncodeKey(types.IntValue(0))
	floatEnc, _ := EncodeKey(types.FloatValue(0))
	textEnc, _ := EncodeKey(types.TextValue(""))
	boolEnc, _ := EncodeKey(types.BoolValue(false))
	if !(bytes.Compare(intEnc, floatEnc) < 0 &&
		bytes.Compare(floatEnc, textEnc) < 0 &&
		bytes.Compare(textEnc, boolEnc) < 0) {
		t.Fatalf("cross-type tag order broken: int < float < text < bool")
	}
}

// TestDecodeKeyRoundtrip 铁律二：可逆。解码回来与原值按 SQL 语义相等。
// （±0.0 在这里表现为"解码后与原值 Compare == 0"，允许位模式归一化。）
func TestDecodeKeyRoundtrip(t *testing.T) {
	var all []types.Value
	for _, vals := range orderedSamples {
		all = append(all, vals...)
	}
	all = append(all, types.FloatValue(math.Copysign(0, -1)))
	for _, v := range all {
		enc, err := EncodeKey(v)
		if err != nil {
			t.Fatalf("encode %v: %v", v, err)
		}
		got, err := DecodeKey(enc)
		if err != nil {
			t.Fatalf("decode %v (encoded % x): %v", v, enc, err)
		}
		cmp, err := types.Compare(v, got)
		if err != nil || cmp != 0 {
			t.Fatalf("roundtrip mismatch: %v -> % x -> %v (cmp=%d err=%v)", v, enc, got, cmp, err)
		}
	}
}

// TestEncodeKeyTextPrefixDisjoint 铁律三：TEXT 的任意两个编码互不为前缀。
// 这是"seek 到 ab 不会误入 abc"的正确性前提（DESIGN §4.1）。
func TestEncodeKeyTextPrefixDisjoint(t *testing.T) {
	encs := make([][]byte, 0, len(orderedSamples[types.Text]))
	for _, v := range orderedSamples[types.Text] {
		enc, err := EncodeKey(v)
		if err != nil {
			t.Fatal(err)
		}
		encs = append(encs, enc)
	}
	for i := 0; i < len(encs); i++ {
		for j := i + 1; j < len(encs); j++ {
			if bytes.HasPrefix(encs[i], encs[j]) || bytes.HasPrefix(encs[j], encs[i]) {
				t.Fatalf("prefix ambiguity between %q and %q:\n%x\n%x",
					orderedSamples[types.Text][i].S, orderedSamples[types.Text][j].S, encs[i], encs[j])
			}
		}
	}
}

// TestEncodeKeyRejectsNull 主键禁 NULL，编码层是最后一道闸。
func TestEncodeKeyRejectsNull(t *testing.T) {
	if _, err := EncodeKey(types.NullValue()); err == nil {
		t.Fatal("EncodeKey(NULL) must fail")
	}
}

// TestDecodeKeyGarbage 垃圾输入必须报错而不是瞎解。
func TestDecodeKeyGarbage(t *testing.T) {
	for _, garbage := range [][]byte{nil, {}, {0x09, 0x01}, {0x01, 0x01}, {0x03, 0x61}} {
		if _, err := DecodeKey(garbage); err == nil {
			t.Fatalf("DecodeKey(% x) must fail", garbage)
		}
	}
}

// TestRowKeyRoundtrip 行键拆装往返，且表内行按键序 == 主键值序。
func TestRowKeyRoundtrip(t *testing.T) {
	const tableID = 42
	vals := slices.Concat(orderedSamples[types.Int], orderedSamples[types.Text])

	var prev []byte
	for _, v := range vals {
		pk, err := EncodeKey(v)
		if err != nil {
			t.Fatal(err)
		}
		key := RowKey(tableID, pk)
		gotID, gotPK, err := SplitRowKey(key)
		if err != nil {
			t.Fatalf("split % x: %v", key, err)
		}
		if gotID != tableID || !bytes.Equal(gotPK, pk) {
			t.Fatalf("row key roundtrip mismatch for %v", v)
		}
		if prev != nil && bytes.Compare(prev, key) >= 0 {
			t.Fatalf("row key order broken at %v", v)
		}
		prev = key
	}

	// 不同表的键空间互不越界：同主键、不同表 ID 的键共享前缀但不同键。
	k1 := RowKey(1, mustEncodeKey(t, types.IntValue(7)))
	k2 := RowKey(2, mustEncodeKey(t, types.IntValue(7)))
	if bytes.HasPrefix(k2, RowKeyPrefix(1)) || bytes.HasPrefix(k1, RowKeyPrefix(2)) {
		t.Fatal("row keys of different tables share a prefix")
	}
}

// TestSplitRowKeyRejectsGarbage 非行键的东西必须被认出来。
func TestSplitRowKeyRejectsGarbage(t *testing.T) {
	for _, garbage := range [][]byte{
		nil, []byte("m\x00schema:users"), []byte("d\x00short"), RowKeyPrefix(7)[:9],
	} {
		if _, _, err := SplitRowKey(garbage); err == nil {
			t.Fatalf("SplitRowKey(% x) must fail", garbage)
		}
	}
}

func mustEncodeKey(t *testing.T, v types.Value) []byte {
	t.Helper()
	enc, err := EncodeKey(v)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}
