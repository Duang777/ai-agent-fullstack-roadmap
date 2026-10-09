# 0.1 Go 并发：Agent 运行时的地基

> 第 0 部分 · 第 1 课 ｜ 预计 3–4 小时 ｜ 前置：会写基本 Go 语法（变量、函数、struct、slice、map）
>
> 本课所有代码都对应仓库里能跑的实现：`projects/00-stream-proxy/`（`go test -race -count=1 ./...` 通过）。

## 读前说明

这一课代码密度很高。第一次读时如果某段看不懂，**先看它下面的「逐行解读」和「时间线」**，再回头看代码。每段代码都按三步讲：

1. **它要解决什么问题**（一句话）
2. **代码**（关键行带注释）
3. **逐行解读 + 容易错的地方**

不需要一次全懂。目标是：读完能说清楚 Agent 里“并行跑工具”和“流式读模型输出”这两件事，在 Go 里是怎么做到**不卡死、不泄漏、能取消**的。

---

## 为什么先学这个

一个生产级 Agent 每一轮大致在做：

```
调用 LLM（流式）→ 解析出 N 个 tool_call → 并行执行工具 → 汇总结果 → 下一轮
                     ↑ 任一环节都要：超时、取消、限流、不泄漏
```

翻成大白话：

- **流式**：模型不是一次性回一整段话，而是一个字一个字往外吐，你要边收边显示。
- **并行**：模型说“帮我同时查天气、查日历、搜网页”，三件事要一起做，不能一个个排队。
- **超时**：某个工具卡住了，不能让整轮对话陪它一起等。
- **取消**：用户关了页面，后台所有活都要立刻停，不然还在花钱调模型。
- **不泄漏**：每个后台任务都必须有结束的那一刻，不能越积越多把内存吃光。

这就是一个“带预算的并发调度器”。Go 的 **goroutine + channel + context** 三件套正好是为这件事设计的。本课结束时，你能写出 Agent Loop 里最核心的两段代码：**并行工具执行** 和 **可取消的流式读取**。

---

## 0. 先补 6 个语法点（看代码前必读）

后面的代码会反复用到下面这些写法。熟悉的可以跳过。

### 0.1 `go` 关键字：开一个后台任务

```go
go doSomething()        // 在后台执行 doSomething，当前代码不等它，继续往下走

go func() {             // 也可以直接写一个匿名函数丢到后台
    fmt.Println("我在后台跑")
}()                     // 注意最后的 ()：定义完立刻调用
```

`go` 启动的东西叫 **goroutine**，可以理解成“非常便宜的线程”，开几万个都没问题。但它有个特点：**启动它的函数返回了，它也不会自动停**。所以后面会一直强调“每个 goroutine 怎么结束”。

### 0.2 channel：goroutine 之间传数据的管道

```go
ch := make(chan string)     // 造一根能传 string 的管道（无缓冲）
ch2 := make(chan string, 3) // 带缓冲：管道里最多能“存” 3 个值

ch <- "hello"   // 往管道里放（发送）
v := <-ch       // 从管道里取（接收）
close(ch)       // 关闭管道：告诉接收方“不会再有数据了”
```

**箭头方向就是数据流向**：`ch <- x` 是 x 流进 ch，`<-ch` 是从 ch 流出来。

### 0.3 只读 / 只写 channel 类型

```go
func produce() <-chan int   // 返回值类型 <-chan int：调用方只能从里面读
func consume(ch chan<- int) // 参数类型 chan<- int：函数只能往里面写
```

这是**给编译器看的约束**：函数返回 `<-chan`，调用方就不可能误往里写、也不可能误 `close`。本课的函数都返回 `<-chan`。

### 0.4 `defer`：函数退出时一定执行

```go
func f() {
    defer fmt.Println("3")  // 先登记，最后才执行
    defer fmt.Println("2")
    fmt.Println("1")
}
// 输出：1 2 3  —— 多个 defer 按“后登记先执行”的顺序
```

不管函数是正常 return、提前 return 还是中途出错 return，defer 都会执行。所以“关连接、关 channel、释放资源”都写在 defer 里，不会漏。

### 0.5 闭包：匿名函数能直接用外面的变量

```go
results := make([]string, 3)
for i := 0; i < 3; i++ {
    go func() {
        results[i] = "done" // 直接用外面的 results 和 i
    }()
}
```

**Go 1.22 起**，`for` 循环每一轮的 `i` 都是新变量，所以上面每个 goroutine 拿到的 `i` 是自己那一轮的。（老版本里所有 goroutine 共享同一个 `i`，是经典 bug。本仓库用 Go 1.23。）

### 0.6 匿名 struct 和 JSON 标签

```go
var chunk struct {
    Name string `json:"name"`  // 反引号里是“标签”：JSON 里的 name 字段解析到这里
}
json.Unmarshal([]byte(`{"name":"go","age":3}`), &chunk)
// chunk.Name == "go"，age 没声明，自动忽略
```

