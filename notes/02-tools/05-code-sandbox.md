# 2.5 Code Execution 沙箱（E2B、Docker、gVisor）

> 第 2 部分 · 第 5 课 ｜ 预计 4–5 小时 ｜ 前置：0.5 工程基础（Docker、Linux 信号）、2.1 Function Calling、2.4 Computer Use
>
> 配套练习：`projects/16-sandbox/`（纯 Go 标准库，12 个测试，会真的启动子进程）。把执行端必须做对的几件事做成可测试的：超时要杀掉整个进程组、环境变量不能带密钥、输出截断保留头尾、CPU 和文件大小限制、产物只收普通文件、代码错误和环境错误分开回给模型、E2B 风格的出站网络规则、加固过的 `docker run` 参数和危险参数检查、沙箱的暂停和销毁。

## 读前说明

2.4 课里模型操作的是屏幕。这一课它直接写代码让你跑：分析一份 CSV、画张图、跑一遍测试、算一道题。代码是模型现场写的，你没审过，而且模型可能被它读到的内容操纵（2.4 课的提示词注入）。所以要把“运行一段不可信代码”当作默认场景来设计。

先看几个问题：

- 三家模型自带的代码执行工具和自己搭沙箱，怎么选？
- `os/exec` 起一个子进程、加个超时，算不算沙箱？
- 超时了，你杀掉了 `sh`，为什么 CPU 还是满的？
- 你的服务进程里有 API Key，代码里一句 `env` 会看到什么？
- Docker 容器为什么不算强隔离？gVisor 和微虚拟机又多隔离了什么？
- 代码输出了 200 MB 日志，或者在产物目录里放了一个指向 `/etc/passwd` 的符号链接，会怎样？
- 代码报错和执行超时，回给模型时应该一样吗？

本课结束时你能：

1. 对照 Anthropic、OpenAI、Gemini 自带的代码执行工具，以及 E2B 这类托管沙箱；
2. 讲清进程、容器、gVisor、微虚拟机四层隔离各挡住了什么；
3. 写一个能正确超时、限制资源、清理环境、截断输出、安全收集产物的执行端；
4. 配置出站网络规则和加固的容器参数；
5. 处理沙箱的生命周期：续期、暂停、销毁后重建。

**关于版本**：本课按 2026 年 10 月的官方文档写：Anthropic 的 `code_execution_20250825` / `20260120` / `20260521`，OpenAI Responses 的 `code_interpreter`，Gemini 的 `code_execution`，E2B SDK 文档，gVisor 与 Docker 官方文档。限额和价格会变，动手前先核对。

---

# 第一部分：用谁的沙箱

## 1. 三家自带的代码执行

最省事的办法是不自己跑：让模型厂商在它们的服务器上跑。这类工具是**服务端工具**，和 2.1 课你自己执行的函数不同，调用和结果都在同一个响应里出现，你只看结果。

| | Anthropic | OpenAI | Gemini |
|---|---|---|---|
| 声明 | `{"type": "code_execution_20250825", "name": "code_execution"}` | `{"type": "code_interpreter", "container": {"type": "auto"}}` | `{"type": "code_execution"}` |
| 能做什么 | Bash 命令和文件操作（子工具 `bash_code_execution`、`text_editor_code_execution`）；`20260120` 起加了 REPL 状态保持和程序化工具调用 | 写并运行 Python，处理文件、生成图表 | 生成并运行 Python |
| 资源 | 5 GiB 内存、5 GiB 磁盘、1 个 CPU | 内存档位 `1g`（默认）、`4g`、`16g`、`64g` | 单次运行最长 30 秒 |
| 网络 | 完全没有，运行时装不了包 | — | — |
| 状态 | 把响应里的 `container.id` 传回去就能复用；闲置约 5 分钟后存档，30 天内可恢复 | 容器 20 分钟不用就过期，数据丢弃，不能恢复 | 出错时模型最多重新生成 5 次代码 |
| 产物 | 写进 `$OUTPUT_DIR` 顶层的文件作为 `file_id` 返回 | 生成的文件在容器里，要在过期前下载 | 输出文件以内联数据返回 |

