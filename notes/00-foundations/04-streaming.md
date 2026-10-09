# 0.4 流式协议：HTTP、SSE 与 WebSocket

> 第 0 部分 · 第 4 课 ｜ 预计 4–5 小时 ｜ 前置：0.1 Go、0.2 TypeScript
>
> 配套练习：`projects/03-streaming/`（TypeScript，零运行时依赖，`npm test` 13 个用例全部通过）。

## 读前说明

几乎所有模型 API 的流式输出都走 **SSE**，几乎所有实时语音 / 双向打断场景都走 **WebSocket**，而它们底下都是 **HTTP**。这一课把三者从“会用 SDK”讲到“自己能写解析器、能排查线上流式卡住”。

每段代码依旧按三步讲：**要解决什么问题 → 代码 → 逐行解读 + 容易错的地方**。

本课结束时你能：

1. 用 `curl` 看懂一次流式请求在网络上到底长什么样；
2. 手写一个符合规范、能扛住任意切块的 SSE 解析器；
3. 用 Go 和 TS 各写一个正确的 SSE 服务端：响应头、flush、心跳、断开即停；
4. 知道什么时候该换 WebSocket，以及它多出来的那些麻烦；
5. 排查“本地好好的，上线后流式一下全吐出来”这类问题。

---

## 为什么 Agent 必须流式

一次模型调用生成 800 个 token，按 50 token/秒算要 16 秒。不流式，用户就盯着空白页 16 秒；流式的话，**首 token 延迟（TTFT）** 可能只有 0.5 秒，后面边生成边显示。

对 Agent 来说流式不只是体验问题：

- **工具调用要尽早开始**：模型一吐完 `tool_call` 的参数，就可以开始执行，不用等整段回复结束。
- **能中途取消**：用户看到方向不对点“停止”，后端要立刻停掉模型调用，省钱。
- **长任务要报进度**：Deep Research 这类跑几分钟的任务，要不断推送“正在搜索…”“读了 12 篇…”。

---

# 第一部分：HTTP，一切的地基

## 1. 一次 HTTP 请求长什么样

**要解决的问题**：SDK 把细节全藏了，出问题时你得知道线上传的到底是什么。

```bash
curl -v https://httpbin.org/get
```

`-v` 会打印出原始报文，去掉 TLS 信息后大致是：

```
> GET /get HTTP/1.1            ← 请求行：方法 路径 协议版本
> Host: httpbin.org            ← 请求头，一行一个
> Accept: */*
>                              ← 空行：请求头结束
< HTTP/1.1 200 OK              ← 状态行
< Content-Type: application/json
< Content-Length: 256          ← 响应体有多少字节
<                              ← 空行：响应头结束
{ ...响应体... }
```

**要记住的几个点**

- HTTP/1.1 是**文本协议**：头部就是一行行 `名字: 值`，以一个空行结束。SSE 的格式就是照这个思路设计的。
- **状态码**：`2xx` 成功，`4xx` 你的错（`400` 参数错、`401` 没鉴权、`429` 被限流），`5xx` 服务端的错（`500`、`502` 网关后面挂了、`503` 过载、`504` 网关等超时）。调模型 API 时 `429` 和 `5xx` 要重试，`4xx` 其他的一般不该重试（第 08 模块详讲）。
- **`Content-Length`**：服务端事先知道响应多大，就写在头里。但流式输出事先不知道多长，怎么办？看下一节。

## 2. 流式的关键：不知道多长也能发

HTTP/1.1 的做法叫 **分块传输**（`Transfer-Encoding: chunked`）：不写 `Content-Length`，响应体被切成一块块发，每块前面写这一块的长度，最后用长度为 0 的块表示结束。

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Transfer-Encoding: chunked

b                       ← 十六进制 b = 11：下面这块 11 字节
data: 你\n\n            （"data: " 6 字节 + 中文 3 字节 + 两个换行）
b
data: 好\n\n
0                       ← 长度 0：结束
```

你几乎不用手写分块，Go 的 `net/http` 和 Node 的 `http` 模块在你**没设 Content-Length 并多次写入**时会自动用它。HTTP/2 和 HTTP/3 没有 chunked，它们本身就是按“帧”传的，天然支持流式。

**真正需要你操心的是“flush”**：你调用 `write` 时，数据可能先进了缓冲区，攒够一大块才发。对普通接口这是优化，对流式就是灾难：用户看到的是“卡半天然后一下全出来”。后面 Go 服务端会讲怎么强制刷出去。

## 3. 用 curl 看真实的流

```bash
curl -N https://api.openai.com/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"数到3"}]}'
```

`-N` 关掉 curl 自己的输出缓冲，你会看到一行行冒出来：

```
data: {"id":"chatcmpl-1","choices":[{"delta":{"role":"assistant","content":""}}]}

