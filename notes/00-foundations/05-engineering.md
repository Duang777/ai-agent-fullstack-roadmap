# 0.5 工程基础：Git、Docker、Linux，把 Agent 服务送上线

> 第 0 部分 · 第 5 课 ｜ 预计 4–5 小时 ｜ 前置：0.1 Go、0.4 流式协议
>
> 配套练习：`projects/04-ship/`（Go 标准库，6 个测试）+ 仓库根目录 `.github/workflows/ci.yml`（每次 push 自动跑全部测试、构建镜像并冒烟测试）。

## 读前说明

前四课写的代码都在你自己电脑上跑。这一课解决的问题是：**怎么让它变成一个别人能用、挂了能查、改了能安全上线的服务。**

不会讲成 Git / Docker / Linux 大全，只讲做 Agent 后端**每天都会用到**的部分，并且都落在同一个练习上：一个带 SSE 接口的 Go 服务，从提交代码 → CI 测试 → 打镜像 → 运行 → 优雅下线 → 线上排查，走一遍完整链路。

本课结束时你能：

1. 用规范的分支和提交工作，出了问题能用 Git 定位是哪次提交引入的；
2. 写一个多阶段 Dockerfile，镜像约 10 MB、不以 root 运行、密钥不进镜像；
3. 让服务正确处理 SIGTERM，发版时进行中的流式对话不被掐断；
4. 写健康检查、结构化日志，配一个 GitHub Actions CI；
5. 服务器上出问题时，知道先敲哪几条命令。

---

# 第一部分：Git 工作流

## 1. 你真正需要的心智模型

Git 里只有三个区域，所有命令都是在它们之间搬东西：

```
工作区（你在改的文件）
   │ git add
   ▼
暂存区（下一次提交的内容）
   │ git commit
   ▼
本地仓库（提交历史）  ──git push──▶  远程仓库（GitHub）
```

每次提交是一个**快照**，有一个哈希值（如 `5600361`）。**分支只是一个指向某次提交的指针**，创建分支几乎零成本。理解了这一点，`rebase`、`reset` 这些命令就不神秘了：它们都只是在移动指针。

## 2. 日常流程：一个功能一个分支

```bash
git switch main && git pull --rebase       # ① 先同步最新的 main
git switch -c feat/sse-heartbeat           # ② 新建功能分支

# ... 改代码 ...
git add -p                                 # ③ 逐块确认要提交的改动
git commit -m "feat(stream): SSE 增加心跳，防止网关空闲断开"

git fetch origin && git rebase origin/main # ④ 推送前再同步一次
git push -u origin feat/sse-heartbeat      # ⑤ 推到远程，开 Pull Request
```

**逐行解读**

- **① `pull --rebase`**：把你本地的提交“挪到”远程最新提交后面，历史保持一条直线；不加 `--rebase` 会多出一个无意义的合并提交。
- **② 分支命名**：`feat/`、`fix/`、`chore/` 前缀，一眼看出是干什么的。
- **③ `git add -p`**：一块一块问你要不要加。能防止把调试用的 `fmt.Println`、临时改的配置一起提交上去。强烈建议养成习惯。
- **④ rebase 到最新 main**：冲突在你自己的分支上解决，不要让合并的人去解决。
- **⑤ Pull Request**：哪怕一个人的项目也建议走 PR，CI 会在 PR 上自动跑，测试不过就不合并。

## 3. 提交信息：Conventional Commits

本仓库的提交都按这个格式写：

```
<类型>(<范围>): <一句话说明做了什么>

feat(0.4): 03-streaming 练习
fix(stream): 空块导致 pendingCR 被重置
docs: README 加入 0.4 流式协议
ci: Go / TS 测试 + Docker 构建
```

| 类型 | 用于 |
|---|---|
| `feat` | 新功能 |
| `fix` | 修 bug |
| `docs` | 只改文档 |
| `refactor` | 重构，不改变行为 |
| `test` | 只改测试 |
| `chore` / `ci` | 构建、依赖、CI 配置 |

好处不只是整齐：工具可以据此自动生成 CHANGELOG、自动决定版本号（`feat` 升次版本，`fix` 升修订号）。

## 4. 出事时救命的几个命令

```bash
git log --oneline --graph -20         # 看最近的历史
git diff main...HEAD                  # 我这个分支相对 main 改了什么
git blame -L 40,60 server.go          # 这几行是谁、哪次提交改的
git stash / git stash pop             # 临时收起手头的改动，先去修别的
git commit --amend                    # 修改最后一次提交（还没 push 时）
git restore --staged file.go          # 把文件从暂存区撤回（改动保留）
git revert <提交>                      # 生成一个“反向提交”来撤销，已 push 的用这个
git reflog                            # 后悔药：HEAD 去过的每个位置都在这里
```

