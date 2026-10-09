package streamproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// streamTokens 从 SSE 响应体中读出 token；ctx 取消时立刻退出，不泄漏 goroutine。
func streamTokens(ctx context.Context, body io.ReadCloser) (<-chan string, <-chan error) {
	tokens := make(chan string)
	errc := make(chan error, 1) // 缓冲 1：即使没人读也不会卡住 goroutine

	go func() {
		defer close(errc) // 结束时关闭，调用方读 errc 不会永远阻塞
		defer close(tokens)
		defer body.Close() // 必须关闭，否则连接无法复用

		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 默认单行上限 64KB，大 JSON 会报错
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue // 跳过空行、注释、event: 行
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				return
			}
			var chunk struct {
				Choices []struct {
					Delta struct{ Content string } `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				errc <- fmt.Errorf("bad chunk: %w", err)
				return
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			select {
			case tokens <- chunk.Choices[0].Delta.Content:
			case <-ctx.Done(): // 下游不读了，立刻退出，不泄漏
				errc <- ctx.Err()
				return
			}
		}
		if err := sc.Err(); err != nil {
			errc <- err
		}
	}()
	return tokens, errc
}
