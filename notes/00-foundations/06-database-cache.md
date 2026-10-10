# 0.6 数据库与缓存：Postgres 与 Redis，给 Agent 一个靠得住的记忆

> 第 0 部分 · 第 6 课 ｜ 预计 5–6 小时 ｜ 前置：0.1 Go、0.5 工程基础
>
> 配套练习：`projects/05-store/`（Go + `pgx/v5` + `go-redis/v9`，12 个集成测试）。CI 里用 GitHub Actions 的 `services` 起真实的 Postgres 17 和 Redis 7 来跑。

## 读前说明

前五课的服务都是“无状态”的：进程重启，什么都没了。真正的 Agent 平台至少要记住这些东西：

- **会话和消息**：用户关掉页面再打开，历史还在；流式输出断了，能从第几条接着读；
- **运行记录**：每次 Agent 运行的状态（排队、运行中、成功、失败）、用了多少 token、花了多少钱；
- **热数据和计数器**：会话标题这种读多写少的数据要快；每个用户每分钟最多调几次模型要准。

前两类放 **Postgres**，第三类放 **Redis**。这一课不讲成数据库教材，只讲做 Agent 后端绕不开的部分，而且每个知识点都对应练习里一段能跑、有测试的代码。

本课结束时你能：

1. 为会话、消息、运行记录设计表结构，知道每一列的类型和约束为什么这么选；
2. 根据查询设计索引，会读 `EXPLAIN ANALYZE`，知道为什么不用 `OFFSET` 分页；
3. 说清楚四种隔离级别各防住什么、防不住什么，亲手复现“丢失更新”和“写偏斜”；
4. 用 Postgres 实现幂等建任务、并发安全的序号、`SKIP LOCKED` 任务队列；
5. 写一个迁移执行器，知道线上改表结构哪些操作会锁表；
6. 用 Redis 做 Cache-Aside 缓存和限流，并处理好穿透、击穿、雪崩和 Redis 故障。

---

# 第一部分：Postgres 建模

## 1. 为什么默认选 Postgres

做 Agent 后端，存储的默认答案是 Postgres，原因很实际：

| 需求 | Postgres 怎么满足 |
|---|---|
| 会话、消息、用户、计费这些关系型数据 | 本职工作，有事务、约束、外键 |
| 工具调用参数、模型返回这类结构不固定的数据 | `JSONB` 列，能建索引、能查询 |
| 向量检索（第 05 模块 RAG） | `pgvector` 扩展，几百万向量以内不必另上向量库 |
| 后台任务队列 | `FOR UPDATE SKIP LOCKED`，本课第 15 节 |
| 全文检索 | 内置 `tsvector` |

一个库能覆盖大部分需求，就少运维一个系统。等某一项真的成了瓶颈（比如上亿向量），再拆出去。**先用一个可靠的系统做对，再根据测量结果拆分。**

## 2. Agent 平台的三张核心表

```
conversations  1 ──── N  messages        一个会话有很多条消息，按 seq 排序
      │
      └────── 1 ──── N  runs             一个会话可以运行很多次 Agent
```

练习里的 `migrations/0001_init.sql`，先看会话和消息：

```sql
CREATE TABLE conversations (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id     TEXT        NOT NULL,
    title       TEXT        NOT NULL DEFAULT '',
    last_seq    INT         NOT NULL DEFAULT 0,          -- 已分配的最大消息序号
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE messages (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    conversation_id  BIGINT      NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    seq              INT         NOT NULL,
    role             TEXT        NOT NULL CHECK (role IN ('system', 'user', 'assistant', 'tool')),
    content          JSONB       NOT NULL,
    tokens           INT         NOT NULL DEFAULT 0 CHECK (tokens >= 0),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, seq)
);
```

**逐行解读**

- **`BIGINT GENERATED ALWAYS AS IDENTITY`**：自增主键的标准写法（取代老的 `SERIAL`）。用 `BIGINT` 而不是 `INT`：`INT` 上限约 21 亿，消息表一天几百万行，几年就能用完，到时候改类型要重写整张表。
- **`user_id TEXT`**：用户体系可能来自外部（OAuth 的 subject），用文本最省事。
- **`last_seq`**：这个会话已经分配到第几号消息。看起来冗余（`max(seq)` 也能算），它的作用在第 13 节讲，是“并发追加消息不重号”的关键。
- **`TIMESTAMPTZ`**：永远用带时区的时间。`TIMESTAMP`（不带时区）存进去的是“墙上时间”，服务器和客户端时区一不一致就会错 8 小时。
- **`REFERENCES ... ON DELETE CASCADE`**：外键。消息不能指向不存在的会话；删会话时消息一起删。
- **`CHECK (role IN (...))`**：数据库层面保证 role 只能是这四个值。应用代码有 bug 写了个 `"assitant"`，会在写入时直接报错，而不是半年后统计时才发现。
- **`content JSONB`**：一条消息可能是纯文本，也可能是工具调用（函数名 + 参数）、工具结果、图片引用。结构各不相同，用 JSONB 存。
- **`UNIQUE (conversation_id, seq)`**：同一会话里序号不能重复。唯一约束会自动建一个索引，这个索引同时服务于“按会话、按顺序读消息”这个最常见的查询，一举两得。

### 主键用自增还是 UUID

| | 自增 `BIGINT` | `UUID` |
|---|---|---|
| 大小 | 8 字节 | 16 字节 |
| 插入性能 | 总是插在索引末尾，最好 | 随机 UUIDv4 插在索引随机位置，表大了明显变慢 |
| 暴露信息 | 能猜出总量、能遍历 | 不能 |
| 客户端生成 | 不行 | 可以，离线创建、合并数据方便 |

折中做法：内部用 `BIGINT`，对外暴露的 ID 用 **UUIDv7**（前缀是时间戳，所以基本有序，插入性能接近自增）。练习为了简单全用 `BIGINT`。

### JSONB 还是拆成列

经验规则：**要用来过滤、排序、关联、求和的字段，拆成独立的列；只需要整体存取的，放 JSONB。** 比如 `tokens` 要按会话求和算钱，所以是独立列；消息正文只会整体读出来发给模型，所以放 `content`。

## 3. 约束是最后一道防线

应用代码里也可以校验，为什么还要在数据库里写 `NOT NULL`、`CHECK`、`UNIQUE`、外键？

因为写数据库的不只是你这一个函数：还有后台任务、数据修复脚本、三个月后的同事、并发执行的两个请求。**应用层校验防的是“正常情况下的错误”，数据库约束防的是“所有情况下的错误”**，尤其是并发：两个请求同时检查“这个幂等键用过没有”，都发现没用过，都去插入。只有 `UNIQUE` 约束能保证最终只有一行。

本课后面的幂等键（第 14 节）、消息序号（第 13 节）都依赖唯一约束兜底。

---

# 第二部分：索引

## 4. 索引的心智模型

没有索引，`WHERE user_id = 'u1'` 就得把整张表从头读到尾（**顺序扫描**，Seq Scan）。

Postgres 默认的 **B-tree 索引**可以想象成一本按某列排好序的目录：每一项是“列的值 → 这一行在表里的位置”。查找是二分，100 万行大约 20 次比较；而且因为有序，**范围查询和排序也能直接用它**。

代价：每次 `INSERT` / `UPDATE` 被索引的列，都要同时维护索引。一张表上 10 个索引，写入就要多做 10 次。所以**索引是为具体查询建的，不是给每一列都建一个**。

## 5. 复合索引：列的顺序决定能不能用上

会话列表页的查询是：“某个用户的会话，最近活跃的排前面”。

```sql
SELECT ... FROM conversations
WHERE user_id = $1
ORDER BY updated_at DESC, id DESC
LIMIT 20;
```

对应的索引：

```sql
CREATE INDEX conversations_user_recent_idx
    ON conversations (user_id, updated_at DESC, id DESC);
```

复合索引按第一列排序，第一列相同再按第二列，以此类推，就像字典先按第一个字母、再按第二个字母排。所以：

- `WHERE user_id = ?` 能用：先定位到这个用户那一段；
- `WHERE user_id = ? ORDER BY updated_at DESC` 能用，而且**不用排序**：这一段里本来就按 `updated_at` 倒序排好了，读前 20 条就停；
- `WHERE updated_at > ?`（不带 user_id）基本用不上：就像只知道第二个字母，没法在字典里查。

