package types

// 三值逻辑的基础用例。完整的三值逻辑矩阵测试按 DESIGN §11 的节奏放在 M4
// （与 HashAgg/JOIN 的 NULL 语义一起钉），这里只锁住三条传染规则的方向。

import "testing"

func TestLogicBasics(t *testing.T) {
	T, F, N := BoolValue(true), BoolValue(false), NullValue()

	cases := []struct {
		name string
		got  Value
		want Value
	}{
		{"false AND null = false", LogicAnd(F, N), F},
		{"null AND false = false", LogicAnd(N, F), F},
		{"true AND null = null", LogicAnd(T, N), N},
		{"null AND null = null", LogicAnd(N, N), N},
		{"true AND false = false", LogicAnd(T, F), F},
		{"true OR null = true", LogicOr(T, N), T},
		{"null OR true = true", LogicOr(N, T), T},
		{"false OR null = null", LogicOr(F, N), N},
		{"false OR false = false", LogicOr(F, F), F},
		{"NOT null = null", LogicNot(N), N},
		{"NOT true = false", LogicNot(T), F},
		{"NOT false = true", LogicNot(F), T},
	}
	for _, c := range cases {
		if c.got.Kind != c.want.Kind || (c.want.Kind == Bool && c.got.B != c.want.B) {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}
