package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// Cache 在 Store 前面加一层 Redis，模式是 Cache-Aside（旁路缓存）：
// 读：先查缓存，未命中查库再回填；写：先写库，再删缓存。
type Cache struct {
	rdb   *redis.Client
	store *Store
	ttl   time.Duration
	sf    singleflight.Group
	loads atomic.Int64 // 实际查库次数，测试用来验证 singleflight
}

func NewCache(rdb *redis.Client, s *Store, ttl time.Duration) *Cache {
	return &Cache{rdb: rdb, store: s, ttl: ttl}
}

const notFoundMarker = "null" // 负缓存：不存在的 id 也缓存一小会儿，防止被同一个不存在的 id 打穿到库

func convKey(id int64) string { return "conv:" + strconv.FormatInt(id, 10) }

// jitter 给 TTL 加 0~10% 的随机量，避免一批同时写入的 key 同时过期（缓存雪崩）。
func (c *Cache) jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int64N(int64(d)/10+1))
}

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
		// 这次查库是替所有等待者做的：不能因为第一个调用方断开就让大家一起失败，
		// 所以去掉它的取消信号，另设一个独立超时
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

// cached 是缓存里读出的一项：要么是会话，要么是“不存在”标记。
type cached struct {
	conv     Conversation
	notFound bool
}

func (c cached) value() (Conversation, error) {
	if c.notFound {
		return Conversation{}, ErrNotFound
	}
	return c.conv, nil
}

// lookup 查缓存。hit=false 表示未命中（包括内容解析失败）；err 只表示 Redis 本身出错。
func (c *Cache) lookup(ctx context.Context, key string) (cached, bool, error) {
	b, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return cached{}, false, nil
	}
	if err != nil {
		return cached{}, false, err
	}
	if string(b) == notFoundMarker {
		return cached{notFound: true}, true, nil
	}
	var conv Conversation
	if err := json.Unmarshal(b, &conv); err != nil {
		return cached{}, false, nil // 结构体改过字段等原因解析失败，当未命中处理
	}
	return cached{conv: conv}, true, nil
}

// RenameConversation 演示写路径：先改库，成功后删缓存（不是更新缓存）。
func (c *Cache) RenameConversation(ctx context.Context, id int64, title string) error {
	if err := c.store.RenameConversation(ctx, id, title); err != nil {
		return err
	}
	return c.rdb.Del(ctx, convKey(id)).Err()
}

func (c *Cache) Loads() int64 { return c.loads.Load() }

// ---------- 限流 ----------

// fixedWindow：INCR 计数，第一次计数时设过期时间。两步放进 Lua 脚本，Redis 保证原子执行。
var fixedWindow = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return n`)

// Allow 按固定窗口限流：每个 key 每个 window 最多 limit 次。返回是否放行和剩余次数。
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