临时只用一次的结构，不用专门起名字，直接 `var x struct{...}`。JSON 里多出来的字段会被忽略，所以**只声明你需要的字段**就行。

---

## 1. goroutine 与 channel：三条规则

**要解决的问题**：后台任务干完活，怎么把结果交回来？

```go
results := make(chan string)      // 无缓冲 channel
go func() {
    results <- "tool A done"      // 后台：把结果放进管道
}()
fmt.Println(<-results)            // 前台：在这里等，直到拿到结果
```

**时间线**：

```
主 goroutine:  创建 channel → 启动后台 → 执行 <-results（卡住等）……………… 拿到值，打印
后台 goroutine:                          执行 results <- "..."（等有人接）→ 交接完成，结束
```

无缓冲 channel 像“当面交接”：发送方和接收方**必须同时到场**，任何一方先到都要等另一方。

记住三条规则，能避开 80% 的 bug：

1. **谁发送，谁关闭。** 接收方永远不要 `close`。关闭后再发送会 panic（程序直接崩）。
2. **每启动一个 goroutine，都要想清楚它怎么结束。** 想不清楚 = 泄漏。
3. **无缓冲 channel 是同步点，有缓冲 channel 是队列。** 缓冲大小 = 允许“发了还没人收”的数量。

### 生产者模式：返回一个 channel

**要解决的问题**：函数要“源源不断”产出数据，调用方边收边处理。

```go
func produce(n int) <-chan int {
    ch := make(chan int)
    go func() {
        defer close(ch) // 规则 1：发送方负责关闭
        for i := 0; i < n; i++ {
            ch <- i     // 每放一个，都要等调用方取走
        }
    }()                 // 后台开始生产
    return ch           // 立刻返回管道，不等生产完
}

for v := range produce(3) { fmt.Println(v) } // 0 1 2
```

**逐行解读**

- `produce` 本身几乎瞬间返回，它只是“造管道 + 启动工人”。真正的生产在后台 goroutine 里。
- `for v := range ch`：反复从 ch 取值，**直到 ch 被关闭且取空**才退出循环。
- 如果忘了 `close(ch)`：生产完 3 个后，调用方的 `range` 还在等第 4 个，永远等不到 → 程序卡死。这就是为什么 `defer close(ch)` 是固定搭配。
- 后台 goroutine 怎么结束？循环跑完 → defer 关闭 → 函数返回。**结束路径清清楚楚**，符合规则 2。

> 这个“函数立刻返回一个 channel，后台 goroutine 往里写，写完关闭”的模式，第 5 节的 `streamTokens` 和练习 1 的 `fanIn` 都是它的升级版。先把它看懂。

---

## 2. select：同时等多件事

**要解决的问题**：等模型吐字的时候，还要同时盯着“超时了没”“用户走了没”。

```go
select {
case tok := <-tokens:                       // 情况 A：等到一个 token
    fmt.Print(tok)
case <-time.After(30 * time.Second):        // 情况 B：30 秒都没等到
    return errors.New("LLM 30 秒没吐 token")
case <-ctx.Done():                          // 情况 C：被取消了
    return ctx.Err()
}
```

**逐行解读**

- `select` 会**同时**挂在所有 case 上，哪个先就绪就执行哪个，其余的放弃。像同时等三个人的电话，谁先打来接谁。
- `time.After(30 * time.Second)` 返回一个 channel，30 秒后会自动往里放一个值。所以它可以当“闹钟”用。
- `ctx.Done()` 也是一个 channel，ctx 被取消时它会被关闭（关闭的 channel 一读就能读到，所以 case 立刻就绪）。下一节详细讲 ctx。
- 多个 case 同时就绪时，Go **随机**挑一个，不保证顺序。

Agent 里最常见的写法就是“**等数据 OR 等取消**”，后面几乎每段代码都有。

---

## 3. context：取消信号的传播链

**要解决的问题**：用户关掉网页后，怎么让后台所有正在跑的工作都停下来？

先看链条：

```
用户关掉网页
  → HTTP 服务端发现连接断了，r.Context() 被取消
    → 你传给 LLM 调用的 ctx 被取消 → 到模型供应商的连接断开，停止计费
    → 你传给每个工具的 ctx 被取消 → 工具停止执行
```

`context.Context`（简称 ctx）就是这根“取消信号线”。它是树状的：父 ctx 被取消，所有从它派生出来的子 ctx 都会被取消。**这条链断了，就是在烧钱。**

```go
func handleChat(w http.ResponseWriter, r *http.Request) {
    // 从请求的 ctx 派生一个子 ctx，并加上“最多 60 秒”的限制
    ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
    defer cancel() // 必写：函数结束时主动释放计时器

    if err := runAgent(ctx); err != nil {   // 把 ctx 一路往下传
        if errors.Is(err, context.Canceled) {
            slog.Info("client gone, stopped") // 用户自己走了，不算错误
            return
        }
        http.Error(w, err.Error(), 500)
    }
}
```