data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"1"}}]}

data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"，2"}}]}

data: [DONE]
```

这就是 SSE。没有 API Key 的话，跑配套练习的 `npm run dev`，或者用 `curl -N -X POST http://127.0.0.1:端口/chat` 打本地服务，效果一样。

---

# 第二部分：SSE（Server-Sent Events）

## 4. SSE 格式：四个字段加一个空行

**要解决的问题**：在一个一直不结束的 HTTP 响应里，怎么分出“一条条消息”？

SSE 的规则很简单，整个规范的核心就这些：

```
event: token          ← 事件类型，可选，不写默认是 "message"
id: 42                ← 事件 id，可选，用于断线续传
retry: 3000           ← 告诉浏览器：断线后等 3000 毫秒再重连，可选
data: 第一行          ← 数据，可以有多行
data: 第二行          ←   多行 data 会用 "\n" 拼起来
                      ← 空行：这条事件结束，派发出去
: ping                ← 以冒号开头的是注释，解析器直接丢弃（常用作心跳）
```

**逐条解读**

- **一行一个字段**，格式是 `字段名: 值`。冒号后面**最多去掉一个空格**：`data:  hi`（两个空格）解析出来是 ` hi`（前面还有一个空格）。
- **空行才是分隔符**，不是换行。一条事件可以有很多行，遇到空行才算完。
- 行尾可以是 `\n`、`\r\n` 或单独的 `\r`，三种都要支持。
- **没有 data 的事件不派发**：只写了 `event: x` 然后空行，什么都不会发生。
- 不认识的字段名直接忽略。
- 响应头必须是 `Content-Type: text/event-stream`。

各家模型 API 的用法：

| 厂商 | 怎么用 SSE |
|---|---|
| OpenAI Chat Completions | 只用 `data:`，最后一条是 `data: [DONE]` |
| OpenAI Responses / Anthropic Messages | 用 `event:` 区分类型，如 `response.output_text.delta`、`content_block_delta`、`message_stop` |
| 多数国产模型 API | 兼容 OpenAI 格式，同样以 `[DONE]` 结束 |

所以写 Agent 后端，**一个正确的 SSE 解析器是必备件**。

## 5. 手写 SSE 解析器：最容易错的是“切块”

**要解决的问题**：网络不会按“一条事件”给你数据。你可能一次收到半行，也可能一次收到三条半事件，甚至 `\r\n` 被从中间劈开，中文的 3 个字节被劈成 1 + 2。

先看一个**错误**写法，很多人第一次都这么写：

```ts
// ❌ 错误：假设每次 read 正好是完整的一行或几行
for await (const chunk of body) {
  for (const line of decoder.decode(chunk).split("\n")) {
    if (line.startsWith("data: ")) handle(line.slice(6));
  }
}
```

它有四个问题：

1. 一行被切成两块时，前半行 `data: {"te` 被当成完整一行，JSON 解析直接报错；
2. `decoder.decode(chunk)` 没加 `{ stream: true }`，中文字节被切开会变成乱码 `�`；
3. 不认 `\r\n`，行尾会残留一个 `\r`；
4. 不处理多行 `data`，也不按空行分事件。

本地测试大概率发现不了，因为本地网络太快，一块里常常就是完整事件；上线经过代理和公网，才开始随机报错。

**正确思路：用一个缓冲区攒着，凑出完整一行才处理。**

```ts
export class SSEParser {
  private buf = "";              // 还没凑成完整一行的残余文本
  private data: string[] = [];   // 当前事件攒的 data 行
  private eventType = "";
  private lastId: string | undefined;
  private pendingCR = false;     // 上一块以 \r 结尾

  feed(chunk: string): SSEEvent[] {
    if (chunk === "") return [];
    if (this.pendingCR && chunk.startsWith("\n")) chunk = chunk.slice(1); // ①
    this.pendingCR = false;
    this.buf += chunk;

    const out: SSEEvent[] = [];
    for (;;) {
      const m = /\r\n|\n|\r/.exec(this.buf);       // ② 找第一个行尾
      if (!m) break;                                 //    没有完整行：等下一块
      if (m[0] === "\r" && m.index === this.buf.length - 1) this.pendingCR = true;
      const line = this.buf.slice(0, m.index);
      this.buf = this.buf.slice(m.index + m[0].length); // ③ 剩下的留在缓冲区
      const ev = this.line(line);
      if (ev) out.push(ev);
    }
    return out;
  }

  private line(line: string): SSEEvent | undefined {
    if (line === "") return this.dispatch();         // ④ 空行：派发事件
    if (line.startsWith(":")) return undefined;      // 注释 / 心跳

    const i = line.indexOf(":");
    const field = i === -1 ? line : line.slice(0, i);
    let value = i === -1 ? "" : line.slice(i + 1);
    if (value.startsWith(" ")) value = value.slice(1); // 只去一个空格

    switch (field) {
      case "data":  this.data.push(value); break;
      case "event": this.eventType = value; break;
      case "id":    this.lastId = value; break;
      // retry 略，完整版见练习
    }
    return undefined;
  }

  private dispatch(): SSEEvent | undefined {
    if (this.data.length === 0) { this.eventType = ""; return undefined; }
    const ev = { event: this.eventType || "message", data: this.data.join("\n"), id: this.lastId };
    this.data = [];
    this.eventType = "";   // 注意：id 不清空，规范规定 lastEventId 会一直沿用
    return ev;
  }
}
```