**`git bisect`：找出是哪次提交引入的 bug**

Agent 服务常见情况：“上周流式还好好的，现在偶尔卡住。”中间有 40 次提交，一个个看不现实。

```bash
git bisect start
git bisect bad                 # 当前版本有问题
git bisect good v0.3.0         # 这个版本是好的
# Git 自动切到中间的某次提交，你测一下，然后告诉它结果：
git bisect good   # 或 git bisect bad
# ... 二分查找，40 次提交最多测 6 次 ...
git bisect reset

# 如果有能复现问题的测试，可以全自动：
git bisect run go test -run TestGracefulShutdown ./...
```

**三条红线**

1. **不要 `git push --force` 到 main**。实在要强推自己的分支，用 `--force-with-lease`，它会检查远程有没有别人的新提交。
2. **不要提交密钥**。`.env` 写进 `.gitignore`。万一提交了，**删文件没用**，它还在历史里，必须**立刻去平台作废这个 Key**，再考虑清理历史。GitHub 会扫描公开仓库里的密钥，很多模型厂商收到通知后会自动禁用。
3. **不要提交大文件**：模型权重、数据集、`node_modules`。用 `.gitignore` 挡住，真要版本化大文件用 Git LFS 或对象存储。

---

# 第二部分：Linux 基础

服务器、Docker 容器、CI 机器，里面全是 Linux。不用背命令，只要掌握下面这些概念，出问题时知道往哪个方向查。

## 5. 进程与信号：理解“优雅退出”的前提

**进程**就是一个运行中的程序，有一个编号 PID。操作系统通过**信号**通知进程发生了什么事：

| 信号 | 编号 | 谁会发 | 进程能处理吗 |
|---|---|---|---|
| `SIGINT` | 2 | 终端里按 Ctrl+C | 能 |
| `SIGTERM` | 15 | `kill <pid>`、`docker stop`、K8s 删除 Pod | 能，**这是“请你收拾一下然后退出”** |
| `SIGKILL` | 9 | `kill -9`、超时后的 `docker stop` | **不能**，立即被杀 |
| `SIGHUP` | 1 | 终端关闭；部分程序约定为“重新加载配置” | 能 |

关键流程：`docker stop` 先发 `SIGTERM`，**等 10 秒**（K8s 默认 30 秒），进程还没退就发 `SIGKILL`。

所以如果你的服务**不处理 SIGTERM**，每次发版都会：进程被直接杀掉 → 所有正在进行的 SSE 流瞬间断开 → 用户看到回复写到一半没了。第四部分会解决它。

```bash
ps aux | grep server          # 找进程
pgrep -af server              # 同上，更简洁
kill <pid>                    # 发 SIGTERM
kill -9 <pid>                 # 发 SIGKILL，最后手段
```

## 6. 环境变量、标准输出和退出码

三个“进程和外界沟通”的约定，容器化之后尤其重要：

- **环境变量**：进程启动时从父进程继承的一组 `键=值`。配置和密钥通过它传入，不写死在代码或镜像里。
  ```bash
  export LLM_API_KEY=sk-xxx     # 当前 shell 及其子进程可见
  LLM_API_KEY=sk-xxx ./server   # 只对这一条命令生效
  env | grep LLM                # 查看
  ```
- **stdout / stderr**：容器里的服务**日志直接写到标准输出**，不要写文件。Docker、K8s 会自动收集，`docker logs` 就能看。
- **退出码**：`0` 表示成功，非 `0` 表示失败。`echo $?` 看上一条命令的退出码。CI、Docker、K8s 都靠它判断成败；配置错了就 `os.Exit(1)` 让平台知道启动失败，别吞掉错误继续跑。

## 7. 权限、端口与文件描述符

- **权限**：`ls -l` 看到的 `-rwxr-xr-x` 分别是所有者、组、其他人的读写执行权限。**服务不要用 root 跑**：一旦被攻破（比如 Agent 被 Prompt Injection 诱导执行命令），root 意味着整台机器沦陷。
- **端口**：1024 以下是特权端口，普通用户不能监听。所以服务监听 `8080`，再由负载均衡或 Nginx 对外暴露 `443`。
- **文件描述符（fd）**：Linux 里“一切皆文件”，每个 TCP 连接也占一个 fd。默认上限常见是 1024（`ulimit -n` 查看）。**每个 SSE 连接都长期占着一个 fd**，并发用户一多就会报 `too many open files`。生产环境要调高这个上限（容器里一般已经够大，自建服务器要注意）。