**逐行解读**

- `r.Context()`：Go 的 HTTP 服务端给每个请求自带一个 ctx，客户端断开时它会自动被取消。
- `context.WithTimeout(父, 60s)` 返回两样东西：
  - `ctx`：新的子 ctx。父 ctx 取消、或者 60 秒到了，它都会被取消（两个条件谁先到算谁）。
  - `cancel`：一个函数，调用它可以**手动**取消这个子 ctx。
- `defer cancel()`：活提前干完了也要调一下，告诉系统“这个计时器不用了”。不调的后果见文末自测题 4。
- `errors.Is(err, context.Canceled)`：判断错误是不是“被取消”造成的。用户主动离开不该报 500。

**ctx 被取消后会发生什么？** 只做一件事：`ctx.Done()` 这个 channel 被关闭，`ctx.Err()` 开始返回非 nil。**它不会强行杀掉任何代码**。下游代码必须自己去 `select` 监听 `ctx.Done()`，才会真的停。这一点后面会反复体现。

规范（照着写就行）：

- ctx 永远是函数的**第一个参数**：`func runAgent(ctx context.Context, ...)`，不要塞进 struct。
- 下层函数只负责**监听** `ctx.Done()`，不负责 `cancel`；谁创建谁 cancel。
- 发 HTTP 请求用 `http.NewRequestWithContext(ctx, ...)`，ctx 取消时连接会自动断开，不用你自己管。

---

## 4. 并行工具调用：errgroup

**要解决的问题**：模型一次要求调用 3 个工具，怎么同时跑、每个有超时、最多同时跑几个、一个失败不影响其他？

串行要 3 × 延迟，并行只要最慢那个：

```
串行：[ 工具A 1s ][ 工具B 2s ][ 工具C 3s ]          = 6s
并行：[ 工具A 1s ]
      [ 工具B 2s     ]
      [ 工具C 3s          ]                        = 3s
```

`errgroup` 是 Go 官方扩展库里的工具，可以理解成“**一组 goroutine + 等它们全部结束 + 收集错误 + 限制并发数**”的打包方案。

```go
import "golang.org/x/sync/errgroup"

// 模型返回的一次工具调用请求
type ToolCall struct {
    ID   string          // 模型给的调用 ID，回传结果时要带上，模型靠它对号入座
    Name string          // 工具名，比如 "search"
    Args json.RawMessage // 参数原文（一段 JSON），先不解析，交给具体工具
}

// 工具执行完的结果
type ToolResult struct {
    ID     string
    Output string
    Err    error
}

func runTools(ctx context.Context, calls []ToolCall) ([]ToolResult, error) {
    g, ctx := errgroup.WithContext(ctx)           // ① 建一个任务组
    g.SetLimit(4)                                 // ② 最多同时跑 4 个
    results := make([]ToolResult, len(calls))     // ③ 提前开好结果数组

    for i, c := range calls {
        g.Go(func() error {                       // ④ 每个工具一个 goroutine
            tctx, cancel := context.WithTimeout(ctx, 20*time.Second) // ⑤ 单工具超时
            defer cancel()
            out, err := execTool(tctx, c)         // ⑥ 真正执行工具
            results[i] = ToolResult{ID: c.ID, Output: out, Err: err} // ⑦ 写自己的格子
            return nil                            // ⑧ 失败也返回 nil
        })
    }
    if err := g.Wait(); err != nil {              // ⑨ 等所有工具结束
        return nil, err
    }
    return results, nil
}
```

**逐行解读**

- **①** `errgroup.WithContext(ctx)` 返回任务组 `g` 和一个**新的** ctx。注意这里把外面的 `ctx` 变量覆盖了，下面用的都是这个新 ctx。它的特点：组里任何一个任务返回了错误，它就会被取消，其他任务就能感知到“该停了”。
- **②** `SetLimit(4)`：同时最多 4 个在跑。第 5 个调用 `g.Go` 时会**等**，直到前面有一个结束空出位置。防止模型一次要 50 个工具把下游打爆。
- **③** 结果用**提前开好长度的 slice**，每个 goroutine 只写 `results[i]` 自己那一格。不同下标互不干扰，所以**不需要加锁**。如果换成 map 一起写，会直接崩（见第 6 节）。
- **④** `g.Go(func() error {...})`：相当于 `go func(){...}()`，但 errgroup 会帮你记账，`Wait` 时知道还有几个没完。闭包里直接用 `i` 和 `c` 是安全的（Go 1.22+，见 0.5）。
- **⑤** 从组 ctx 再派生一个 20 秒超时的 `tctx`，只给这一个工具用。于是这个工具会在三种情况下被要求停：自己超过 20 秒、组里别人报错、最外层用户取消。
- **⑥** `execTool` 是你实现的具体工具（查天气、跑代码……）。它**必须**监听 `tctx`，否则超时没用，见练习 2 的解读。
- **⑦** 结果按下标写，顺序和 `calls` 一一对应。回传给模型时 `ID` 不会错位。
- **⑧** 关键设计：工具失败了**不返回 err**，而是把错误塞进 `ToolResult.Err`。
- **⑨** `g.Wait()` 阻塞到组里所有 goroutine 都结束，返回其中第一个非 nil 错误（这里永远是 nil，因为 ⑧）。

