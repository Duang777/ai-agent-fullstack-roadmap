# 第 0 部分 · 基础功底（建议 5–7 天）

目标：能用 Go 写一个稳健的、可取消的流式 LLM 服务，用 TS 写客户端消费它，并能读懂 Python 生态的 Agent 代码。

---

## 0.1 Go 并发与工程（2 天）

**必会知识点**
- goroutine、channel（有/无缓冲）、`select`、关闭 channel 的约定（只由发送方关闭）
- `context.Context`：`WithCancel` / `WithTimeout`，在 HTTP 请求、LLM 调用、工具调用之间层层传递取消信号
- `golang.org/x/sync/errgroup`：并行工具调用，任一失败即取消其余
- `net/http` Client：超时、连接复用、指数退避重试（429/5xx）
- 流式读取：`bufio.Scanner` / `bufio.Reader` 逐行解析 SSE
- 泛型、`encoding/json`（`json.RawMessage` 处理工具参数）、`log/slog`、`testing` + 表驱动测试
- 常见坑：goroutine 泄漏、未 drain 的 response body、共享 map 并发写

**为什么对 Agent 重要**：Agent Loop 本质是“带预算的并发调度器”——并行调工具、超时取消、流式输出，全靠这一套。

**资料**：Go Tour（go.dev/tour）、Go Blog《Go Concurrency Patterns: Context》《Pipelines and cancellation》、Effective Go。

## 0.2 TypeScript（1.5 天）

**必会知识点**
- `strict` 模式、联合类型与可辨识联合（建模 `message` / `tool_call` / `tool_result` 事件）
- Zod：schema 定义 → 运行时校验 → 导出 JSON Schema（用于工具定义、结构化输出）
- `async/await`、`for await...of`、AsyncGenerator（流式 token 的自然表达）
- `fetch` 的 `ReadableStream` + `TextDecoder` 解析流；`AbortController` 取消请求
- Node 20+ 运行时、pnpm、tsx / tsup

**资料**：TypeScript Handbook、zod.dev、MDN《Using readable streams》。

## 0.3 Python（够用即可，0.5 天）

- `asyncio`（`gather`、`TaskGroup`）、类型标注、Pydantic v2 模型
- 用 uv 管理环境；能跑通并读懂 LangGraph / SWE-agent 这类仓库的入口代码
- 定位：读论文代码、写评测脚本，不做主力服务

## 0.4 流式协议（1 天）

- HTTP/1.1 chunked、HTTP/2 多路复用
- **SSE 格式**：`event:` / `data:` / `id:` / `retry:`，空行分隔事件；OpenAI / Anthropic / 智谱 GLM 的流式接口都基于 SSE
- WebSocket：双向场景（语音、实时协作），心跳与重连
- 背压（backpressure）：上游快、下游慢时如何不爆内存
- 断线续传：`Last-Event-ID`

**资料**：MDN《Using server-sent events》、WHATWG HTML 规范 SSE 一节。

## 0.5 工程底座（1 天）

- Git：分支、rebase、conventional commits
- Docker：Go 多阶段构建（distroless 镜像）、docker compose 起 Postgres/Redis
- Linux：进程/信号（优雅退出 SIGTERM）、`curl -N` 调试流、`strace`/`lsof` 入门

---

## 本部分实战：`projects/00-stream-proxy`

1. **Go 服务**：`POST /chat` → 调用任一 OpenAI 兼容流式接口（如 GLM）→ 逐 token 以 SSE 转发给客户端。
   - 客户端断开时通过 context 取消上游请求（验证：断开后上游连接立即关闭）
   - 429/5xx 指数退避重试；整次请求 60s 超时
2. **TS CLI 客户端**：用 `fetch` + AsyncGenerator 打印流式输出，`Ctrl+C` 触发 `AbortController`。
3. **结构化输出**：TS 端用 Zod 定义 `{title, tags[], summary}`，要求模型输出 JSON 并校验，失败自动重试一次。
4. **Docker 化**：多阶段构建，镜像 < 30MB。

**完成标准**（达到才在 ROADMAP 打勾）
- [ ] 断开客户端后，服务端日志显示上游已取消
- [ ] 并发 20 个请求无 goroutine 泄漏（`runtime.NumGoroutine` 回落）
- [ ] Zod 校验失败能自动重试并记录日志
- [ ] 当天日志写进 `logs/YYYY-MM-DD.md`