这叫**最左前缀**原则。设计复合索引的顺序：**等值条件的列在前，范围条件或排序的列在后。**

末尾的 `id DESC` 是为了让排序**唯一**：两个会话 `updated_at` 完全相同时，顺序由 id 决定。没有它，分页时同一行可能在两页都出现或者都不出现（第 7 节）。

## 6. 读懂 EXPLAIN ANALYZE

判断一个查询有没有用上索引，不靠猜，靠 `EXPLAIN ANALYZE`。它会真的执行一次查询，并告诉你每一步做了什么、花了多久。

```
Limit  (cost=0.42..2.31 rows=20 width=60) (actual time=0.031..0.049 rows=20 loops=1)
  ->  Index Scan using conversations_user_recent_idx on conversations
        (cost=0.42..946.12 rows=10000 width=60) (actual time=0.030..0.045 rows=20 loops=1)
        Index Cond: (user_id = 'u1'::text)
Planning Time: 0.120 ms
Execution Time: 0.068 ms
```

从下往上、从里往外读：

- **`Index Scan using conversations_user_recent_idx`**：用上了我们的索引；
- **`Index Cond`**：索引用来定位的条件；
- **`actual ... rows=20`**：实际只读了 20 行就停了，因为 `Limit` 够了；
- **`cost=...`**：优化器的估算，单位是相对值，不是毫秒；**`actual time`** 才是真实耗时。

常见的几种节点：

| 节点 | 含义 | 什么时候是问题 |
|---|---|---|
| `Seq Scan` | 全表扫描 | 大表上、只要少量行时 |
| `Index Scan` | 走索引，再回表取整行 | 一般是好事 |
| `Index Only Scan` | 索引里就有所需的全部列，不用回表 | 最好 |
| `Bitmap Heap Scan` | 先从索引收集一批位置，再批量读表 | 返回行数中等时的正常选择 |
| `Sort` | 在内存或磁盘上排序 | 出现 `external merge`（排序溢出到磁盘）就要警惕 |

> 小表上优化器选 `Seq Scan` 很正常：几百行的表，直接读一遍比走索引还快。验证索引要先灌足够多的数据，练习 1 会做这件事。

## 7. 分页：别用 OFFSET

最直观的分页写法是：

```sql
SELECT ... ORDER BY updated_at DESC, id DESC LIMIT 20 OFFSET 10000;
```

两个问题：

1. **越往后越慢**。`OFFSET 10000` 的意思是“读 10020 行，扔掉前 10000 行”，数据库没法跳过。
2. **结果会错**。用户翻页的间隙里有一个会话更新了、跑到了最前面，所有行往后挪一位，下一页就会把上一页最后一条再显示一次。

**键集分页**（keyset pagination，也叫游标分页）：记住上一页最后一行的排序键，下一页从它后面开始。

```go
// store.go · ListConversations（节选）
rows, err = s.pool.Query(ctx, cols+`
    WHERE user_id = $1 AND (updated_at, id) < ($2, $3)
    ORDER BY updated_at DESC, id DESC LIMIT $4`, userID, cursor.UpdatedAt, cursor.ID, limit+1)
// ...
// 多查一行用来判断“还有没有下一页”，省掉一次 COUNT(*)
if len(list) <= limit {
    return list, nil, nil
}
list = list[:limit]
last := list[limit-1]
return list, &Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}, nil
```

**逐行解读**

- **`(updated_at, id) < ($2, $3)`**：行比较，意思是“先比 `updated_at`，相同再比 `id`”，正好和 `ORDER BY` 的顺序一致，所以能直接用第 5 节的索引定位到起点，不管翻到第几页都只读 `limit+1` 行。
- **为什么要带上 `id`**：只用 `updated_at < $2` 的话，和上一页最后一行时间相同的其他行会被跳过。测试 `TestListConversationsCursor` 故意把所有会话的 `updated_at` 设成同一时刻，就是为了测这种情况。
- **`limit+1`**：多查一行。查到了说明还有下一页，把多的那行去掉；查不到说明到底了，`next` 返回 `nil`。
- **游标交给客户端**：实际接口里会把 `Cursor` 序列化成 JSON 再 base64，作为不透明的字符串返回给前端，前端下次原样带回来。

代价：键集分页不能直接“跳到第 50 页”。但聊天列表、消息流、日志这类场景本来就是“加载更多”，正好适用。

消息的续读更简单：`seq` 本身就是唯一有序的，`ListMessages(convID, afterSeq, limit)` 直接用 `seq > afterSeq`。这和 0.4 课 SSE 的 `Last-Event-ID` 断线续传是一个思路：**客户端告诉服务端“我收到第几条了”，服务端从下一条开始发。**

## 8. 部分索引和 JSONB 索引

**部分索引**只索引满足条件的行：

```sql
CREATE INDEX runs_queued_idx ON runs (created_at, id) WHERE status = 'queued';
```

`runs` 表会越来越大（每次运行一行，成功的永远留着），但任务队列只关心“排队中”的那一小部分。这个索引只包含 `queued` 的行，任务被领走后就从索引里消失，所以表再大，这个索引也只有“当前待处理数”那么大。查询的 `WHERE` 里必须带 `status = 'queued'`，优化器才会用它。

**JSONB 的 GIN 索引**，用来查 JSON 内部的字段：

```sql
CREATE INDEX messages_content_gin ON messages USING gin (content jsonb_path_ops);
-- 能加速：找出调用过 web_search 工具的消息
SELECT id FROM messages WHERE content @> '{"tool": "web_search"}';
```

练习里没有建这个索引，因为目前没有这个查询。**先有查询，再有索引。**

---

# 第三部分：事务与并发

## 9. 事务：要么全做，要么全不做

追加一条消息要做两件事：会话的 `last_seq` 加一、插入消息。如果第一步成功、第二步失败，就会留下一个永远空着的序号。事务保证这两步**要么都生效，要么都不生效**。

```go
// store.go · AppendMessage
err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
    var seq int
    err := tx.QueryRow(ctx,
        `UPDATE conversations SET last_seq = last_seq + 1, updated_at = now()
         WHERE id = $1 RETURNING last_seq`, convID).Scan(&seq)
    if errors.Is(err, pgx.ErrNoRows) {
        return ErrNotFound
    }
    if err != nil {
        return err
    }
    return tx.QueryRow(ctx,
        `INSERT INTO messages (conversation_id, seq, role, content, tokens)
         VALUES ($1, $2, $3, $4, $5)
         RETURNING id, conversation_id, seq, role, content, tokens, created_at`,
        convID, seq, role, content, tokens).
        Scan(&m.ID, &m.ConversationID, &m.Seq, &m.Role, &m.Content, &m.Tokens, &m.CreatedAt)
})
```

**逐行解读**

- **`pgx.BeginFunc`**：开事务，执行函数，函数返回 `nil` 就提交，返回错误或 panic 就回滚。比手写 `Begin` / `Commit` / `Rollback` 不容易漏。
- **函数里必须用 `tx` 而不是 `s.pool`**：用 `s.pool` 执行的语句会拿另一个连接，不在这个事务里。这是新手最常见的事务 bug。
- **`RETURNING last_seq`**：`UPDATE` 完直接把新值带回来，一次往返，不用再 `SELECT`。
- **`pgx.ErrNoRows`**：会话不存在，`UPDATE` 影响 0 行，`RETURNING` 没有结果。转换成我们自己的 `ErrNotFound`，上层（HTTP handler）不需要知道底下用的是 pgx。

ACID 四个字母里，这里用到的是 **A（原子性）**。**I（隔离性）** 才是并发问题的来源，接下来重点讲。

## 10. 隔离级别：并发事务能看到彼此多少

两个事务同时运行时，一个能不能看到另一个还没提交、或者刚提交的修改？SQL 标准定义了四个级别，Postgres 实际实现了三个：

