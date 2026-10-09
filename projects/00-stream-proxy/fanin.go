package streamproxy

import (
	"context"
	"sync"
)

// fanIn 把多个输入 channel 合并成一个；ctx 取消或所有输入关闭后，out 被关闭。
func fanIn(ctx context.Context, chs ...<-chan string) <-chan string {
	out := make(chan string)
	var wg sync.WaitGroup
	wg.Add(len(chs))

	for _, ch := range chs {
		go func() {
			defer wg.Done()
			for {
				select {
				case v, ok := <-ch:
					if !ok {
						return // 这一路输入结束
					}
					select {
					case out <- v: // 发送也要能被取消
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		wg.Wait()  // 所有输入都结束（或被取消）后
		close(out) // 由“发送方”统一关闭
	}()
	return out
}