几个值得记的细节：

- Anthropic 的三个版本都不需要 beta 头。`20260521` 和 `20260120` 是同一个运行环境，区别是工具描述里告诉模型：程序化工具调用里每个 Python 单元有 90 秒的墙钟上限，让它自己安排长任务。计费是每个组织每月 1,550 小时免费，之后每个容器每小时 0.05 美元；和新版 web search / web fetch 一起用时，代码执行不额外收费。它**不在 ZDR（零数据保留）范围内**，容器数据最多保留 30 天。
- OpenAI 的容器可以“自动模式”随响应创建，也可以用 `/v1/containers` 显式创建再引用。过期规则很严格：20 分钟不用，内存里的 Python 对象和文件全部没了，只能新建一个、重新上传文件。
- Gemini 的环境装不了你自己的库，适合计算和画图这种轻任务。

什么时候用自带工具：数据分析、计算、画图、处理用户上传的文件，代码不需要访问你的内网和数据库。它们的隔离、资源、清理都由厂商负责。

什么时候自己搭：代码要访问你的私有依赖、内部服务或者大量数据；要用特定语言和工具链（Go、Node、编译器）；要运行很久；数据不能离开你的环境；或者你在做 Coding Agent，要在仓库里跑测试。这时候可以用托管沙箱（E2B 等），也可以自己用容器和 gVisor 搭。

## 2. 托管沙箱：E2B

E2B 提供按需创建的 Linux 虚拟机，几行代码就能起一个：

```python
from e2b import Sandbox

sandbox = Sandbox.create(timeout=60)   # Python 的单位是秒
result = sandbox.commands.run('echo "Hello from E2B Sandbox!"')
print(result.stdout)
```

注意一个坑：JavaScript SDK 的 `timeoutMs` 单位是**毫秒**，Python 的 `timeout` 是**秒**，文档里专门用警告标出来了。

生命周期上，E2B 的几个规则：超时到了可以自动暂停而不是销毁；`setTimeout` / `set_timeout` 会把超时重置为“从现在起”的新值，适合每次用户操作时续期；连续运行最长 24 小时（Pro）或 1 小时（Hobby），暂停会重置这个窗口；暂停会保存文件系统**和内存**（运行中的进程、加载的变量），暂停的沙箱永久保留，直到你主动销毁。

网络上，**E2B 默认允许出站访问**。可以用 `allow_internet_access=False` 完全关掉，或者用 `allow_out` / `deny_out` 做细粒度控制。规则在练习的 `netpolicy.go` 里实现了，下一部分细讲。

---

# 第二部分：隔离的层次

## 3. 威胁模型

先想清楚要防什么。不可信代码可能做的事：

| 威胁 | 例子 | 靠什么挡 |
|---|---|---|
| 逃逸到宿主机 | 利用内核漏洞 | 隔离层次：容器 < gVisor < 微虚拟机 |
| 耗尽资源 | 死循环、fork 炸弹、写满磁盘、吃光内存 | 超时、CPU / 内存 / 进程数 / 文件大小限制 |
| 偷密钥 | `env`、读 `~/.aws/credentials` | 干净的环境变量、不挂载宿主机目录 |
| 横向移动 | 访问内网服务、`169.254.169.254` 云元数据 | 关网络或出站白名单 |
| 外传数据 | 把用户文件 POST 到外部服务器 | 出站白名单 |
| 跨用户残留 | 上一个用户的文件还在 | 一次性环境、用完销毁 |
| 坑执行端 | 200 MB 输出、符号链接指向宿主机文件、输出里写着注入指令 | 截断输出、只收普通文件、输出当不可信内容 |

前两类靠隔离和资源限制，后面几类常常是执行端自己写出来的漏洞，和用不用容器无关。

## 4. 四层隔离

**第一层：普通进程加 ulimit。** 练习的 `Run` 就在这一层：子进程、自己的进程组、干净的环境变量、临时目录、`ulimit` 限制 CPU 时间和文件大小。它能挡住资源耗尽和一部分密钥泄漏，但**不是安全边界**：子进程和你的服务是同一个用户，能读你能读的所有文件，能访问你能访问的所有网络。练习用它是因为它能在任何机器上跑测试，真实系统至少要到下一层。

