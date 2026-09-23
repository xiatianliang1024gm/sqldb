package parser

// M6 索引 DDL 的解析单测（DESIGN §11）：CREATE/DROP INDEX 的全部形状
// 与错误定位。表达式层不受影响 —— INDEX/UNIQUE 进保留字不改变已有语句。

import "testing"

func TestParseCreateIndex(t *testing.T) {
	cases := []struct {
		sql     string
		unique  bool
		ifNotEx bool
		name    string
		table   string
		column  string
	}{
		{"CREATE INDEX ix ON users (age)", false, false, "ix", "users", "age"},
		{"create unique index ix on users (age)", true, false, "ix", "users", "age"},
		{"CREATE INDEX IF NOT EXISTS ix ON t (c)", false, true, "ix", "t", "c"},
		{"CREATE UNIQUE INDEX IF NOT EXISTS ix ON t (c)", true, true, "ix", "t", "c"},
		{"CREATE INDEX ix_users_age ON users(age);", false, false, "ix_users_age", "users", "age"},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			stmt, err := Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			ci, ok := stmt.(*CreateIndexStmt)
			if !ok {
				t.Fatalf("got %T, want *CreateIndexStmt", stmt)
			}
			if ci.Unique != tc.unique || ci.IfNotExists != tc.ifNotEx ||
				ci.Name != tc.name || ci.Table != tc.table || ci.Column != tc.column {
				t.Fatalf("got %+v", ci)
			}
		})
	}
}

func TestParseDropIndex(t *testing.T) {
	stmt, err := Parse("DROP INDEX ix")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	di, ok := stmt.(*DropIndexStmt)
	if !ok || di.Name != "ix" || di.IfExists {
		t.Fatalf("got %+v", di)
	}
	stmt, err = Parse("DROP INDEX IF EXISTS ix;")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	di = stmt.(*DropIndexStmt)
	if !di.IfExists || di.Name != "ix" {
		t.Fatalf("got %+v", di)
	}
}

func TestParseIndexErrors(t *testing.T) {
	cases := []string{
		"CREATE INDEX ON t (c)",          // 缺索引名
		"CREATE INDEX ix t (c)",          // 缺 ON
		"CREATE INDEX ix ON t (c",        // 缺右括号
		"CREATE INDEX ix ON t ()",        // 缺列名
		"CREATE INDEX ix ON t (c) extra", // 语句后多余 token
		"DROP INDEX",                     // 缺索引名
		"DROP TABLE ix ON t (c)",         // 不是索引语句（走 DROP TABLE 分支后报错）
	}
	for _, sql := range cases {
		if _, err := Parse(sql); err == nil {
			t.Errorf("Parse(%q) = nil error, want error", sql)
		}
	}
}

func TestIndexKeywordsAreReserved(t *testing.T) {
	// INDEX / UNIQUE 收编为关键字后，标识符位置不再接受它们（§6 约定的
	// 反面代价，与真实 SQL 一致）。
	for _, sql := range []string{
		"CREATE TABLE index (a INT)",
		"CREATE TABLE t (unique TEXT)",
	} {
		if _, err := Parse(sql); err == nil {
			t.Errorf("Parse(%q) = nil error, want error (reserved keyword)", sql)
		}
	}
	// 但 keyword 之外的既有语句不受影响。
	if _, err := Parse("CREATE TABLE t (idx INT, uniq TEXT)"); err != nil {
		t.Fatalf("parse: %v", err)
	}
}
