# AI Agent 全栈开发工程师 · 知识点路线图（生产前沿级）

> 勾选规则：学完并有笔记/代码产出才打 `[x]`。面板按本文件的勾选数统计进度。

## 0. 基础功底
- [ ] Python 进阶：asyncio、类型标注、Pydantic v2
- [ ] TypeScript / Node.js：异步模型、Zod
- [ ] HTTP / SSE / WebSocket 流式协议
- [ ] Git 工作流、Docker、Linux 基础

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
- [ ] Coding Agent 架构（Claude Code / Codex / SWE-agent 拆解）
- [ ] Deep Research Agent 架构

## 4. 框架与 SDK
- [ ] LangGraph（状态图、持久化、interrupt）
- [ ] OpenAI Agents SDK
- [ ] Claude Agent SDK
- [ ] Google ADK / Vercel AI SDK
- [ ] 框架取舍：何时不用框架

## 5. 记忆与检索（RAG）
- [ ] Embedding、向量库（pgvector、Qdrant、Milvus）
- [ ] 分块、混合检索（BM25 + 向量）、Rerank
- [ ] GraphRAG / Agentic RAG
- [ ] Agent 记忆：短期/长期/情景记忆、上下文压缩

## 6. 评测与可观测性
- [ ] Eval 体系：离线集、LLM-as-Judge、人工标注
- [ ] Agent Benchmark：SWE-bench、Terminal-Bench、τ-bench、GAIA
- [ ] Tracing：OpenTelemetry GenAI 语义、Langfuse / LangSmith
- [ ] 回归测试与 CI 中的 Eval

## 7. 安全与可靠性
- [ ] Prompt Injection / 间接注入防御
- [ ] 权限与沙箱：最小权限、工具审批
- [ ] Guardrails、输出校验、可验证完成（Verification）
- [ ] Reward Hacking 与测试篡改检测

## 8. 工程化与生产部署
- [ ] 后端：FastAPI / Node 服务、任务队列（Celery、Temporal）
- [ ] 持久化工作流（Temporal / Inngest / Durable Execution）
- [ ] 流式前端：React/Next.js、Generative UI
- [ ] 模型网关：路由、降级、限流（LiteLLM 等）
- [ ] 成本与延迟优化：缓存、批处理、小模型蒸馏
- [ ] Kubernetes、Serverless、GPU 推理（vLLM、SGLang）

## 9. 模型侧前沿
- [ ] 微调：LoRA / QLoRA、SFT
- [ ] 偏好与强化学习：DPO、GRPO、Agentic RL
- [ ] 合成数据与轨迹数据构造
- [ ] 开源模型部署与量化

## 10. 项目实战（每项一个可运行仓库/目录）
- [ ] 带 MCP 工具的个人助理 Agent
- [ ] 生产级 RAG 问答服务（含 Eval + Tracing）
- [ ] 多智能体 Coding Agent（沙箱执行 + 测试验证）
- [ ] 部署上线：监控、告警、成本看板