**第二层：容器。** Linux 命名空间（看不到宿主机的进程、文件、网络）加 cgroups（限资源）加 seccomp（限系统调用）。Docker 的默认 seccomp 配置是一个白名单：默认拒绝，再放行一批具体的系统调用，在 300 多个里禁掉了大约 44 个，官方不建议改它。但容器和宿主机**共享同一个内核**，内核有漏洞就可能逃逸。

**第三层：gVisor。** gVisor 官方文档说它既不是 seccomp 那种系统调用过滤器，也不是普通意义上的虚拟机，而是第三种方式：在用户态拦截应用的系统调用，自己扮演“客户机内核”。应用碰不到宿主机内核的大部分代码，攻击面小很多；代价是兼容性差一些、每次系统调用开销更大。用法是换一个容器运行时：`docker run --runtime=runsc ...`。

**第四层：微虚拟机。** Firecracker 是 AWS 为 Lambda 和 Fargate 开发的开源虚拟机监控器，基于 KVM 创建“微虚拟机”：只保留最少的设备，内存占用小、启动快，同时有硬件虚拟化的隔离。每个沙箱有自己的内核。Kata Containers 也把这类技术集成进了容器运行时。

选择上的经验：自己内部的、代码来源可控的任务，加固过的容器就够；运行用户提交的代码、多租户、模型可能被注入的场景，至少用 gVisor，最好是微虚拟机或托管沙箱。

## 5. 加固容器

练习的 `DockerArgs` 生成一组加固参数，每一项都在收窄攻击面：

| 参数 | 作用 |
|---|---|
| `--rm` | 用完删除，不留残留 |
| `--runtime=runsc` | 用 gVisor |
| `--network=none` | 没有网络 |
| `--read-only` + `--tmpfs /tmp:...,noexec` | 根文件系统只读，只有 `/tmp` 可写且不能执行 |
| `--cap-drop=ALL` | 去掉所有 Linux capabilities |
| `--security-opt=no-new-privileges` | setuid 程序也不能提权 |
| `--user=65534:65534` | 用 nobody，不用 root |
| `--memory=512m --memory-swap=512m` | 内存上限，并且不许用 swap 绕过 |
| `--cpus=1` | CPU 上限 |
| `--pids-limit=128` | 防 fork 炸弹 |

`LintDockerArgs` 反过来检查几个“一出现就等于没隔离”的参数：`--privileged`（访问宿主机全部设备）、`--network=host`（共享宿主机网络，能访问宿主机上的本地服务）、`--pid=host`、挂载 `/var/run/docker.sock`（能控制宿主机的 Docker，等于宿主机 root）、`seccomp=unconfined`。这些常常是为了“先跑起来”加上去的，然后就忘了删。`TestDockerArgs` 两个方向都测了。

## 6. 出站网络

最安全的是没有网络。但 Agent 常常需要装包（PyPI、npm）或调用某个 API，这时用白名单。

E2B 的规则（练习的 `NetPolicy` 照着实现）：

- `allow_out` 和 `deny_out` 可以写 IP 和 CIDR，`allow_out` 还可以写域名；
- **命中 `allow_out` 就放行，优先于 `deny_out`**；
- 按域名放行时，必须同时在 `deny_out` 里拒绝全部流量（`0.0.0.0/0`），`deny_out` 不支持域名；
- 用了域名规则时，默认的 DNS 服务器 `8.8.8.8` 自动放行，否则域名解析不了；
- 什么都不配时，**默认放行**。

```go
d := NetPolicy{AllowOut: []string{"pypi.org"}, DenyOut: []string{AllTraffic}}
d.Allowed("pypi.org", ip("151.101.0.223"))  // true
d.Allowed("evil.example", ip("6.6.6.6"))    // false
```

`TestNetPolicy` 覆盖了这几条，`Validate` 会拒绝“只写域名白名单、不拒绝其他流量”这种看起来有白名单、实际全开的配置。