**逐行解读**

- **② 找行尾**：正则 `/\r\n|\n|\r/` 按顺序尝试，所以 `\r\n` 会被当成一个整体，不会被识别成两个行尾。
- **③ 剩下的留着**：`buf` 里永远只存“最后那半行”。下一块来了拼在后面接着找。这是所有流式解析器的通用套路，解析 JSON Lines、日志、TCP 协议都一样。
- **① `pendingCR`**：如果这块恰好以 `\r` 结尾，我们无法知道下一块是不是以 `\n` 开头（`\r\n` 被劈开了）。先把 `\r` 当成行尾处理，记一个标记，下一块若以 `\n` 开头就吞掉，避免凭空多出一个空行，把事件提前派发。
- **`chunk === ""` 直接返回**：这一行是练习里的模糊测试抓出来的真 bug。空块进来如果走下去，会把 `pendingCR` 清成 `false`，下一块的 `\n` 就没被吞掉。**“在所有位置切开都要得到同样结果”这种测试，比你手写十个用例都管用。**
- **④ 空行派发**：data 多行用 `\n` 拼接；派发后清掉 data 和 event，但 **id 保留**。

**字节到文本**：

```ts
const decoder = new TextDecoder();
for (;;) {
  const { value, done } = await reader.read();
  if (done) break;
  parser.feed(decoder.decode(value, { stream: true })); // stream: true 是关键
}
parser.feed(decoder.decode()); // 最后冲刷一次，吐出解码器里剩的字节
```

`"你"` 在 UTF-8 里是 3 个字节 `E4 BD A0`。如果一块只到 `E4 BD`，不加 `stream: true` 的 decode 会直接输出 `�`。加了以后，解码器会把不完整的字节先留着，等下一块。Go 里不需要操心这个，因为 `bufio.Scanner` 按行切的是**字节**，切出来的一整行一定是完整的 UTF-8（0.1 课第 5 节就是这么写的）。

## 6. 客户端：为什么不用 EventSource

浏览器自带 `EventSource`，用起来很简单：

```ts
const es = new EventSource("/events");
es.addEventListener("token", (e) => console.log(e.data));
```

它还会**自动重连**，重连时自动带上 `Last-Event-ID` 请求头。但它有两个致命限制：

- **只能 GET**，不能带请求体。而调模型要 POST 一大段 messages。
- **不能自定义请求头**，没法带 `Authorization`。

所以调模型 API、或者自己的 Agent 前端调后端时，**统一用 `fetch` + 自己解析**：

```ts
export async function* streamChat(url: string, body: unknown, opts: { signal?: AbortSignal } = {}) {
  const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
    body: JSON.stringify(body),
    signal: opts.signal,                 // 0.2 课的 AbortController：用户点停止就 abort
  });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);   // ① 先看状态码
  if (!res.headers.get("content-type")?.startsWith("text/event-stream"))
    throw new Error("not SSE");                          // ② 再看类型
  for await (const ev of parseSSE(res.body!, opts.signal)) {
    if (ev.event === "token") yield { type: "token", text: JSON.parse(ev.data).text };
    else if (ev.event === "done") { yield { type: "done" }; return; }
  }
  throw new Error("stream ended without done");          // ③
}
```

**逐行解读**

- **① 先查状态码**：出错时（`401`、`429`）服务端一般返回普通 JSON，不是 SSE。直接拿去解析，你只会得到“什么事件都没有”，排查半天。
- **③ 没收到结束标记就断了，要当作失败**。连接可能因为网关超时、服务重启被掐断，这时 `reader.read()` 也会返回 `done: true`，看起来像“正常结束”。**以收到 `[DONE]` / `done` 事件为准**，否则交给上层重试或续传。这一点线上非常常见。

## 7. 服务端：Go 版

**要解决的问题**：写一个把模型输出转发给前端的 SSE 接口，要求每个 token 立即送达、客户端断开立即停。

