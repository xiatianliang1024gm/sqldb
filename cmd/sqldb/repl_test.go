// render_test.go / repl_test.go 覆盖 M5 新增的两个面：
// 展示层（表格对齐、CREATE TABLE 反渲染、语句完整性判断）和
// REPL 主循环（多行拼接、元命令、错误续跑、管道端到端）。
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xiatianliang1024gm/sqldb"
	"github.com/xiatianliang1024gm/sqldb/internal/catalog"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"中文", 4},
		{"a中b文", 6}, // 混排：1+2+1+2
		{"name", 4},
		{"，", 2}, // 全角标点
		{"", 0},
	}
	for _, c := range cases {
		if got := displayWidth(c.in); got != c.want {
			t.Errorf("displayWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFormatTable(t *testing.T) {
	cols := []string{"id", "name", "age"}
	rows := [][]types.Value{
		{types.IntValue(1), types.TextValue("alice"), types.IntValue(30)},
		{types.IntValue(2), types.NullValue(), types.TextValue("中文昵称")},
	}
	got := formatTable(cols, rows)
	// name 列最宽是 "alice"(5)，age 列最宽是 "中文昵称"(显示宽 8) ——
	// CJK 按 2 列计，表格才能对齐。
	want := strings.Join([]string{
		"┌────┬───────┬──────────┐",
		"│ id │ name  │ age      │",
		"├────┼───────┼──────────┤",
		"│ 1  │ alice │ 30       │",
		"│ 2  │ NULL  │ 中文昵称 │",
		"└────┴───────┴──────────┘",
		"",
	}, "\n")
	if got != want {
		t.Errorf("formatTable mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTableEmpty(t *testing.T) {
	got := formatTable([]string{"x"}, nil)
	want := strings.Join([]string{
		"┌───┐",
		"│ x │",
		"├───┤",
		"└───┘",
		"",
	}, "\n")
	if got != want {
		t.Errorf("empty result:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderCreateTable(t *testing.T) {
	withPK := &catalog.Table{Name: "users", Columns: []catalog.Column{
		{Name: "id", Kind: types.Int, PrimaryKey: true},
		{Name: "name", Kind: types.Text, NotNull: true},
		{Name: "age", Kind: types.Int},
	}}
	got := renderCreateTable(withPK)
	for _, want := range []string{"CREATE TABLE users (", "id INT PRIMARY KEY", "name TEXT NOT NULL", "age INT", ");"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderCreateTable missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "隐藏") {
		t.Errorf("explicit-PK table should not mention hidden rowid:\n%s", got)
	}

	rowid := &catalog.Table{Name: "logs", HasRowid: true, Columns: []catalog.Column{
		{Name: "msg", Kind: types.Text},
		{Name: "rowid", Kind: types.Int, NotNull: true, PrimaryKey: true},
	}}
	got = renderCreateTable(rowid)
	if !strings.Contains(got, "rowid INT PRIMARY KEY  -- 隐藏 rowid") {
		t.Errorf("hidden rowid not annotated:\n%s", got)
	}
}

func TestStatementComplete(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"SELECT 1;", true},
		{"SELECT 1", false},
		{"  SELECT 1;  \n\t", true},
		{"INSERT INTO t VALUES ('a;');", true},    // 字符串里的分号不截断判定 —— 语句仍完整
		{"INSERT INTO t VALUES ('a;')", false},    // 没分号
		{"INSERT INTO t VALUES ('a", false},       // 未闭合字符串 + 分号在串里
		{"INSERT INTO t VALUES ('it''s');", true}, // '' 转义不影响奇偶
		{"", false},
	}
	for _, c := range cases {
		if got := statementComplete(c.in); got != c.want {
			t.Errorf("statementComplete(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestFirstWord(t *testing.T) {
	for sql, want := range map[string]string{
		"SELECT * FROM t": "SELECT",
		"select 1":        "SELECT",
		"  insert into t": "INSERT",
		"create table x":  "CREATE",
		"drop\n table x":  "DROP",
	} {
		if got := firstWord(sql); got != want {
			t.Errorf("firstWord(%q) = %q, want %q", sql, got, want)
		}
	}
}

// newTestDB 起一个临时目录的库，建好两张演示表。
func newTestDB(t *testing.T) *sqldb.DB {
	t.Helper()
	db, err := sqldb.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, sql := range []string{
		"CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL)",
		"INSERT INTO users VALUES (1, 'alice'), (2, '小夏')",
	} {
		if _, err := db.Exec(sql); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	return db
}

// TestRunPipeline 端到端：把脚本当管道喂给 REPL，检查展示输出。
func TestRunPipeline(t *testing.T) {
	db := newTestDB(t)
	var out bytes.Buffer
	script := strings.Join([]string{
		".tables",
		"SELECT name FROM users",
		"WHERE id > 1;",
		".schema users",
		"UPDATE users SET name = 'x' WHERE id = 99;", // 影响 0 行也要报对
		"SELECT * FROM nosuch;",
		"DELETE FROM users WHERE id = 2;",
		".quit",
		"SELECT 1;", // .quit 之后不再执行
	}, "\n")
	run(db, strings.NewReader(script), &out)
	got := out.String()

	checks := []string{
		"sqldb> ",                      // 提示符
		"  ...> ",                      // 跨行续行提示
		"users",                        // .tables
		"小夏",                           // 跨行查询命中的数据（id > 1 只有 2 号）
		"CREATE TABLE users",           // .schema
		"0 rows affected",              // UPDATE 命中 0 行
		"error: no such table: nosuch", // 错误续跑不退出
		"1 row affected",               // DELETE（单数形式）
		"bye",                          // .quit 收尾
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("REPL output missing %q\n--- output ---\n%s", want, got)
		}
	}
	if i := strings.Index(got, "bye"); i >= 0 && strings.Contains(got[i:], "sqldb>") {
		t.Errorf("statements after .quit should not run:\n%s", got[i:])
	}
	// 跨行语句（SELECT ... WHERE 拆两行）只执行了一次且出表。
	if !strings.Contains(got, "(1 row)") {
		t.Errorf("multi-line SELECT should return 1 row:\n%s", got)
	}
}

func TestRunIncompleteAtEOF(t *testing.T) {
	db := newTestDB(t)
	var out bytes.Buffer
	run(db, strings.NewReader("SELECT 1"), &out) // 没有 ';' 就 EOF
	if !strings.Contains(out.String(), "error: 语句不完整") {
		t.Errorf("missing incomplete-statement error:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "bye") {
		t.Errorf("EOF should end with bye:\n%s", out.String())
	}
}
