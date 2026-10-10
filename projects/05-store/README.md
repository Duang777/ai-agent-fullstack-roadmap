# 05-store · 0.6 数据库与缓存练习

对应课件：[`notes/00-foundations/06-database-cache.md`](../../notes/00-foundations/06-database-cache.md)

Agent 平台的持久层：Postgres 存会话、消息和运行记录，Redis 做缓存和限流。用 `pgx/v5` 和 `go-redis/v9`，不用 ORM。

| 文件 | 内容 |
|---|---|
| `migrations/*.sql` | 建表、约束、索引（含部分索引）；`0002` 演示安全加列 |
| `migrate.go` | 嵌入式迁移执行器：`schema_migrations` 记录版本 + 咨询锁防多实例并发迁移 |
| `store.go` | 并发安全的消息序号、键集分页、幂等建任务、`FOR UPDATE SKIP LOCKED` 任务队列、状态机守卫、原子累加 |
| `cache.go` | Cache-Aside + singleflight 防击穿 + 负缓存防穿透 + TTL 抖动防雪崩 + Redis 故障降级；Lua 固定窗口限流 |
| `store_test.go` | 12 个集成测试，包括一个故意复现“丢失更新”的反例 |

```bash
make up      # docker compose 启动 Postgres 17 + Redis 7
make test    # go vet + go test -race（未设 DATABASE_URL / REDIS_URL 时集成测试自动跳过）
make psql    # 进数据库手动做课件里的隔离级别实验
make down
```