```go
func chatHandler(w http.ResponseWriter, r *http.Request) {
    flusher, ok := w.(http.Flusher) // ① 断言：这个 ResponseWriter 支不支持 Flush
    if !ok {
        http.Error(w, "streaming unsupported", http.StatusInternalServerError)
        return
    }

    h := w.Header()                 // ② 响应头必须在第一次写 body 之前设好
    h.Set("Content-Type", "text/event-stream; charset=utf-8")
    h.Set("Cache-Control", "no-cache, no-transform")
    h.Set("X-Accel-Buffering", "no")
    w.WriteHeader(http.StatusOK)
    flusher.Flush()                 // 先把头发出去，客户端马上知道“连上了”

    ctx := r.Context()              // ③ 客户端断开时，这个 ctx 会被取消
    tokens := callLLM(ctx)          // 0.1 课写过的：返回 <-chan string，ctx 取消就停

    heartbeat := time.NewTicker(15 * time.Second)
    defer heartbeat.Stop()

    for {
        select {
        case <-ctx.Done():          // ④ 客户端走了：直接返回，callLLM 也因同一个 ctx 停下
            return
        case <-heartbeat.C:
            fmt.Fprint(w, ": ping\n\n") // ⑤ 注释行当心跳
            flusher.Flush()
        case tok, ok := <-tokens:
            if !ok {                // channel 关了：模型输出完毕
                fmt.Fprint(w, "event: done\ndata: [DONE]\n\n")
                flusher.Flush()
                return
            }
            b, _ := json.Marshal(map[string]string{"text": tok})
            fmt.Fprintf(w, "event: token\ndata: %s\n\n", b)
            flusher.Flush()         // ⑥ 每写一条就 Flush，否则会被缓冲
        }
    }
}
```

**逐行解读**

- **① `w.(http.Flusher)`**：这是“类型断言”，问一下接口变量里装的具体类型是否还实现了 `Flusher` 接口（0.1 课 B8）。标准库的 ResponseWriter 都支持；但你如果套了一层自己写的中间件（比如记录响应大小的包装器），包装器没实现 `Flush` 就会断言失败，流式失效。Go 1.20 起也可以用 `http.NewResponseController(w).Flush()`，它能穿透包装器。
- **② 先设头再写**：第一次 `Write` 或 `WriteHeader` 之后，头就已经发出去了，再 `Set` 没有任何效果，也不会报错。
- **③ `r.Context()`**：Go 的 HTTP 服务器在检测到客户端断开连接时，会取消这个请求的 context。把它一路传给 `callLLM`，就得到了 0.1 课讲的“取消链”：用户关页面 → ctx 取消 → 模型调用和所有工具一起停。
- **④ 和 ⑥ 放在同一个 select 里**：和 0.1 课第 5 节一样，任何会“等”的地方都要能被 ctx 打断。
- **⑤ 心跳**：很多负载均衡和代理（Nginx 默认 60 秒、云厂商 LB 常见 60–350 秒）会掐掉一段时间没有数据的连接。模型在“思考”或工具在执行时可能几十秒没有输出，定时发一条注释行就能保活，客户端解析器会直接丢弃它。
- **⑥ 每条都 Flush**：不 Flush，数据会留在 `bufio.Writer` 里（Go 默认 4KB），攒满才发。

**`Cache-Control: no-transform` 和 `X-Accel-Buffering: no` 是干什么的？** 见第 10 节。

## 8. 服务端：TypeScript（Node）版

配套练习 `src/server.ts` 的核心部分：

```ts
res.writeHead(200, {
  "Content-Type": "text/event-stream; charset=utf-8",
  "Cache-Control": "no-cache, no-transform",
  "X-Accel-Buffering": "no",
});

const ac = new AbortController();
res.on("close", () => {                 // ① 连接关闭（不管谁关的）
  if (!res.writableFinished) ac.abort(); //    还没正常写完就关了 = 客户端断开
});

const hb = setInterval(() => res.write(": ping\n\n"), 15_000);
try {
  for (let i = start; i < tokens.length; i++) {
    await sleep(interval, ac.signal);    // ② 所有等待都挂在 signal 上
    res.write(encodeSSE({ event: "token", id: String(i), data: JSON.stringify({ text: tokens[i] }) }));
  }
  res.write(encodeSSE({ event: "done", data: "[DONE]" }));
  res.end();
} finally {
  clearInterval(hb);                     // ③ 无论怎么结束，定时器都要清
}
```

**逐行解读**

- **① 监听 `res` 的 `close`**，不要监听 `req` 的 `close`。新版 Node 里 `req` 在请求体读完时就会触发 `close`，那时连接还好好的，用它判断“断开”会误杀。
- Node 的 `res.write` **不需要手动 flush**，写了就进内核发送缓冲区。但如果你用了 `compression` 之类的压缩中间件，它会攒数据，需要关掉或调用 `res.flush()`。
- **② 和 Go 版一个思路**：`AbortController` 就是 TS 里的 context（0.2 课）。真实服务里 `signal` 要传给调模型的 `fetch`，用户一断开，上游请求也跟着取消。
- **③ `finally` 清定时器**：不清的话，连接断了心跳定时器还在跑，往一个已关闭的连接上写，还会让进程一直退不出。

