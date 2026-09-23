// Package sqldb 是在 kvdb 之上的嵌入式 SQL 引擎顶层入口（DESIGN §9）。
//
// 一个 Exec 通吃 DDL / DML / 查询：Result 里按语句类型填充不同字段，
// REPL 好用优先。数据库就是一个 kvdb 目录 —— Open 它，schema 与数据
// 全部回来，没有伴随文件（DESIGN §5）。
//
// 里程碑进度（DESIGN §11）：M0~M5 全部落地 —— 完整 DDL/DML/SELECT
// （含聚合、JOIN、主键范围下推）+ 交互式终端（cmd/sqldb）。
package sqldb

import (
	"errors"
	"fmt"

	"github.com/xiatianliang1024gm/kvdb"
	"github.com/xiatianliang1024gm/sqldb/internal/catalog"
	"github.com/xiatianliang1024gm/sqldb/internal/exec"
	"github.com/xiatianliang1024gm/sqldb/internal/parser"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

// Result 是一条语句的执行结果。查询填 Columns/Rows，写语句填 RowsAffected，
// DDL 两者皆空 —— 字段按语句类型各自为政，调用方按需读取。
type Result struct {
	Columns      []string        // SELECT 的输出列名（含别名）
	Rows         [][]types.Value // SELECT 的结果行（列顺序与 Columns 一致）
	RowsAffected int64           // INSERT/UPDATE/DELETE 受影响行数（M3 起使用）
}

// DB 是一个已打开的 SQL 数据库。它只是两个现成部件的组装：
// kvdb 提供有序 KV 原语，catalog 提供表 schema —— SQL 语义全部长在它们之上。
type DB struct {
	kv  *kvdb.DB
	cat *catalog.Catalog
}

// Open 打开（或创建）一个数据库目录。
func Open(dir string) (*DB, error) {
	kv, err := kvdb.Open(kvdb.Options{Dir: dir})
	if err != nil {
		return nil, fmt.Errorf("sqldb: open %s: %w", dir, err)
	}
	cat, err := catalog.Load(kv)
	if err != nil {
		kv.Close()
		return nil, fmt.Errorf("sqldb: load catalog from %s: %w", dir, err)
	}
	return &DB{kv: kv, cat: cat}, nil
}

// Close 关闭数据库。
func (db *DB) Close() error { return db.kv.Close() }

// Tables 返回全部表名，按名字排序（REPL 的 .tables 命令用）。
func (db *DB) Tables() []string {
	ts := db.cat.Tables()
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Name)
	}
	return names
}

// TableSchema 返回一张表的 schema 定义，表不存在报错。
// 返回 catalog 的结构体而不是渲染好的 SQL 文本 —— 展示层长什么样
// 是调用方（REPL 的 .schema）的自由，引擎只负责事实。
func (db *DB) TableSchema(name string) (*catalog.Table, error) {
	t, ok := db.cat.GetTable(name)
	if !ok {
		return nil, fmt.Errorf("no such table: %s", name)
	}
	return t, nil
}

// TableIndexes 返回一张表的全部二级索引定义（M6，REPL 的 .schema 用）。
// 表不存在报错；没有索引返回空切片。
func (db *DB) TableIndexes(name string) ([]*catalog.IndexDef, error) {
	t, ok := db.cat.GetTable(name)
	if !ok {
		return nil, fmt.Errorf("no such table: %s", name)
	}
	return db.cat.IndexesOf(t), nil
}

// Exec 解析并执行一条 SQL 语句。一次一条 —— 多语句由调用方拆分
// （解析器遇到多余 token 会报错，不会静默吞掉后半句）。
func (db *DB) Exec(sql string) (*Result, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	switch s := stmt.(type) {
	case *parser.CreateTableStmt:
		return db.execCreateTable(s)
	case *parser.DropTableStmt:
		return db.execDropTable(s)
	case *parser.CreateIndexStmt:
		if err := exec.ExecCreateIndex(db.kv, db.cat, s); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *parser.DropIndexStmt:
		if err := exec.ExecDropIndex(db.kv, db.cat, s); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *parser.SelectStmt:
		return db.execSelect(s)
	case *parser.InsertStmt:
		n, err := exec.ExecInsert(db.kv, db.cat, s)
		if err != nil {
			return nil, err
		}
		return &Result{RowsAffected: n}, nil
	case *parser.UpdateStmt:
		n, err := exec.ExecUpdate(db.kv, db.cat, s)
		if err != nil {
			return nil, err
		}
		return &Result{RowsAffected: n}, nil
	case *parser.DeleteStmt:
		n, err := exec.ExecDelete(db.kv, db.cat, s)
		if err != nil {
			return nil, err
		}
		return &Result{RowsAffected: n}, nil
	}
	return nil, fmt.Errorf("unsupported statement %T", stmt)
}

func (db *DB) execCreateTable(s *parser.CreateTableStmt) (*Result, error) {
	cols := make([]catalog.Column, len(s.Columns))
	for i, c := range s.Columns {
		cols[i] = catalog.Column{Name: c.Name, Kind: c.Type, NotNull: c.NotNull, PrimaryKey: c.PrimaryKey}
	}
	if _, err := db.cat.CreateTable(s.Table, cols); err != nil {
		if errors.Is(err, catalog.ErrTableExists) && s.IfNotExists {
			return &Result{}, nil // IF NOT EXISTS：表已在，按承诺静默成功
		}
		return nil, fmt.Errorf("create table %s: %w", s.Table, err)
	}
	return &Result{}, nil
}

func (db *DB) execDropTable(s *parser.DropTableStmt) (*Result, error) {
	if err := db.cat.DropTable(s.Table); err != nil {
		if errors.Is(err, catalog.ErrTableNotFound) && s.IfExists {
			return &Result{}, nil // IF EXISTS：表不在，按承诺静默成功
		}
		return nil, fmt.Errorf("drop table %s: %w", s.Table, err)
	}
	return &Result{}, nil
}

// execSelect 构建计划后用 Volcano 循环把结果整个拉出来。
// 物化进内存是 M2 的明确取舍（Sort/HashAgg 也是这么干的，DESIGN §7.4）；
// 流式结果集（游标语义）等真的需要时再改。
func (db *DB) execSelect(s *parser.SelectStmt) (*Result, error) {
	plan, err := exec.Build(db.kv, db.cat, s)
	if err != nil {
		return nil, err
	}
	defer plan.Root.Close()

	var rows [][]types.Value
	for {
		row, err := plan.Root.Next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			break
		}
		rows = append(rows, row)
	}
	return &Result{Columns: plan.Columns, Rows: rows}, nil
}
