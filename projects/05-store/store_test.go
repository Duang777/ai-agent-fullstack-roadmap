package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 集成测试：需要真实的 Postgres 和 Redis。
//
//	make up      # docker compose 起依赖
//	make test
//
// 没设环境变量时跳过，保证 go test ./... 在没有数据库的机器上也能跑。
func setup(t *testing.T) (*Store, *redis.Client) {
	t.Helper()
	dbURL, redisURL := os.Getenv("DATABASE_URL"), os.Getenv("REDIS_URL")
	if dbURL == "" || redisURL == "" {
		t.Skip("DATABASE_URL / REDIS_URL 未设置，跳过集成测试")
	}
	ctx := context.Background()
	s, err := Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// 每个测试从空表开始
	if _, err := s.pool.Exec(ctx, `TRUNCATE conversations, messages, runs RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { rdb.Close() })
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	return s, rdb
}

func text(s string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"text": s})
	return b
}

func TestMigrateIsIdempotent(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	// setup 已经迁移过一次，再并发跑 5 次：咨询锁保证不会重复执行，也不会报错
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applied, err := s.Migrate(ctx)
			if err == nil && len(applied) > 0 {
				err = fmt.Errorf("迁移被重复执行: %v", applied)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n)
	if n != 2 {
		t.Fatalf("schema_migrations 应有 2 行, got %d", n)
	}
}

func TestAppendMessageConcurrentSeq(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	conv, _ := s.CreateConversation(ctx, "u1", "并发追加")

	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.AppendMessage(ctx, conv, "user", text(fmt.Sprint(i)), 3); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	msgs, err := s.ListMessages(ctx, conv, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != n {
		t.Fatalf("want %d messages, got %d", n, len(msgs))
	}
	for i, m := range msgs {
		if m.Seq != i+1 {
			t.Fatalf("seq 不连续: 第 %d 条 seq=%d", i, m.Seq)
		}
	}
	c, _ := s.GetConversation(ctx, conv)
	if c.LastSeq != n {
		t.Fatalf("last_seq want %d got %d", n, c.LastSeq)
	}
}

func TestAppendMessageUnknownConversation(t *testing.T) {
	s, _ := setup(t)
	_, err := s.AppendMessage(context.Background(), 999, "user", text("hi"), 1)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListMessagesResume(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	conv, _ := s.CreateConversation(ctx, "u1", "续读")
	for i := range 10 {
		s.AppendMessage(ctx, conv, "assistant", text(fmt.Sprint(i)), 1)
	}
	// 客户端说“我收到 seq=7 为止”，只应返回 8、9、10
	msgs, _ := s.ListMessages(ctx, conv, 7, 100)
	if len(msgs) != 3 || msgs[0].Seq != 8 || msgs[2].Seq != 10 {
		t.Fatalf("续读错误: %+v", msgs)
	}
}

func TestListConversationsCursor(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	for i := range 7 {
		s.CreateConversation(ctx, "u1", fmt.Sprint("c", i))
	}
	s.CreateConversation(ctx, "u2", "别人的")
	// 所有会话的 updated_at 设成同一时刻：排序只能靠 id 兜底，这正是最容易出错的情况
	s.pool.Exec(ctx, `UPDATE conversations SET updated_at = '2026-10-11T00:00:00Z'`)

	var seen []int64
	var cur *Cursor
	pages := 0
	for {
		list, next, err := s.ListConversations(ctx, "u1", cur, 3)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, c := range list {
			seen = append(seen, c.ID)
		}
		if next == nil {
			break
		}
		cur = next
	}
	if pages != 3 || len(seen) != 7 {
		t.Fatalf("want 3 pages / 7 rows, got %d / %d: %v", pages, len(seen), seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] {
			t.Fatalf("顺序错误或重复: %v", seen)
		}
	}
}

func TestEnqueueRunIdempotent(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	conv, _ := s.CreateConversation(ctx, "u1", "幂等")

	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[int64]bool{}
	created := 0
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, ok, err := s.EnqueueRun(ctx, conv, "req-abc", "gpt-x")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			ids[r.ID] = true
			if ok {
				created++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(ids) != 1 || created != 1 {
		t.Fatalf("同一幂等键应只建 1 条, ids=%v created=%d", ids, created)
	}
}

func TestClaimRunSkipLocked(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	conv, _ := s.CreateConversation(ctx, "u1", "队列")
	const jobs = 30
	for i := range jobs {
		s.EnqueueRun(ctx, conv, fmt.Sprint("job-", i), "m")
	}

	var mu sync.Mutex
	claimed := map[int64]string{}
	var wg sync.WaitGroup
	for w := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprint("worker-", w)
			for {
				r, err := s.ClaimRun(ctx, name)
				if errors.Is(err, ErrNotFound) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if prev, dup := claimed[r.ID]; dup {
					t.Errorf("run %d 被 %s 和 %s 重复领取", r.ID, prev, name)
				}
				claimed[r.ID] = name
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != jobs {
		t.Fatalf("want %d claimed, got %d", jobs, len(claimed))
	}
}

func TestFinishRunGuardsState(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	conv, _ := s.CreateConversation(ctx, "u1", "状态机")
	r, _, _ := s.EnqueueRun(ctx, conv, "k1", "m")

	if err := s.FinishRun(ctx, r.ID, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("queued 状态不能直接结束, got %v", err)
	}
	s.ClaimRun(ctx, "w")
	if err := s.FinishRun(ctx, r.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, r.ID, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("已结束的 run 不能再结束, got %v", err)
	}
	got, _ := s.GetRun(ctx, r.ID)
	if got.Status != "succeeded" {
		t.Fatalf("status = %s", got.Status)
	}
}

// addUsageNaive 是反例：先读出来，在 Go 里加，再写回去。并发时会丢更新。
func addUsageNaive(ctx context.Context, s *Store, runID, in int64) error {
	var cur int64
	if err := s.pool.QueryRow(ctx, `SELECT input_tokens FROM runs WHERE id = $1`, runID).Scan(&cur); err != nil {
		return err
	}
	time.Sleep(time.Millisecond) // 放大读和写之间的窗口，让问题稳定复现
	_, err := s.pool.Exec(ctx, `UPDATE runs SET input_tokens = $2 WHERE id = $1`, runID, cur+in)
	return err
}

func TestLostUpdate(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	conv, _ := s.CreateConversation(ctx, "u1", "计费")
	naive, _, _ := s.EnqueueRun(ctx, conv, "naive", "m")
	atomic, _, _ := s.EnqueueRun(ctx, conv, "atomic", "m")

	const n = 40
	var wg sync.WaitGroup
	for range n {
		wg.Add(2)
		go func() { defer wg.Done(); addUsageNaive(ctx, s, naive.ID, 10) }()
		go func() { defer wg.Done(); s.AddUsage(ctx, atomic.ID, 10, 1) }()
	}
	wg.Wait()

	a, _ := s.GetRun(ctx, atomic.ID)
	if a.InputTokens != n*10 || a.OutputTokens != n {
		t.Fatalf("原子累加结果错误: in=%d out=%d", a.InputTokens, a.OutputTokens)
	}
	b, _ := s.GetRun(ctx, naive.ID)
	t.Logf("读-改-写: 期望 %d, 实际 %d（丢了 %d）", n*10, b.InputTokens, n*10-b.InputTokens)
	if b.InputTokens == n*10 {
		t.Fatalf("预期读-改-写会丢更新，但这次没丢；重跑一次或加大 n")
	}
}

func TestCacheAsideSingleflight(t *testing.T) {
	s, rdb := setup(t)
	ctx := context.Background()
	id, _ := s.CreateConversation(ctx, "u1", "旧标题")
	c := NewCache(rdb, s, time.Minute)

	// 100 个并发请求同时未命中，只应查一次库
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.GetConversation(ctx, id); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if c.Loads() != 1 {
		t.Fatalf("want 1 load, got %d", c.Loads())
	}
	ttl := rdb.TTL(ctx, convKey(id)).Val()
	if ttl < time.Minute || ttl > 66*time.Second {
		t.Fatalf("TTL 应在 60~66 秒之间（含抖动）, got %v", ttl)
	}

	// 写路径：改库 + 删缓存，下一次读拿到新值
	if err := c.RenameConversation(ctx, id, "新标题"); err != nil {
		t.Fatal(err)
	}
	conv, _ := c.GetConversation(ctx, id)
	if conv.Title != "新标题" || c.Loads() != 2 {
		t.Fatalf("删缓存后应重新加载: title=%q loads=%d", conv.Title, c.Loads())
	}
}

func TestCacheNegativeAndFallback(t *testing.T) {
	s, rdb := setup(t)
	ctx := context.Background()
	c := NewCache(rdb, s, time.Minute)

	// 不存在的 id：第一次查库，之后命中负缓存
	for range 5 {
		if _, err := c.GetConversation(ctx, 424242); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
	if c.Loads() != 1 {
		t.Fatalf("负缓存未生效, loads=%d", c.Loads())
	}

	// Redis 不可用时降级查库，而不是报错
	id, _ := s.CreateConversation(ctx, "u1", "降级")
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer dead.Close()
	conv, err := NewCache(dead, s, time.Minute).GetConversation(ctx, id)
	if err != nil || conv.Title != "降级" {
		t.Fatalf("Redis 挂了应降级查库: %v %+v", err, conv)
	}
}

func TestRateLimitFixedWindow(t *testing.T) {
	_, rdb := setup(t)
	ctx := context.Background()
	window := 300 * time.Millisecond

	for i := range 5 {
		ok, left, err := Allow(ctx, rdb, "u1", 5, window)
		if err != nil || !ok || left != 4-i {
			t.Fatalf("第 %d 次应放行, ok=%v left=%d err=%v", i+1, ok, left, err)
		}
	}
	if ok, _, _ := Allow(ctx, rdb, "u1", 5, window); ok {
		t.Fatal("第 6 次应被拒绝")
	}
	if ok, _, _ := Allow(ctx, rdb, "u2", 5, window); !ok {
		t.Fatal("不同用户互不影响")
	}
	time.Sleep(window + 50*time.Millisecond)
	if ok, _, _ := Allow(ctx, rdb, "u1", 5, window); !ok {
		t.Fatal("窗口过期后应恢复")
	}
}