## 9. 断线续传：id 和 Last-Event-ID

**要解决的问题**：手机切网络、地铁进隧道，连接断了。重新请求要从头生成吗？

SSE 自带了续传机制：

1. 服务端给每条事件带上 `id:`；
2. 客户端记住最后收到的 id；
3. 重连时带请求头 `Last-Event-ID: 最后的 id`；
4. 服务端从这个 id 之后接着发。

`EventSource` 会自动做第 2、3 步，用 `fetch` 时自己做。练习的服务端是这样处理的：

```ts
const lastId = Number(req.headers["last-event-id"] ?? -1);
const start = Number.isInteger(lastId) && lastId >= 0 ? lastId + 1 : 0;
```

**但真实 Agent 服务里没这么简单**：模型输出是一次性的，断了就没了，你不能“从第 42 个 token 重新生成”。生产做法是**把生成和推送解耦**：

```
模型调用 ──写入──▶ 消息缓冲（Redis Stream / 内存环形缓冲，按会话 id 存）
                         │
前端连接 ◀──按 id 读取───┘   断线重连时从 Last-Event-ID 之后读
```

这样即使前端断开，后台生成也可以继续（或者按策略在一段时间后取消），用户回来接着看。这是第 03 模块“长任务 Agent”和第 08 模块“断点恢复”的基础，这里先有个印象。

## 10. 线上最常见的坑：中间层缓冲

**症状**：本地流式好好的，部署到服务器后变成“等十几秒，一下全出来”。

**原因**：你和用户之间隔着一串中间层，任何一层在缓冲，流式就失效。

| 中间层 | 问题 | 解决 |
|---|---|---|
| 你的代码 | 没 Flush（Go）/ 压缩中间件攒数据（Node） | 每条 Flush；流式路由不走压缩 |
| Nginx 反向代理 | 默认 `proxy_buffering on`，攒满缓冲区才转发 | 响应头 `X-Accel-Buffering: no`，或配置 `proxy_buffering off;` |
| Nginx / 网关 | 开了 gzip，压缩需要攒数据 | `Cache-Control: no-transform`，或对 `text/event-stream` 关 gzip |
| 负载均衡 / CDN | 空闲超时掐连接；部分 CDN 默认缓冲响应 | 心跳；调大 idle timeout；流式接口不过 CDN 缓存 |
| Serverless 平台 | 部分平台默认整段返回响应 | 用平台专门的流式响应模式 |

Nginx 推荐配置：

```nginx
location /api/chat {
    proxy_pass http://backend;
    proxy_http_version 1.1;          # 长连接需要 1.1
    proxy_set_header Connection "";
    proxy_buffering off;             # 关缓冲
    proxy_read_timeout 300s;         # 模型慢的时候别被 60 秒默认值掐掉
    gzip off;
}
```

**排查顺序**：先在服务器本机 `curl -N localhost:端口` 绕过所有中间层，流式正常说明是中间层问题；再逐层往外 curl，看从哪一层开始变成“一下全出来”。

**另一个坑：HTTP/1.1 的连接数上限**。浏览器对同一个域名最多同时开 6 个 HTTP/1.1 连接。每个 SSE 都占一个且一直不关，用户开 6 个标签页，第 7 个就卡住了，连普通接口都发不出去。HTTP/2 下多个流共用一个连接，没有这个问题，所以线上**流式接口务必走 HTTP/2**（一般在 Nginx / LB 上开启即可）。

---

# 第三部分：WebSocket

## 11. WebSocket 解决什么问题

SSE 是**单向**的：服务端推，客户端只能听。客户端想说话，只能另发一个 HTTP 请求。

大多数文字聊天这就够了：用户发消息是一个 POST，回复走 SSE。但有些场景需要**双向、低延迟、持续**的通道：

- **实时语音 Agent**：客户端持续上传音频帧，服务端持续下发音频和文字，用户一开口就要能打断模型说话（OpenAI Realtime API、Gemini Live 都用 WebSocket）。
- **协同编辑 / Coding Agent 的 IDE 插件**：双方随时互发消息。
- **一个连接上多路复用多个会话**。

## 12. 握手与帧

WebSocket 以一个普通 HTTP 请求开头，然后“升级”成另一种协议：

```
GET /ws HTTP/1.1
Host: example.com
Upgrade: websocket                         ← 我想换协议
Connection: Upgrade
Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==
Sec-WebSocket-Version: 13

HTTP/1.1 101 Switching Protocols           ← 好，换了
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=
```