| 级别 | 脏读 | 不可重复读 | 幻读 | 写偏斜 | Postgres 里 |
|---|---|---|---|---|---|
| Read Uncommitted | 可能 | 可能 | 可能 | 可能 | 当作 Read Committed |
| **Read Committed** | 不会 | 可能 | 可能 | 可能 | **默认** |
| Repeatable Read | 不会 | 不会 | 不会 | 可能 | 快照隔离 |
| Serializable | 不会 | 不会 | 不会 | 不会 | 检测到冲突时报错，需重试 |

术语翻译成人话：

- **脏读**：读到了别人还没提交的数据（别人后来回滚了，你读到的就是不存在的数据）。Postgres 任何级别都不会出现。
- **不可重复读**：同一个事务里读两次同一行，结果不一样，因为中间有别人提交了修改。
- **幻读**：同一个事务里执行两次同一个范围查询，第二次多出了别人新插入的行。
- **写偏斜**（write skew）：两个事务各自读了一些数据、做出判断，然后各自写**不同的行**；单看每一个都没问题，合在一起违反了规则。第 12 节有实例。

**Read Committed** 的规则：**每条语句**开始时拍一个快照，只能看到在这之前已提交的数据。所以同一事务里的两条 `SELECT` 可能看到不同结果。

**Repeatable Read** 的规则：**整个事务**用第一条语句时的快照。如果你要修改的行在你的快照之后被别人改过，Postgres 直接报错 `could not serialize access due to concurrent update`，让你重试。

### 动手：两个 psql 窗口

打开两个终端，都执行 `make psql`（会话 A 和会话 B），先准备一行数据：

```sql
INSERT INTO conversations (user_id, title) VALUES ('u1', '原标题');
```

**实验 1：Read Committed 的不可重复读**

```sql
-- A
BEGIN;
SELECT title FROM conversations WHERE id = 1;        -- 原标题

-- B
UPDATE conversations SET title = '新标题' WHERE id = 1;  -- 自动提交

-- A
SELECT title FROM conversations WHERE id = 1;        -- 新标题：同一事务里读到了不同结果
COMMIT;
```

**实验 2：Repeatable Read 的快照**

```sql
-- A
BEGIN ISOLATION LEVEL REPEATABLE READ;
SELECT title FROM conversations WHERE id = 1;        -- 新标题

-- B
UPDATE conversations SET title = '第三个标题' WHERE id = 1;

-- A
SELECT title FROM conversations WHERE id = 1;        -- 还是“新标题”：一直读同一个快照
UPDATE conversations SET title = 'A 改的' WHERE id = 1;
-- ERROR: could not serialize access due to concurrent update
ROLLBACK;
```

第二个实验最后那一步很重要：Repeatable Read 不会让 A 在旧快照上覆盖 B 的修改，而是报错。**报错不是 bug，是数据库在告诉你“你基于的数据已经过时了，请重试”。**

## 11. 丢失更新：最常见的并发 bug

计费场景：模型每返回一段，就把 token 数累加到运行记录上。直觉写法是“读出来，加上，写回去”：

```go
// store_test.go · addUsageNaive（反例，只在测试里）
var cur int64
s.pool.QueryRow(ctx, `SELECT input_tokens FROM runs WHERE id = $1`, runID).Scan(&cur)
time.Sleep(time.Millisecond) // 放大读和写之间的窗口，让问题稳定复现
s.pool.Exec(ctx, `UPDATE runs SET input_tokens = $2 WHERE id = $1`, runID, cur+in)
```

两个请求同时执行：都读到 100，都算出 110，都写入 110。本该是 120，丢了一次。`TestLostUpdate` 并发 40 次，每次加 10，实际跑出来的结果是：

```
读-改-写: 期望 400, 实际 20（丢了 380）
```

这就是**丢失更新**（lost update）。在 Agent 平台里它意味着少算钱、配额失效。四种解法，从简单到复杂：

**解法 1：在 SQL 里做运算（首选）**

```go
// store.go · AddUsage
`UPDATE runs SET input_tokens = input_tokens + $2, output_tokens = output_tokens + $3
 WHERE id = $1`
```

`UPDATE` 执行时会给这一行加**行锁**，并且读的是最新提交的值。两个并发的 `+10` 会排队执行，结果一定是 120。**能写成一条 SQL 的，就别在应用里读改写。**

**解法 2：悲观锁 `SELECT ... FOR UPDATE`**

逻辑复杂、没法写成一条 SQL 时（比如要根据当前值调用其他服务做判断）：

```sql
BEGIN;
SELECT input_tokens FROM runs WHERE id = $1 FOR UPDATE;  -- 锁住这一行，别人要改得等我提交
-- ... 应用里计算 ...
UPDATE runs SET input_tokens = $2 WHERE id = $1;
COMMIT;
```

注意：锁一直持有到事务结束，**事务里千万不要调用模型 API**，否则一次模型调用 30 秒，这行就被锁 30 秒。

**解法 3：乐观锁（版本号）**

表上加一个 `version` 列，更新时检查版本没变：

```sql
UPDATE conversations SET title = $2, version = version + 1
WHERE id = $1 AND version = $3;   -- 影响 0 行 = 被别人抢先改了，重新读再试
```

不持锁，适合冲突很少的场景，比如用户在两个标签页里编辑同一个会话标题。

**解法 4：Repeatable Read / Serializable + 重试**

把事务开在更高的隔离级别，冲突时数据库报 `40001` 错误，应用捕获后整个事务重试。最通用，但需要所有相关代码都有重试逻辑。

## 12. 写偏斜：只有 Serializable 防得住

规则：**一个会话同时最多只能有一个运行中的 run**（不然两个 Agent 同时往一个会话里写消息，就乱了）。直觉写法：

```sql
BEGIN;
SELECT count(*) FROM runs WHERE conversation_id = 1 AND status = 'running';  -- 0
-- 是 0，可以启动
INSERT INTO runs (conversation_id, idempotency_key, status) VALUES (1, 'a', 'running');
COMMIT;
```

两个请求同时执行，都查到 0，都插入。两个事务改的是**不同的行**（各插各的），行锁防不住，Repeatable Read 也防不住。实测三个隔离级别下的结果：

| 隔离级别 | 两个事务都提交了吗 | 最终 running 数 |
|---|---|---|
| Read Committed | 都提交 | 2 ❌ |
| Repeatable Read | 都提交 | 2 ❌ |
| Serializable | 第二个提交时报错 `40001 could not serialize access due to read/write dependencies` | 1 ✅ |

两种正确做法：

1. **用约束表达规则（首选）**：部分唯一索引，“running 状态下，每个会话只能有一行”。

   ```sql
   CREATE UNIQUE INDEX runs_one_running_per_conv ON runs (conversation_id) WHERE status = 'running';
   ```

   第二个插入直接报唯一冲突。规则写在数据库里，任何代码路径都绕不过去。

2. **Serializable + 重试**：规则复杂到没法用约束表达时（比如“这个用户今天所有 run 的花费加起来不超过 10 美元”）。

**经验**：默认用 Read Committed；能用约束和原子 SQL 解决的就用它们解决；剩下的少数复杂规则，单独对那几个事务用 Serializable 并写好重试。

## 13. 消息序号：为什么要 last_seq

回到 `AppendMessage`。最直觉的序号分配方式是：

```sql
SELECT coalesce(max(seq), 0) + 1 FROM messages WHERE conversation_id = $1;
INSERT INTO messages (..., seq, ...) VALUES (..., 上一步的结果, ...);
```

这就是第 11 节的“读改写”：两个并发请求读到同一个 `max`，插入同一个 `seq`。好在我们有 `UNIQUE (conversation_id, seq)`，第二个会报错而不是写入脏数据。练习 2 你会亲手改成这个写法，看到测试里出现：

```
ERROR: duplicate key value violates unique constraint "messages_conversation_id_seq_key" (SQLSTATE 23505)
```

约束兜住了数据正确性，但用户的消息发送失败了。正确做法是让分配序号这一步本身变成原子操作：

```sql
UPDATE conversations SET last_seq = last_seq + 1 WHERE id = $1 RETURNING last_seq;
```

`UPDATE` 给会话这一行加了行锁，**同一个会话**的并发追加在这里排队，拿到 1、2、3……；**不同会话**锁的是不同的行，互不影响。而且锁只持有到事务结束（插入一条消息，几毫秒）。`TestAppendMessageConcurrentSeq` 并发追加 50 条，验证 seq 正好是 1 到 50。

