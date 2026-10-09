package streamproxy

import (
	"context"
	"sort"
	"testing"
	"time"
)

func produce(vals ...string) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		for _, v := range vals {
			ch <- v
		}
	}()
	return ch
}

func TestFanInMergesAll(t *testing.T) {
	out := fanIn(context.Background(), produce("a", "b"), produce("c"), produce("d", "e"))
	var got []string
	for v := range out {
		got = append(got, v)
	}
	sort.Strings(got)
	want := []string{"a", "b", "c", "d", "e"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestFanInCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	never := make(chan string) // 永远不发也不关
	out := fanIn(ctx, never, never)
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected out to be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("fanIn did not exit after cancel")
	}
}