**设计取舍（面试常问）**：为什么工具失败返回 `nil`？因为对 Agent 来说，“工具报错”是一条**有用的信息**，比如“文件不存在”“参数格式不对”。把它作为 `tool_result` 回传给模型，模型通常会换个参数重试。如果 `return err`，errgroup 会取消其他正在跑的工具，整轮白做。只有“基础设施故障”（比如用户已经走了）才应该中断整轮，做法见自测题 3。

---

## 5. 可取消的流式读取：解析 SSE

**要解决的问题**：模型的流式接口一行一行推数据过来，怎么边读边把文字交给前端，同时在用户取消时立刻停下、不留后台垃圾？

### 5.1 SSE 长什么样

LLM 流式接口返回的是 SSE（Server-Sent Events），本质就是一段**不断变长的纯文本**：

```
data: {"choices":[{"delta":{"content":"你"}}]}

data: {"choices":[{"delta":{"content":"好"}}]}

data: [DONE]
```

- 每个事件一行，以 `data: ` 开头，事件之间空一行。
- `delta.content` 是“这次**新增**的文字”，不是全文。把所有 delta 拼起来才是完整回答。
- `[DONE]` 是 OpenAI 格式约定的结束标记。

### 5.2 整体设计

```
              ┌──────────── 后台 goroutine ────────────┐
HTTP body ──▶ │ 一行行读 → 过滤 data: → 解析 JSON → 取字 │ ──▶ tokens  （正常数据，一个个字）
              │            出错 / 被取消 / 读完        │ ──▶ errc    （为什么结束，最多一个值）
              └────────────────────────────────────────┘
```

函数**立刻返回两根管道**：`tokens` 送文字，`errc` 送“结束原因”。分开的好处：调用方先 `for range tokens` 专心收文字，收完再看一眼 `errc` 知道是正常结束还是出错。

### 5.3 代码

```go
func streamTokens(ctx context.Context, body io.ReadCloser) (<-chan string, <-chan error) {
    tokens := make(chan string)     // 无缓冲：调用方读一个，这边才发下一个
    errc := make(chan error, 1)     // 缓冲 1：放错误时不用等人来取

    go func() {
        defer close(errc)   // 第三个执行
        defer close(tokens) // 第二个执行
        defer body.Close()  // 第一个执行（defer 后登记先执行）

        sc := bufio.NewScanner(body)                     // 按行读
        sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)  // 单行上限放宽到 1MB
        for sc.Scan() {                                  // 每次读一行，读不到就结束循环
            line := sc.Text()
            if !strings.HasPrefix(line, "data: ") {
                continue // 跳过空行、注释、event: 行
            }
            data := strings.TrimPrefix(line, "data: ")
            if data == "[DONE]" {
                return // 正常结束：errc 里什么都不放
            }
            var chunk struct {
                Choices []struct {
                    Delta struct{ Content string } `json:"delta"`
                } `json:"choices"`
            }
            if err := json.Unmarshal([]byte(data), &chunk); err != nil {
                errc <- fmt.Errorf("bad chunk: %w", err) // JSON 坏了：报错并退出
                return
            }
            if len(chunk.Choices) == 0 {
                continue // 有的 chunk 没有 choices（比如只带用量统计），跳过
            }
            select {
            case tokens <- chunk.Choices[0].Delta.Content: // 把字交出去
            case <-ctx.Done():                              // 或者：调用方不要了
                errc <- ctx.Err()
                return
            }
        }
        if err := sc.Err(); err != nil { // 循环结束不是因为读完，而是读出错（比如断网）
            errc <- err
        }
    }()
    return tokens, errc
}
```

调用方这样用：

```go
tokens, errc := streamTokens(ctx, resp.Body)
for tok := range tokens {          // 一直读到 tokens 被关闭
    fmt.Print(tok)
}
if err := <-errc; err != nil {     // 正常结束时 errc 里没值且已关闭，读到 nil
    return err
}
```

### 5.4 逐段解读

**函数签名**

- 参数 `body io.ReadCloser`：就是 HTTP 响应体 `resp.Body`，能读、也能关。
- 返回 `(<-chan string, <-chan error)`：两根**只读**管道，调用方没法误写误关（见 0.3）。

**三个 defer（顺序很重要）**

后台 goroutine 不管从哪个 `return` 退出，都会按这个顺序收尾：

1. `body.Close()`：关闭 HTTP 响应体，释放网络连接，让它能被下一个请求复用。
2. `close(tokens)`：通知调用方“没有更多字了”，调用方的 `for range tokens` 因此退出。
3. `close(errc)`：如果前面没放错误，调用方 `<-errc` 读到的就是 nil，不会永远卡住。

