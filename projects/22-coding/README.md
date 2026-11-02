# 22-coding · 3.6 Coding Agent 架构：拆解 Claude Code、Codex、SWE-agent

对应课件：[`notes/03-agents/06-coding-agent.md`](../../notes/03-agents/06-coding-agent.md)

一个最小的 coding agent harness。模型用脚本化假实现，工具在临时目录里真实执行。

| 文件 | 内容 |
|---|---|
| `workspace.go` | 路径逃逸检查（`..`、绝对路径、符号链接）；100 行窗口 `Read`；精确替换 `Edit`（先读、唯一、过期重读、`.go` 语法关卡）；`Write`；`Undo` 快照；`Grep` 超过 50 条拒绝 |
| `shell.go` | 权限 `Policy`（deny → 复合命令 → 按词 allow → 问人，fail closed）；每命令独立进程、进程组超时、头尾截断、环境白名单、空输出提示 |
| `context.go` | 分层 `AGENTS.md`（`AGENTS.override.md` 优先、由浅到深、32 KiB 上限）；按 Codex 顺序拼开头 |
| `agent.go` | 循环；配置变化追加而非修改；可选折叠旧观察；`finish` 前 harness 强制验证 |
| `coding_test.go` | 15 个测试，最后一个让脚本模型修好一个 Go 函数并通过 `go test` |

```bash
make test
```

练习 2（编辑后 `go vet` 诊断）和练习 3（只读探索子代理）需要你自己写，参考答案在课件末尾。