## 8. 线上排查工具箱

服务出问题，按这个顺序查：

```bash
# 1. 服务还活着吗？
curl -i localhost:8080/healthz
systemctl status myagent          # systemd 管理的服务
docker ps                         # 容器在不在、重启了几次

# 2. 日志说了什么？
docker logs --tail 100 -f <容器>
journalctl -u myagent -f          # systemd 的日志

# 3. 端口在监听吗？谁在用？
ss -ltnp | grep 8080

# 4. 资源够吗？
top / htop                        # CPU、内存
free -h                           # 内存
df -h                             # 磁盘：日志写满磁盘是经典事故

# 5. 网络通吗？
curl -v https://api.openai.com    # 能连上上游模型吗，TLS 握手卡在哪
dig api.openai.com                # DNS 解析
```

再加两个处理文本的利器，配合 JSON 日志特别好用：

```bash
docker logs ship | grep '"status":500'                    # 找出所有 500
docker logs ship | jq -r 'select(.dur_ms > 5000) | .path'  # 找出超过 5 秒的请求
```

---

# 第三部分：Docker

## 9. 为什么要容器

“我这能跑”的问题根源是**环境不一致**：Go 版本、系统库、配置文件。容器把**程序 + 它需要的一切**打包成一个**镜像**，在哪运行都一样。

三个概念：

- **镜像（image）**：只读的模板，由 Dockerfile 构建，一层层叠起来。
- **容器（container）**：镜像跑起来的实例，有自己的进程空间、文件系统、网络。本质上就是一个被隔离起来的 Linux 进程，不是虚拟机。
- **镜像仓库（registry）**：存镜像的地方，如 Docker Hub、GitHub Container Registry、阿里云 ACR。

## 10. 一个生产级 Dockerfile

**要解决的问题**：直接 `FROM golang` 打出来的镜像有 800 MB，带着编译器和 shell，以 root 运行，每次改一行代码都要重新下载依赖。

练习 `projects/04-ship/Dockerfile`：

```dockerfile
# ---------- 第一阶段：编译 ----------
FROM golang:1.23-alpine AS build                 # ①
WORKDIR /src

COPY go.mod ./                                   # ② 先只拷依赖清单
RUN go mod download

COPY . .                                         # ③ 再拷源码
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" -o /out/server .   # ④

# ---------- 第二阶段：运行 ----------
FROM gcr.io/distroless/static-debian12:nonroot   # ⑤
COPY --from=build /out/server /server            # ⑥ 只拿编译产物
EXPOSE 8080
USER nonroot:nonroot                             # ⑦
ENTRYPOINT ["/server"]                           # ⑧
```

**逐行解读**

- **① 多阶段构建**：第一阶段有完整的 Go 工具链，用来编译；第二阶段从一个几乎空的镜像开始，只放编译好的二进制。最终镜像不含编译器、源码，约 10 MB。
- **② 和 ③ 分开拷：利用层缓存。** Docker 每条指令是一层，某层的输入没变就直接用缓存。依赖清单很少改，源码经常改。先拷 `go.mod` 下载依赖，改代码时只有 ③ 之后的层重新执行，构建从几分钟变成几秒。**顺序反过来，缓存就废了**。Node 项目同理：先拷 `package.json` 和 `package-lock.json` 跑 `npm ci`，再拷源码。
- **④ 编译参数**：
  - `CGO_ENABLED=0`：生成不依赖 C 库的纯静态二进制，才能放进没有 libc 的镜像；
  - `-trimpath`：去掉二进制里你电脑上的绝对路径；
  - `-s -w`：去掉调试符号，体积小一半左右；
  - `-X main.version=...`：构建时把 Git 提交号写进 `version` 变量，线上 `curl /version` 就知道跑的是哪个版本。
- **⑤ distroless**：Google 维护的极简镜像，**没有 shell、没有包管理器**。就算服务被攻破，攻击者也没有 `sh`、`curl` 可用。代价是你也没法 `docker exec` 进去调试，需要时用带 `:debug` 标签的版本。
- **⑥ `COPY --from=build`**：从第一阶段拿文件。
- **⑦ 非 root 运行**：见第 7 节。
- **⑧ exec 形式**：`ENTRYPOINT ["/server"]`（JSON 数组）让 server 直接成为容器里的 1 号进程，能收到 `docker stop` 发的 SIGTERM。如果写成 `ENTRYPOINT /server`（shell 形式），实际是 `/bin/sh -c /server`，1 号进程是 sh，**sh 默认不转发信号**，你的服务永远收不到 SIGTERM，10 秒后被 SIGKILL。这是非常常见的坑。

