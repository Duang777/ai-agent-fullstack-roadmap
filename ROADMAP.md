# AI Agent 全栈开发工程师 · 知识点路线图（生产级）

> 勾选规则：学完并有笔记 + 可运行代码 + 测试通过才打 `[x]`。
>
> 技术栈：主力 Go / TypeScript，Python 作为读懂 ML 生态与评测脚本的辅助语言。

**14 个模块 · 84 个知识点 · 4 个阶段 · 1 条主线项目**

## 学习阶段

| 阶段 | 目标 | 模块 |
|---|---|---|
| A 打地基 | 会用：语言、模型 API、工具调用 | 00、01、02 |
| B 能做出来 | 能跑：Agent 架构、框架、RAG | 03、04、05 |
| C 上生产 | 扛得住：评测、可观测、高可用、高性能、安全、部署 | 06、07、08、09、10、11 |
| D 深入与实战 | 做得深：模型侧训练 + 端到端项目 | 12、13 |

> 阶段 C 是“Demo”和“生产系统”的分水岭：一个 Agent 能演示不难，难的是知道它**有多准**（Eval）、**哪里坏了**（Tracing）、**挂了怎么办**（高可用）、**为什么慢、为什么贵**（性能与成本）。

## 0. 基础功底
- [x] Go 基础与并发：模块、错误处理、接口、JSON、测试；goroutine/channel/select、context 取消、errgroup、泛型
- [x] TypeScript：严格类型、Zod、async iterator、AbortController
- [x] Python（够用即可）：asyncio、Pydantic、uv
- [x] HTTP / SSE / WebSocket 流式协议
- [x] Git 工作流、Docker、Linux 基础
- [ ] 数据库与缓存基础：Postgres 建模、索引、事务与隔离级别、迁移；Redis 数据结构

## 1. LLM 原理与使用
- [ ] Transformer、Tokenizer、采样参数（temperature/top-p）
- [ ] 主流 API：OpenAI Responses、Anthropic Messages、Gemini
- [ ] 结构化输出：JSON Schema、constrained decoding
- [ ] 推理模型（reasoning / thinking budget）与成本权衡
- [ ] 上下文窗口、Prompt Caching、长上下文退化
- [ ] Prompt / Context Engineering 系统方法

## 2. 工具调用与协议
- [ ] Function Calling / Tool Use 机制与并行调用
- [ ] MCP（Model Context Protocol）：Server/Client、Tools/Resources/Prompts
- [ ] A2A（Agent-to-Agent）协议
- [ ] Computer Use / Browser Use 代理
- [ ] Code Execution 沙箱（E2B、Docker、gVisor）

## 3. Agent 架构
- [ ] ReAct、Plan-and-Execute、Reflexion
- [ ] Agent Loop 设计：停止条件、预算、重试
- [ ] 多智能体：Supervisor、Handoff、Swarm
- [ ] 长任务 Agent：检查点、可恢复执行、Human-in-the-loop
- [ ] Agent 状态与数据模型：会话 / 消息 / run / step / 工具调用的存储设计、事件溯源、完整重放
- [ ] Coding Agent 架构（Claude Code / Codex / SWE-agent 拆解）
- [ ] Deep Research Agent 架构

## 4. 框架与 SDK
- [ ] LangGraph（状态图、持久化、interrupt）
- [ ] OpenAI Agents SDK
- [ ] Claude Agent SDK
- [ ] Go 生态：Eino（CloudWeGo）、Genkit Go、官方 MCP Go SDK
- [ ] TS 生态：Vercel AI SDK、Mastra、OpenAI Agents SDK (JS)
- [ ] 框架取舍：何时不用框架

## 5. 记忆与检索（RAG）
- [ ] Embedding、向量库（pgvector、Qdrant、Milvus）
- [ ] 分块、混合检索（BM25 + 向量）、Rerank
- [ ] GraphRAG / Agentic RAG
- [ ] Agent 记忆：短期/长期/情景记忆、上下文压缩
- [ ] 检索质量评测：Recall@k、MRR、上下文相关性、答案忠实度

## 6. 评测体系（Eval）
- [ ] Agent 测试工程：假模型与录制回放（VCR）、工具契约测试、轨迹快照；分清确定性单测与概率性 Eval
- [ ] Eval 设计：从真实失败案例构造任务集、黄金集、数据版本化
- [ ] 指标：任务成功率、pass@k 与 pass^k（稳定性）、工具调用正确率、轨迹评估
- [ ] LLM-as-Judge：评分细则（rubric）、位置/长度偏差、与人工标注一致性校准
- [ ] 公开 Benchmark：SWE-bench Verified、Terminal-Bench、τ²-bench、GAIA、BrowseComp
- [ ] Eval 框架：Inspect AI、promptfoo、OpenAI Evals、Braintrust
- [ ] CI 回归门禁：改 prompt / 换模型 / 改工具时自动跑 Eval，分数下降即阻断
- [ ] 模型版本管理与迁移：锁定模型快照版本、下线前迁移、换模型的回归对比与灰度
- [ ] 线上评估：A/B 实验、灰度对比、用户反馈回流成新用例

## 7. 可观测性（Tracing / Metrics / Logs）
- [ ] Tracing：OpenTelemetry GenAI 语义约定，LLM / 工具 / 检索 span 设计
- [ ] 平台：Langfuse、LangSmith、Arize Phoenix；自建 OTel Collector + Jaeger / Tempo
- [ ] 核心指标：TTFT、tokens/s、P50/P99 延迟、单请求成本、工具错误率（Prometheus + Grafana）
- [ ] 结构化日志与会话回放：一次失败能从 trace 定位到具体哪步、哪个工具
- [ ] SLO 与告警：可用性 / 延迟目标、错误预算、值班手册

