<div align="center">

<img src="docs/hero-mono.webp" alt="AI Agent 全栈学习路线" width="100%" />

# AI Agent 全栈开发路线

**从并发基础到生产上线：一份能跑、能测、每天更新的 AI Agent 工程师学习路线。**

用 Go / TypeScript 写 Agent 运行时，用 Python 读懂模型生态。<br/>
每一课都有课件、可运行练习、测试和参考答案。

[![Progress](https://img.shields.io/badge/progress-2%2F77-111111?style=flat-square)](ROADMAP.md)
[![Go](https://img.shields.io/badge/Go-1.23+-111111?style=flat-square&logo=go&logoColor=white)](projects/00-stream-proxy)
[![TypeScript](https://img.shields.io/badge/TypeScript-strict-111111?style=flat-square&logo=typescript&logoColor=white)](projects/01-ts-agent-core)
[![Python](https://img.shields.io/badge/Python-3.12+-111111?style=flat-square&logo=python&logoColor=white)](notes/00-foundations/03-python.md)
[![License](https://img.shields.io/badge/license-MIT%20%2B%20CC%20BY--NC--SA-111111?style=flat-square)](#许可证)
[![Last commit](https://img.shields.io/github/last-commit/Duang777/ai-agent-fullstack-roadmap?style=flat-square&color=111111)](https://github.com/Duang777/ai-agent-fullstack-roadmap/commits/main)
[![Stars](https://img.shields.io/github/stars/Duang777/ai-agent-fullstack-roadmap?style=flat-square&color=111111)](https://github.com/Duang777/ai-agent-fullstack-roadmap/stargazers)

[**在线主页**](https://duang777.github.io/ai-agent-fullstack-roadmap/) · [**完整路线图**](ROADMAP.md) · [**每日日志**](logs/) · [**课件**](notes/)

</div>

---

## 为什么有这个仓库

市面上的 Agent 教程大多停在“调一下 API、套一个框架”。真正把 Agent 做进生产，难点在别处：

- 用户关掉页面，**上游模型调用和工具有没有真的停下来**？
- 三个工具并行，一个卡住 5 秒，**整轮会不会被拖死**？
- 模型给的 JSON 参数是错的，**程序崩溃还是把错误回传给模型自我修正**？
- 怎么证明 Agent“做完了”，而不是“说做完了”？

这个仓库按工程师的视角，把这些问题拆成 77 个知识点、14 个模块，**每个知识点都要求有代码、有测试、能跑通**才算完成。

## 特点

| | |
|---|---|
| **代码先行** | 每课配一个可运行目录，`go test -race` / `vitest` / `pytest` 全绿才勾选 |
| **三语对照** | 同一个问题（并行工具、超时、取消、流式）分别用 Go、TS、Python 实现并对比 |
| **逐行讲解** | 课件里每段核心代码后都有逐行解读、设计取舍和常见坑 |
| **附参考答案** | 每课都有练习、自测题、参考实现和详细解释 |
| **公开进度** | 每天一份日志，ROADMAP 勾选即进度，主页同步展示 |

## 课程进度

### 00 · 基础功底

| # | 主题 | 核心内容 | 课件 | 练习 | 状态 |
|---|---|---|---|---|---|
| 0.1 | Go 基础与并发 | 模块、错误处理、接口、JSON、表驱动测试；goroutine、channel、context、errgroup、可取消 SSE | [课件](notes/00-foundations/01-go.md) | [`00-stream-proxy`](projects/00-stream-proxy) | ✅ 完成 |
| 0.2 | TypeScript | 可辨识联合、Zod、AsyncGenerator、AbortController | [课件](notes/00-foundations/02-typescript.md) | [`01-ts-agent-core`](projects/01-ts-agent-core) | ✅ 完成 |
| 0.3 | Python | uv、Pydantic、asyncio TaskGroup、异步生成器 | [课件](notes/00-foundations/03-python.md) | 进行中 | 🔄 学习中 |
| 0.4 | 流式协议 | HTTP 分块与 Flush、SSE 解析器、断线续传、中间层缓冲、WebSocket 双向打断 | [课件](notes/00-foundations/04-streaming.md) | [`03-streaming`](projects/03-streaming) | 📖 课件已出 |
| 0.5 | 工程基础 | Git 工作流、Docker、Linux | 即将更新 | | ⏳ |

### 全部模块

四个阶段：**A 打地基** → **B 能做出来** → **C 上生产** → **D 深入与实战**。阶段 C 是 Demo 和生产系统的分水岭：Eval 回答“有多准”，Tracing 回答“哪里坏了”，高可用回答“挂了怎么办”，性能与成本回答“为什么慢、为什么贵”。

| 阶段 | 模块 | 内容 | 进度 |
|---|---|---|---|
| A | 00 基础功底 | Go / TS / Python、流式协议、工程基础 | `██░░░` 2/5 |
| A | 01 LLM 原理与使用 | Transformer、主流 API、结构化输出、推理模型、Prompt Caching | `░░░░░░` 0/6 |
| A | 02 工具调用与协议 | Function Calling、MCP、A2A、Computer Use、代码沙箱 | `░░░░░` 0/5 |
| B | 03 Agent 架构 | ReAct、Agent Loop、多智能体、长任务、Coding / Deep Research Agent | `░░░░░░` 0/6 |
| B | 04 框架与 SDK | LangGraph、OpenAI / Claude Agent SDK、Eino、Vercel AI SDK、Mastra | `░░░░░░` 0/6 |
| B | 05 记忆与检索 | 向量库、混合检索、Rerank、GraphRAG、记忆、检索评测 | `░░░░░` 0/5 |
| C | 06 评测体系 | 任务集构造、pass^k、LLM-as-Judge 校准、SWE-bench / τ²-bench、CI 门禁、A/B | `░░░░░░░` 0/7 |
| C | 07 可观测性 | OpenTelemetry GenAI、Langfuse、TTFT / P99 / 成本指标、会话回放、SLO | `░░░░░` 0/5 |
| C | 08 高可用与可靠性 | 超时重试幂等、熔断限流、多供应商容灾、Temporal 断点恢复、故障演练 | `░░░░░░` 0/6 |
| C | 09 高性能与成本 | 首 token 延迟、缓存、压测与 pprof、vLLM / SGLang、模型路由、成本治理 | `░░░░░░` 0/6 |
| C | 10 安全与治理 | Prompt Injection、权限沙箱、Guardrails、Reward Hacking、PII 与审计 | `░░░░░` 0/5 |
| C | 11 工程化与部署 | Go / Node 服务、流式前端、模型网关、K8s、Prompt 版本化、灰度回滚 | `░░░░░░` 0/6 |
| D | 12 模型侧前沿 | LoRA、DPO / GRPO、Agentic RL、合成数据、量化部署 | `░░░░` 0/4 |
| D | 13 项目实战 | MCP 助理、生产级 RAG、Coding Agent、高可用网关、上线运营 | `░░░░░` 0/5 |

完整清单和**生产就绪检查清单**见 [ROADMAP.md](ROADMAP.md)。

## 快速开始

```bash
git clone https://github.com/Duang777/ai-agent-fullstack-roadmap.git
cd ai-agent-fullstack-roadmap
```

运行任意一课的练习：

```bash
# 0.1 Go
cd projects/00-stream-proxy && go test -race -count=1 ./...

# 0.2 TypeScript
cd projects/01-ts-agent-core && npm install && npm run typecheck && npm test

# 0.4 流式协议
cd projects/03-streaming && npm install && npm run typecheck && npm test
```

推荐学习方式：先读课件 → 关掉参考答案自己写练习 → 跑测试 → 再对照参考实现。

## 仓库结构

```
.
├── ROADMAP.md              # 77 个知识点清单 + 生产就绪检查清单
├── notes/                  # 课件，按模块分目录
│   └── 00-foundations/
│       ├── 01-go.md
│       ├── 02-typescript.md
│       ├── 03-python.md
│       └── 04-streaming.md
├── projects/               # 每课一个可运行的练习目录
│   ├── 00-stream-proxy/    # Go：fanIn、并行工具、可取消 SSE
│   ├── 01-ts-agent-core/   # TS：Zod 事件、可取消 sleep、并行工具、SSE
│   └── 03-streaming/       # TS：SSE 解析器、SSE 服务端、续传与断开即停
├── logs/                   # 每日学习日志
└── docs/                   # GitHub Pages 主页
```

## 每一课的结构

```
为什么学这个  →  核心概念（Go / TS / Python 对照）  →  逐行讲解的代码
      →  常见坑  →  练习  →  自测题  →  参考答案与详解
```

## 一起学

- 觉得有用，点个 **Star** 方便追更，每天都会有新内容。
- 发现错误或有更好的写法，欢迎提 [Issue](https://github.com/Duang777/ai-agent-fullstack-roadmap/issues) 或 PR。
- 想一起打卡：Fork 这个仓库，用 `logs/TEMPLATE.md` 写你自己的日志。

## 许可证

- 代码（`projects/` 及其他源码）：[MIT](LICENSE)
- 课件与文档（`notes/`、`logs/`、`ROADMAP.md`）：[CC BY-NC-SA 4.0](notes/LICENSE)，转载请注明出处，禁止商用，改编需以相同协议共享

## Star History

<a href="https://star-history.com/#Duang777/ai-agent-fullstack-roadmap&Date">
  <img src="https://api.star-history.com/svg?repos=Duang777/ai-agent-fullstack-roadmap&type=Date" alt="Star History Chart" width="100%" />
</a>

---

<div align="center">

由 [@duang777](https://github.com/Duang777) 持续更新

</div>