**`.dockerignore`**：和 `.gitignore` 一样的语法，决定哪些文件不发给 Docker 构建。至少要排除 `.git`、`.env`、`node_modules`。不排除 `.env` 的话，`COPY . .` 会把你的密钥打进镜像，任何拿到镜像的人都能看到。

## 11. 运行容器

```bash
docker build --build-arg VERSION=$(git rev-parse --short HEAD) -t agent-ship:dev .

docker run --rm \
  -p 8080:8080 \                # 宿主机端口:容器端口
  --env-file .env \             # 从文件读环境变量，密钥这样传
  --memory 512m --cpus 1 \      # 资源限制：防止一个服务吃光整台机器
  --name ship \
  agent-ship:dev

docker logs -f ship             # 看日志
docker stop ship                # 发 SIGTERM，看日志里的 "shutting down" 和 "bye"
docker images agent-ship        # 看镜像大小
```

**密钥的正确姿势**：运行时通过环境变量或密钥管理服务注入，**永远不要**写在 Dockerfile 的 `ENV` 或 `ARG` 里。`docker history` 能看到每一层的构建命令，`ARG` 的值也会留在里面。

## 12. docker compose：本地起一整套依赖

Agent 服务通常还依赖 Redis（会话、限流）、Postgres + pgvector（记忆、RAG）。本地开发用 compose 一条命令拉起：

```yaml
# compose.yaml
services:
  agent:
    build: .
    ports: ["8080:8080"]
    env_file: .env
    environment:
      REDIS_URL: redis://redis:6379      # 服务名就是主机名
    depends_on:
      redis:
        condition: service_healthy       # 等 redis 健康了再启动 agent
  redis:
    image: redis:7-alpine
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 2s
```

```bash
docker compose up -d --build
docker compose logs -f agent
docker compose down          # 加 -v 连数据卷一起删
```

第 05 模块（RAG）和第 08 模块（高可用）会在这个基础上加数据库和队列。

---

# 第四部分：让服务“可上线”

前三部分是工具，这一部分是用这些工具把服务改造成能在生产环境跑的样子。对应练习的 `main.go` 和 `server.go`。

## 13. 配置：全部来自环境变量

```go
type Config struct {
    Addr            string
    ShutdownTimeout time.Duration
    APIKey          string // 只从环境变量读
}

func LoadConfig(getenv func(string) string) (Config, error) {
    c := Config{
        Addr:            envOr(getenv, "ADDR", ":8080"),
        ShutdownTimeout: 25 * time.Second,
        APIKey:          getenv("LLM_API_KEY"),
    }
    if v := getenv("SHUTDOWN_TIMEOUT"); v != "" {
        d, err := time.ParseDuration(v)
        if err != nil {
            return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err)
        }
        c.ShutdownTimeout = d
    }
    return c, nil
}
```

**逐行解读**

- **同一个镜像跑所有环境**：开发、测试、生产只是环境变量不同。这是 12-Factor App 的原则。
- **`getenv` 作为参数传进来**：`main` 里传 `os.Getenv`，测试里传一个从 map 读的假函数。这样测试不用真的去改进程的环境变量，也不会互相干扰（0.1 课 B8“依赖接口而不是具体实现”的思路，这里用函数代替接口）。
- **配置错了立刻失败**：`SHUTDOWN_TIMEOUT=soon` 这种值在启动时就报错退出，比跑起来以后出奇怪的行为好查得多。
- **默认值 25 秒**：比 K8s 默认的 30 秒宽限期短，确保在被 SIGKILL 之前自己先退出。

## 14. 健康检查：liveness 和 readiness 是两回事

```go
// healthz：进程活着就 200。liveness 探针，失败会重启容器。
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
    fmt.Fprintln(w, "ok")
}

// readyz：能不能接新流量。正在下线时返回 503，负载均衡就不再转新请求过来。
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
    if s.draining.Load() {
        http.Error(w, "draining", http.StatusServiceUnavailable)
        return
    }
    fmt.Fprintln(w, "ready")
}
```

| | liveness（`/healthz`） | readiness（`/readyz`） |
|---|---|---|
| 问的问题 | 进程是不是卡死了？ | 现在能接新请求吗？ |
| 失败后果 | **重启容器** | 从负载均衡**摘掉**，不重启 |
| 应该检查 | 只看进程本身 | 是否在下线、关键依赖是否就绪 |