**按行读：`bufio.Scanner`**

- `sc.Scan()` 每调用一次读一行，返回 true；读到末尾或出错返回 false，循环结束。
- `sc.Text()` 拿到这一行的内容（不含换行符）。
- `sc.Buffer(初始, 上限)`：Scanner 默认一行最多 64KB。如果模型一次推了一大段工具参数，单行可能超过，会直接报 `token too long`。这里放宽到 1MB。

**过滤和解析**

- 不以 `data: ` 开头的行（空行、`event:`、`: keep-alive` 注释）都跳过。
- 匿名 struct 只声明了 `choices[].delta.content` 这一条路径，JSON 里其他字段（id、model、role……）自动忽略（见 0.6）。`Content` 没写标签也能匹配 `content`，因为 Go 的 JSON 字段名匹配不区分大小写。

**最关键的 select**

```go
select {
case tokens <- 字:     // 尝试把字交给调用方
case <-ctx.Done():     // 同时盯着取消
}
```

为什么不直接写 `tokens <- 字`？因为 `tokens` 是无缓冲的，**调用方不来取，这行就会永远卡住**。如果调用方已经不读了（用户关了页面），这个 goroutine 就永远停在这里，连接也永远不关 → 泄漏。和 `ctx.Done()` 放进同一个 select，取消时就能跳出来退出。

> **一句话规则：每一次 channel 发送，都要和 `ctx.Done()` 放进同一个 select。**

**为什么 errc 要缓冲 1**（最容易卡住的地方，画个时间线）：

假设 JSON 解析出错，errc **无缓冲**：

```
后台:   errc <- err   ← 卡住：无缓冲，要等有人来取
调用方: for range tokens  ← 卡住：在等 tokens 被关闭
        （tokens 要等后台 return 后的 defer 才关闭，而后台卡在上一行）
结果:   两边互相等 → 死锁
```

errc **缓冲 1**：

```
后台:   errc <- err   ← 立刻完成，错误存进缓冲
        return → defer 关 body、关 tokens、关 errc
调用方: for range tokens 发现 tokens 关了，退出循环
        <-errc 拿到刚才那个错误
```

规律：**最多只发一次的“结果/错误” channel，给缓冲 1**，发送方永远不会因为没人读而卡住。

### 5.5 四种结束方式汇总

| 怎么结束的 | errc 里有什么 | 调用方看到 |
|---|---|---|
| 读到 `[DONE]` | 空 | `err == nil`，正常结束 |
| JSON 解析失败 | `bad chunk: ...` | 报错 |
| ctx 被取消 | `context.Canceled` 或 `DeadlineExceeded` | 报错 |
| 网络断了 / body 读失败 | `sc.Err()` 的错误 | 报错 |

四条路都会走到 defer，所以**连接一定关、两根管道一定关、goroutine 一定退出**。这就是“不泄漏”的完整含义。

---

## 6. 三个高频坑

| 坑 | 症状 | 修法 |
|---|---|---|
| goroutine 泄漏 | 内存、goroutine 数只涨不跌 | 所有阻塞的发送都配 `ctx.Done()`；测试里检查 `runtime.NumGoroutine()` 或用 goleak |
| response body 没关 | 连接耗尽、请求越来越慢 | `defer resp.Body.Close()`，必要时 `io.Copy(io.Discard, body)` 先读干净再关 |
| map 并发写 | `fatal error: concurrent map writes`，程序直接退出 | 按下标写 slice，或用 `sync.Mutex` 加锁；跑测试加 `-race` |

**泄漏的最小例子**，对照着看就明白了：

```go
// ❌ 泄漏：调用方只读一个就走了，后台卡在第二次发送，永远不退出
func leaky() <-chan int {
    ch := make(chan int)
    go func() {
        for i := 0; ; i++ {
            ch <- i
        }
    }()
    return ch
}

// ✅ 不泄漏：发送时同时监听 ctx，调用方 cancel 后后台立刻退出
func safe(ctx context.Context) <-chan int {
    ch := make(chan int)
    go func() {
        defer close(ch)
        for i := 0; ; i++ {
            select {
            case ch <- i:
            case <-ctx.Done():
                return
            }
        }
    }()
    return ch
}
```

---

## 本课小结

| 概念 | 一句话 |
|---|---|
| goroutine | 后台任务，便宜，但必须想清楚怎么结束 |
| channel | goroutine 之间传数据；发送方负责关闭 |
| select | 同时等多件事，谁先来处理谁；“等数据 OR 等取消”是标配 |
| context | 取消信号线，父取消子全取消；只发信号，不强杀，下游要自己监听 |
| errgroup | 并行一组任务 + 限并发 + 等全部结束 |
| 缓冲 1 的 errc | 只发一次的结果用缓冲 1，避免互相等待 |

---

## 练习（今天做完）

