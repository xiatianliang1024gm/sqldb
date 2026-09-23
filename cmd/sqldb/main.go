// Command sqldb 是 sqldb 的交互式终端（REPL，DESIGN §11 的 M5 交付物）。
//
// 用法：
//
//	sqldb [数据库目录]    // 目录不存在则创建，默认 ./sqldata
//
// 输入规则：
//   - SQL 语句以 ';' 结尾，可以跨多行；字符串字面量里的 ';' 不会截断语句；
//   - '.' 开头的行是元命令：.help / .tables / .schema / .quit；
//   - 标准输入不是终端时同样逐行执行 —— 用管道喂脚本即可复现一份会话记录
//     （README 里的会话就是这么录的）。
//
// 一次一条语句的模型与 sqldb.Exec 一致：这里只做行拼接和展示，
// 不缓存、不批处理、不重试 —— REPL 不该有引擎没有的语义。
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/xiatianliang1024gm/sqldb"
)

const version = "0.1.0"

const banner = `sqldb %s —— LSM-Tree KV 之上的 SQL 引擎（学习项目）
数据库目录: %s
输入 SQL（以 ; 结尾），.help 查看命令，.quit 退出。
`

const helpText = `元命令:
  .tables            列出所有表
  .schema [表名]     查看建表语句（无参数 = 全部表）
  .help              显示本帮助
  .quit / .exit      退出

SQL 语句以 ';' 结尾，可跨多行；字符串字面量里的 ';' 不截断语句。
`

func usage() {
	fmt.Println("用法: sqldb [数据库目录]")
}

func main() {
	dir := "sqldata"
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			usage()
			return
		case "-v", "--version":
			fmt.Println("sqldb", version)
			return
		default:
			dir = os.Args[1]
		}
	}

	db, err := sqldb.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Printf(banner, version, dir)
	run(db, os.Stdin, os.Stdout)
}

// run 是 REPL 主循环：按行读入，拼到语句完整（statementComplete）为止，
// 交给 execSQL 执行并展示。错误只报告不退出 —— REPL 的基本操守。
func run(db *sqldb.DB, in io.Reader, out io.Writer) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 容忍超长语句

	var buf strings.Builder
	for {
		if buf.Len() == 0 {
			fmt.Fprint(out, "sqldb> ")
		} else {
			fmt.Fprint(out, "  ...> ")
		}
		if !sc.Scan() {
			if buf.Len() > 0 {
				fmt.Fprintln(out, "error: 语句不完整（缺 ';'）")
			}
			fmt.Fprintln(out, "bye")
			return
		}
		line := sc.Text()

		// 元命令只在语句缓冲为空时生效 —— 拼了一半的语句不允许被打断。
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 && strings.HasPrefix(trimmed, ".") {
			if dotCommand(db, trimmed, out) {
				return
			}
			continue
		}
		if buf.Len() == 0 && trimmed == "" {
			continue
		}

		buf.WriteString(line)
		buf.WriteByte('\n')
		if statementComplete(buf.String()) {
			execSQL(db, buf.String(), out)
			buf.Reset()
		}
	}
}

// execSQL 执行一条完整语句并按类型展示：SELECT 出表格，写语句报
// 受影响行数，DDL 报 ok。语句类型看首单词而不是 Result 字段 ——
// Result 无法区分"DDL 成功"和"DML 影响 0 行"，首单词是最便宜且
// 足够可靠的判据（parser 保证语句以关键字开头）。
func execSQL(db *sqldb.DB, sql string, out io.Writer) {
	stmt := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(sql), ";"))
	res, err := db.Exec(stmt)
	if err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
		return
	}
	switch firstWord(stmt) {
	case "SELECT":
		fmt.Fprint(out, formatTable(res.Columns, res.Rows))
		n := len(res.Rows)
		unit := "rows"
		if n == 1 {
			unit = "row"
		}
		fmt.Fprintf(out, "(%d %s)\n", n, unit)
	case "INSERT", "UPDATE", "DELETE":
		n := res.RowsAffected
		unit := "rows"
		if n == 1 {
			unit = "row"
		}
		fmt.Fprintf(out, "%d %s affected\n", n, unit)
	default:
		fmt.Fprintln(out, "ok")
	}
}

// dotCommand 处理元命令。返回 true 表示要求退出。
func dotCommand(db *sqldb.DB, cmd string, out io.Writer) bool {
	fields := strings.Fields(cmd)
	switch fields[0] {
	case ".quit", ".exit":
		fmt.Fprintln(out, "bye")
		return true
	case ".help":
		fmt.Fprint(out, helpText)
	case ".tables":
		names := db.Tables()
		if len(names) == 0 {
			fmt.Fprintln(out, "(no tables)")
			return false
		}
		for _, n := range names {
			fmt.Fprintln(out, n)
		}
	case ".schema":
		if len(fields) == 1 {
			for _, n := range db.Tables() {
				printSchema(db, n, out)
			}
			return false
		}
		printSchema(db, fields[1], out)
	default:
		fmt.Fprintf(out, "unknown command: %s（.help 查看可用命令）\n", fields[0])
	}
	return false
}

func printSchema(db *sqldb.DB, name string, out io.Writer) {
	t, err := db.TableSchema(name)
	if err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
		return
	}
	fmt.Fprint(out, renderCreateTable(t))
	// 索引跟着 schema 一起展示（M6）：引擎给定义，展示层负责反渲染。
	idxs, err := db.TableIndexes(name)
	if err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
		return
	}
	for _, d := range idxs {
		fmt.Fprint(out, renderCreateIndex(d))
	}
}