## 8. 高可用与可靠性
- [ ] 超时、重试（指数退避 + 抖动）、幂等键，避免重复扣费和重复执行工具
- [ ] 熔断、限流（令牌桶）、背压、舱壁隔离
- [ ] 多模型供应商容灾：fallback 链、健康检查、自动降级到小模型
- [ ] 持久化执行：Temporal / Restate / Inngest，进程崩溃后从断点恢复
- [ ] 异步与后台 Agent：任务队列、Webhook、定时触发、结果通知，长任务不挂在一条 HTTP 连接上
- [ ] 无状态服务与水平扩展：状态外置到 Postgres / Redis，流式连接的会话保持
- [ ] 故障演练：注入超时、429、断流，验证系统按预期降级

## 9. 高性能与成本
- [ ] 流式链路与首 token 延迟优化
- [ ] 缓存：Prompt Caching、语义缓存、工具结果缓存
- [ ] 服务端性能：连接池、并发调优、压测（k6 / vegeta）与性能剖析（pprof）
- [ ] 推理服务：vLLM / SGLang 连续批处理、投机解码、量化
- [ ] 模型路由：大小模型分层、按任务难度路由
- [ ] 成本治理：按用户 / 租户计量、预算上限、用量看板

## 10. 安全与治理
- [ ] Prompt Injection / 间接注入防御、第三方 MCP Server 审查与工具投毒
- [ ] 权限与沙箱：最小权限、高风险工具人工审批
- [ ] 身份、凭证与多租户：用户鉴权、租户隔离、代用户调用第三方的 OAuth 2.1（MCP 授权规范）、密钥管理与轮换
- [ ] Guardrails、输出校验、可验证完成（Verification）
- [ ] Reward Hacking 与测试篡改检测
- [ ] 数据与合规：PII 脱敏、审计日志、多租户隔离
- [ ] 内容安全与合规（国内）：生成式 AI 服务备案、输入输出内容审核、日志留存、数据出境

## 11. 工程化与部署
- [ ] 后端：Go 服务（net/http、gRPC）+ Node 服务、任务队列
- [ ] 流式前端：React / Next.js、Generative UI、审批 / 进度 / 中途打断等 Agent 交互
- [ ] 模型网关：统一接口、鉴权、配额（LiteLLM 或自研）
- [ ] 容器化与编排：Docker、Kubernetes、HPA 自动扩缩容
- [ ] CI/CD 与版本管理：prompt、模型、工具定义像代码一样版本化
- [ ] 发布策略：灰度、Feature Flag、一键回滚

## 12. 模型侧前沿
- [ ] 微调：LoRA / QLoRA、SFT
- [ ] 偏好与强化学习：DPO、GRPO、Agentic RL
- [ ] 合成数据与轨迹数据构造
- [ ] 开源模型部署与量化

## 主线项目：agent-platform

每课的练习是独立的小目录，用来把一个知识点讲透。但生产能力来自**同一个系统在不断加需求中活下来**，所以另有一条贯穿全程的主线项目 `projects/agent-platform/`，以 0.5 的 `04-ship` 为起点，每个模块结束时给它加一层：

| 模块 | 给主线项目加什么 |
|---|---|
| 00 | 服务骨架：健康检查、优雅退出、Docker、CI；Postgres 与 Redis |
| 01 | 接入多家模型 API，统一流式接口与结构化输出 |
| 02 | 工具调用与 MCP 客户端，代码执行沙箱 |
| 03 | Agent Loop、会话与运行记录持久化、可重放 |
| 04 | 与一个框架实现对照，决定保留什么 |
| 05 | 记忆与 RAG |
| 06 | 测试分层 + Eval 集 + CI 回归门禁 |
| 07 | OpenTelemetry Tracing、指标看板 |
| 08 | 重试幂等、多供应商容灾、后台任务、断点恢复 |
| 09 | 压测报告、缓存、模型路由、成本计量 |
| 10 | 鉴权与多租户、权限审批、内容审核 |
| 11 | 前端、K8s 部署、灰度与回滚 |
| 13 | 真实部署上线，跑出 SLO 数据，写一次事故复盘 |

## 13. 项目实战（每项一个可运行目录）

> 每个项目开工前先写一页设计文档：目标、容量估算、关键取舍、失败模式。
- [ ] 带 MCP 工具的个人助理 Agent
- [ ] 生产级 RAG 问答服务：Eval + Tracing + 缓存
- [ ] 多智能体 Coding Agent：沙箱执行 + 测试验证 + SWE-bench 子集评测
- [ ] 高可用 Agent 网关：多供应商容灾、限流、计量，附压测报告
- [ ] 上线运营：SLO 看板、告警、成本看板、一次完整的事故复盘

## 生产就绪检查清单（每个实战项目上线前对照）

- 有离线 Eval 集，CI 里跑，分数有基线
- 每次请求都有完整 trace，能从一条用户投诉定位到具体步骤
- 所有外部调用都有超时、重试和幂等保护
- 主模型不可用时能自动切换或降级
- 用户断开后，上游模型调用和工具执行会停止
- 压测过：知道单实例 QPS 上限、P99 延迟和单请求成本
- 有 SLO 和告警，告警有对应的处理手册
- 高风险工具有权限控制和审批，日志可审计
- 能灰度发布、能一键回滚
- 模型版本已锁定，换模型有回归对比报告
- 任意一次运行都能从存储里完整重放
- 单元测试不调用真实模型，CI 稳定且不花钱
- 租户之间数据和凭证隔离，密钥可轮换
- 有数据保留与删除策略
- 有单用户 / 单租户的成本告警