1. **基础**：写 `fanIn(ctx, chs ...<-chan string) <-chan string`，把多个 channel 合并成一个；ctx 取消后所有 goroutine 退出。
   - 提示：参考第 1 节生产者模式；每路输入一个 goroutine；用 `sync.WaitGroup` 等它们都结束后再关闭输出。
2. **核心**：用第 4 节的 `runTools`，写三个假工具（分别 sleep 1s / 2s / 25s），验证：总耗时约 20s（第三个超时）、超时工具的错误被写进 `ToolResult.Err`。
   - 提示：假工具里不要用 `time.Sleep`，要用 `select` 同时等 `time.After` 和 `ctx.Done()`。
3. **进阶**：用 `httptest.NewServer` 写一个假 SSE 服务端，每 100ms 吐一个 token；客户端读 5 个后 cancel，断言服务端检测到连接断开（handler 里 `<-r.Context().Done()`）。
4. 所有测试用 `go test -race` 跑通。

## 自测题

1. 为什么 `errc` 要用缓冲 1 的 channel？换成无缓冲会发生什么？
2. `errgroup.WithContext` 返回的 ctx，在什么情况下会被取消？
3. 工具执行失败时，为什么本课选择 `return nil`？什么情况下应该 `return err`？
4. `context.WithTimeout` 之后不调用 `cancel()`，会有什么后果？

> 做完练习后，把代码放到 `projects/00-stream-proxy/`，在聊天里告诉我结果或卡点，我来批改并写当天日志。

---

# 参考答案与详解

> 先自己做，卡住超过 20 分钟再看。

## 自测题答案

### 1. `errc` 为什么要缓冲 1？换成无缓冲会怎样？

出错时，后台 goroutine 的顺序是：`errc <- err` → `return` → 执行 defer（关闭 tokens）。

- **无缓冲**：发送必须等到有人接收才完成。但此时调用方正卡在 `for tok := range tokens`，在等 tokens 被关闭；而 tokens 要等 goroutine 退出才关闭。两边互相等待 → **死锁**（或 goroutine 永久泄漏）。
- **缓冲 1**：错误直接放进缓冲区，发送立即完成，goroutine 退出，tokens 关闭，调用方跳出循环后再从 errc 读到错误。

时间线图见第 5.4 节。规律：**“报告最终结果”的 channel，如果最多只发一次，就给缓冲 1。**

### 2. `errgroup.WithContext` 返回的 ctx 什么时候会被取消？

三种情况，任一发生即取消：

1. 某个 `g.Go` 里的函数**第一次返回非 nil 错误**（其余 goroutine 通过这个 ctx 感知到，应尽快退出）；
2. `g.Wait()` 返回时（所有任务都结束了，ctx 随之取消）；
3. 传进来的**父 ctx 被取消**（比如客户端断开、整轮超时）。

所以第 4 节里每个工具都要用这个派生 ctx，而不是外层的 ctx，才能做到“一个失败、全体停”。

### 3. 工具失败为什么 `return nil`？什么时候该 `return err`？

- **`return nil`**：工具报错（文件不存在、命令退出码非 0、单个工具超时）对模型来说是一条**有用的观察**。把错误写进 `ToolResult.Err`，作为 `tool_result` 发回给模型，它往往能自己换参数重试。如果 `return err`，errgroup 会取消其他正在跑的工具，整轮白做。
- **`return err`**：当错误说明**这一轮已经没意义**时：
  - 父 ctx 被取消（用户走了、整轮超时）；
  - 基础设施故障，比如沙箱进程崩溃、鉴权失效，所有工具都会失败；
  - 安全违规，比如工具试图越权访问，应立即中止。

怎么区分“单个工具超时”和“用户走了”？看是哪个 ctx 出的问题：

```go
out, err := execTool(tctx, c)
if ctx.Err() != nil {          // 组 ctx（上一层）被取消：整轮中止
    return ctx.Err()
}
// 走到这里说明只是这个工具自己的 tctx 超时或工具本身出错：记下来，继续
results[i] = ToolResult{ID: c.ID, Output: out, Err: err}
return nil
```

`tctx` 是 `ctx` 的子节点：`tctx` 超时不影响 `ctx`；但 `ctx` 取消一定会连带 `tctx`。所以检查 `ctx.Err()` 就能区分。

### 4. `context.WithTimeout` 之后不调 `cancel()` 会怎样？

- 派生出的 ctx 会一直挂在父 ctx 的子节点列表上，**直到超时触发才释放**。如果父 ctx 生命周期很长（比如服务级的 ctx），高 QPS 下这些对象会堆积，表现为内存缓慢上涨。
- 依赖这个 ctx 的下游资源（HTTP 连接、子 goroutine）也不会被提前释放。
- `go vet` 会报 `lostcancel` 警告。

结论：**`ctx, cancel := context.WithXxx(...)` 的下一行永远是 `defer cancel()`**。提前完成时它会立刻释放资源；多调一次 cancel 也是安全的。