**容易错的地方**：在 `/healthz` 里检查上游模型 API 或数据库。模型 API 抖动一下，所有实例的 liveness 一起失败，K8s 把它们全部重启，**一次上游故障被放大成你自己的全面宕机**。liveness 只检查自己。

## 15. 优雅退出：发版时不掐断对话

这是本课最重要的一段代码。

```go
func main() {
    log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
    cfg, err := LoadConfig(os.Getenv)
    // ...
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt) // ①
    defer stop()
    ln, _ := net.Listen("tcp", cfg.Addr)
    if err := Run(ctx, ln, cfg, log); err != nil { /* 退出码 1 */ }
}

func Run(ctx context.Context, ln net.Listener, cfg Config, log *slog.Logger) error {
    s := NewServer(cfg, log)
    srv := &http.Server{
        Handler:           s.Handler(),
        ReadHeaderTimeout: 5 * time.Second,  // ②
        IdleTimeout:       120 * time.Second,
    }

    errc := make(chan error, 1)
    go func() { errc <- srv.Serve(ln) }()

    select {
    case err := <-errc:                      // ③ 启动就失败
        return err
    case <-ctx.Done():                       //    收到信号
    }

    s.draining.Store(true)                   // ④ 第一步：标记下线，readyz 返回 503

    sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout) // ⑤
    defer cancel()
    err := srv.Shutdown(sctx)                // ⑥ 第二步：不接新连接，等进行中的请求结束

    if errors.Is(err, context.DeadlineExceeded) {
        _ = srv.Close()                      // ⑦ 第三步：超时还没结束的，强制关闭
    }
    if e := <-errc; e != nil && !errors.Is(e, http.ErrServerClosed) { // ⑧
        return e
    }
    return err
}
```

**逐行解读**

- **① `signal.NotifyContext`**：把“收到 SIGTERM / Ctrl+C”变成一个 context 被取消。和 0.1 课的取消链完全是一套东西，只是取消的源头从“客户端断开”变成了“操作系统通知”。
- **② 超时设置**：`ReadHeaderTimeout` 防止有人连上来却慢慢发请求头，长期占住连接（Slowloris 攻击）。**注意没有设 `WriteTimeout`**：它会限制整个响应的写入时间，一个持续 2 分钟的 SSE 流会被它从中间掐断。流式服务的整体超时应该在业务层用 context 控制。
- **③ `Serve` 放进 goroutine**：`Serve` 会阻塞直到服务器关闭。主 goroutine 用 select 同时等“服务挂了”和“该退出了”两件事。`errc` 缓冲为 1，原因和 0.1 课一样：保证发送方不会因为没人接收而永远阻塞。
- **④ 先 draining**：K8s 里，Pod 收到 SIGTERM 和它从负载均衡里被摘除**是同时发生、互不等待的**，可能还有几秒新请求会进来。先让 readyz 失败，生产环境里通常还会在这里 `sleep` 几秒，等负载均衡确认摘除后再关闭监听。
- **⑤ 新建 context，不要用 ctx**：`ctx` 已经被取消了，拿它做 Shutdown 的超时会立即返回。这是写优雅退出时最常见的 bug。
- **⑥ `srv.Shutdown`**：关闭监听端口，不再接受新连接；关闭空闲连接；**等待所有活跃请求处理完**。正在吐 token 的 SSE 流会正常写完 `[DONE]`。
- **⑦ 强制关闭**：总有请求很久都结束不了（比如一个跑 5 分钟的 Deep Research 任务）。到了期限就 `Close` 强行断开，保证在被 SIGKILL 之前自己可控地退出。对这类长任务，真正的解决方案是第 03、08 模块讲的检查点和可恢复执行。
- **⑧ `ErrServerClosed` 不是错误**：Shutdown 后 `Serve` 一定返回它，表示“按要求关闭了”，要排除掉。

**测试怎么验证的**（`main_test.go`）：

```go
res, _ := http.Post(url+"/chat", "application/json", strings.NewReader(`{"prompt":"1 2 3 4 5"}`))
time.Sleep(60 * time.Millisecond) // 让流先开始
cancel()                          // 相当于收到 SIGTERM

toks, ok := readTokens(t, res.Body)
if !ok || len(toks) != 6 { // 流必须完整结束：6 个 token + [DONE]
    t.Fatalf("in-flight stream was cut")
}
```

`Run` 接收 `ctx` 和 `net.Listener` 而不是自己去读信号、自己监听端口，就是为了能在测试里控制它：用 `cancel()` 模拟信号，用 `127.0.0.1:0` 让系统分配一个空闲端口。**把“难测的外部依赖”提到函数参数上**，是写可测试 Go 代码的通用技巧。

