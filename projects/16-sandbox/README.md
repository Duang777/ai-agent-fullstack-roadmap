# 16-sandbox · 2.5 Code Execution 沙箱练习

对应课件：[`notes/02-tools/05-code-sandbox.md`](../../notes/02-tools/05-code-sandbox.md)

让 Agent 运行它自己写的代码，但不让它伤到你。测试会真的启动子进程（需要 Linux 或 macOS 的 `/bin/sh`）。注意：本机进程加 ulimit 不是安全边界，真实系统至少用容器加 gVisor，或微虚拟机、托管沙箱。

| 文件 | 内容 |
|---|---|
| `runner.go` | 执行端：进程组超时、环境变量白名单、临时目录、`ulimit` 限 CPU 和文件大小、输出保留头尾、`$OUTPUT_DIR` 只收普通文件 |
| `tool.go` | 结果回给模型：代码错误是正常结果，超时和资源限制是 `is_error`，并给出调整建议 |
| `netpolicy.go` | E2B 风格的出站规则：`AllowOut` 优先、域名只能放行、按域名放行必须拒绝其他全部流量 |
| `docker.go` | 加固的 `docker run` 参数（gVisor、无网络、只读、去 capabilities、非 root、内存 / CPU / 进程数）与危险参数检查 |
| `lifecycle.go` | 沙箱生命周期：续期、超时暂停或销毁、销毁后报错重建 |
| `sandbox_test.go` | 12 个测试 |

```bash
make test
```

练习 2（并发与租户配额）和练习 3（产物限额）需要你自己写，参考答案在课件末尾。