---

## 练习参考实现

### 练习 1：`fanIn`

**要做的事**：有好几根输入管道（比如几个工具同时在吐日志），合并成一根输出管道给调用方读。

```
ch1 ──▶ worker1 ──┐
ch2 ──▶ worker2 ──┼──▶ out ──▶ 调用方
ch3 ──▶ worker3 ──┘
          全部 worker 结束后，由一个“收尾 goroutine”关闭 out
```

```go
func fanIn(ctx context.Context, chs ...<-chan string) <-chan string {
    out := make(chan string)
    var wg sync.WaitGroup
    wg.Add(len(chs))            // 计数器：有几路输入就记几

    for _, ch := range chs {
        go func() {             // 每路输入一个 worker
            defer wg.Done()     // worker 退出时计数 -1
            for {
                select {
                case v, ok := <-ch:          // 外层：等输入
                    if !ok {
                        return               // 这一路输入已关闭，worker 结束
                    }
                    select {
                    case out <- v:           // 内层：把值转发出去
                    case <-ctx.Done():       // 转发时也能被取消
                        return
                    }
                case <-ctx.Done():           // 外层：等输入时也能被取消
                    return
                }
            }
        }()
    }

    go func() {                 // 收尾 goroutine
        wg.Wait()               // 等计数归零 = 所有 worker 都退出了
        close(out)              // 这时才安全地关闭 out
    }()
    return out
}
```

**逐行解读**

- `chs ...<-chan string`：可变参数，调用时可以传任意多根管道，函数里 `chs` 就是一个 slice。
- `sync.WaitGroup`：一个计数器。`Add(n)` 加 n，`Done()` 减 1，`Wait()` 阻塞到归零。
- `v, ok := <-ch`：从关闭的 channel 读，会立刻拿到零值且 `ok == false`。这是判断“输入结束了”的标准写法。
- **为什么要两层 select**：外层解决“等输入时被取消”，内层解决“发给 out 时被取消”。只写外层的话，如果调用方不读 out 了，worker 会卡在 `out <- v` 上永远出不来。这正是第 5 节那条规则：**每次发送都配 `ctx.Done()`**。
- **为什么不让 worker 自己 `close(out)`**：out 有好几个发送方。第一个 worker 结束时如果关了 out，别的 worker 还在往里发 → panic。所以单独开一个收尾 goroutine，等**所有**发送方都结束再关。这是规则 1 在“多个发送方”时的写法。

**它在三种情况下都能正确结束**：

| 情况 | 发生了什么 |
|---|---|
| 所有输入正常关闭 | 每个 worker 读到 `ok == false` 退出 → 计数归零 → out 关闭 → 调用方的 `range` 结束 |
| ctx 取消 | 每个 worker 不管卡在外层还是内层 select 都会命中 `ctx.Done()` 退出 → out 关闭 |
| 调用方不读了但 cancel 了 | 卡在 `out <- v` 的 worker 被内层 select 救出来 |

### 练习 2：并行工具与超时

**要验证的事**：三个工具并行跑，最慢的那个被超时截断，总时间 ≈ 超时时间，而不是三者相加。

为了不让测试真跑 20 秒，把超时提成包级变量，测试里调成 2 秒：

```go
var toolTimeout = 20 * time.Second // runTools 里用它替换写死的 20*time.Second

var fakeDelay = map[string]time.Duration{} // 每个假工具要“干”多久

func execTool(ctx context.Context, c ToolCall) (string, error) {
    select {
    case <-time.After(fakeDelay[c.Name]):   // 假装干活这么久
        return c.Name + " ok", nil
    case <-ctx.Done():                       // 被超时或取消打断
        return "", ctx.Err()
    }
}

func TestRunTools(t *testing.T) {
    toolTimeout = 2 * time.Second
    fakeDelay = map[string]time.Duration{
        "a": 100 * time.Millisecond,
        "b": 200 * time.Millisecond,
        "c": 2500 * time.Millisecond, // 比超时长，会被打断
    }
    calls := []ToolCall{{ID: "1", Name: "a"}, {ID: "2", Name: "b"}, {ID: "3", Name: "c"}}

    start := time.Now()
    res, err := runTools(context.Background(), calls)
    elapsed := time.Since(start)

    if err != nil {
        t.Fatalf("unexpected err: %v", err)
    }
    if elapsed < 1900*time.Millisecond || elapsed > 2300*time.Millisecond {
        t.Fatalf("elapsed = %v, want ~2s (并行，受最慢的超时限制)", elapsed)
    }
    if res[0].Err != nil || res[1].Err != nil {
        t.Fatalf("a/b should succeed: %+v", res)
    }
    if !errors.Is(res[2].Err, context.DeadlineExceeded) {
        t.Fatalf("c should time out, got %v", res[2].Err)
    }
}
```

**逐行解读**