## 16. 结构化日志

```go
log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur_ms", 123)
```

输出：

```json
{"time":"2026-10-10T14:30:00Z","level":"INFO","msg":"request","method":"POST","path":"/chat","status":200,"dur_ms":123}
```

**为什么用 JSON 而不是 `fmt.Printf`**：日志平台可以按字段检索和聚合，比如“过去一小时 `/chat` 的 P99 耗时”“所有 `status=500` 的请求”。纯文本只能靠正则硬抠。

中间件里记录状态码要包装 ResponseWriter，这里有个和 0.4 课呼应的坑：

```go
type statusRecorder struct {
    http.ResponseWriter
    status int
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter } // 关键
```

包装器没有实现 `Flush`。如果 handler 里用 `w.(http.Flusher)` 断言，会失败，流式失效。练习里 handler 用的是 `http.NewResponseController(w).Flush()`，它会调用 `Unwrap` 一层层找到底层真正的 ResponseWriter。**写任何 ResponseWriter 包装器，都记得实现 `Unwrap`。**

第 07 模块（可观测性）会在这个基础上加 trace id，把一次 Agent 运行里的每次模型调用和工具调用串起来。

## 17. CI：每次推送自动验证

仓库根目录 `.github/workflows/ci.yml`，核心结构：

```yaml
on:
  push:
    branches: [main]
  pull_request:

jobs:
  go:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        project: [00-stream-proxy, 04-ship]        # ① 矩阵：每个项目一个并行任务
    defaults:
      run:
        working-directory: projects/${{ matrix.project }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.23" }
      - run: go vet ./...
      - run: go test -race -count=1 ./...          # ② 一定带 -race

  ts:  # 同样结构，跑 01-ts-agent-core 和 03-streaming 的 npm ci / typecheck / test

  docker:
    needs: go                                       # ③ Go 测试通过才构建镜像
    steps:
      - uses: actions/checkout@v4
      - run: docker build -t agent-ship projects/04-ship
      - run: |                                      # ④ 冒烟测试
          docker run -d --name ship -p 8080:8080 agent-ship
          for i in $(seq 1 20); do curl -fs localhost:8080/healthz && break; sleep 0.5; done
          curl -fsN -X POST localhost:8080/chat -d '{"prompt":"hi"}' | grep -q '\[DONE\]'
          docker stop -t 10 ship
          docker logs ship | grep -q '"msg":"bye"'
```

**逐行解读**

- **① matrix**：一份配置跑多个项目，各自并行，互不影响。
- **② `-race`**：数据竞争检测器需要 CGO，有些开发环境跑不了，CI 上一定要跑。0.1 课讲过的 map 并发写，只有它能稳定抓到。`-count=1` 禁用测试缓存。
- **③ `needs`**：任务之间的依赖。测试没过，构建镜像也没有意义。
- **④ 冒烟测试**：镜像能构建不代表能跑。这几行真的把容器跑起来，打一次健康检查和一次流式请求，再 `docker stop` 检查日志里有没有 `"bye"`，**这一步同时验证了 Dockerfile 的 exec 形式写对了**：要是写成 shell 形式，进程收不到 SIGTERM，就没有 `bye`。

`curl` 的参数：`-f` 遇到 4xx/5xx 返回非 0 退出码（让 CI 失败），`-s` 静默，`-N` 不缓冲。

---

## 本课小结

| 概念 | 一句话 |
|---|---|
| 分支 + PR | 一个功能一个分支，CI 通过才合并 |
| Conventional Commits | `类型(范围): 说明`，可自动生成 CHANGELOG |
| `git bisect` | 二分查找引入 bug 的提交，配合测试可全自动 |
| 密钥 | `.gitignore` + `.dockerignore` 挡住；泄露了先作废 Key |
| SIGTERM / SIGKILL | 前者可处理、是“请退出”；后者不可处理、立即死 |
| 多阶段构建 | 编译和运行分开，镜像约 10 MB |
| 层缓存 | 先拷依赖清单再拷源码 |
| exec 形式 ENTRYPOINT | 服务是 1 号进程，才能收到 SIGTERM |
| 非 root + distroless | 被攻破时损失最小 |
| liveness vs readiness | 前者失败重启，后者失败摘流量；liveness 不查外部依赖 |
| 优雅退出 | draining → Shutdown(新 context) → 超时 Close |
| 不设 WriteTimeout | 否则长 SSE 被掐断 |
| JSON 日志写 stdout | 平台收集，按字段检索 |
| ResponseWriter 包装器 | 要实现 `Unwrap`，否则 Flush 失效 |

