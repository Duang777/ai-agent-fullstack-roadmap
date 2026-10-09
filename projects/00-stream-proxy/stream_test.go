package streamproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStreamCancel(t *testing.T) {
	disconnected := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done(): // 客户端断开时触发
				close(disconnected)
				return
			case <-time.After(100 * time.Millisecond):
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", i)
				flusher.Flush() // 不 Flush，数据会留在缓冲区里，客户端收不到
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	tokens, errc := streamTokens(ctx, resp.Body)
	for i := 0; i < 5; i++ {
		<-tokens
	}
	cancel()

	for range tokens { // 读干净，直到后台 goroutine 关闭 tokens
	}
	if err := <-errc; err == nil {
		t.Fatal("want a cancellation error, got nil")
	}

	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the disconnect: 上游连接没断，还在烧钱")
	}
}
