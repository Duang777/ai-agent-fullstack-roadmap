package loop

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// APIError 是适配层把 HTTP 错误整理后的样子。
type APIError struct {
	Status     int
	Code       string        // 例如 enforced_spend_limit_reached、credit_balance_exhausted
	RetryAfter time.Duration // 0 表示没有 retry-after 头
}

func (e *APIError) Error() string { return fmt.Sprintf("API 错误 %d %s", e.Status, e.Code) }

// 这些 429 不是限流，是钱或额度用完了：重试多少次都一样失败。
var notRetryableCodes = map[string]bool{
	"enforced_spend_limit_reached": true, // Anthropic 月度花费上限
	"credit_balance_exhausted":     true, // OpenAI 预付额度用完
	"insufficient_quota":           true,
}

// Retryable 判断要不要重试。网络错误、超时、429 限流、5xx（含 Anthropic 的 529）重试；
// 400/401/403/404/413 以及额度类 429 不重试，原样报给上层。
func Retryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		return true // 连接被重置、读超时之类：重试
	}
	switch {
	case ae.Status == 429:
		return !notRetryableCodes[ae.Code]
	case ae.Status == 408 || ae.Status == 409:
		return true
	case ae.Status >= 500:
		return true
	}
	return false
}

type Retry struct {
	MaxAttempts int           // 含第一次
	Base, Cap   time.Duration // 退避基数和单次上限
	MaxTotal    time.Duration // 所有等待加起来的上限
	Sleep       func(context.Context, time.Duration) error
	Rand        *rand.Rand
}

// Delay：有 retry-after 就把它当下限，再加一点随机；没有就用“全抖动”指数退避：
// 在 [0, min(Cap, Base*2^attempt)] 里均匀取值，避免很多客户端同一时刻一起重试。
func (r Retry) Delay(attempt int, err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		return ae.RetryAfter + time.Duration(r.Rand.Int63n(int64(r.Base)+1))
	}
	d := r.Base << attempt
	if d <= 0 || d > r.Cap { // <=0 是移位溢出
		d = r.Cap
	}
	return time.Duration(r.Rand.Int63n(int64(d) + 1))
}

type RetryError struct {
	Attempts int
	Last     error
}

func (e *RetryError) Error() string {
	return fmt.Sprintf("重试 %d 次后仍失败：%v", e.Attempts, e.Last)
}
func (e *RetryError) Unwrap() error { return e.Last }

// Do 执行 f，按规则重试。不可重试的错误立即返回；等待会超过 MaxTotal 时不再等。
func (r Retry) Do(ctx context.Context, f func(context.Context) error) error {
	var waited time.Duration
	var err error
	for a := 0; a < r.MaxAttempts; a++ {
		if err = f(ctx); err == nil {
			return nil
		}
		if !Retryable(err) {
			return err
		}
		if a == r.MaxAttempts-1 {
			break
		}
		d := r.Delay(a, err)
		if r.MaxTotal > 0 && waited+d > r.MaxTotal {
			return &RetryError{a + 1, err}
		}
		if e := r.Sleep(ctx, d); e != nil {
			return e
		}
		waited += d
	}
	return &RetryError{r.MaxAttempts, err}
}

func SleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
