package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// ServeStdio：一行一条 JSON-RPC 消息。每个请求在自己的 goroutine 里处理，
// 所以响应可能乱序返回，客户端靠 id 对应。stdout 只能写合法的 MCP 消息，日志写 stderr。
func ServeStdio(ctx context.Context, s *Server, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := s.Handle(ctx, line)
			if resp == nil {
				return
			}
			b, _ := json.Marshal(resp)
			mu.Lock()
			out.Write(append(b, '\n'))
			mu.Unlock()
		}()
	}
	wg.Wait()
	return sc.Err() // 读到 EOF：客户端关了 stdin，这是正常的退出信号
}

// StdioTransport 是客户端一侧：写请求、按 id 把响应分发给等待者。
type StdioTransport struct {
	w       io.Writer
	wmu     sync.Mutex
	mu      sync.Mutex
	pending map[string]chan Response
	nextID  atomic.Int64
}

func NewStdioTransport(r io.Reader, w io.Writer) *StdioTransport {
	t := &StdioTransport{w: w, pending: map[string]chan Response{}}
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var resp Response
			if json.Unmarshal(sc.Bytes(), &resp) != nil {
				continue
			}
			t.mu.Lock()
			ch := t.pending[string(resp.ID)]
			delete(t.pending, string(resp.ID))
			t.mu.Unlock()
			if ch != nil {
				ch <- resp
			}
		}
	}()
	return t
}

func (t *StdioTransport) RoundTrip(ctx context.Context, method string, params map[string]any) (Response, error) {
	id := strconv.FormatInt(t.nextID.Add(1), 10)
	ch := make(chan Response, 1)
	t.mu.Lock()
	t.pending[id] = ch
	t.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params})
	t.wmu.Lock()
	_, err := t.w.Write(append(b, '\n'))
	t.wmu.Unlock()
	if err != nil {
		return Response{}, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		// stdio 上取消请求要发 notifications/cancelled（HTTP 上直接关掉响应流即可）
		nb, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
			"params": map[string]any{"requestId": json.RawMessage(id)}})
		t.wmu.Lock()
		t.w.Write(append(nb, '\n'))
		t.wmu.Unlock()
		return Response{}, fmt.Errorf("cancelled: %w", ctx.Err())
	}
}
