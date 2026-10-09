<div align="center">

<img src="docs/hero-mono.webp" alt="AI Agent 全栈学习路线" width="100%" />

# AI Agent 全栈开发路线

**从并发基础到生产上线：一份能跑、能测、每天更新的 AI Agent 工程师学习路线。**

用 Go / TypeScript 写 Agent 运行时，用 Python 读懂模型生态。<br/>
每一课都有课件、可运行练习、测试和参考答案。

[![Progress](https://img.shields.io/badge/progress-2%2F54-111111?style=flat-square)](ROADMAP.md)
[![Go](https://img.shields.io/badge/Go-1.23+-111111?style=flat-square&logo=go&logoColor=white)](projects/00-stream-proxy)
[![TypeScript](https://img.shields.io/badge/TypeScript-strict-111111?style=flat-square&logo=typescript&logoColor=white)](projects/01-ts-agent-core)
[![Python](https://img.shields.io/badge/Python-3.12+-111111?style=flat-square&logo=python&logoColor=white)](notes/00-foundations/03-python.md)
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

这个仓库按工程师的视角，把这些问题拆成 54 个知识点、11 个模块，**每个知识点都要求有代码、有测试、能跑通**才算完成。

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
| 0.1 | Go 并发 | goroutine、channel、context 取消链、errgroup、可取消 SSE | [课件](notes/00-foundations/01-go-concurrency.md) | [`00-stream-proxy`](projects/00-stream-proxy) | ✅ 完成 |
| 0.2 | TypeScript | 可辨识联合、Zod、AsyncGenerator、AbortController | [课件](notes/00-foundations/02-typescript.md) | [`01-ts-agent-core`](projects/01-ts-agent-core) | ✅ 完成 |
| 0.3 | Python | uv、Pydantic、asyncio TaskGroup、异步生成器 | [课件](notes/00-foundations/03-python.md) | 进行中 | 🔄 学习中 |
| 0.4 | 流式协议 | HTTP / SSE / WebSocket | 即将更新 | | ⏳ |
| 0.5 | 工程基础 | Git 工作流、Docker、Linux | 即将更新 | | ⏳ |

### 全部模块

| 模块 | 内容 | 进度 |
|---|---|---|
| 00 基础功底 | Go / TS / Python、流式协议、工程基础 | `██░░░` 2/5 |
| 01 LLM 原理与使用 | Transformer、主流 API、结构化输出、推理模型、Prompt Caching | `░░░░░░` 0/6 |
| 02 工具调用与协议 | Function Calling、MCP、A2A、Computer Use、代码沙箱 | `░░░░░` 0/5 |
| 03 Agent 架构 | ReAct、Agent Loop、多智能体、长任务、Coding / Deep Research Agent | `░░░░░░` 0/6 |
| 04 框架与 SDK | LangGraph、OpenAI / Claude Agent SDK、Eino、Vercel AI SDK、Mastra | `░░░░░░` 0/6 |
| 05 记忆与检索 | 向量库、混合检索、Rerank、GraphRAG、Agent 记忆 | `░░░░` 0/4 |
| 06 评测与可观测性 | Eval 体系、SWE-bench / τ-bench、OpenTelemetry、CI Eval | `░░░░` 0/4 |
| 07 安全与可靠性 | Prompt Injection、权限沙箱、Guardrails、Reward Hacking | `░░░░` 0/4 |
| 08 工程化与部署 | Go / Node 服务、Temporal、流式前端、模型网关、vLLM | `░░░░░░` 0/6 |
| 09 模型侧前沿 | LoRA、DPO / GRPO、Agentic RL、合成数据、量化部署 | `░░░░` 0/4 |
| 10 项目实战 | MCP 助理、生产级 RAG、多智能体 Coding Agent、上线监控 | `░░░░` 0/4 |

完整清单见 [ROADMAP.md](ROADMAP.md)。

## 快速开始

```bash
git clone https://github.com/Duang777/ai-agent-fullstack-roadmap.git
cd ai-agent-fullstack-roadmap
```

运行任意一课的练习：

```bash
# 0.1 Go 并发
cd projects/00-stream-proxy && go test -race -count=1 ./...

# 0.2 TypeScript
cd projects/01-ts-agent-core && npm install && npm run typecheck && npm test
```

推荐学习方式：先读课件 → 关掉参考答案自己写练习 → 跑测试 → 再对照参考实现。

## 仓库结构

```
.
├── ROADMAP.md              # 54 个知识点清单，勾选即进度
├── notes/                  # 课件，按模块分目录
│   └── 00-foundations/
│       ├── 01-go-concurrency.md
│       ├── 02-typescript.md
│       └── 03-python.md
├── projects/               # 每课一个可运行的练习目录
│   ├── 00-stream-proxy/    # Go：fanIn、并行工具、可取消 SSE
│   └── 01-ts-agent-core/   # TS：Zod 事件、可取消 sleep、并行工具、SSE
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

## Star History

<a href="https://star-history.com/#Duang777/ai-agent-fullstack-roadmap&Date">
  <img src="https://api.star-history.com/svg?repos=Duang777/ai-agent-fullstack-roadmap&type=Date" alt="Star History Chart" width="100%" />
</a>

---

<div align="center">

由 [@duang777](https://github.com/Duang777) 持续更新

</div>
