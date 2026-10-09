package streamproxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunTools(t *testing.T) {
	toolTimeout = 2 * time.Second
	delay := map[string]time.Duration{
		"a": 100 * time.Millisecond,
		"b": 200 * time.Millisecond,
		"c": 2500 * time.Millisecond, // 会超时
	}
	execTool = func(ctx context.Context, c ToolCall) (string, error) {
		select {
		case <-time.After(delay[c.Name]):
			return c.Name + " ok", nil
		case <-ctx.Done(): // 工具必须响应取消
			return "", ctx.Err()
		}
	}
	calls := []ToolCall{{ID: "1", Name: "a"}, {ID: "2", Name: "b"}, {ID: "3", Name: "c"}}

	start := time.Now()
	res, err := runTools(context.Background(), calls)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if elapsed < 1900*time.Millisecond || elapsed > 2300*time.Millisecond {
		t.Fatalf("elapsed = %v, want ~2s", elapsed)
	}
	if res[0].Err != nil || res[1].Err != nil {
		t.Fatalf("a/b should succeed: %+v", res)
	}
	if !errors.Is(res[2].Err, context.DeadlineExceeded) {
		t.Fatalf("c should time out, got %v", res[2].Err)
	}
}