即使开了白名单，也要想到：放行的域名本身可能被用来外传数据（比如允许了一个可以上传文件的服务）。白名单越窄越好，按任务开，用完关。

---

# 第三部分：执行端

这一部分是练习的主体，每一节对应一两个测试。就算你用托管沙箱，这些问题大部分也要你自己处理。

## 7. 超时要杀整个进程组

开头第三个问题。

```go
cmd := exec.Command("/bin/sh", "-c", prefix.String()+s.Code)
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
...
case <-ctx.Done():
	r.TimedOut = true
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // 负号 = 整个进程组
```

逐行看：

- `exec.Command` 而不是 `exec.CommandContext`：后者超时只杀直接子进程 `sh`；
- `Setpgid: true`：让 `sh` 自己当一个新进程组的组长，它派生的子进程默认都在这个组里；
- `syscall.Kill(-pid, SIGKILL)`：pid 取负数表示发给整个进程组。0.5 课讲过信号，这里是它的实际用法。

`TestTimeoutKillsProcessGroup` 的代码是 `sleep 30 & echo $! > child.pid; wait`：后台起一个 `sleep`，把它的 pid 写到文件里。300 毫秒后超时，测试检查后台的 `sleep` 是否也死了（`kill(pid, 0)` 返回错误说明进程不存在）。如果只杀 `sh`，后台进程会变成孤儿继续跑。真实场景里那可能是一个训练脚本、一个启动起来的 Web 服务、一个挖矿程序。

在容器里这一步更简单：杀掉容器就是杀掉里面的一切。但如果你在一个长期存在的沙箱里执行多次命令（E2B、复用的容器），每次命令的超时仍然要杀进程组。

## 8. 环境变量和工作目录

开头第四个问题。`exec.Command` 默认**继承父进程的全部环境变量**，你的服务里放着的 API Key、数据库密码、云凭证，代码里一句 `env` 全部看到。

```go
func cleanEnv(dir, out string) []string {
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "LANG=C.UTF-8", "OUTPUT_DIR=" + out}
}
```

要用**白名单**：只给这几个，而不是继承之后删掉“看起来像密钥”的。`TestEnvHasNoSecrets` 在测试进程里设了一个令牌，沙箱里读不到，而且环境变量总数不超过 8 个。

如果代码确实要调用某个 API，不要把密钥放进沙箱：让沙箱通过一个你控制的代理出去，由代理在网络层注入凭证。这样代码能用，但拿不到。

工作目录用 `os.MkdirTemp` 建一个新的，`HOME` 也指向它，跑完 `os.RemoveAll`（`TestTempDirRemoved`）。不同任务、不同用户之间不共享目录。

## 9. 资源限制

`ulimit` 在 `sh` 里设置，对它和它的所有子进程生效：

- `ulimit -t 1`：CPU 时间 1 秒。`TestCPULimit` 跑一个死循环，墙钟超时设了 10 秒，但 1 秒左右就被 CPU 限制杀掉了（信号 SIGXCPU）。墙钟超时防“卡住不动”（等网络、等输入），CPU 限制防“埋头死算”，两个都要；
- `ulimit -f 100`：单个文件最大 100 块，写超了收到 SIGXFSZ。

`TestFileSizeLimit` 还演示了一个容易忽略的问题：不加 `set -e` 时，`head` 被杀掉了，脚本却继续执行 `echo after`，最后退出码是 0，失败被藏起来了。加上 `set -e` 后第一处失败就停，退出码和信号都看得到。给模型的执行环境默认开 `set -e`（或者让模型知道要检查每一步）。

内存和进程数在进程层面不好可靠地限制（`ulimit -u` 是按用户算的，同一个用户下的其他进程也算进去），这正是要用容器 cgroups（`--memory`、`--pids-limit`）的原因。

## 10. 输出

**截断，保留头尾。** 1.6 课讲过：工具输出太长时保留开头和结尾，报错信息通常在最后。练习的 `capped` 是一个 `io.Writer`，开头和结尾各留一半，中间写明省略了多少字节：

