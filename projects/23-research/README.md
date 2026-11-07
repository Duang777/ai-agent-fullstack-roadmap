# 23-research · 3.7 Deep Research Agent 架构：拆解 Anthropic Research、OpenAI、Gemini

对应课件：[`notes/03-agents/07-deep-research.md`](../../notes/03-agents/07-deep-research.md)

一个最小的 deep research 系统。网络是内存里的假语料（含一个 SEO 内容农场和一条注入指令），所有行为可重复。

| 文件 | 内容 |
|---|---|
| `web.go` | 假搜索引擎（命中词 × 10 + SEO 加成）、`Fetch`、来源质量判断、URL 归一、调用计数与最大并发 |
| `plan.go` | 按问题类型分配预算（查事实 / 对比 / 广泛调研）；拆任务 |
| `researcher.go` | 子研究员：先宽后窄、按来源质量重排、去重、够了就停、工具失败继续、注入隔离、工件 + 摘要 |
| `lead.go` | 主研究员：计划落盘、信号量并行、缺口驱动追加、每轮检查点、恢复 |
| `cite.go` | 写报告、引用核对（优先一手来源，没有出处标 `[?]`） |
| `eval.go` | 五项评分细则：事实、引用、完整、来源质量、效率 |
| `research_test.go` | 13 个测试 |

```bash
make test
```

练习 2（全局预算 + 子代理截止时间）和练习 3（协作式计划）需要你自己写，参考答案在课件末尾。