`101` 之后，这条 TCP 连接就不再说 HTTP 了，双方互发**帧**。你需要知道的帧类型：

| 帧 | 用途 |
|---|---|
| text | UTF-8 文本，通常是 JSON |
| binary | 二进制，如音频 |
| ping / pong | 心跳：一方发 ping，另一方必须回 pong |
| close | 带状态码的关闭，如 `1000` 正常、`1001` 离开、`1011` 服务端出错 |

和 SSE 的关键区别：**WebSocket 有消息边界**。发一条就是一条，接收方不用像 SSE 那样自己按空行切。

## 13. Go 服务端

标准库不带 WebSocket，社区常用 `github.com/coder/websocket`（原 nhooyr.io/websocket，支持 context）和 `github.com/gorilla/websocket`。下面用前者：

```go
import "github.com/coder/websocket"
import "github.com/coder/websocket/wsjson"

type ClientMsg struct {
    Type string `json:"type"` // "user_message" | "interrupt"
    Text string `json:"text,omitempty"`
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
    c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
        OriginPatterns: []string{"app.example.com"}, // ① 只允许自己的前端连
    })
    if err != nil {
        return
    }
    defer c.CloseNow()

    ctx, cancel := context.WithCancel(r.Context())
    defer cancel()

    var (
        mu        sync.Mutex
        cancelGen context.CancelFunc = func() {}    // 当前这次生成的取消函数
    )

    for {
        var msg ClientMsg
        if err := wsjson.Read(ctx, c, &msg); err != nil { // ② 读循环：阻塞等客户端消息
            return                                        //    连接断开就退出
        }
        switch msg.Type {
        case "interrupt":
            mu.Lock()
            cancelGen()                                   // ③ 打断正在进行的生成
            mu.Unlock()
        case "user_message":
            mu.Lock()
            cancelGen()                                   // 新消息到来，先停掉旧的
            genCtx, genCancel := context.WithCancel(ctx)
            cancelGen = genCancel
            mu.Unlock()
            go generate(genCtx, c, msg.Text)              // ④ 生成放在另一个 goroutine
        }
    }
}

func generate(ctx context.Context, c *websocket.Conn, prompt string) {
    for tok := range callLLM(ctx, prompt) {
        if err := wsjson.Write(ctx, c, map[string]string{"type": "token", "text": tok}); err != nil {
            return
        }
    }
}
```

**逐行解读**

- **① 检查 Origin**：WebSocket 不受浏览器 CORS 限制，任何网站的页面都能尝试连你的服务。不检查 Origin，别的网站就能借用户登录态的 Cookie 连进来（跨站 WebSocket 劫持）。
- **② 读循环 + ④ 生成分开**：这是 WebSocket 服务的标准结构。如果在读循环里同步生成，生成期间就读不到“打断”消息了。
- **③ 打断**：每次生成都有自己的子 context，收到 `interrupt` 就取消它。这就是语音 Agent“用户一开口模型就闭嘴”的骨架。
- **并发写**：`coder/websocket` 的 `Write` 可以多个 goroutine 同时调用；`gorilla/websocket` **不行**，同时写会损坏帧，必须加锁或者用单独一个写 goroutine 配 channel。用哪个库都要先查清楚这一点。

## 14. TS 客户端

浏览器和 Node 22+ 都有全局 `WebSocket`：

```ts
const ws = new WebSocket("wss://api.example.com/ws"); // 线上一定用 wss://（TLS）

ws.addEventListener("open", () => {
  ws.send(JSON.stringify({ type: "user_message", text: "你好" }));
});

ws.addEventListener("message", (e) => {
  const msg = JSON.parse(e.data as string);
  if (msg.type === "token") render(msg.text);
});

ws.addEventListener("close", (e) => {
  console.log("closed", e.code, e.reason); // 1000 正常，1006 异常断开（没收到 close 帧）
  // ⚠️ WebSocket 不会自动重连，要自己做：指数退避 + 重新订阅会话
});

stopButton.onclick = () => ws.send(JSON.stringify({ type: "interrupt" }));
```

**浏览器 WebSocket 不能自定义请求头**，所以没法用 `Authorization`。常见做法：先调一个 HTTP 接口拿一个短期 token，再放在 URL 参数或第一条消息里；或者依赖同域 Cookie（这时一定要配合 Origin 检查）。

## 15. WebSocket 多出来的麻烦

选 WebSocket 之前，要知道你放弃了什么：