这个写法还顺带检查了会话是否存在：`UPDATE` 影响 0 行就返回 `ErrNotFound`。用 `max(seq)` 的写法，会话不存在时会一直走到 `INSERT`，被外键拦下，报的是 `violates foreign key constraint (SQLSTATE 23503)`，上层很难把它转成清晰的 404。

## 14. 幂等键：客户端重试不重复扣费

客户端发起一次 Agent 运行，网络超时了。它不知道服务端到底收到没有，于是重试。如果服务端每次都新建一个 run，用户就被扣了两次钱。

做法：客户端为每个**逻辑请求**生成一个唯一的幂等键（比如 UUID，放在 `Idempotency-Key` 请求头里），重试时带同一个键。服务端用唯一约束保证一个键只对应一个 run：

```go
// store.go · EnqueueRun
r, err := scanRun(s.pool.QueryRow(ctx,
    `INSERT INTO runs (conversation_id, idempotency_key, model) VALUES ($1, $2, $3)
     ON CONFLICT (idempotency_key) DO NOTHING
     RETURNING `+runCols, convID, key, model))
if err == nil {
    return r, true, nil
}
if !errors.Is(err, pgx.ErrNoRows) {
    return Run{}, false, err
}
// 冲突了：说明已经有人用这个 key 建过，查出来返回
r, err = scanRun(s.pool.QueryRow(ctx,
    `SELECT `+runCols+` FROM runs WHERE idempotency_key = $1`, key))
return r, false, err
```

**逐行解读**

- **`ON CONFLICT (idempotency_key) DO NOTHING`**：键已经存在时不报错，什么也不做。判断“存在不存在”和“插入”是数据库在一个原子操作里完成的，不存在“都检查了、都插入了”的窗口。
- **`RETURNING`**：插入成功时返回新行；`DO NOTHING` 时没有返回行，`Scan` 得到 `pgx.ErrNoRows`。
- **查已有的那条**：重试的请求拿到和第一次完全相同的 run，前端可以接着订阅它的进度。`created` 告诉调用方这是新建的还是重放的，HTTP 层可以据此返回 201 或 200。

`TestEnqueueRunIdempotent` 让 10 个 goroutine 用同一个键同时调用，验证只建了 1 条。

> 不要先 `SELECT` 看键存不存在、再决定 `INSERT`，那又是一个读改写。第 08 模块“重试与幂等”会把这个模式扩展到整个请求链路。

## 15. 用 Postgres 做任务队列：SKIP LOCKED

Agent 运行可能要几分钟，不能放在 HTTP 请求里同步执行。常见架构：API 只负责建一条 `queued` 状态的 run 并立即返回，后台多个 worker 从表里领任务执行。

难点在于**多个 worker 同时领，不能领到同一个**：

```go
// store.go · ClaimRun
`UPDATE runs SET status = 'running', worker = $1, started_at = now()
 WHERE id = (
     SELECT id FROM runs WHERE status = 'queued'
     ORDER BY created_at, id
     FOR UPDATE SKIP LOCKED
     LIMIT 1)
 RETURNING ...`
```

**逐行解读**

- **子查询**：找最早的一条排队中的任务。`WHERE status = 'queued'` 和 `ORDER BY created_at, id` 正好命中第 8 节的部分索引。
- **`FOR UPDATE`**：给选中的行加锁，别人在我提交前改不了它。
- **`SKIP LOCKED`**：如果某一行已经被别的 worker 锁住了，**不等它，直接跳过**，去拿下一条。这样 6 个 worker 同时来，会各自拿到不同的 6 条。
- **外层 `UPDATE`**：把领到的任务改成 `running`，记上是谁领的。整条语句是一个原子操作。

两个对照实验（练习 2 里你会亲手做）：

| 改动 | 结果 |
|---|---|
| 去掉 `FOR UPDATE SKIP LOCKED` | 测试失败，出现 `run 18 被 worker-0 和 worker-5 重复领取`。两个 worker 的子查询选中了同一个 id，外层 `UPDATE` 只按 `id` 过滤，两次都成功 |
| 只去掉 `SKIP LOCKED` | 测试仍然通过，但所有 worker 都在等同一行的锁，变成一个接一个地领，并发 worker 失去了意义 |

**为什么不直接用 Kafka / RabbitMQ？** 规模不大时（每秒几百个任务以内），用 Postgres 做队列有一个巨大优势：**建任务和写业务数据可以在同一个事务里**。“创建会话消息 + 创建 run”要么都成功要么都失败，不会出现“消息写了但任务没进队列”。用外部消息队列就得处理这种不一致（第 08 模块的 Outbox 模式）。

## 16. 状态机守卫：用 WHERE 防止非法状态转换

run 的状态只能这样走：`queued → running → succeeded / failed`。不允许 `queued` 直接变 `succeeded`，也不允许已经 `succeeded` 的再被改成 `failed`（比如一个超时的 worker 迟到了才来汇报）。

```go
// store.go · FinishRun
tag, err := s.pool.Exec(ctx,
    `UPDATE runs SET status = $2, finished_at = now()
     WHERE id = $1 AND status = 'running'`, id, status)
// ...
if tag.RowsAffected() == 0 {
    return ErrConflict
}
```

把“前一个状态必须是 running”写进 `WHERE`。不满足就影响 0 行，返回 `ErrConflict`。检查和修改在同一条语句里，没有并发窗口。这其实就是第 11 节乐观锁的思路，只是用 `status` 代替了 `version`。

## 17. 连接池

每个 Postgres 连接在服务端是一个独立的进程，大约占 10 MB 内存，建立连接要几毫秒。所以应用不能每个请求新建连接，而是维护一个**连接池**：

```go
pool, err := pgxpool.New(ctx, url)   // url 里可以带 ?pool_max_conns=10
```

要点：

- **池子不是越大越好**。Postgres 的并发能力受 CPU 核数限制，一个 8 核数据库，几十个同时执行的查询就饱和了。10 个服务实例、每个池子 100 个连接，就是 1000 个连接，数据库先被压垮。经验起点：每个实例 `pool_max_conns` 设成 10 到 20，再根据压测调整。
- **连接被长时间占着，池子就被耗尽**。最常见的原因是事务里做了慢操作（调模型、调外部 API）。**事务要短：只包住数据库操作。**
- **服务实例很多时**，在应用和数据库之间加 **PgBouncer** 做连接复用（第 09 模块）。

---

# 第四部分：迁移

## 18. 迁移：表结构也要版本管理

表结构的每一次改动都写成一个 SQL 文件，按顺序编号，提交到 Git，服务启动时自动执行还没执行过的那些。这样任何环境（你的电脑、CI、线上）的表结构都能从零重建，而且一致。

练习里的执行器 `migrate.go` 只有几十行，主流工具（goose、golang-migrate、Atlas）原理是一样的：

```go
//go:embed migrations/*.sql
var migrationFS embed.FS

func (s *Store) Migrate(ctx context.Context) ([]string, error) {
    // ...
    tx, err := s.pool.Begin(ctx)
    defer tx.Rollback(ctx) // 已 Commit 时这是空操作

    // 事务级咨询锁：事务结束自动释放，进程崩了也不会留下死锁
    tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID)
    tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (...)`)
    // 读出已执行的版本 ...
    for _, f := range files {
        if done[version] { continue }
        tx.Exec(ctx, string(sql))
        tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
    }
    return applied, tx.Commit(ctx)
}
```

**逐行解读**

- **`//go:embed`**：编译时把 SQL 文件打进二进制。0.5 课的 distroless 镜像里只有一个可执行文件，迁移文件跟着走，不用额外拷贝。
- **`schema_migrations` 表**：记录执行过哪些版本。执行器每次启动先读它，只执行没有记录的文件。
- **咨询锁 `pg_advisory_xact_lock`**：K8s 一次滚动发布可能同时启动 3 个新实例，3 个都去执行迁移就会冲突。咨询锁是 Postgres 提供的“按数字加锁”，和任何表无关。拿到锁的执行，其余的等；轮到它们时发现版本已经记录了，直接跳过。`_xact_` 表示锁跟着事务，提交或回滚时自动释放，进程崩溃也不会留下一把永远不释放的锁。
- **所有迁移在一个事务里**：Postgres 的 DDL（建表、改表）是事务性的，这是它比 MySQL 方便的地方：迁移跑到一半失败，整个回滚，表结构不会停在一个半成品状态。
- **`defer tx.Rollback(ctx)`**：任何一步出错提前返回，都会回滚；已经提交了的话 `Rollback` 什么也不做。