```go
func (c *capped) Write(p []byte) (int, error) {
	n0 := len(p)
	c.total += n0
	...
	return n0, nil
}
```

这里有一个我写练习时真的踩到的坑：`Write` 必须返回**传进来的** `len(p)`，哪怕你只保留了一部分。最早的版本返回了累计总数，`exec` 的拷贝协程认为写入异常（`invalid write result`），直接停止读取子进程的输出。`io.Writer` 的约定是：返回值小于 `len(p)` 时必须同时返回错误。

**产物只收普通文件。** 学 Anthropic 的做法，约定一个 `$OUTPUT_DIR`，只收它顶层的文件。收集时用 `os.Lstat`（不跟随符号链接），只要普通文件：

```go
info, err := os.Lstat(filepath.Join(out, e.Name()))
if err != nil || !info.Mode().IsRegular() {
	continue
}
```

开头第六个问题的后半句：代码可以执行 `ln -s /etc/passwd $OUTPUT_DIR/passwd`。如果你用 `os.ReadFile` 收产物，它会跟着链接读到宿主机（或者执行端所在机器）的文件，然后你把它当成产物返回给用户。`TestArtifactsOnlyRegularFiles` 放了一个普通文件、一个目录外的文件、一个符号链接和一个子目录，只有普通文件被收下。

**输出也是不可信内容。** 代码可以打印“忽略之前的指令……”。2.4 课的结论同样适用：工具结果不能授予权限。

## 11. 结果怎么回给模型

开头最后一个问题。要区分两种失败：

- **代码自己出错**（语法错误、异常、退出码非 0）：这是正常结果。模型需要看到 stderr，去改代码。Gemini 的工具出错时模型会自动重新生成代码，最多 5 次，就是这个思路；
- **执行环境层面的失败**（超时、被资源限制杀死）：标成 `is_error`，并告诉模型怎么调整：缩小数据量、拆成几步、把大输出写进文件。

```go
case r.TimedOut:
	p.Note, isError = fmt.Sprintf("超时（%s）被终止：缩小数据量，或把长任务拆成几步", ...), true
case r.Signal != "":
	p.Note, isError = "被资源限制终止："+r.Signal, true
case r.Truncated:
	p.Note = "输出过长已截断：只打印需要的部分，或写入 $OUTPUT_DIR"
```

这和 2.1 课的“错误变结果”是一回事：错误要以模型能理解、能行动的方式出现在结果里。Anthropic 的代码执行也是这样区分的：命令的非 0 退出码出现在正常结果的 `return_code` 里，整个调用超时才返回 `execution_time_exceeded` 错误。

## 12. 生命周期

托管沙箱和复用的容器都有状态：

- **续期**：每次使用后把截止时间往后推（E2B 的 `set_timeout`）；
- **超时**：要么暂停（保留文件和内存，下次用时恢复），要么销毁（全丢）。OpenAI 是 20 分钟不用就销毁，E2B 可以配成自动暂停，Anthropic 闲置后存档、30 天内可恢复；
- **销毁之后**：明确报错，调用方新建一个、重新上传文件。不要假装它还在，否则模型会以为之前的变量和文件都在，写出依赖它们的代码。

练习的 `Manager` 用假时钟测了这几条（`TestLifecycle`）。实际系统里还要考虑：多个对话复用沙箱时按用户隔离；暂停的沙箱永久保留（E2B）意味着要有自己的清理策略，否则存储费用会一直涨；用户删除对话时，沙箱和里面的文件一起删。

---

## 本课小结

1. 三家都有自带的服务端代码执行，隔离和清理由厂商负责，适合不需要访问你内部环境的任务；Anthropic 没有网络，OpenAI 容器 20 分钟不用就过期，Gemini 单次 30 秒。
2. 自己搭时，隔离从弱到强：进程加 ulimit（不是安全边界）、容器（共享内核）、gVisor（用户态内核）、微虚拟机（独立内核）。不可信代码至少用 gVisor。
3. 加固容器：不给网络、只读根目录、去掉全部 capabilities、不许提权、非 root、限内存（不许 swap）、限 CPU、限进程数；警惕 `--privileged`、`--network=host`、挂载 Docker socket。
4. 出站网络默认关；要开就用白名单，域名白名单必须同时拒绝其他全部流量。
5. 超时杀整个进程组；环境变量用白名单；工作目录一次性。
6. 墙钟超时和 CPU 限制都要；脚本默认 `set -e`，否则失败被藏起来。
7. 输出截断保留头尾；产物用 `Lstat` 只收普通文件；输出也是不可信内容。
8. 代码错误是正常结果，环境失败是 `is_error`，都要告诉模型怎么改；沙箱销毁后明确报错、重建。