- `context.Background()`：最顶层的空 ctx，永远不会被取消。测试和 `main` 里用它当根。
- 假工具用 `select` 同时等“活干完”和“被打断”，哪个先到算哪个。工具 c 要 2.5 秒，但 2 秒时 `tctx` 超时，`ctx.Done()` 先就绪，返回 `context.DeadlineExceeded`。
- 时间断言给了一个范围（1.9s ~ 2.3s），因为调度有误差，不能写死等于 2 秒。
- `errors.Is(err, context.DeadlineExceeded)`：判断错误是不是“超时”。超时和取消是两种不同的错误：超时是 `DeadlineExceeded`，手动取消是 `Canceled`。
- `t.Fatalf`：测试失败并立刻停止这个测试，打印格式化信息。

**最重要的认识：超时只是发信号，工具自己必须监听。** 如果假工具里写的是 `time.Sleep(2500 * time.Millisecond)`，ctx 超时了它也照样睡满 2.5 秒，测试会失败。真实工具里用 `exec.CommandContext`（跑命令）、`http.NewRequestWithContext`（发请求），它们内部已经帮你监听了 ctx。

> 仓库实现里 `execTool` 写成了 `var execTool = func(...)`（变量），测试里直接替换成假实现，效果一样。

### 练习 3：取消流式读取，服务端感知断开

**要验证的事**：客户端 cancel 之后，服务端那边真的知道连接断了。在生产里就是“用户关页面 → 到模型供应商的连接也断开 → 停止计费”。

```go
func TestStreamCancel(t *testing.T) {
    disconnected := make(chan struct{}) // 服务端发现断开时关闭它，当“信号灯”用

    // 1. 起一个假 SSE 服务端
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
                flusher.Flush() // 立刻推给客户端，不然会攒在缓冲区里
            }
        }
    }))
    defer srv.Close()

    // 2. 客户端发请求，请求绑定一个可取消的 ctx
    ctx, cancel := context.WithCancel(context.Background())
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatal(err)
    }

    // 3. 读 5 个 token 后取消
    tokens, errc := streamTokens(ctx, resp.Body)
    for i := 0; i < 5; i++ {
        <-tokens
    }
    cancel()

    // 4. 排空 tokens，确认后台 goroutine 退出，并拿到一个错误
    for range tokens {
    }
    if err := <-errc; err == nil {
        t.Fatal("want a cancellation error, got nil")
    }

    // 5. 断言服务端在 2 秒内感知到断开
    select {
    case <-disconnected:
    case <-time.After(2 * time.Second):
        t.Fatal("server never saw the disconnect: 上游连接没断，还在烧钱")
    }
}
```

**分步解读**

1. **假服务端**：`httptest.NewServer` 在本机随便找个端口起一个真的 HTTP 服务，`srv.URL` 就是它的地址。handler 每 100ms 推一行 SSE，同时监听 `r.Context().Done()`：客户端一断开，就关闭 `disconnected` 通知测试。
   - `w.(http.Flusher)`：类型断言，拿到“能立刻推送”的能力。不调 `Flush()`，写的内容会攒在缓冲区里，客户端一个字都收不到。
   - `chan struct{}`：不传数据、只当信号用的 channel，关闭它就等于“亮灯”。
2. **客户端**：用 `NewRequestWithContext` 把请求和 ctx 绑在一起。这是能“取消请求”的前提。
3. **读 5 个后 cancel**：`cancel()` 触发两件事：
   - HTTP 客户端发现 ctx 取消，**关闭底层 TCP 连接**，服务端的 `r.Context()` 随之被取消；
   - `streamTokens` 里，要么 `sc.Scan()` 读 body 失败，要么 select 命中 `ctx.Done()`，后台 goroutine 退出。
4. **排空**：`for range tokens {}` 把剩下可能还在路上的值读掉，直到 tokens 被关闭，确保后台 goroutine 真的退出了，再去读 errc。这里只断言 `err != nil`，而不是 `errors.Is(err, context.Canceled)`：取决于后台当时卡在哪一步，你可能拿到 `ctx.Err()`，也可能拿到“读 body 失败”的错误，两种都对。
5. **断言服务端感知**：用 select 给 2 秒期限。2 秒内 `disconnected` 被关闭就通过；否则说明连接没断，测试失败。

### 练习 4：`go test -race`

```bash
go test -race -count=1 ./...
```

- `./...`：当前目录及所有子目录下的包都测。
- `-race`：开启数据竞争检测。如果第 4 节把 `results` 换成 `map[string]ToolResult` 并发写而不加锁，这里会直接报 `DATA RACE` 并指出是哪两行代码在抢。
- `-count=1`：关闭测试缓存。Go 默认会缓存“没改过代码的测试结果”，并发测试建议每次都真跑。
- 想自动检查 goroutine 泄漏，可以引入 `go.uber.org/goleak`，在 `TestMain` 里加 `goleak.VerifyTestMain(m)`：测试结束时还有多余 goroutine 活着，就判失败。