`TestMigrateIsIdempotent` 并发调用 5 次 `Migrate`，验证没有一次重复执行。

### 只写 up，不写 down

很多工具支持写“回滚迁移”（down）。实践中线上很少真的用它：删掉一个已经有数据的列是不可逆的。更常见的做法是**只往前走**：发现问题，再写一个新的迁移修正。

## 19. 线上改表：哪些操作会锁表

线上的表一直在被读写。有些 DDL 会拿到很强的锁，执行期间整张表的读写都被阻塞。表小时无感，表大了就是一次事故。

| 操作 | 风险 | 安全做法 |
|---|---|---|
| `ADD COLUMN ... DEFAULT 常量` | Postgres 11 起只改元数据，瞬间完成 | 直接做（练习的 `0002_runs_model.sql`） |
| `ADD COLUMN ... DEFAULT now()` 这类易变默认值 | 重写整张表 | 先加可空列，再分批回填 |
| `CREATE INDEX` | 建索引期间阻塞写入 | `CREATE INDEX CONCURRENTLY`（不能放在事务里，需要单独执行） |
| `ALTER COLUMN TYPE` | 重写整张表并锁表 | 新建列 → 双写 → 回填 → 切读 → 删旧列 |
| `ADD CONSTRAINT ... NOT NULL / CHECK / FK` | 扫全表校验，期间锁表 | 先 `NOT VALID` 加上，再单独 `VALIDATE CONSTRAINT` |
| 重命名或删除列 | 正在运行的旧版本代码立刻报错 | 分两次发版，见下 |

**扩展-收缩**（expand / contract）模式，以“把 `title` 改名为 `name`”为例：

1. **扩展**：加 `name` 列；代码同时写 `title` 和 `name`；回填旧数据；
2. **切换**：代码改为只读 `name`；
3. **收缩**：确认没有旧版本代码在跑了，再删 `title`。

核心原则：**任何时刻，线上同时在跑的新旧两个版本的代码，都要能和当前的表结构正常工作。** 因为滚动发布时新旧版本就是会同时存在（0.5 课优雅退出的那几十秒）。

另一个保命设置：迁移前 `SET lock_timeout = '5s'`。DDL 拿不到锁时，与其无限等下去、在它后面堵住所有正常查询，不如 5 秒后失败，换个时间再试。

---

# 第五部分：Redis

## 20. Redis 是什么，适合放什么

Redis 是一个**内存里的键值存储**，单线程执行命令，每条命令都是原子的，单实例每秒能处理十万级的操作。它的值不只是字符串，还有好几种数据结构：

| 结构 | 常用命令 | Agent 平台里的用途 |
|---|---|---|
| String | `GET` `SET EX` `INCR` | 缓存（会话、用户配置）、计数器、固定窗口限流 |
| Hash | `HSET` `HGET` `HINCRBY` | 一个对象的多个字段，比如一次 run 的实时进度 |
| List | `LPUSH` `BRPOP` | 简单队列（但没有确认机制，worker 崩了任务就丢） |
| Set | `SADD` `SISMEMBER` | 去重，比如“这条 webhook 处理过了吗” |
| Sorted Set | `ZADD` `ZRANGEBYSCORE` `ZREMRANGEBYSCORE` | 排行榜、滑动窗口限流、延迟任务 |
| Stream | `XADD` `XREADGROUP` `XACK` | 带确认的消息流，比如把流式 token 广播给多个订阅者 |
| Pub/Sub | `PUBLISH` `SUBSCRIBE` | 实时通知，但订阅者不在线时消息直接丢失 |

**Redis 不是数据库的替代品。** 它默认的持久化是定期快照（RDB）和追加日志（AOF），配置得当也可能丢最近一秒的数据，内存满了还会按策略淘汰 key。规则：**丢了能从别处重建的数据才放 Redis**（缓存、计数器、限流），丢了就找不回来的放 Postgres（消息、运行记录、账单）。

## 21. Cache-Aside：最常用的缓存模式

**读**：先查缓存，命中直接返回；没命中查数据库，再写回缓存。
**写**：先写数据库，成功后**删除**缓存（不是更新缓存）。

```go
// cache.go · GetConversation
func (c *Cache) GetConversation(ctx context.Context, id int64) (Conversation, error) {
    key := convKey(id)
    conv, hit, err := c.lookup(ctx, key)
    if err != nil {
        // Redis 挂了：降级直接查库，缓存是加速，不能变成单点
        return c.store.GetConversation(ctx, id)
    }
    if hit {
        return conv.value()
    }

    // 未命中：同一个 key 的并发请求只放一个去查库（防缓存击穿）
    v, err, _ := c.sf.Do(key, func() (any, error) {
        lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
        defer cancel()
        // 双重检查：可能在我们 GET 未命中之后、进入 Do 之前，上一轮 Do 刚把缓存填好
        if conv, hit, err := c.lookup(lctx, key); err == nil && hit {
            return conv.value()
        }
        c.loads.Add(1)
        conv, err := c.store.GetConversation(lctx, id)
        if errors.Is(err, ErrNotFound) {
            c.rdb.Set(lctx, key, notFoundMarker, 30*time.Second)
            return nil, err
        }
        if err != nil {
            return nil, err
        }
        if b, err := json.Marshal(conv); err == nil {
            c.rdb.Set(lctx, key, b, c.jitter(c.ttl)) // 回填失败不影响返回结果
        }
        return conv, nil
    })
    if err != nil {
        return Conversation{}, err
    }
    return v.(Conversation), nil
}
```

**逐行解读**

- **`lookup` 把三种结果分开**：命中（`hit=true`）、未命中（`hit=false`）、Redis 本身出错（`err != nil`）。这三种情况的处理完全不同，混在一起就会出 bug。
- **Redis 出错时降级查库**：缓存的作用是加速。如果 Redis 一挂整个服务就不可用，你等于给系统加了一个单点故障。`TestCacheNegativeAndFallback` 连一个不存在的 Redis 地址，验证仍然能从库里读到数据。
- **`singleflight`**：`golang.org/x/sync/singleflight`，同一个 key 同时只有一个函数在执行，其他调用者等着共享它的结果。一个热门会话的缓存过期瞬间涌进来 100 个请求，只有 1 个去查库。
- **`context.WithoutCancel(ctx)`**：这次查库是替所有等待者做的。如果直接用第一个调用者的 `ctx`，它的用户一关页面，ctx 取消，其他 99 个等待者也一起拿到 `context canceled`。所以去掉取消信号（Go 1.21 起有这个函数），另设 3 秒超时，保证这次加载不会无限挂着。
- **双重检查**：这是写测试时真实踩到的坑。最初没有这一步，`TestCacheAsideSingleflight` 连跑 30 次有 18 次失败（期望查库 1 次，实际 2 次）。原因是时序：goroutine X 执行 `GET` 时缓存还空着；接着第一轮 `Do` 完成、写好缓存、退出；然后 X 才进入 `Do`，此时已经没有正在执行的同 key 函数可以共享，于是又查了一次库。进入 `Do` 后先再查一次缓存就解决了。**`singleflight` 只合并“同时”的调用，不合并“先后”的调用。**
- **`c.loads`**：一个原子计数器，记录真实查库次数，测试用它验证 singleflight 和负缓存是否生效。
- **回填失败不报错**：缓存写不进去，数据仍然是对的，下次再查库而已。

### 写路径：为什么删缓存而不是更新缓存

```go
// cache.go · RenameConversation
if err := c.store.RenameConversation(ctx, id, title); err != nil {
    return err
}
return c.rdb.Del(ctx, convKey(id)).Err()
```

如果改成“写库后更新缓存”，两个并发的修改可能这样交错：

```
请求 1：写库 title=A
请求 2：写库 title=B
请求 2：写缓存 title=B
请求 1：写缓存 title=A      ← 库里是 B，缓存里是 A，直到过期都是错的
```

