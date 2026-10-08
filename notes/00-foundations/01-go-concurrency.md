# 0.1 Go 并发：Agent 运行时的地基

> 第 0 部分 · 第 1 课 ｜ 预计 2–3 小时 ｜ 前置：会写基本 Go 语法

## 为什么先学这个

一个生产级 Agent 每一轮大致在做：

```
调用 LLM（流式）→ 解析出 N 个 tool_call → 并行执行工具 → 汇总结果 → 下一轮
                     ↑ 任一环节都要：超时、取消、限流、不泄漏
```

这就是一个“带预算的并发调度器”。Go 的 goroutine + channel + context 正好是为这件事设计的。本课结束时，你能写出 Agent Loop 里最核心的两段代码：**并行工具执行** 和 **可取消的流式读取**。

---

## 1. goroutine 与 channel：三条规则

```go
results := make(chan string)      // 无缓冲：发送会阻塞，直到有人接收
go func() {
    results <- "tool A done"
}()
fmt.Println(<-results)
```

记住三条规则，能避开 80% 的 bug：

1. **谁发送，谁关闭。** 接收方永远不要 `close`，关闭后再发送会 panic。
2. **每启动一个 goroutine，都要想清楚它怎么结束。** 想不清楚 = 泄漏。
3. **无缓冲 channel 是同步点，有缓冲 channel 是队列。** 缓冲大小 = 允许“发了没人收”的数量。

`for range ch` 会一直读到 channel 被关闭：

```go
func produce(n int) <-chan int {
    ch := make(chan int)
    go func() {
        defer close(ch) // 规则 1：发送方关闭
        for i := 0; i < n; i++ {
            ch <- i
        }
    }()
    return ch
}

for v := range produce(3) { fmt.Println(v) } // 0 1 2
```

## 2. select：同时等多件事

```go
select {
case tok := <-tokens:
    fmt.Print(tok)
case <-time.After(30 * time.Second):
    return errors.New("LLM 30 秒没吐 token")
case <-ctx.Done():
    return ctx.Err() // 用户取消 / 整体超时
}
```

`select` 会阻塞到某个 case 就绪；多个就绪时随机选一个。Agent 里最常见的就是“等数据 OR 等取消”。

## 3. context：取消信号的传播链

用户关掉网页 → HTTP handler 的 `r.Context()` 被取消 → 你传给 LLM 调用和工具的 ctx 全部被取消 → 上游连接关闭、不再计费。**这条链断了，就是在烧钱。**

```go
func handleChat(w http.ResponseWriter, r *http.Request) {
    // 整轮对话最多 60 秒；客户端断开也会触发取消
    ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
    defer cancel() // 必写，否则计时器资源泄漏

    if err := runAgent(ctx); err != nil {
        if errors.Is(err, context.Canceled) {
            slog.Info("client gone, stopped")
            return
        }
        http.Error(w, err.Error(), 500)
    }
}
```

规范：
- ctx 永远是函数的**第一个参数**，不要塞进 struct。
- 下层函数只读 `ctx.Done()`，不负责 `cancel`；谁创建谁 cancel。
- 发起 HTTP 请求用 `http.NewRequestWithContext(ctx, ...)`，取消会自动断开连接。

## 4. 并行工具调用：errgroup

模型一次返回 3 个 tool_call，串行执行要 3×延迟，并行只要最慢那个。

```go
import "golang.org/x/sync/errgroup"

type ToolCall struct {
    ID   string
    Name string
    Args json.RawMessage // 先不解析，交给具体工具
}

type ToolResult struct {
    ID     string
    Output string
    Err    error
}

func runTools(ctx context.Context, calls []ToolCall) ([]ToolResult, error) {
    g, ctx := errgroup.WithContext(ctx)
    g.SetLimit(4) // 最多同时 4 个，防止打爆下游
    results := make([]ToolResult, len(calls)) // 每个 goroutine 写自己的下标，无需加锁

    for i, c := range calls {
        g.Go(func() error { // Go 1.22+ 循环变量每轮独立，可直接捕获
            tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
            defer cancel()
            out, err := execTool(tctx, c)
            results[i] = ToolResult{ID: c.ID, Output: out, Err: err}
            return nil // 单个工具失败不中断其他工具，把错误交给模型
        })
    }
    if err := g.Wait(); err != nil {
        return nil, err
    }
    return results, nil
}
```

