// Package types 定义 SQL 层的值类型与比较规则。
//
// 它是解析器、编码器、执行器三方的共同语言：解析器把字面量变成 Value，
// 编码器把 Value 变成字节，执行器用 Compare 比大小。类型系统刻意做小：
// 只有 NULL / INT / FLOAT / TEXT / BOOL 五种，够表达 SQL 核心语义，
// 又不会让"类型转换矩阵"喧宾夺主。
package types

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kind 是值（也是列）的种类。
type Kind uint8

const (
	Null  Kind = iota // 只作为值的种类出现；列类型里不会声明 NULL
	Int               // SQL 里的 INT / INTEGER
	Float             // SQL 里的 FLOAT / DOUBLE
	Text              // SQL 里的 TEXT / VARCHAR(n)
	Bool              // SQL 里的 BOOL / BOOLEAN
)

func (k Kind) String() string {
	switch k {
	case Null:
		return "NULL"
	case Int:
		return "INT"
	case Float:
		return "FLOAT"
	case Text:
		return "TEXT"
	case Bool:
		return "BOOL"
	}
	return "UNKNOWN"
}

// ParseKind 把 SQL 类型名解析成 Kind，不认识就报错。
func ParseKind(name string) (Kind, error) {
	switch strings.ToUpper(name) {
	case "INT", "INTEGER", "BIGINT":
		return Int, nil
	case "FLOAT", "DOUBLE", "REAL":
		return Float, nil
	case "TEXT", "VARCHAR", "STRING":
		return Text, nil
	case "BOOL", "BOOLEAN":
		return Bool, nil
	}
	return Null, fmt.Errorf("unknown type %q", name)
}

// Value 是一个运行时 SQL 值。五种 Kind 各用自己的字段，
// 无效字段必须为对应 Kind 的零值 —— Compare 和编码器都依赖这一点。
type Value struct {
	Kind Kind
	I    int64
	F    float64
	S    string
	B    bool
}

func NullValue() Value { return Value{Kind: Null} }
func IntValue(v int64) Value {
	return Value{Kind: Int, I: v}
}
func FloatValue(v float64) Value { return Value{Kind: Float, F: v} }
func TextValue(v string) Value   { return Value{Kind: Text, S: v} }
func BoolValue(v bool) Value     { return Value{Kind: Bool, B: v} }

// IsNull 报告值是否为 SQL NULL。
func (v Value) IsNull() bool { return v.Kind == Null }

// String 返回给人看的表示（不是编码格式！）。
func (v Value) String() string {
	switch v.Kind {
	case Null:
		return "NULL"
	case Int:
		return strconv.FormatInt(v.I, 10)
	case Float:
		if v.F == math.Trunc(v.F) && math.Abs(v.F) < 1e15 {
			return strconv.FormatFloat(v.F, 'f', 1, 64)
		}
		return strconv.FormatFloat(v.F, 'g', -1, 64)
	case Text:
		return v.S
	case Bool:
		if v.B {
			return "true"
		}
		return "false"
	}
	return "?"
}

// Compare 比较两个非 NULL 值，语义与 SQL 一致：
// 整数和浮点混比时升格为浮点（可能损失精度，工程上可接受的教学取舍）；
// 不同族（数 vs 文本 vs 布尔）比较返回错误而不是硬比。
// 调用方负责先用 IsNull 处理 NULL —— SQL 里 NULL 和任何值比较结果都是 UNKNOWN。
func Compare(a, b Value) (int, error) {
	if a.Kind == Null || b.Kind == Null {
		return 0, fmt.Errorf("internal error: Compare on NULL")
	}
	switch a.Kind {
	case Int, Float:
		if b.Kind != Int && b.Kind != Float {
			return 0, fmt.Errorf("cannot compare %s with %s", a.Kind, b.Kind)
		}
		af, bf := a.toFloat(), b.toFloat()
		switch {
		case af < bf:
			return -1, nil
		case af > bf:
			return 1, nil
		}
		return 0, nil
	case Text:
		if b.Kind != Text {
			return 0, fmt.Errorf("cannot compare %s with %s", a.Kind, b.Kind)
		}
		return strings.Compare(a.S, b.S), nil
	case Bool:
		if b.Kind != Bool {
			return 0, fmt.Errorf("cannot compare %s with %s", a.Kind, b.Kind)
		}
		bi, bj := boolInt(a.B), boolInt(b.B)
		switch {
		case bi < bj:
			return -1, nil
		case bi > bj:
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("cannot compare kind %d", a.Kind)
}

func (v Value) toFloat() float64 {
	if v.Kind == Int {
		return float64(v.I)
	}
	return v.F
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