删除没有这个问题：不管谁先删，缓存都是空的，下次读取从库里拿最新值。删缓存的方案也不是绝对一致（极端时序下仍可能写回旧值），所以**TTL 是最后的兜底**：再坏的情况，过期后也会恢复正确。对一致性要求极高的数据（余额），干脆不缓存。

## 22. 缓存的三个经典问题

| 问题 | 现象 | 练习里的对策 |
|---|---|---|
| **穿透** | 大量请求查询**根本不存在**的 key（恶意扫描、bug），每次都未命中，全部打到数据库 | **负缓存**：不存在也缓存一个 `"null"` 标记，TTL 30 秒 |
| **击穿** | 一个**热点 key** 过期的瞬间，大量并发请求同时未命中，同时查库 | **singleflight**：同一实例内同 key 只放一个请求去查库 |
| **雪崩** | **大量 key 同时过期**（比如服务启动时一起写入、TTL 都是 1 小时），或 Redis 整体宕机 | **TTL 加随机抖动**（`jitter`，0~10%）；Redis 故障时**降级查库** |

两点补充：

- singleflight 只在**单个进程内**合并请求。10 个实例最多还是有 10 个请求同时查库，一般可以接受；如果要做到全局只有 1 个，需要用 Redis 分布式锁（`SET key value NX PX 3000`），复杂度上升很多，一般不值得。
- 负缓存的 TTL 要短：用户刚创建的会话，如果之前被查过一次“不存在”，在负缓存过期前都会被当成不存在。

## 23. 限流：用 Lua 保证原子性

每个用户每分钟最多发起 N 次模型调用，防止滥用和账单爆炸。最简单的是**固定窗口**：每个窗口一个计数器，超过上限就拒绝。

```go
// cache.go
var fixedWindow = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return n`)