## 练习（今天做完）

在 `projects/04-ship/` 里：

1. **读懂并跑通测试**：`make test`。重点读 `TestGracefulShutdownWaitsForInflight` 和 `TestShutdownTimeoutForcesClose`，说清楚两者的区别。
2. **故意弄坏，看测试能不能抓到**（每次改完记得还原）：
   - 把 `context.WithTimeout(context.Background(), cfg.ShutdownTimeout)` 里的 `context.Background()` 换成 `ctx`；
   - 删掉 `statusRecorder` 的 `Unwrap` 方法；
   - 给 `http.Server` 加上 `WriteTimeout: 100 * time.Millisecond`。
3. **Docker**（本机有 Docker 的话）：`make docker-build docker-run`，然后 `docker images` 看镜像大小，`docker stop` 看日志。再把 `ENTRYPOINT` 改成 shell 形式重新构建，对比 `docker stop` 花了多久、日志里有没有 `bye`。
4. **看 CI**：打开仓库的 Actions 页面，找到最新一次 `ci` 运行，看 4 个 Go / TS 任务和 docker 任务的日志。

**选做**：写一个 `compose.yaml`，加一个 Redis，给 `/readyz` 加上“Redis 能 ping 通”的检查。想一想：为什么这个检查能放在 readyz，却不能放在 healthz？

## 自测题

1. `git revert` 和 `git reset --hard` 都能“撤销提交”，已经推到远程 main 的提交该用哪个？为什么？
2. 不小心把 `.env`（含模型 API Key）提交并推送到了公开仓库。第一件事做什么？
3. 下面这个 Dockerfile 有哪些问题？
   ```dockerfile
   FROM golang:1.23
   ENV LLM_API_KEY=sk-abc123
   COPY . .
   RUN go mod download && go build -o server .
   ENTRYPOINT ./server
   ```
4. `docker stop` 一个容器总是要等整整 10 秒才停下，可能是什么原因？
5. 为什么流式服务不设 `http.Server.WriteTimeout`？那怎么防止某个请求无限期地跑下去？
6. 写 Shutdown 时，为什么不能直接用 `signal.NotifyContext` 返回的那个 ctx 当超时 context？
7. 有人在 `/healthz` 里加了“调用一次模型 API，成功才返回 200”。这有什么风险？
8. 线上用户反馈“偶尔报错”，你登录服务器，按什么顺序排查？

---

# 参考答案与详解

## 自测题答案

**1.** 用 `git revert`。它新建一个“反向提交”来抵消原提交的改动，历史只增不减，其他人 `pull` 时不会有任何问题。`reset --hard` 是把分支指针往回挪，相当于改写了历史，要推上去只能强推，会让所有基于旧历史工作的人出问题。规则：**没推送的随便改，推送了的只用 revert。**

**2.** 立刻到模型平台**作废（rotate）这个 Key** 并生成新 Key。删掉文件再提交没有用，旧提交里依然能看到；而且公开仓库会被机器人在几分钟内扫描到。作废之后再考虑用 `git filter-repo` 清理历史，并检查账单是否有异常调用。

**3.** 至少五个问题：
- 密钥写在 `ENV` 里，打进了镜像，`docker history` / `docker inspect` 就能看到；
- 单阶段构建，最终镜像带着完整 Go 工具链，800 MB 左右；
- `COPY . .` 在 `go mod download` 之前，改任何一行代码都会重新下载依赖，缓存失效；没有 `.dockerignore` 的话还会把 `.git`、`.env` 拷进去；
- 以 root 运行；
- `ENTRYPOINT ./server` 是 shell 形式，1 号进程是 `sh`，服务收不到 SIGTERM，无法优雅退出。

**4.** 进程没有收到或没有处理 SIGTERM，Docker 等满默认的 10 秒后发 SIGKILL。常见原因：ENTRYPOINT 用了 shell 形式，信号发给了 sh 而没有转发；或者程序根本没有监听 SIGTERM；或者处理了但优雅退出本身超过了 10 秒（这时应调小 `ShutdownTimeout` 或调大 `docker stop -t`）。

**5.** `WriteTimeout` 限制的是从读完请求头到写完整个响应的总时间，一个正常的、持续一两分钟的 SSE 流也会被它从中间切断。防止请求无限运行，应该在业务层控制：给每次 Agent 运行设一个 `context.WithTimeout`，并且在 Agent Loop 里设置最大步数、最大 token 预算（第 03 模块详讲）。