| 问题 | SSE | WebSocket |
|---|---|---|
| 断线重连 | EventSource 自动重连 + Last-Event-ID | 全部自己写 |
| 心跳 | 注释行 | ping/pong，或应用层心跳 |
| 鉴权 | 普通 HTTP 头 | 浏览器里不能带头，要绕 |
| 负载均衡 | 普通 HTTP，随便转 | 要支持 Upgrade，长连接粘在一台机器上 |
| 扩缩容 / 发版 | 请求结束连接就释放 | 连接常驻，发版要优雅地断开并让客户端重连 |
| 调试 | curl 即可 | 要专门工具（浏览器 DevTools、websocat） |
| 背压 | TCP 自动处理 | 客户端收得慢时，服务端发送缓冲会堆积，要自己限 |

---

# 第四部分：怎么选

## 16. 决策表

| 场景 | 选 | 理由 |
|---|---|---|
| 调模型 API 的流式输出 | SSE（fetch 解析） | 厂商都是这么提供的 |
| 自己的 Agent 后端 → 网页聊天 | **SSE** | 简单、可 curl、好部署；用户发消息走 POST |
| 长任务进度推送 | SSE + 消息缓冲 | 支持续传，断线不丢 |
| 实时语音、需要随时打断 | **WebSocket** | 双向、低延迟、二进制 |
| 服务和服务之间流式调用 | gRPC 流 / HTTP/2 | 有类型、有背压（后续模块讲） |
| MCP 远程传输 | Streamable HTTP | 就是 HTTP POST + 可选 SSE 响应，第 02 模块讲 |

**经验法则：能用 SSE 就用 SSE。** 等你确实需要“服务端正在说话时，客户端随时插话”，再上 WebSocket。

---

## 本课小结

| 概念 | 一句话 |
|---|---|
| chunked / HTTP/2 帧 | 不知道响应多长也能边生成边发 |
| Flush | 不刷出去，流式就变成“攒一波” |
| SSE 格式 | `字段: 值` 一行一个，空行结束一条事件，冒号开头是注释 |
| 解析器 | 缓冲区攒残余半行；`TextDecoder` 加 `stream: true`；三种行尾都认 |
| 结束标记 | 以 `[DONE]` / `done` 为准，连接断了不等于正常结束 |
| fetch vs EventSource | 要 POST、要带鉴权头，就用 fetch |
| 断开即停 | Go 用 `r.Context()`，Node 监听 `res` 的 `close` 再 abort |
| 心跳 | 防止中间层因空闲掐连接 |
| 中间层缓冲 | `X-Accel-Buffering: no`、`no-transform`、关 gzip、走 HTTP/2 |
| WebSocket | 双向有消息边界；重连、鉴权、扩缩容全要自己管 |

## 练习（今天做完）

在 `projects/03-streaming/` 里：

1. **SSE 解析器**（`src/sse.ts`）：实现 `SSEParser.feed`，支持 `data` / `event` / `id` / `retry`、注释、三种行尾、多行 data。必须通过“任意两个位置切开都得到同样结果”的模糊测试。
2. **字节流解析**（`parseSSE`）：每次只给 1 个字节，中文也不能乱码；`signal` 中止时释放底层连接。
3. **SSE 服务端**（`src/server.ts`）：`POST /chat` 逐个吐 token，正确的响应头、心跳、支持 `Last-Event-ID` 续传、客户端断开后停止生成（`stats.aborted` 加 1）。
4. **客户端**（`src/client.ts`）：`fetch` 发 POST，先查状态码和 Content-Type，没收到 `done` 就断开要抛错。

```bash
cd projects/03-streaming
npm install
npm run typecheck && npm test   # 13 个用例全部通过
npm run dev                     # 看终端里逐字打印
```

**选做**：用 Go 写一个同样的 `/chat` SSE 服务（第 7 节代码），然后 `curl -N` 打它，再故意删掉 `flusher.Flush()` 看看区别。

## 自测题

1. 下面这段 SSE 会派发几个事件？每个的 `event` 和 `data` 是什么？
   ```
   : hello

   event: a

   data: 1
   data: 2

   event: b
   data:3

   ```
2. 为什么调模型 API 不用浏览器的 `EventSource`？
3. 客户端的 `reader.read()` 返回了 `done: true`，可以认为模型已经输出完了吗？
4. 本地流式正常，上线后变成一次性全出来。说出至少三个可能的原因。
5. Go 里为什么要先 `w.(http.Flusher)` 断言？什么情况下会失败？
6. Node 里判断客户端断开，为什么监听 `res.on("close")` 而不是 `req.on("close")`？
7. 用户开了 7 个标签页，第 7 个页面的所有请求都卡住了。最可能的原因是什么？怎么解决？
8. 做一个文字聊天 Agent，你会选 SSE 还是 WebSocket？做一个可以随时打断的语音 Agent 呢？

---

# 参考答案与详解

## 自测题答案