**设计取舍（面试常问）**：工具失败时返回 `nil` 而不是 `err`。因为对 Agent 来说，“工具报错”是一条有用的观察，应该作为 `tool_result` 回传给模型让它自我修正；只有“基础设施故障”（如 ctx 被取消）才应该中断整轮。

## 5. 可取消的流式读取：解析 SSE

LLM 流式接口返回的是 SSE：

```
data: {"choices":[{"delta":{"content":"你"}}]}

data: {"choices":[{"delta":{"content":"好"}}]}

data: [DONE]
```

```go
func streamTokens(ctx context.Context, body io.ReadCloser) (<-chan string, <-chan error) {
    tokens := make(chan string)
    errc := make(chan error, 1) // 缓冲 1：即使没人读也不会卡住 goroutine

    go func() {
        defer close(errc)   // 结束时关闭，调用方读 errc 不会永远阻塞
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
```

调用方这样用：

```go
tokens, errc := streamTokens(ctx, resp.Body)
for tok := range tokens { // 一直读到 tokens 被关闭
    fmt.Print(tok)
}
if err := <-errc; err != nil { // errc 已关闭且无值时拿到 nil
    return err
}
```

### 逐段解读

- **SSE 是什么**：服务器不一次性返回结果，而是保持连接、一行行推文本。每个事件以 `data: ` 开头，空行分隔。模型每生成一小段就推一个事件，`delta.content` 是这次新增的文字；`[DONE]` 是 OpenAI 约定的结束标记。
- **为什么返回两个 channel**：函数立即返回，真正的读取在后台 goroutine 里做。`tokens` 传正常数据，`errc` 传“为什么结束”。调用方用 `for range` 边收边显示。
- **三个 defer**：退出时按“后进先出”执行——先关 body（释放连接），再关 tokens（让调用方的 `for range` 结束），最后关 errc。
- **`sc.Buffer`**：Scanner 默认一行最多 64KB，带长工具参数的 chunk 可能超过，所以放宽到 1MB。
- **只取需要的字段**：匿名 struct 只声明 `choices[].delta.content`，其余字段 JSON 解码时自动忽略；Go 的 JSON 字段名匹配不区分大小写，所以 `Content` 能对上 `content`。
- **`len(Choices) == 0`**：有些 chunk（比如最后带 usage 统计的那条）没有 choices，直接跳过。
- **为什么 errc 缓冲 1**：出错时 goroutine 先往 errc 发错误再退出。若无缓冲，它会卡在发送上，而调用方还卡在 `for range tokens` 等关闭——互相等待，死锁。缓冲 1 让发送立刻完成。

关键点：**每一次 channel 发送都要和 `ctx.Done()` 放进同一个 select。** 否则下游走了，这个 goroutine 会永远卡在 `tokens <- ...`。

## 6. 三个高频坑

| 坑 | 症状 | 修法 |
|---|---|---|
| goroutine 泄漏 | 内存、goroutine 数只涨不跌 | 所有阻塞发送配 `ctx.Done()`；测试里检查 `runtime.NumGoroutine()` |
| response body 没关 | 连接耗尽、请求越来越慢 | `defer resp.Body.Close()`，必要时 `io.Copy(io.Discard, body)` |
| map 并发写 | `fatal error: concurrent map writes` | 按下标写 slice，或用 `sync.Mutex`；跑测试加 `-race` |

---

## 练习（今天做完）

1. **基础**：写 `fanIn(ctx, chs ...<-chan string) <-chan string`，把多个 channel 合并成一个；ctx 取消后所有 goroutine 退出。
2. **核心**：用第 4 节的 `runTools`，写三个假工具（分别 sleep 1s / 2s / 25s），验证：总耗时约 20s（第三个超时）、超时工具的错误被写进 `ToolResult.Err`。
3. **进阶**：用 `httptest.NewServer` 写一个假 SSE 服务端，每 100ms 吐一个 token；客户端读 5 个后 cancel，断言服务端检测到连接断开（handler 里 `<-r.Context().Done()`）。
4. 所有测试用 `go test -race` 跑通。

## 自测题

1. 为什么 `errc` 要用缓冲 1 的 channel？换成无缓冲会发生什么？
2. `errgroup.WithContext` 返回的 ctx，在什么情况下会被取消？
3. 工具执行失败时，为什么本课选择 `return nil`？什么情况下应该 `return err`？
4. `context.WithTimeout` 之后不调用 `cancel()`，会有什么后果？

> 做完练习后，把代码放到 `projects/00-stream-proxy/`，在聊天里告诉我结果或卡点，我来批改并写当天日志。