**6.** 因为收到信号的那一刻，这个 ctx 就已经被取消了。拿一个已取消的 context 调 `Shutdown`，它会立即返回 `context.Canceled`，根本不等进行中的请求，效果等于直接强关。必须用 `context.Background()` 新建一个带超时的 context。

**7.** 两个风险。第一，模型 API 抖动或限流时，所有实例的 liveness 同时失败，K8s 会把它们全部重启，**一次上游的小故障被放大成你自己服务的全面宕机**，而且重启解决不了上游问题。第二，探针每几秒调一次模型，白白花钱、消耗配额。liveness 只应检查进程本身；依赖检查如果要做，放在 readiness，并且要考虑“依赖挂了是否真的应该把所有实例都摘掉”。

**8.** 参考顺序：
1. `curl /healthz`、`docker ps` 看服务是否存活、是否在频繁重启；
2. `docker logs` / `journalctl` 看错误日志，用 `grep` 或 `jq` 筛出报错请求，看错误类型和时间分布；
3. `curl /version` 确认线上版本，对照最近一次发版时间，判断是不是新版本引入的；
4. `top`、`free -h`、`df -h` 看 CPU、内存、磁盘，`ss` 看连接数和 fd 是否接近上限；
5. `curl -v` 上游模型 API，确认是不是上游的问题（429、5xx、超时）；
6. 能稳定复现且确认是代码问题，用 `git bisect` 找到引入问题的提交。

## 练习参考答案

**练习 1：两个优雅退出测试的区别**

- `TestGracefulShutdownWaitsForInflight`：`ShutdownTimeout` 2 秒，流 0.3 秒就能写完。验证的是**正常发版**：收到信号后，进行中的流完整收到 `[DONE]`，`Run` 返回 nil，之后新连接被拒绝。
- `TestShutdownTimeoutForcesClose`：`ShutdownTimeout` 只有 100 毫秒，流要 1.6 秒。验证的是**兜底**：到期后强制断开，客户端收不到 `[DONE]`，`Run` 返回 `context.DeadlineExceeded`。

两个测试合在一起，覆盖了“尽量不掐断”和“到期一定退出”两个要求。

**练习 2：弄坏之后**

| 改动 | 哪个测试失败 | 为什么 |
|---|---|---|
| 超时 context 基于 `ctx` | `TestGracefulShutdownWaitsForInflight`、`TestShutdownTimeoutForcesClose` | 派生自已取消的 ctx，一创建就是取消状态，Shutdown 立即返回 `context.Canceled`，根本没等；第二个测试期望的是 `DeadlineExceeded`，拿到的却是 `context canceled` |
| 删 `Unwrap` | `TestChatStreams` 和两个退出测试 | `NewResponseController` 找不到底层的 Flusher，`Flush` 返回 `http.ErrNotSupported`，handler 写完第一个 token 就 `return` 了 |
| 加 `WriteTimeout: 100ms` | `TestGracefulShutdownWaitsForInflight` | 流写到一半就超过 100 毫秒，连接被服务器关闭，收不到 `[DONE]` |

第一项是“直接用 ctx”这个 bug 的变体：直接写 `srv.Shutdown(ctx)` 的话 Go 会因为 `sctx` 未使用而编译失败，反而救了你；但从 ctx 派生超时 context 能编译通过，bug 就藏住了。第二项说明 `NewResponseController` 不是魔法，它依赖包装器正确实现 `Unwrap`。第三项里超时强关的测试仍然通过，因为它本来就期望流被掐断，**一个测试通过不代表行为正确，要看它验证的是什么**。

**练习 3：shell 形式 ENTRYPOINT**

改成 `ENTRYPOINT /server` 后，`docker stop` 会稳定地花约 10 秒，日志里只有 `listening`，没有 `shutting down` 和 `bye`。因为 SIGTERM 发给了 1 号进程 `sh`，`sh` 不转发也不退出，10 秒后整个容器被 SIGKILL。另外 distroless 镜像里根本没有 `/bin/sh`，shell 形式在这个镜像里会直接启动失败，这也是 distroless 帮你提前暴露问题的一个例子。

**选做：为什么 Redis 检查能放 readyz 不能放 healthz**

Redis 不可用时，这个实例确实处理不了请求，从负载均衡摘掉是合理的；Redis 恢复后 readyz 自动变回 200，流量回来，**不需要重启**。如果放在 healthz，Redis 一挂所有实例都被重启，重启后 Redis 还是挂的，于是反复重启，问题更大。但也要想到：如果所有实例共用一个 Redis，它挂了所有实例都会被摘掉，服务一样不可用。到底是“摘掉”还是“降级继续服务”（比如没有缓存也能工作），是第 08 模块高可用要讨论的取舍。
