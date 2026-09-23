# sqldb

在单机 LSM-Tree KV 引擎 [`kvdb`](../kvdb) 之上实现的嵌入式 SQL 引擎 ——
学习项目，目的是看清"SQL 层怎么长在 KV 存储上"，不是造可用数据库。

设计文档：[`docs/DESIGN.md`](docs/DESIGN.md)（键空间布局、保序编码、catalog、
Volcano 执行器、写路径、12 条关键取舍）。**里程碑 M0~M5 全部完成**，
`go test ./...` 全绿。

## 能做什么

| 能力 | 说明 |
|---|---|
| DDL | `CREATE TABLE` / `DROP TABLE`（含 IF [NOT] EXISTS） |
| DML | `INSERT`（多行、一个 WriteBatch 原子提交）、`UPDATE`、`DELETE` |
| 查询 | 投影、别名、`WHERE`、`ORDER BY`、`LIMIT/OFFSET`、`DISTINCT` |
| 聚合 | `COUNT/SUM/AVG/MIN/MAX` + `GROUP BY` + `HAVING` |
| 连接 | `INNER JOIN ... ON`（嵌套循环，左深树） |
| 表达式 | 比较、四则、`AND/OR/NOT`、`IS [NOT] NULL`、`IN`、`BETWEEN`、`LIKE` |
| 类型 | `INT / FLOAT / TEXT / BOOL` + `NULL`，三值逻辑，`NOT NULL`、主键约束 |
| 优化 | 主键范围下推：`WHERE pk <op> 字面量` 折算成 KV 扫描区间（可观测） |

不做：事务、外键/CHECK、二级索引、LEFT JOIN、子查询、视图 —— 边界见 DESIGN §1。

## 快速上手

```powershell
go run ./cmd/sqldb mydata          # 目录不存在则创建；默认 ./sqldata
go test ./...                      # 全量测试
```

REPL 输入规则：SQL 以 `;` 结尾（可跨多行）；`.` 开头是元命令
（`.tables` / `.schema [表名]` / `.help` / `.quit`）。

## REPL 会话记录

下面是一段真实会话（管道喂给 `sqldb` 二进制原样捕获，未手工编辑）——
覆盖建表、插入、多行 JOIN+聚合、NULL 传染（`bob` 的 age 加 1 仍是 NULL）、
点命令：

```text
sqldb 0.1.0 —— LSM-Tree KV 之上的 SQL 引擎（学习项目）
数据库目录: demo.db
输入 SQL（以 ; 结尾），.help 查看命令，.quit 退出。
sqldb> CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL, age INT);
ok
sqldb> CREATE TABLE orders (id INT PRIMARY KEY, uid INT, amount FLOAT);
ok
sqldb> INSERT INTO users VALUES (1, 'alice', 30), (2, 'bob', NULL), (3, '小夏', 25);
3 rows affected
sqldb> INSERT INTO orders VALUES (100, 1, 99.5), (101, 1, 42.0), (102, 3, 7.5), (103, 2, NULL);
4 rows affected
sqldb> SELECT * FROM users;
┌────┬───────┬──────┐
│ id │ name  │ age  │
├────┼───────┼──────┤
│ 1  │ alice │ 30   │
│ 2  │ bob   │ NULL │
│ 3  │ 小夏  │ 25   │
└────┴───────┴──────┘
(3 rows)
sqldb> SELECT name, age FROM users WHERE age >= 25 ORDER BY age DESC;
┌───────┬─────┐
│ name  │ age │
├───────┼─────┤
│ alice │ 30  │
│ 小夏  │ 25  │
└───────┴─────┘
(2 rows)
sqldb> SELECT u.name, COUNT(*) AS cnt, SUM(o.amount) AS total
  ...> FROM users u JOIN orders o ON u.id = o.uid
  ...> GROUP BY u.name
  ...> HAVING COUNT(*) >= 1
  ...> ORDER BY cnt DESC, u.name;
┌────────┬─────┬───────┐
│ u.name │ cnt │ total │
├────────┼─────┼───────┤
│ alice  │ 2   │ 141.5 │
│ bob    │ 1   │ NULL  │
│ 小夏   │ 1   │ 7.5   │
└────────┴─────┴───────┘
(3 rows)
sqldb> UPDATE users SET age = age + 1 WHERE name = 'bob';
1 row affected
sqldb> SELECT * FROM users WHERE id = 2;
┌────┬──────┬──────┐
│ id │ name │ age  │
├────┼──────┼──────┤
│ 2  │ bob  │ NULL │
└────┴──────┴──────┘
(1 row)
sqldb> SELECT COUNT(*) AS n, SUM(amount) AS total FROM orders;
┌───┬───────┐
│ n │ total │
├───┼───────┤
│ 4 │ 149.0 │
└───┴───────┘
(1 row)
sqldb> DELETE FROM orders WHERE amount < 10;
1 row affected
sqldb> .tables
orders
users
sqldb> .schema orders
CREATE TABLE orders (
    id INT PRIMARY KEY,
    uid INT,
    amount FLOAT
);
sqldb> bye
```

注意其中的语义细节（都有单测钉住）：

- `bob` 的 `age` 是 NULL，`SET age = age + 1` 后仍是 NULL —— NULL 传染；
- `SUM(o.amount)` 对 `bob` 组是 NULL（唯一订单金额为 NULL），不是 0；
- 无主键的表 `.schema` 会标出隐藏 `rowid` 主键（DESIGN §5）。

## 代码结构

```
sqldb.go              顶层 API：Open / Exec / Result（一个 Exec 通吃 DDL/DML/查询）
cmd/sqldb/            REPL（M5）：多行输入、元命令、结果表格化（CJK 对齐）
internal/parser/      词法 + 递归下降 → AST（不认识存储）
internal/exec/        AST → 计划树 → Volcano 拉取；主键范围下推；写路径
internal/catalog/     schema 的内存缓存 + KV meta 区持久化
internal/encoding/    保序主键编码 / TLV 行编码 / 行键布局
internal/types/       五种 Kind、三值逻辑、比较原语
docs/DESIGN.md        设计文档（键空间、保序编码、取舍表、里程碑）
```

对 kvdb 的依赖面只有五个原语：`Put / Delete / Write(batch)`、`Get`、
`NewIterator`、`GetSnapshot`（暂未使用）、`Open / Close`。
多绕过一个原语，就是设计错了层次（DESIGN §1）。

## 已知边界（如实记录）

- 无事务：单条语句靠 WriteBatch 原子，语句间无一致性快照；
- INSERT 主键查重与提交之间无隔离（先 Get 再写的代价，DESIGN §8）；
- 主键列不允许 UPDATE（改主键 = 删旧 + 插新，留给用户自己做）；
- Sort / HashAgg / DISTINCT 内存物化，无溢写。