func Allow(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) (bool, int, error) {
    n, err := fixedWindow.Run(ctx, rdb, []string{"rl:" + key}, window.Milliseconds()).Int()
    if err != nil {
        return false, 0, fmt.Errorf("rate limit: %w", err)
    }
    if n > limit {
        return false, 0, nil
    }
    return true, limit - n, nil
}
```

**逐行解读**

- **`INCR`**：计数加一并返回新值。key 不存在时从 0 开始，所以第一次返回 1。
- **`n == 1` 时设过期**：第一次计数时开启这个窗口，到期 key 自动消失，下一个窗口从头计数。
- **为什么用 Lua**：如果在 Go 里分两次调用 `INCR` 和 `PEXPIRE`，进程恰好在两次调用之间崩溃，这个 key 就永远没有过期时间，这个用户被永久限流。Redis 执行一段 Lua 脚本时不会插入其他命令，两步是原子的。
- **`redis.NewScript`**：go-redis 会先用 `EVALSHA` 按脚本哈希执行，Redis 里没有缓存这个脚本时自动退回 `EVAL` 上传，不用每次传输整段脚本。
- **返回剩余次数**：HTTP 层可以放进 `X-RateLimit-Remaining` 响应头，被拒绝时返回 `429 Too Many Requests` 和 `Retry-After`。

固定窗口的缺点：限制每分钟 100 次，用户在 0:59 打 100 次、1:00 再打 100 次，两秒内打了 200 次。要更平滑，用**滑动窗口**（选做练习）或**令牌桶**。另外，按“次数”限流对 Agent 不够：一次调用可能是 100 token 也可能是 10 万 token，生产中往往还要按 **token 数**限流（第 09 模块）。

---

## 本课小结

| 问题 | 方案 | 练习里的位置 |
|---|---|---|
| 数据必须对，不管调用方多粗心 | `NOT NULL` / `CHECK` / `UNIQUE` / 外键 | `0001_init.sql` |
| 列表页翻页又慢又重复 | 复合索引 + 键集分页 | `ListConversations` |
| 并发追加消息重号 | `UPDATE ... last_seq + 1 RETURNING` 行锁分配序号 | `AppendMessage` |
| 计费并发累加丢数 | SQL 内原子运算 | `AddUsage` / `TestLostUpdate` |
| “最多一个运行中”被并发绕过 | 部分唯一索引，或 Serializable + 重试 | 第 12 节 |
| 客户端重试重复扣费 | 幂等键 + `ON CONFLICT DO NOTHING` | `EnqueueRun` |
| 多 worker 领到同一个任务 | `FOR UPDATE SKIP LOCKED` | `ClaimRun` |
| 迟到的 worker 覆盖最终状态 | `WHERE status = 'running'` 状态守卫 | `FinishRun` |
| 多实例同时迁移 | 咨询锁 + `schema_migrations` | `migrate.go` |
| 热点数据读得慢 | Cache-Aside + 写后删缓存 | `cache.go` |
| 穿透 / 击穿 / 雪崩 / Redis 故障 | 负缓存 / singleflight / TTL 抖动 / 降级查库 | `cache.go` |
| 防滥用 | Lua 固定窗口限流 | `Allow` |

一句话：**正确性交给数据库的约束和原子语句，速度交给缓存，缓存挂了不能影响正确性。**

---

## 练习（今天做完）

**准备**：装好 Docker，进入 `projects/05-store`，执行 `make up && make test`，12 个测试全部通过（`TestLostUpdate` 会打印丢了多少次更新，每次数字不同）。

**练习 1：验证索引和分页**

`make psql` 进入数据库，灌入 20 万个会话：

```sql
INSERT INTO conversations (user_id, title, updated_at)
SELECT 'u' || (i % 100), 'c' || i, now() - (i || ' seconds')::interval
FROM generate_series(1, 200000) AS i;
ANALYZE conversations;
```

然后：

1. 对 `ListConversations` 第一页的查询（`user_id = 'u7'`，`LIMIT 21`）执行 `EXPLAIN ANALYZE`，确认用的是 `conversations_user_recent_idx`，记下 `Execution Time`；
2. 写一个等价的 `OFFSET 1900 LIMIT 21` 查询，和一个从第 1900 行之后开始的键集查询（游标取第 1900 行的 `updated_at` 和 `id`），分别 `EXPLAIN ANALYZE`，对比两者实际读取的行数和耗时；
3. `DROP INDEX conversations_user_recent_idx;` 后再执行第 1 步，看计划变成了什么。做完重新执行 `0001_init.sql` 里那条 `CREATE INDEX` 恢复。

**练习 2：弄坏它，看哪个测试报警**

每次只做一处修改，运行 `make test`，记下哪个测试失败、错误信息是什么、为什么，然后 `git checkout .` 还原：

1. `AppendMessage` 改为先 `SELECT coalesce(max(seq), 0) + 1 FROM messages WHERE conversation_id = $1` 取序号，再插入；
2. `ClaimRun` 去掉 `FOR UPDATE SKIP LOCKED` 整行；再试只去掉 `SKIP LOCKED`；
3. `cache.go` 删掉 `Do` 里的双重检查那三行，用 `go test -race -count=30 -run TestCacheAsideSingleflight ./...` 跑 30 次，数一下失败几次。

**练习 3：复现写偏斜**

在两个 psql 窗口里，按第 12 节的步骤交错执行（A 查询、B 查询、A 插入、B 插入、A 提交、B 提交），分别用默认级别和 `BEGIN ISOLATION LEVEL SERIALIZABLE`，记录最终 running 的行数。然后加上第 12 节的部分唯一索引，用默认级别再做一次，看是哪一步报错、报的什么错。

**选做：滑动窗口限流**

用 Sorted Set 实现 `AllowSliding(ctx, rdb, key, limit, window)`：每次请求以当前毫秒时间戳为 score 加入集合，先删掉窗口之外的旧记录，再数集合里有多少个。整个过程写成一个 Lua 脚本。仿照 `TestRateLimitFixedWindow` 写一个测试，并额外验证固定窗口的“边界两倍流量”问题在滑动窗口下不会出现。

---

## 自测题

1. 为什么 `created_at` 要用 `TIMESTAMPTZ` 而不是 `TIMESTAMP`？主键为什么用 `BIGINT` 而不是 `INT`？
2. 有一个复合索引 `(user_id, updated_at DESC, id DESC)`。下面三个查询，哪个能用它避免排序，哪个基本用不上？
   (a) `WHERE user_id = ? ORDER BY updated_at DESC LIMIT 20`
   (b) `WHERE updated_at > now() - interval '1 day'`
   (c) `WHERE user_id = ? AND updated_at > ?`
3. 键集分页的游标为什么必须包含 `id`，只用 `updated_at` 会出什么问题？
4. 在 Read Committed 下，`UPDATE runs SET input_tokens = input_tokens + 10 WHERE id = 1` 被两个事务并发执行，结果会丢更新吗？为什么？
5. 同事说：“我把事务改成 Repeatable Read 了，‘每个会话最多一个 running’ 的检查就安全了。”对吗？你会怎么改？
6. `EnqueueRun` 如果写成“先 `SELECT` 查幂等键是否存在，不存在再 `INSERT`”，在没有唯一约束的情况下会发生什么？有唯一约束的情况下呢？
7. 一个 worker 领了任务后进程被 `kill -9`，这个 run 会永远停在 `running`。你会怎么设计，让它能被别的 worker 重新领走？
8. 为什么写数据后选择“删缓存”而不是“更新缓存”？删缓存就能保证缓存和数据库完全一致吗？
9. 解释缓存穿透、击穿、雪崩的区别，并分别说出练习里的对策。
10. 固定窗口限流的 `INCR` 和 `PEXPIRE` 为什么要放在 Lua 脚本里？不放会出什么问题？
11. 迁移里要给一张 5000 万行的 `messages` 表加索引，直接写 `CREATE INDEX` 会有什么后果？练习里的迁移执行器能直接跑 `CREATE INDEX CONCURRENTLY` 吗？

---

# 参考答案与详解

## 自测题答案

**1.** `TIMESTAMPTZ` 存的是一个绝对时刻（内部统一转成 UTC），读出时按会话时区显示；`TIMESTAMP` 只存“年月日时分秒”这几个数字，不知道是哪个时区的。服务器在 UTC、开发机在北京时间时，同一条记录在两边会差 8 小时，而且无法事后修正，因为原始时区信息已经丢了。`INT` 上限约 21.4 亿，消息、运行记录这种按天几百万增长的表几年就会用完，到时改成 `BIGINT` 需要重写整张表并长时间锁表；一开始就用 `BIGINT` 每行只多 4 字节。

**2.** (a) 能用，而且不需要排序：定位到 `user_id` 那一段后，这一段本来就按 `updated_at DESC` 排好，读前 20 行就停。(b) 基本用不上：缺少最左列 `user_id`，就像只知道第二个字母没法查字典，优化器通常会选全表扫描（Postgres 有时会做 Skip Scan 之类的优化，但不能指望）。(c) 能用：`user_id` 等值定位，`updated_at` 在那一段里做范围扫描。

**3.** 只用 `updated_at < 游标时间` 的话，所有与上一页最后一行 `updated_at` **相同**的行都会被跳过（它们不满足严格小于）；改成 `<=` 又会把上一页最后一行重复返回。时间戳精度是微秒，看起来很难相同，但批量导入、同一个事务里创建的多行，`now()` 返回的是同一个值（事务开始时间）。带上唯一的 `id` 组成 `(updated_at, id)`，排序就是全序，每一行都有唯一的位置。

**4.** 不会。`UPDATE` 会对目标行加行锁，第二个事务在这里等待第一个提交。第一个提交后，Read Committed 下第二个事务会**重新读取这一行的最新版本**，再计算 `input_tokens + 10`，所以结果是 +20。丢失更新只发生在“值在应用里算好、再写回去”的情况，因为那样数据库不知道你是基于哪个旧值算的。

**5.** 不对。实测 Repeatable Read 下两个事务都能提交，最终有 2 个 running。这是写偏斜：两个事务读的是同一个范围，但写的是不同的行（各自插入一行），没有写写冲突，快照隔离检测不到。改法：首选加部分唯一索引 `CREATE UNIQUE INDEX ... ON runs (conversation_id) WHERE status = 'running'`，让第二个插入（或第二个“改成 running 的更新”）直接因唯一冲突失败；如果规则无法用约束表达，就用 Serializable 并在应用层捕获 `40001` 重试。

**6.** 没有唯一约束时：两个并发请求都 `SELECT` 到“不存在”，都执行 `INSERT`，产生两条 run，用户被扣两次钱，这就是“检查再执行”（check-then-act）的竞态。有唯一约束时：第二个 `INSERT` 会因为唯一冲突报错（23505），数据是对的，但调用方拿到的是一个错误，还得写代码捕获这个错误再去查已有的记录。`ON CONFLICT DO NOTHING` 把“检查 + 插入”合成一个原子操作，并且冲突时不报错，代码最简洁。

**7.** 常见方案是**租约**（lease）：领任务时写入 `lease_until = now() + interval '60 seconds'`，worker 运行期间定期（比如每 20 秒）续约，延长 `lease_until`。`ClaimRun` 的条件改成 `status = 'queued' OR (status = 'running' AND lease_until < now())`，过期的任务就能被别的 worker 领走。配套要做两件事：一是 `FinishRun` 的守卫里加上 `worker = $我`，防止原来那个 worker 只是卡住而不是死了、恢复后覆盖新 worker 的结果；二是任务本身要能安全重做（幂等）或从断点恢复，这是第 08 模块“断点恢复”的内容。

**8.** 更新缓存时，两个并发写入的“写库”和“写缓存”顺序可能交错，导致库里是新值、缓存里是旧值，并一直保持到过期。删除缓存是幂等的，无论顺序如何结果都是“缓存为空”，下次读取会从库里拿最新值。但删缓存也不能保证完全一致：比如读请求 R 未命中、从库里读到旧值；此时写请求 W 写库并删缓存；然后 R 才把旧值写回缓存。这个窗口很小（需要 R 的读库和写缓存之间恰好夹着 W 的整个过程），所以实践中可以接受，并靠 TTL 兜底。需要更强一致时可以“延迟双删”（写库后删一次，过几百毫秒再删一次），或者干脆不缓存这类数据。

**9.** **穿透**：查询的数据在库里根本不存在，所以永远不会被缓存，每次都打到库，对策是负缓存（缓存一个短 TTL 的“不存在”标记），还可以在入口用布隆过滤器拦截明显不存在的 id。**击穿**：某一个热点 key 过期的瞬间，大量请求同时未命中并同时查库，对策是 singleflight（进程内合并同 key 请求）。**雪崩**：大量 key 在同一时刻过期，或 Redis 整体不可用，压力全部落到数据库，对策是 TTL 加随机抖动打散过期时间，以及 Redis 故障时降级查库（还可以配合限流，避免数据库被降级流量压垮）。

**10.** 分两次调用时，如果进程在 `INCR` 之后、`PEXPIRE` 之前崩溃或网络断开，这个 key 就没有过期时间，计数只增不减，这个用户从此被永久限流。Lua 脚本在 Redis 里原子执行，两步要么都执行要么都不执行。另一种写法是 `SET key 0 PX window NX` 后再 `INCR`，同样需要考虑原子性，用脚本最简单。

**11.** 普通 `CREATE INDEX` 在建索引期间持有阻止写入的锁，5000 万行可能要几分钟甚至更久，这段时间所有往 `messages` 写消息的请求都会被阻塞，用户发不出消息，相当于服务中断。应该用 `CREATE INDEX CONCURRENTLY`，它允许建索引期间正常读写（代价是更慢，失败时会留下一个无效索引需要手动删掉）。练习里的执行器**不能**直接跑它：`CONCURRENTLY` 不能在事务块里执行，而我们的执行器把所有迁移放在一个事务里，会报 `CREATE INDEX CONCURRENTLY cannot run inside a transaction block`。成熟的迁移工具会提供“这个文件不包事务”的标记（比如 goose 的 `-- +goose NO TRANSACTION`），我们的执行器要支持的话，需要对带这个标记的文件单独执行、单独记录版本。

## 练习参考答案

**练习 1：索引和分页**

第一页查询的计划是 `Limit → Index Scan using conversations_user_recent_idx`，`Index Cond: (user_id = 'u7')`，实际读取 21 行，耗时在 0.1 毫秒以内。

键集查询的写法：

```sql
-- 先拿到第 1900 行的排序键
SELECT updated_at, id FROM conversations WHERE user_id = 'u7'
ORDER BY updated_at DESC, id DESC OFFSET 1899 LIMIT 1;

