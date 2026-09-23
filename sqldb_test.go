package sqldb

// 顶层端到端单测：Exec 全链路（解析 → 绑定 → 计划 → Volcano 拉取 → Result）。
// M3 起写链路（INSERT/UPDATE/DELETE）也走 Exec 验收；mustPut 仍保留，
// 给个别需要手工摆行的旧测试用 —— SQL 层本来就架在一个普通 kvdb 上。

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiatianliang1024gm/sqldb/internal/encoding"
	"github.com/xiatianliang1024gm/sqldb/internal/parser"
	"github.com/xiatianliang1024gm/sqldb/internal/types"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// mustPut 直接按 M0 的编码约定写一行（M3 有了 INSERT 后这个助手可以退休）。
func mustPut(t *testing.T, db *DB, table string, vals ...types.Value) {
	t.Helper()
	tab, ok := db.cat.GetTable(table)
	if !ok {
		t.Fatalf("table %s missing", table)
	}
	pkIdx := tab.PrimaryKeyIndex()
	enc, err := encoding.EncodeKey(vals[pkIdx])
	if err != nil {
		t.Fatalf("encode pk: %v", err)
	}
	if err := db.kv.Put(encoding.RowKey(tab.ID, enc), encoding.EncodeRow(vals)); err != nil {
		t.Fatalf("put: %v", err)
	}
}

func TestExecDDLAndSelect(t *testing.T) {
	db := openTestDB(t)

	// DDL：建表（Result 应为空壳）
	res, err := db.Exec("CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL, score FLOAT)")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Columns != nil || res.Rows != nil || res.RowsAffected != 0 {
		t.Fatalf("DDL result should be empty: %+v", res)
	}

	// 重复建表报错；IF NOT EXISTS 静默成功
	if _, err := db.Exec("CREATE TABLE users (id INT PRIMARY KEY)"); err == nil {
		t.Fatal("duplicate create should fail")
	}
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS users (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("IF NOT EXISTS: %v", err)
	}

	// 灌数据（M3 之前的后门）
	mustPut(t, db, "users",
		types.IntValue(1), types.TextValue("ann"), types.FloatValue(3.5))
	mustPut(t, db, "users",
		types.IntValue(2), types.TextValue("bob"), types.FloatValue(9))
	mustPut(t, db, "users",
		types.IntValue(3), types.TextValue("cat"), types.FloatValue(1))

	// 查询：投影 + WHERE + 结果形状
	res, err = db.Exec("SELECT id, name AS n, score * 2 AS dbl FROM users WHERE id >= 2")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if want := []string{"id", "n", "dbl"}; len(res.Columns) != 3 ||
		res.Columns[0] != want[0] || res.Columns[1] != want[1] || res.Columns[2] != want[2] {
		t.Fatalf("columns: %v", res.Columns)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows: %d, want 2", len(res.Rows))
	}
	if res.Rows[0][1].S != "bob" || res.Rows[0][2].F != 18 || res.Rows[1][1].S != "cat" {
		t.Fatalf("row content: %v", res.Rows)
	}

	// 空结果：类型正确，不报错
	res, err = db.Exec("SELECT id FROM users WHERE id > 100")
	if err != nil || len(res.Rows) != 0 {
		t.Fatalf("empty select: %v %v", err, res.Rows)
	}
}

func TestExecReopenPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE kv (k TEXT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	mustPut(t, db, "kv", types.TextValue("alpha"), types.IntValue(1))
	mustPut(t, db, "kv", types.TextValue("beta"), types.IntValue(2))
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重开：schema 和数据都该原样回来（M0+M2 的拼装验收）
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	res, err := db2.Exec("SELECT k, v FROM kv WHERE k >= 'b'")
	if err != nil {
		t.Fatalf("select after reopen: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0].S != "beta" || res.Rows[0][1].I != 2 {
		t.Fatalf("rows after reopen: %v", res.Rows)
	}
}

func TestExecDropAndErrors(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec("CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := db.Exec("DROP TABLE t"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec("SELECT id FROM t"); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("select after drop: %v", err)
	}
	// IF EXISTS 静默成功；不带 IF 的重复 DROP 报错
	if _, err := db.Exec("DROP TABLE IF EXISTS t"); err != nil {
		t.Fatalf("IF EXISTS drop: %v", err)
	}
	if _, err := db.Exec("DROP TABLE t"); err == nil {
		t.Fatal("second drop should fail")
	}

	// M4 查询子句经顶层 Exec 跑通：排序 + 截断的端到端形状
	if _, err := db.Exec("CREATE TABLE m (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("create m: %v", err)
	}
	if _, err := db.Exec("INSERT INTO m (id) VALUES (1), (2), (3)"); err != nil {
		t.Fatalf("insert m: %v", err)
	}
	res, err := db.Exec("SELECT id FROM m ORDER BY id DESC LIMIT 2")
	if err != nil {
		t.Fatalf("order/limit select: %v", err)
	}
	if len(res.Rows) != 2 || res.Rows[0][0].I != 3 || res.Rows[1][0].I != 2 {
		t.Fatalf("order/limit rows: %v", res.Rows)
	}

	// 解析错误原样透出，且是带位置信息的 *parser.ParseError
	_, err = db.Exec("SELEC 1 FROM t")
	var perr *parser.ParseError
	if err == nil || !errors.As(err, &perr) {
		t.Fatalf("parse error should surface as *parser.ParseError: %v", err)
	}
}

// ── M3：写路径端到端（DESIGN §11 验收线）─────────────────────────

// INSERT → SELECT → UPDATE → DELETE 全链路 + 约束报错 + 受影响行数。
func TestExecWritePath(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec("CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL, score FLOAT)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// INSERT 多行：原子提交，RowsAffected = 3
	res, err := db.Exec("INSERT INTO users (id, name, score) VALUES (1, 'ann', 1.5), (2, 'bob', 9), (3, 'cat', 1)")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if res.Columns != nil || res.Rows != nil || res.RowsAffected != 3 {
		t.Fatalf("insert result: %+v", res)
	}

	// 主键冲突 / NOT NULL / 主键 UPDATE，全部报错
	for _, tc := range [][2]string{
		{"INSERT INTO users VALUES (1, 'dup', 0)", "duplicate primary key"},
		{"INSERT INTO users (id, name) VALUES (4, NULL)", "NOT NULL"},
		{"UPDATE users SET id = 9 WHERE id = 1", "primary key"},
	} {
		if _, err := db.Exec(tc[0]); err == nil || !strings.Contains(err.Error(), tc[1]) {
			t.Fatalf("%q: error %v, want %q", tc[0], err, tc[1])
		}
	}

	// UPDATE：受影响行数 + 内容确实变了
	res, err = db.Exec("UPDATE users SET score = score * 2 WHERE id > 1")
	if err != nil || res.RowsAffected != 2 {
		t.Fatalf("update: %v %+v", err, res)
	}
	res, err = db.Exec("SELECT id, name, score FROM users WHERE id = 2")
	if err != nil || res.Rows[0][2].F != 18 {
		t.Fatalf("select after update: %v %v", err, res.Rows)
	}

	// DELETE：按范围删
	res, err = db.Exec("DELETE FROM users WHERE id = 3")
	if err != nil || res.RowsAffected != 1 {
		t.Fatalf("delete: %v %+v", err, res)
	}
	res, err = db.Exec("SELECT id FROM users")
	if err != nil || len(res.Rows) != 2 {
		t.Fatalf("select after delete: %v %d rows", err, len(res.Rows))
	}

	// 失败的 INSERT 不落行：第 2 行冲突时第 1 行也不进
	if _, err := db.Exec("INSERT INTO users VALUES (7, 'a', 0), (1, 'b', 0)"); err == nil {
		t.Fatal("conflicting insert should fail")
	}
	res, err = db.Exec("SELECT id FROM users WHERE id = 7")
	if err != nil || len(res.Rows) != 0 {
		t.Fatalf("partial insert leaked: %v", res.Rows)
	}
}

// 重开不丢（M3 验收线）：写进去的行、UPDATE 过的值、rowid 计数器的游标
// 都要原样回来。rowid 表的计数器游标尤其关键 —— 回退会分配出重复主键。
func TestExecWriteReopenPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE kv (k TEXT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE logs (msg TEXT NOT NULL)"); err != nil { // rowid 表
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec("INSERT INTO kv VALUES ('alpha', 1), ('beta', 2)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Exec("UPDATE kv SET v = v + 10 WHERE k = 'alpha'"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := db.Exec("DELETE FROM kv WHERE k = 'beta'"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.Exec("INSERT INTO logs (msg) VALUES ('m0'), ('m1'), ('m2')"); err != nil {
		t.Fatalf("insert rowid table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	res, err := db2.Exec("SELECT k, v FROM kv WHERE k = 'alpha'")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][1].I != 11 {
		t.Fatalf("kv after reopen: %v %v", err, res.Rows)
	}
	// rowid 游标接续：重开后的下一行拿 3
	if _, err := db2.Exec("INSERT INTO logs (msg) VALUES ('m3')"); err != nil {
		t.Fatalf("insert after reopen: %v", err)
	}
	res, err = db2.Exec("SELECT rowid, msg FROM logs WHERE msg = 'm3'")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0].I != 3 {
		t.Fatalf("rowid cursor after reopen: %v %v", err, res.Rows)
	}
	// rowid 表已有行的内容不丢
	res, err = db2.Exec("SELECT msg FROM logs WHERE rowid = 1")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0].S != "m1" {
		t.Fatalf("rowid row after reopen: %v %v", err, res.Rows)
	}
}