**1.** 两个。
- `: hello` 是注释，丢弃；后面的空行触发派发，但没有 data，不派发。
- `event: a` 后面跟空行：没有 data，不派发，event 类型被清空。
- `data: 1`、`data: 2`、空行：派发 `{ event: "message", data: "1\n2" }`。注意类型是 `message`，因为上一个 `event: a` 已经在空行时被清掉了。
- `event: b`、`data:3`、空行：派发 `{ event: "b", data: "3" }`。`data:3` 冒号后没有空格，值就是 `3`。

**2.** `EventSource` 只能发 GET，不能带请求体，也不能自定义请求头（没法带 `Authorization`）。模型 API 需要 POST 一大段 JSON 并鉴权，只能用 `fetch` 自己读流、自己解析。

**3.** 不能。网关超时、服务重启、网络中断都可能让连接被正常关闭，`read()` 同样返回 `done: true`。要以收到协议里的结束标记（`data: [DONE]`、`message_stop`、`done` 事件）为准，没收到就当作失败处理：重试或用 `Last-Event-ID` 续传。

**4.** 任意三个即可：
- Go 代码里没调 `Flush`，或者中间件包装了 ResponseWriter 导致 `Flush` 不生效；
- Node 用了压缩中间件；
- Nginx `proxy_buffering` 默认开启；
- 网关或 Nginx 开了 gzip；
- CDN 或 Serverless 平台默认缓冲整个响应。

排查方法：在服务器本机 `curl -N` 直连应用端口，正常则逐层往外查。

**5.** 流式需要在每条事件后主动刷出缓冲区，而 `http.ResponseWriter` 接口本身没有 `Flush` 方法，只能通过类型断言看具体实现是否支持。标准库的实现都支持；但如果套了自定义中间件包装器而包装器没实现 `Flush()`，断言就会失败。解决：包装器实现 `Flush`（以及 `Unwrap`），或者改用 `http.NewResponseController(w).Flush()`，它会通过 `Unwrap` 找到底层实现。

**6.** 在较新的 Node 版本里，`req` 的 `close` 表示“请求体读完了”，这时连接还在，用它判断会把正常请求当成断开。`res` 的 `close` 才表示底层连接关闭；再配合 `res.writableFinished` 判断是不是我们自己正常 `end()` 之后关闭的。

**7.** 浏览器对同一域名的 HTTP/1.1 连接数上限是 6。每个标签页的 SSE 占着一个连接不放，6 个占满后，第 7 个页面的任何请求都要排队。解决：让流式接口走 HTTP/2（同一连接多路复用）；或者在多个标签页之间共享一条连接（`SharedWorker` / `BroadcastChannel`）。

**8.** 文字聊天选 SSE：用户发消息走 POST，回复走 SSE，简单、好调试、好部署，“停止生成”用 `AbortController` 断开即可。可打断的语音 Agent 选 WebSocket：需要持续双向传输二进制音频，并且在服务端说话时随时接收客户端的打断信号。

## 练习参考实现

完整代码在 `projects/03-streaming/src/`，下面是要点。

**SSE 解析器的模糊测试**：这是本课最值得带走的测试方法。

```ts
it("任意位置切块，结果都与整块一致", () => {
  const raw = 'event: token\r\nid: 1\r\ndata: {"text":"你好"}\r\n\r\n: ping\n\ndata: x\ndata: y\n\n';
  const whole = parseAll([raw]);
  for (let i = 0; i <= raw.length; i++) {
    for (let j = i; j <= raw.length; j++) {
      expect(parseAll([raw.slice(0, i), raw.slice(i, j), raw.slice(j)])).toEqual(whole);
    }
  }
});
```

双重循环把同一段数据在所有可能的两个位置切成三块，包括切出空块、切在 `\r` 和 `\n` 中间。第一版实现在空块时会重置 `pendingCR`，就是被这个测试抓出来的。手写用例很难想到“空块恰好落在 `\r\n` 中间”这种情况。

**1 字节一块的中文测试**：

```ts
const body = new ReadableStream<Uint8Array>({
  start(c) {
    for (const b of bytes) c.enqueue(new Uint8Array([b])); // 每次只给 1 个字节
    c.close();
  },
});
```

去掉 `decoder.decode` 的 `{ stream: true }` 跑一下，这个测试会立刻失败，输出乱码。

**断开即停的集成测试**：

```ts
const ac = new AbortController();
for await (const ev of streamChat(url, {}, { signal: ac.signal })) {
  if (ev.type === "token") {
    got.push(ev.text);
    if (got.length === 2) ac.abort();   // 收到 2 个 token 就取消
  }
}
// 断言：服务端的 stats.aborted 变成 1，completed 仍为 0
```

这个测试同时验证了三件事：客户端能取消、取消会真的关掉 TCP 连接、服务端能感知断开并停止生成。生产里第三件最容易漏，漏了的代价是用户走了你还在为模型输出付费。