---

## 练习（今天做完）

### 练习 1：跑通并读懂

```bash
cd projects/16-sandbox
make test
```

回答：

1. 把 `Run` 里的 `exec.Command` 改成 `exec.CommandContext(ctx, ...)` 并删掉 `Setpgid` 和 `syscall.Kill`，哪个测试会失败？为什么？
2. `cleanEnv` 为什么用白名单？如果改成“继承全部，再删掉名字里带 KEY、TOKEN、SECRET 的”，会漏掉哪些东西？
3. `TestFileSizeLimit` 第一段不加 `set -e`，退出码是 0。如果你的 Agent 只看退出码判断成功，会发生什么？

### 练习 2：并发与租户配额

沙箱很贵，一台机器同时能跑的有限，而且不能让一个用户占满所有名额。

要求：写一个 `Pool`，构造时给全局并发上限和每个租户的并发上限。`Run(ctx, tenant, spec)`：该租户已经满了，立即返回 `ErrBusy`（让模型或用户知道要等）；全局满了，就排队等，直到有空位或 `ctx` 取消。

测试：全局 2、每租户 1。租户 a 先跑一个 0.3 秒的任务，50 毫秒后 a 再提交一个，应当得到 `ErrBusy`；同时租户 b 提交的任务正常完成。用 `-race` 跑。

### 练习 3：产物限额

现在 `collect` 会收下 `$OUTPUT_DIR` 里任意多、任意大的文件。

要求：给 `Limits` 加 `MaxFiles` 和 `MaxFileBytes`；超过数量的文件不收，超过大小的文件不收，并在 `Result` 里记录被拒绝的文件名和原因，`ToolResult` 把原因告诉模型。文件名只允许字母、数字、点、横线和下划线，其他的拒绝（防止文件名里带换行或路径分隔符，坑到后面处理产物的代码）。

---

## 自测题

1. 三家自带的代码执行工具各有什么限制？什么情况下要自己搭沙箱？
2. 一个沙箱要防的威胁有哪些？哪些靠隔离层次，哪些靠执行端自己写对？
3. 容器、gVisor、微虚拟机分别隔离到什么程度？代价是什么？
4. 说出至少六个加固容器的参数和它们的作用，再说三个危险参数。
5. E2B 的出站规则是怎么判定的？为什么“只写域名白名单”是错误配置？
6. 为什么超时只杀 `sh` 不够？Go 里怎么杀整个进程组？
7. 为什么环境变量要用白名单？代码确实需要调用外部 API 时怎么办？
8. 墙钟超时和 CPU 时间限制分别防什么？为什么默认要 `set -e`？
9. 收集产物时有哪些坑？`io.Writer` 的返回值约定是什么？
10. 代码报错和执行超时回给模型时有什么区别？沙箱被销毁后应该怎么处理？

---

# 参考答案与详解

## 自测题答案

**1.** Anthropic：没有网络，装不了包，5 GiB 内存和磁盘、1 个 CPU，不在 ZDR 范围内。OpenAI：容器 20 分钟不用就过期且不可恢复，内存按档位计费。Gemini：单次最长 30 秒，不能装自己的库。需要访问私有依赖或内部服务、要特定语言和工具链、要长时间运行、数据不能离开自己的环境、或者要在仓库里跑测试时，自己搭（或用 E2B 这类托管沙箱）。

