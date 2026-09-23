package types

// 三值逻辑（DESIGN §4.4）：NULL 会传染，但 false 是"更强的传染源"。
//
//	AND: 有 false → false；否则有 NULL → NULL；否则 true
//	OR : 有 true  → true； 否则有 NULL → NULL；否则 false
//
// 这与 PostgreSQL 的三值逻辑一致，WHERE 子句里 NULL 一律视为不通过。
// NOT 则是平凡取反：NOT NULL = NULL。
//
// 只接受 Bool / NULL 两种输入 —— 比较结果和谓词是它们唯一的产地；
// 传进别的 Kind 说明上游类型检查失守，直接 panic 让它尽早暴露。

// LogicAnd 返回 a AND b 的三值逻辑结果。
func LogicAnd(a, b Value) Value {
	mustLogic(a, "AND")
	mustLogic(b, "AND")
	if a.Kind == Bool && !a.B {
		return BoolValue(false)
	}
	if b.Kind == Bool && !b.B {
		return BoolValue(false)
	}
	if a.IsNull() || b.IsNull() {
		return NullValue()
	}
	return BoolValue(true)
}

// LogicOr 返回 a OR b 的三值逻辑结果。
func LogicOr(a, b Value) Value {
	mustLogic(a, "OR")
	mustLogic(b, "OR")
	if a.Kind == Bool && a.B {
		return BoolValue(true)
	}
	if b.Kind == Bool && b.B {
		return BoolValue(true)
	}
	if a.IsNull() || b.IsNull() {
		return NullValue()
	}
	return BoolValue(false)
}

// LogicNot 返回 NOT a 的三值逻辑结果。
func LogicNot(a Value) Value {
	mustLogic(a, "NOT")
	if a.IsNull() {
		return NullValue()
	}
	return BoolValue(!a.B)
}

func mustLogic(v Value, op string) {
	if v.Kind != Bool && v.Kind != Null {
		panic("types: " + op + " on non-boolean value " + v.Kind.String())
	}
}
