// render.go 是 REPL 的展示层：查询结果表格化、schema 反渲染成
// CREATE TABLE、终端显示宽度估算。引擎只给事实（Result / catalog.Table），
// 长什么样由这里决定 —— 所以 sqldb.TableSchema 返回结构体而不是文本。
package main

import (
	"fmt"
	"strings"

	"github.com/xiatianliang1024gm/sqldb/internal/catalog"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// displayWidth 估算 s 在等宽终端里占的列数：CJK 全角字符算 2，其余算 1。
// 只覆盖常用的 CJK 区块 —— 教学项目够用，不需要引 x/text 的完整宽度表。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
			r >= 0x2E80 && r <= 0x303E, // CJK 部首/符号
			r >= 0x3041 && r <= 0x33FF, // 平假名/片假名/兼容
			r >= 0x3400 && r <= 0x4DBF, // CJK 扩展 A
			r >= 0x4E00 && r <= 0x9FFF, // CJK 统一表意
			r >= 0xAC00 && r <= 0xD7A3, // 谚文音节
			r >= 0xF900 && r <= 0xFAFF, // CJK 兼容表意
			r >= 0xFE30 && r <= 0xFE4F, // CJK 兼容形式
			r >= 0xFF00 && r <= 0xFF60, // 全角 ASCII/标点
			r >= 0xFFE0 && r <= 0xFFE6, // 全角符号
			r >= 0x20000 && r <= 0x3FFFD:
			w += 2
		default:
			w++
		}
	}
	return w
}

// pad 把 s 补空格到显示宽度 w（中文列名/数据不会错位）。
func pad(s string, w int) string {
	gap := w - displayWidth(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}

// formatTable 把查询结果渲染成等宽表格。NULL 原样显示为 NULL ——
// 与 types.Value.String 的约定一致，空串和 NULL 在这里看得见区别。
func formatTable(cols []string, rows [][]types.Value) string {
	cells := make([][]string, len(rows))
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = displayWidth(c)
	}
	for r, row := range rows {
		cells[r] = make([]string, len(row))
		for i, v := range row {
			if i >= len(cols) {
				break // 行长不一致是引擎 bug，展示层不替它圆谎
			}
			cells[r][i] = v.String()
			if w := displayWidth(cells[r][i]); w > widths[i] {
				widths[i] = w
			}
		}
	}

	var b strings.Builder
	sep := border(widths, "┌", "┬", "┐")
	mid := border(widths, "├", "┼", "┤")
	end := border(widths, "└", "┴", "┘")

	b.WriteString(sep)
	b.WriteByte('\n')
	b.WriteString(datarow(cols, widths))
	b.WriteByte('\n')
	b.WriteString(mid)
	b.WriteByte('\n')
	for _, cell := range cells {
		b.WriteString(datarow(cell, widths))
		b.WriteByte('\n')
	}
	b.WriteString(end)
	b.WriteByte('\n')
	return b.String()
}

// datarow 渲染一行数据：│ a │ bb │ c │
func datarow(cells []string, widths []int) string {
	var b strings.Builder
	for i, c := range cells {
		b.WriteString("│ ")
		b.WriteString(pad(c, widths[i]))
		b.WriteString(" ")
	}
	b.WriteString("│")
	return b.String()
}

// border 渲染表格边框线：┌────┬────┐ 等。
func border(widths []int, left, mid, right string) string {
	var b strings.Builder
	b.WriteString(left)
	for i, w := range widths {
		if i > 0 {
			b.WriteString(mid)
		}
		b.WriteString(strings.Repeat("─", w+2)) // 两侧各留 1 空格
	}
	b.WriteString(right)
	return b.String()
}

// renderCreateTable 把 schema 反渲染成 CREATE TABLE 语句（.schema 命令）。
// 这不是 parser 的反向证明 —— 是给人看的近似：主键/NOT NULL 还原，
// 隐藏 rowid 用注释标出来，让"无主键表其实有隐藏主键"这件事可见。
func renderCreateTable(t *catalog.Table) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", t.Name)
	for i, col := range t.Columns {
		b.WriteString("    ")
		b.WriteString(col.Name)
		b.WriteByte(' ')
		b.WriteString(col.Kind.String())
		switch {
		case col.PrimaryKey:
			b.WriteString(" PRIMARY KEY")
		case col.NotNull:
			b.WriteString(" NOT NULL")
		}
		if i < len(t.Columns)-1 {
			b.WriteByte(',')
		}
		if t.HasRowid && col.PrimaryKey {
			b.WriteString("  -- 隐藏 rowid，系统分配（DESIGN §5）")
		}
		b.WriteByte('\n')
	}
	b.WriteString(");\n")
	return b.String()
}

// firstWord 返回语句的首个单词（大写），REPL 用它决定结果的展示方式。
// 跳过前导空白 —— execSQL 传进来的语句已去尾部分号，但不保证头部干净。
func firstWord(sql string) string {
	i := 0
	for i < len(sql) && isSpaceByte(sql[i]) {
		i++
	}
	start := i
	for i < len(sql) && !isSpaceByte(sql[i]) {
		i++
	}
	return strings.ToUpper(sql[start:i])
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// statementComplete 报告缓冲区是否构成一条完整语句：
// 去掉尾随空白后以 ';' 结束，且不在未闭合的字符串字面量里。
// 单引号成对计数即可 —— 解析器的 ” 转义是成对出现的，不影响奇偶性。
func statementComplete(sql string) bool {
	t := strings.TrimRight(sql, " \t\r\n")
	if !strings.HasSuffix(t, ";") {
		return false
	}
	inStr := false
	for _, r := range t {
		if r == '\'' {
			inStr = !inStr
		}
	}
	return !inStr
}