**2.** 逃逸到宿主机、耗尽资源、偷密钥、横向移动（内网、云元数据）、外传数据、跨用户残留、坑执行端（巨量输出、符号链接、输出里的注入）。逃逸靠隔离层次；资源耗尽靠资源限制；后面几类大多靠执行端写对：干净的环境变量、网络白名单、一次性环境、输出截断、只收普通文件、把输出当不可信内容。

**3.** 容器用命名空间、cgroups、seccomp 隔离，但和宿主机共享内核，内核漏洞可能导致逃逸，开销最小。gVisor 在用户态拦截系统调用、自己充当客户机内核，应用碰不到宿主机内核的大部分代码；代价是兼容性差一些、系统调用开销更大。微虚拟机（Firecracker）有自己的内核和硬件虚拟化隔离，最强；代价是需要 KVM，启动和资源开销比容器大（但比传统虚拟机小得多）。

**4.** `--network=none` 没有网络；`--read-only` 根文件系统只读；`--tmpfs /tmp:noexec` 只有临时目录可写且不能执行；`--cap-drop=ALL` 去掉所有 capabilities；`--security-opt=no-new-privileges` 禁止提权；`--user=65534:65534` 非 root；`--memory` 加相同的 `--memory-swap` 限内存且不许 swap；`--cpus` 限 CPU；`--pids-limit` 防 fork 炸弹；`--rm` 用完删除；`--runtime=runsc` 用 gVisor。危险参数：`--privileged`、`--network=host`、`--pid=host`、挂载 `/var/run/docker.sock`、`seccomp=unconfined`。

**5.** 先看 `allow_out`，命中就放行；没命中再看 `deny_out`，命中就拒绝；都没命中默认放行。用了域名规则时 DNS 服务器自动放行。默认放行意味着只写 `allow_out: ["pypi.org"]` 而不写 `deny_out: 全部`，其他地址照样能访问，白名单形同虚设，所以 E2B 要求按域名放行时必须同时拒绝全部流量。

**6.** `sh` 派生的子进程（后台进程、它启动的程序）不会因为父进程死了而死，会变成孤儿继续运行，占着 CPU、端口和文件。Go 里设置 `SysProcAttr{Setpgid: true}` 让子进程成为新进程组的组长，超时时用 `syscall.Kill(-pid, syscall.SIGKILL)` 给整个进程组发信号。

**7.** 黑名单永远列不全：密钥的变量名千奇百怪（`DATABASE_URL` 里带密码、`GOOGLE_APPLICATION_CREDENTIALS` 指向凭证文件、云厂商的各种变量），而且新加的变量默认就会泄漏。白名单默认不给，只给明确需要的。需要调用外部 API 时，不把密钥放进沙箱，让请求经过你控制的代理，由代理注入凭证并限制能访问的地址。

**8.** 墙钟超时防“不动”：等网络、等输入、`sleep`，CPU 不忙但一直不结束。CPU 时间限制防“死算”：死循环、超大计算，CPU 满载。只有前者，死循环要等到墙钟超时才停，期间占满 CPU 影响别人；只有后者，卡住不动的任务永远不结束。`set -e` 让脚本在第一处失败时停下并返回非 0 退出码；不加的话，前面的命令失败了，后面的命令照样执行，最后一条成功就返回 0，失败被藏起来。

**9.** 产物数量和大小不受控，可能塞满你的存储；符号链接可能指向宿主机文件，用跟随链接的读法会把它们当产物送出去；子目录、设备文件、命名管道（读它会卡住）都不该收；文件名里可能有换行或路径分隔符。所以用 `Lstat`、只收普通文件、限数量和大小、校验文件名。`io.Writer` 的约定是返回写入的字节数，小于 `len(p)` 时必须同时返回非 nil 的错误；只保留部分数据的 Writer 也要返回 `len(p)`。

**10.** 代码报错是正常结果：把退出码和 stderr 原样给模型，它会修代码。超时、被资源限制杀死是环境层面的失败：标 `is_error`，并说明原因和调整方法（缩小数据、拆分任务、把输出写文件）。沙箱被销毁后明确告诉调用方（和模型）环境已经重置，新建沙箱、重新上传文件，不能让模型以为之前的变量和文件还在。

## 练习参考答案