-- 用它做游标
EXPLAIN ANALYZE
SELECT id, title FROM conversations
WHERE user_id = 'u7' AND (updated_at, id) < ('上一步的 updated_at', 上一步的 id)
ORDER BY updated_at DESC, id DESC LIMIT 21;
```

一次实测结果（Postgres 17，具体毫秒数因机器而异）：

| 查询 | 计划 | 实际读取 | 耗时 |
|---|---|---|---|
| 第一页 `LIMIT 21` | `Limit → Index Scan using conversations_user_recent_idx` | 21 行 | 0.04 ms |
| `OFFSET 1900 LIMIT 21` | `Limit → Sort → Bitmap Heap Scan`（`Bitmap Index Scan` 取出 u7 全部行） | 2000 行，排序后扔掉前 1900 行 | 1.6 ms |
| 键集，从第 1900 行之后 | `Limit → Index Scan`，`Index Cond` 里多了行比较条件 | 21 行 | 和第一页相当 |
| 删掉索引后的第一页 | `Limit → Gather Merge → Sort (top-N heapsort) → Parallel Seq Scan` | 扫描 20 万行，`Rows Removed by Filter: 99000`（每个并行进程） | 9.6 ms |

几点观察：

- OFFSET 这一行里优化器甚至放弃了“按索引顺序读”，因为它算出反正要读 1921 行，不如用 Bitmap 一次把 u7 的 2000 行全取出来再排序。**OFFSET 的代价随页码线性增长**，每个用户的会话越多、页码越深越慢；键集分页的代价是常数。
- 删掉索引后要扫全表，耗时上升两个多数量级。`top-N heapsort` 是优化器知道只要前 21 行时用的堆排序，内存只要 26 kB，但它省不掉扫描本身。
- 20 万行的表上差距是毫秒级，看起来不大；表到几千万行、并发上来以后，就是慢查询和数据库 CPU 打满的区别。

**练习 2：弄坏之后**

| 改动 | 失败的测试 | 原因 |
|---|---|---|
| 用 `max(seq)+1` 分配序号 | `TestAppendMessageConcurrentSeq`：大量 `duplicate key value violates unique constraint "messages_conversation_id_seq_key" (SQLSTATE 23505)`；`TestAppendMessageUnknownConversation`：期望 `ErrNotFound`，实际是 `violates foreign key constraint "messages_conversation_id_fkey" (SQLSTATE 23503)` | 并发事务读到同一个 `max`，插入同一个 seq，被唯一约束拦下；会话不存在时没有 `UPDATE` 那一步来发现，一路走到 `INSERT` 才被外键拦下 |
| 去掉 `FOR UPDATE SKIP LOCKED` | `TestClaimRunSkipLocked`：`run 18 被 worker-0 和 worker-5 重复领取` | 两个 worker 的子查询看到同一个最早的 queued 行；第二个 worker 的外层 `UPDATE` 等第一个提交后重新检查，条件只有 `id = 18`，仍然成立，于是又改了一次 |
| 只去掉 `SKIP LOCKED` | 测试通过 | 第二个 worker 在被锁的行上等待，等到后发现它已不是 queued，再去找下一行。结果正确，但 6 个 worker 实际上在排队 |
| 删双重检查 | `TestCacheAsideSingleflight` 在 30 次里失败十几次（实测 18 次），`want 1 load, got 2` | 第 21 节讲的时序：未命中的 goroutine 进入 `Do` 时上一轮已经结束，没有可共享的调用，于是再次查库 |

第一项值得多想一步：唯一约束保住了数据，**测试照样报警**，因为用户的请求失败了。“数据不会错”和“功能是对的”是两个层次。第二、三项说明，有些问题只在并发时出现，单线程测试永远发现不了；而第四项说明，有些问题即使并发也只是**有时**出现，所以写并发测试要用 `-count` 多跑几遍。

**练习 3：写偏斜**

默认级别（Read Committed）下两个事务都提交，最终 2 个 running。Serializable 下 A 正常提交，B 在 `COMMIT` 时报错：

```
ERROR:  could not serialize access due to read/write dependencies among transactions
DETAIL:  Reason code: Canceled on identification as a pivot, during commit attempt.
HINT:  The transaction might succeed if retried.
```

（报错也可能出现在 B 的 `INSERT` 那一步，取决于检测到冲突的时机。）最终 1 个 running。`HINT` 明确告诉你要重试：重试时 B 会重新查询，看到 A 插入的那一行，于是不再插入。

加上部分唯一索引后，用默认级别：两个 `SELECT` 都返回 0，A 的 `INSERT` 成功；B 的 `INSERT` **会卡住**，因为它要插入的唯一键和 A 未提交的行冲突，需要等 A 的结果。A 提交后，B 报错：

```
ERROR:  duplicate key value violates unique constraint "runs_one_running_per_conv"
```

如果 A 回滚，B 的插入就会成功。这说明唯一约束在任何隔离级别下都有效，而且不需要应用写重试逻辑，只需要把这个错误转成“该会话已有运行中的任务”返回给用户。

**选做：滑动窗口限流**

```go
var slidingWindow = redis.NewScript(`
local key    = KEYS[1]
local now    = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit  = tonumber(ARGV[3])
local member = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)  -- 删掉窗口之外的记录
local n = redis.call('ZCARD', key)
if n >= limit then
  return -1
end
redis.call('ZADD', key, now, member)
redis.call('PEXPIRE', key, window)                         -- 整个 key 在空闲一个窗口后自动清理
return limit - n - 1`)

func AllowSliding(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) (bool, int, error) {
	now := time.Now().UnixMilli()
	member := fmt.Sprintf("%d-%d", now, rand.Int64()) // 同一毫秒的多次请求也要是不同的成员
	left, err := slidingWindow.Run(ctx, rdb, []string{"rls:" + key},
		now, window.Milliseconds(), limit, member).Int()
	if err != nil {
		return false, 0, err
	}
	if left < 0 {
		return false, 0, nil
	}
	return true, left, nil
}
```

要点：

- **成员要唯一**：Sorted Set 的成员不能重复，同一毫秒内两次请求如果都用时间戳做成员，第二次会覆盖第一次，少计一次。所以加一个随机后缀。
- **被拒绝的请求不记录**：先判断再 `ZADD`，否则一个被限流的用户不停重试，会让自己永远出不了限流。
- **时间用应用的时钟**：多个实例的时钟有偏差时会有误差。更严格的做法是在脚本里用 `redis.call('TIME')` 取 Redis 自己的时间。
- **代价**：每个请求在集合里占一个成员，限额 1 万次每分钟的用户就要存 1 万个成员。量大时用令牌桶（只存“剩余令牌数”和“上次补充时间”两个数）更省内存。

验证边界问题的测试思路：窗口 300 毫秒、上限 5；先打 5 次，等 250 毫秒再打 5 次。固定窗口如果恰好跨过了窗口边界，第二批会全部放行（0.3 秒内 10 次）；滑动窗口下第二批全部被拒绝，再等 60 毫秒（第一批出了窗口）后才恢复放行。

---

## 完成标准

- [ ] `make test` 12 个测试全部通过，并且能说出每个测试在验证什么；
- [ ] 练习 1：拿到三组 `EXPLAIN ANALYZE` 结果，能解释 OFFSET 和键集分页读取行数的差别，以及删掉索引后计划的变化；
- [ ] 练习 2：四处修改都做过，每一处都能说出失败的测试、错误码（23505、23503）和根本原因；
- [ ] 练习 3：在两个 psql 窗口里复现了写偏斜，看到 Serializable 的 40001 错误和部分唯一索引的冲突错误；
- [ ] 自测题 11 道都能不看答案讲清楚，尤其是第 5、7、8 题；
- [ ] 能凭记忆写出 `ClaimRun` 那条 SQL 和 Cache-Aside 的读写流程。

做到以上几条，0.6 就可以勾选。模块 00 到此全部完成，下一课进入 **01 LLM 原理与使用**：Transformer、Tokenizer 和采样参数。