### 练习 1

1. `TestTimeoutKillsProcessGroup` 会失败。`CommandContext` 超时只给直接子进程 `sh` 发 SIGKILL，后台的 `sleep 30` 不在被杀的范围里，测试检查它的 pid 还活着。另外，`cmd.Wait` 要等到所有持有输出管道的进程退出才返回，后台的 `sleep` 还持有 stdout，`Wait` 可能一直等到它结束（30 秒），超时形同虚设。
2. 会漏掉：名字不含这些词的敏感变量（`DATABASE_URL` 里的密码、`PGPASSWORD`、`GOOGLE_APPLICATION_CREDENTIALS` 指向的凭证文件、`AWS_PROFILE`、`KUBECONFIG`）；以后新加的变量；还有 `LD_PRELOAD` 这类会改变程序行为的变量。白名单是唯一可靠的写法。
3. Agent 会以为文件写成功了，继续基于一个被截断的文件往下做，最后给用户一个错误的结果，而且日志里看不出任何异常。

### 练习 2

```go
var ErrBusy = errors.New("该租户并发已满")

type Pool struct {
	global    chan struct{} // 带缓冲的 channel 当信号量
	perTenant int
	mu        sync.Mutex
	running   map[string]int
}

func NewPool(global, perTenant int) *Pool {
	return &Pool{global: make(chan struct{}, global), perTenant: perTenant, running: map[string]int{}}
}

func (p *Pool) Run(ctx context.Context, tenant string, s Spec) (Result, error) {
	p.mu.Lock()
	if p.running[tenant] >= p.perTenant {
		p.mu.Unlock()
		return Result{}, ErrBusy // 租户满了：立即拒绝，不排队
	}
	p.running[tenant]++
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.running[tenant]--; p.mu.Unlock() }()

	select {
	case p.global <- struct{}{}: // 全局满了：排队
		defer func() { <-p.global }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	return Run(ctx, s)
}
```

讨论：为什么租户满了立即拒绝、全局满了排队？租户满了说明这个用户自己已经有任务在跑，排队只会让他的任务越积越多；立即拒绝，模型可以等前一个结果。全局满了是大家一起等，排队更公平。生产环境里还要给排队加上限和超时，否则请求会无限堆积；按租户计数也可以放进 Redis（0.6 课），支持多台机器。

### 练习 3

```go
// Limits 加两个字段
MaxFiles     int
MaxFileBytes int64

// Result 加一个字段
Rejected []string // "名字：原因"

var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func collect(out string, l Limits) (fs []File, rejected []string, err error) {
	ents, err := os.ReadDir(out)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range ents {
		info, err := os.Lstat(filepath.Join(out, e.Name()))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		switch {
		case !safeName.MatchString(e.Name()):
			rejected = append(rejected, fmt.Sprintf("%q：文件名不合法", e.Name()))
		case l.MaxFileBytes > 0 && info.Size() > l.MaxFileBytes:
			rejected = append(rejected, fmt.Sprintf("%s：%d 字节，超过上限 %d", e.Name(), info.Size(), l.MaxFileBytes))
		case l.MaxFiles > 0 && len(fs) >= l.MaxFiles:
			rejected = append(rejected, e.Name()+"：超过文件数上限")
		default:
			fs = append(fs, File{Name: e.Name(), Size: info.Size()})
		}
	}
	return fs, rejected, nil
}
```

`ToolResult` 里把 `Rejected` 放进 `note`，例如“以下文件没有收下：big.csv：52428800 字节，超过上限 10485760”，模型就知道要压缩或者只输出摘要。文件名的 `%q` 让带换行的名字在日志里也是一行。

---

## 完成标准

- `make test` 全部通过（12 个测试）；
- 练习 1 的三个问题能讲清楚，尤其是第 1 题“为什么要杀进程组”；
- 练习 2 的 `Pool` 写完，`-race` 下测试通过；
- 练习 3 的产物限额写完并有测试（至少覆盖超数量、超大小、文件名不合法三种情况）；
- 自测题 10 道能不看答案讲出来，尤其是第 3、5、6、9 题。
