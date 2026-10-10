# 0.3 Python（够用即可）：读懂 ML 生态的那一半

> 第 0 部分 · 第 3 课 ｜ 预计 3 小时 ｜ 前置：学完 0.1 Go、0.2 TypeScript，Python 3.12+
>
> 目标不是成为 Python 专家，而是**读得懂、改得动**：模型 SDK、评测脚本、LangGraph、vLLM、训练代码，几乎都是 Python。本课每个语法都对照 Go 和 TS 讲，每段代码后面有逐行解读。

## 为什么学这个

你的主力是 Go / TS，但 Agent 生态里这些东西基本只有 Python 版或 Python 版最好：

```
评测：SWE-bench、τ-bench、Inspect、各种 LLM-as-Judge 脚本
框架：LangGraph、OpenAI Agents SDK、Claude Agent SDK 的 Python 版最先更新
模型侧：vLLM、SGLang、transformers、TRL（DPO / GRPO）
```

本课只学写 Agent 用得到的三件事，和前两课一一对应：

| 问题 | Go（0.1） | TS（0.2） | Python（本课） |
|---|---|---|---|
| 项目与依赖 | `go mod` | `npm` | **uv** |
| 校验外部 JSON | `json.Unmarshal` + 手写校验 | Zod | **Pydantic** |
| 并发模型 | goroutine（多线程真并行） | 事件循环（单线程） | **asyncio**（单线程事件循环，和 JS 一样） |
| 并行工具 | `errgroup` | `Promise.all` + worker | **`asyncio.TaskGroup` + `Semaphore`** |
| 超时 / 取消 | `context.WithTimeout` | `AbortSignal.timeout` | **`asyncio.timeout`** + 任务取消 |
| 流式读取 | goroutine + channel | AsyncGenerator | **async generator**（几乎和 TS 一样） |

一句话：**Python 的 asyncio 心智模型和 TS 几乎一样**，你学 0.2 时建立的直觉可以直接搬过来。

---

## 0. Go / TS 程序员的 Python 语法速成

### 0.1 缩进就是代码块

```python
def add(a: int, b: int) -> int:
    if a > 0:
        return a + b
    return b
```

- 没有 `{}`，**冒号 + 缩进**表示代码块。缩进统一用 4 个空格，混用 tab 会报错。
- 没有分号。
- `def` = Go 的 `func` = TS 的 `function`。
- `a: int` 是**类型注解**，`-> int` 是返回类型。和 TS 一样写在名字后面。

**关键区别：Python 的类型注解运行时不检查。** `add("x", "y")` 照样能跑（结果是 `"xy"`）。类型注解只给编辑器和类型检查工具（`mypy` / `pyright`）看，这一点和 TS 一样：类型只在“编译期”有用，运行时要靠 Pydantic 校验。

### 0.2 变量和基本类型

```python
name = "search"          # 没有 const / let / var，直接赋值
limit: int = 4           # 可以加类型注解
MAX_RETRY = 3            # 全大写只是约定“这是常量”，语言不阻止你改
```

| Python | Go | TS |
|---|---|---|
| `str` | `string` | `string` |
| `int` / `float` | `int` / `float64` | `number` |
| `bool`（`True` / `False`，首字母大写） | `bool` | `boolean` |
| `None` | `nil` | `null` / `undefined` |
| `list[str]` | `[]string` | `string[]` |
| `dict[str, int]` | `map[string]int` | `Record<string, number>` |
| `tuple[str, int]` | 多返回值 `(string, int)` | `[string, number]` |
| `str \| None` | `*string` | `string \| undefined` |
| `Any` | `any` | `any` |

```python
nums = [1, 2, 3]                  # list
nums.append(4)                    # Go: nums = append(nums, 4)
first = nums[0]                   # 越界会抛 IndexError（类似 Go 的 panic）
last = nums[-1]                   # 负下标从末尾数，Go/TS 都没有
part = nums[1:3]                  # 切片 [2, 3]，和 Go 一样左闭右开

cfg = {"model": "gpt", "temp": 0.7}   # dict，= Go map / TS 对象
cfg["temp"]                       # 0.7；键不存在会抛 KeyError
cfg.get("top_p")                  # 键不存在返回 None，不抛错
cfg.get("top_p", 1.0)             # 带默认值
"model" in cfg                    # True，= Go 的 _, ok := cfg["model"]
```

### 0.3 字符串

```python
name = "search"
msg = f"未知工具 {name}"           # f-string，= TS 模板字符串 `未知工具 ${name}`
line = "  data: hi \n".strip()    # 去首尾空白，= strings.TrimSpace / trim()
line.startswith("data:")          # = strings.HasPrefix / startsWith
line[5:]                          # 去掉前 5 个字符，= line[5:] (Go) / slice(5) (TS)
```

### 0.4 函数：默认参数、关键字参数、多返回值

```python
def run(calls: list[str], limit: int = 4, timeout_s: float = 20.0) -> list[str]:
    ...

run(["a"])                         # limit=4, timeout_s=20.0
run(["a"], timeout_s=0.2)          # 关键字参数：按名字传，跳过 limit
```

- `limit: int = 4`：带默认值的参数。Go 没有默认参数；TS 有，写法一样。
- `run(["a"], timeout_s=0.2)`：**关键字参数**，调用时按名字传。这在 Python 里非常常见，相当于 TS 里传一个选项对象 `{ timeoutS: 0.2 }`，但更轻。
- `...`：Python 的“这里先空着”，合法的语法，相当于 Go 里写个 `panic("TODO")`。

多返回值用 tuple：

```python
def parse(raw: str) -> tuple[dict | None, str | None]:
    if not raw:
        return None, "empty"       # 相当于 Go 的 return nil, errors.New("empty")
    return {"ok": True}, None

data, err = parse("x")             # 解包，= Go 的 data, err := parse("x")
if err is not None:                # 判断 None 用 is / is not，不要用 ==
    ...
```

### 0.5 异常：try / except / finally / raise

```python
try:
    n = int("abc")                 # 抛 ValueError
except ValueError as e:            # = TS 的 catch (e)，但可以按类型分别捕获
    print("不是数字：", e)
except (KeyError, IndexError):     # 一次捕获多种
    print("下标或键错误")
finally:
    print("总会执行")               # = defer / finally

raise RuntimeError("出错了")        # = TS 的 throw new Error(...)
```

和 TS 的区别：**`except` 可以按异常类型分别处理**，不需要在 `catch` 里 `if (err instanceof ...)`。这一点更像 Go 的 `errors.As`。

### 0.6 `with`：自动清理（Python 版 defer）

```python
with open("a.txt") as f:           # 进入时打开文件
    text = f.read()
# 离开 with 块时自动 f.close()，不管是正常结束还是抛异常
```

对照 Go：

```go
f, err := os.Open("a.txt")
if err != nil { return err }
defer f.Close()
```

`with` 后面的对象叫**上下文管理器**（context manager，和 Go 的 context 不是一个东西）。HTTP 客户端、锁、超时、文件……凡是“用完要清理”的，Python 都用 `with`。异步版本是 `async with`，后面大量出现。

### 0.7 类和 dataclass

```python
from dataclasses import dataclass

@dataclass
class ToolCall:
    id: str
    name: str
    args: str

c = ToolCall("1", "search", "{}")         # 按顺序传
c = ToolCall(id="1", name="search", args="{}")  # 或按名字传
print(c.name)                              # "search"
print(c)                                   # ToolCall(id='1', name='search', args='{}')
```

- `@dataclass` 是**装饰器**：写在类上面，自动生成构造函数、打印、比较等方法。效果约等于 Go 的 struct：只放数据。
- `from dataclasses import dataclass`：从标准库模块 `dataclasses` 导入名字 `dataclass`，= TS 的 `import { dataclass } from "dataclasses"`。

> `@xxx` 这种写法统称装饰器，就是“把下面的函数/类交给 xxx 加工一下”。看到 `@app.get("/")`、`@pytest.fixture` 都是同一回事。

### 0.8 列表推导式

```python
results = [r.is_error for r in res]              # = res.map(r => r.isError)
ok = [r for r in res if not r.is_error]          # = res.filter(r => !r.isError)
```

Go 里你会写一个 for 循环 append。读 Python 代码时这个写法到处都是，看懂就行。

### 0.9 模块和包

```python
import asyncio                         # 导入整个模块，用 asyncio.sleep(...)
from pydantic import BaseModel, Field  # 只导入几个名字
from py_agent_core.tools import run_tools  # 导入自己项目里的模块（目录 = 包）
```

- 一个 `.py` 文件就是一个模块，一个目录就是一个包（相当于 Go 的 package）。
- 没有 export 关键字：所有顶层名字都能被导入。**以下划线开头的名字（`_adapter`）约定为私有**，相当于 Go 的小写开头。

---

## 1. uv：Python 的 go mod

Python 的老问题是“环境地狱”：系统 Python、pip、venv、requirements.txt、poetry……**uv** 是 2024 年后的事实标准，一个工具全包，而且很快。

```bash
uv init --lib my-agent        # 新建项目，= go mod init / npm init
cd my-agent
uv add pydantic httpx         # 加依赖，= go get / npm i
uv add --dev pytest pytest-asyncio   # 开发依赖，= npm i -D
uv run pytest                 # 在项目的虚拟环境里运行命令，= npx
uv run python -m my_agent     # 运行你的代码
uv sync                       # 按锁文件装依赖，= go mod download / npm ci
uv python install 3.12        # 连 Python 本身都能装
```

| uv 生成的文件 | 对应 | 作用 |
|---|---|---|
| `pyproject.toml` | `go.mod` / `package.json` | 项目名、Python 版本、依赖列表 |
| `uv.lock` | `go.sum` / `package-lock.json` | 锁定每个依赖的精确版本，**要提交到 git** |
| `.venv/` | `node_modules/` | 虚拟环境，项目独立的 Python + 依赖，**不提交** |
| `.python-version` | `go` 指令那一行 | 项目用哪个 Python 版本 |

**虚拟环境（venv）是什么**：Python 默认把包装到全局，A 项目要 pydantic 1、B 项目要 pydantic 2 就打架。venv 给每个项目一个独立目录放依赖，Go 和 Node 天生就是按项目隔离的，Python 要靠 venv。uv 自动帮你建、自动激活，你只要记住**所有命令前面加 `uv run`**。

`--lib` 生成的目录结构：

```
my-agent/
├── pyproject.toml
├── uv.lock
├── src/
│   └── my_agent/          # 包名：目录名里用下划线
│       └── __init__.py    # 有这个文件，目录才是一个包（可以是空的）
└── tests/
    └── test_xxx.py        # pytest 自动找 test_ 开头的文件和函数
```

## 2. Pydantic：Python 的 Zod

和 TS 一样，Python 的类型注解运行时不检查，模型返回的 JSON 必须真校验。**Pydantic v2** 就是 Python 的 Zod，而且几乎是所有 AI SDK 的底座：OpenAI SDK、Anthropic SDK、LangChain、FastAPI 的数据类型全是 Pydantic。

### 定义工具参数

```python
from pydantic import BaseModel, Field


class SearchArgs(BaseModel):
    query: str = Field(min_length=1, description="搜索关键词")
    top_k: int = Field(default=5, ge=1, le=20)
```

逐行解读：

- `class SearchArgs(BaseModel):`：定义一个类，**继承** `BaseModel`。括号里是父类，相当于“这个 struct 自带校验能力”。
- `query: str = Field(min_length=1, description="搜索关键词")`：
  - `query: str`：字段名和类型，Pydantic 会**真的在运行时检查**它是字符串
  - `Field(...)`：附加约束，`min_length=1` = Zod 的 `.min(1)`，`description` = `.describe()`，会进 JSON Schema 给模型看
- `top_k: int = Field(default=5, ge=1, le=20)`：整数，默认 5，`ge` = greater or equal（≥1），`le` = less or equal（≤20）。

和 Zod 对照：

```ts
const SearchArgs = z.object({
  query: z.string().min(1).describe("搜索关键词"),
  topK: z.number().int().min(1).max(20).default(5),
});
type SearchArgs = z.infer<typeof SearchArgs>;
```

Pydantic 更省一步：**类本身既是校验器，又是类型**，不需要 `z.infer`。

### 生成 JSON Schema（发给模型）

```python
schema = SearchArgs.model_json_schema()
```

得到：

```json
{
  "properties": {
    "query": {"description": "搜索关键词", "minLength": 1, "title": "Query", "type": "string"},
    "top_k": {"default": 5, "maximum": 20, "minimum": 1, "title": "Top K", "type": "integer"}
  },
  "required": ["query"],
  "title": "SearchArgs",
  "type": "object"
}
```

= `z.toJSONSchema(SearchArgs)`。OpenAI / Anthropic 的 Python SDK 里，你经常直接把 Pydantic 类传给 `response_format` 或工具定义，SDK 内部就是调用这个方法。

### 校验模型返回的参数

```python
from pydantic import ValidationError


def parse_args(raw: str) -> tuple[SearchArgs | None, str | None]:
    try:
        return SearchArgs.model_validate_json(raw), None
    except ValidationError as e:
        return None, str(e)
```

逐行解读：

- `SearchArgs.model_validate_json(raw)`：**一步完成** JSON 解析 + 校验，返回一个 `SearchArgs` 实例。TS 里要先 `JSON.parse` 再 `safeParse` 两步；Pydantic 一步，而且非法 JSON 也会变成 `ValidationError`，不用单独 `try json.loads`。
- 校验失败抛 `ValidationError`，我们捕获它，转成文字返回。Pydantic 没有 `safeParse`，Python 的习惯就是“抛异常 + 在边界处捕获”。
- 返回 `(结果, 错误)` 的 tuple，模仿 Go 风格，调用方：

```python
args, err = parse_args(tool_call.args)
if err is not None:
    return ToolResult(tool_call.id, err, is_error=True)   # 错误文本回传给模型
await search(args.query, args.top_k)
```

`str(e)` 的内容大致是：

```
1 validation error for SearchArgs
top_k
  Input should be less than or equal to 20 [type=less_than_equal, input_value=50, input_type=int]
```

模型看到这段很容易自己改对。和前两课同一个原则：**参数错误是给模型的观察，不是让程序崩的异常。**

另外两个常用方法：

```python
SearchArgs.model_validate({"query": "x"})   # 校验一个 dict（已经 json.loads 过的数据）
args.model_dump()                           # 实例 → dict，= Go 的 struct 转 map
args.model_dump_json()                      # 实例 → JSON 字符串，= json.Marshal
```

### 可辨识联合

```python
from typing import Annotated, Literal

from pydantic import BaseModel, Field, TypeAdapter


class TextEvent(BaseModel):
    type: Literal["text"]
    delta: str


class ToolCallEvent(BaseModel):
    type: Literal["tool_call"]
    id: str
    name: str
    args: str


StreamEvent = Annotated[
    TextEvent | ToolCallEvent,
    Field(discriminator="type"),
]

adapter = TypeAdapter(StreamEvent)
ev = adapter.validate_json('{"type":"text","delta":"你"}')   # → TextEvent(type='text', delta='你')
```

逐行解读：

- `Literal["text"]`：字面量类型，这个字段只能是 `"text"`。= TS 的 `type: "text"` / Zod 的 `z.literal("text")`。
- `TextEvent | ToolCallEvent`：联合类型，和 TS 写法一样。
- `Annotated[类型, Field(discriminator="type")]`：`Annotated` 是“给类型附加额外信息”的标准写法，这里附加的信息是“按 `type` 字段区分”。= `z.discriminatedUnion("type", [...])`。
- `TypeAdapter(StreamEvent)`：`StreamEvent` 不是一个 `BaseModel` 类，没有 `model_validate_json` 方法。`TypeAdapter` 把任意类型包装成一个校验器。记住：**单个 model 用类方法，联合类型 / list / dict 用 `TypeAdapter`**。

用的时候用 `match`（Python 3.10+ 的模式匹配，= `switch`）：

```python
match ev:
    case TextEvent():
        print(ev.delta, end="")
    case ToolCallEvent():
        print(ev.name, ev.args)
```

- `case TextEvent():` 意思是“如果 ev 是 TextEvent 实例”，和 Go 的 `case TextEvent:`（type switch）几乎一样。
- `print(x, end="")`：不换行打印，= `fmt.Print`。
- Python 的 `match` **不会**像 TS `assertNever` 那样在漏写 case 时报错（除非用 pyright 的严格模式）。这是 Python 类型系统比 TS 弱的地方。

## 3. asyncio：和 JS 一样的事件循环

### 心智模型

Python 有两种并发方式：线程（`threading`）和 asyncio。**写 Agent 用 asyncio**：调模型、调工具几乎全是网络 I/O，asyncio 正是为此设计的。

asyncio 和 JS 的模型**几乎完全一样**：

- 一个线程，一个事件循环。
- `async def` 定义的函数叫**协程函数**，调用它得到一个**协程对象**（= JS 的 Promise，但有一个关键区别，见下面）。
- `await` 暂停当前协程，让事件循环去跑别的；I/O 完成后再回来。
- 同一时刻只有一段 Python 代码在执行，**普通变量读写不需要锁**（和 0.2 里 `next++` 的结论一样）。

```python
import asyncio


async def get_weather(city: str) -> str:
    await asyncio.sleep(1)        # 模拟网络请求；= TS 的 await sleep(1000)
    return f"{city}: 晴"


async def main() -> None:
    w = await get_weather("北京")
    print(w)


asyncio.run(main())               # 启动事件循环，跑 main，跑完退出
```

逐行解读：

- `async def get_weather(...)`：协程函数，= TS 的 `async function`。
- `await asyncio.sleep(1)`：异步睡 1 **秒**（注意单位是秒，TS 是毫秒）。
- `asyncio.run(main())`：程序入口。JS 的事件循环是自动的，Python 要你**显式启动**。整个程序通常只调用一次，= Go 的 `func main()`。

### 和 JS 的关键区别：协程不 await 就不会运行

```python
async def main():
    get_weather("北京")             # ❌ 什么也不会发生！只是创建了一个协程对象
    await get_weather("北京")       # ✅ 运行并等待
```

- **TS**：`doWork()` 不 await，它也会立刻开始执行（Promise 是“热”的）。
- **Python**：`get_weather("北京")` 只创建协程对象，**不 await 也不交给事件循环，就永远不执行**（协程是“冷”的）。Python 会打一行警告 `RuntimeWarning: coroutine 'get_weather' was never awaited`。

想“开始运行但先不等”（= Go 的 `go f()`），要显式创建任务：

```python
task = asyncio.create_task(get_weather("北京"))   # 交给事件循环，开始跑
# ... 做别的事
w = await task                                    # 需要结果时再等
```

### 并发跑多个：`gather` 与 `TaskGroup`

```python
# 方式 1：gather，= Promise.all
a, b = await asyncio.gather(get_weather("北京"), get_weather("上海"))   # 总共约 1 秒

# 方式 2：TaskGroup（Python 3.11+），= errgroup
async with asyncio.TaskGroup() as tg:
    t1 = tg.create_task(get_weather("北京"))
    t2 = tg.create_task(get_weather("上海"))
# 离开 async with 时，所有任务都已完成
print(t1.result(), t2.result())
```

逐行解读 TaskGroup：

- `async with asyncio.TaskGroup() as tg:`：开一个任务组。`async with` 是 `with` 的异步版：进入时创建，**离开时等待组内所有任务完成**。= `g, ctx := errgroup.WithContext(ctx)` + 块结束时自动 `g.Wait()`。
- `tg.create_task(coro)`：在组里启动一个任务，= `g.Go(func() error {...})`。
- **任一任务抛异常，TaskGroup 会取消组内其他所有任务**，然后把异常抛出来（包在 `ExceptionGroup` 里）。这和 `errgroup.WithContext` 一模一样：一个失败，ctx 取消，其他全停。
- `t1.result()`：拿任务的返回值。

**新代码优先用 `TaskGroup`**：它保证“块结束时没有任务在后台乱跑”，相当于语言层面帮你防 goroutine 泄漏。`gather` 在一个任务失败时不会自动取消其他任务。

### 限流：Semaphore

```python
sem = asyncio.Semaphore(4)        # 最多 4 个同时进入

async def limited(city: str) -> str:
    async with sem:               # 进入时占一个名额，满了就在这里等
        return await get_weather(city)
    # 离开 with 自动释放名额
```

= Go 的 `g.SetLimit(4)`，或者用带缓冲 channel 当信号量 `sem := make(chan struct{}, 4)`。

### 超时：`asyncio.timeout`

```python
try:
    async with asyncio.timeout(2):         # 2 秒
        w = await get_weather("北京")
except TimeoutError:
    print("超时了")
```

- `async with asyncio.timeout(2):` 里面的代码超过 2 秒，就会被**取消**，然后在块的出口抛出 `TimeoutError`。= `ctx, cancel := context.WithTimeout(ctx, 2*time.Second); defer cancel()`。
- 超时可以嵌套：外层 60 秒整轮超时，内层 20 秒单工具超时，各管各的。

### 取消：Python 的取消是“注入异常”

这是和 Go / TS 最不同的地方，务必理解：

| | 怎么取消 | 被取消的代码怎么知道 |
|---|---|---|
| Go | `cancel()` | 自己检查 `<-ctx.Done()`，**不检查就不会停** |
| TS | `ctrl.abort()` | 自己监听 `signal`，**不监听就不会停** |
| Python | `task.cancel()` / 超时 | **在它下一个 `await` 处自动抛出 `CancelledError`** |

也就是说，Python 里你**不需要把 ctx / signal 一路往下传**。任务被取消时，它正卡在哪个 `await` 上，那里就会抛出 `asyncio.CancelledError`，异常一路往上冒，沿途的 `finally` / `with` 都会执行清理。这让代码简洁很多。

但有两个代价：

1. **不 `await` 的代码取消不了**。一个死循环纯计算、或者调用了阻塞函数（`time.sleep`、`requests.get`），中间没有 `await`，取消信号就进不去。和 Go 里不读 `ctx.Done()` 是一个道理。
2. **不要吞掉 `CancelledError`**。`CancelledError` 继承自 `BaseException` 而不是 `Exception`，所以 `except Exception:` 不会误抓它，这是故意设计的。但如果你写了 `except BaseException:` 或裸 `except:` 而且不重新抛出，取消就失效了。

## 4. 并行工具调用：TaskGroup + Semaphore + timeout

0.1 `runTools`（Go）和 0.2 `runTools`（TS）的 Python 版：

```python
import asyncio
from collections.abc import Awaitable, Callable
from dataclasses import dataclass


@dataclass
class ToolCall:
    id: str
    name: str
    args: str


@dataclass
class ToolResult:
    id: str
    output: str
    is_error: bool


Tool = Callable[[str], Awaitable[str]]


async def run_tools(
    calls: list[ToolCall],
    tools: dict[str, Tool],
    limit: int = 4,
    per_tool_s: float = 20.0,
) -> list[ToolResult]:
    results: list[ToolResult | None] = [None] * len(calls)
    sem = asyncio.Semaphore(limit)

    async def run_one(i: int, c: ToolCall) -> None:
        tool = tools.get(c.name)
        if tool is None:
            results[i] = ToolResult(c.id, f"未知工具 {c.name}", True)
            return
        async with sem:
            try:
                async with asyncio.timeout(per_tool_s):
                    output = await tool(c.args)
                results[i] = ToolResult(c.id, output, False)
            except TimeoutError:
                results[i] = ToolResult(c.id, f"超时（{per_tool_s}s）", True)
            except Exception as e:
                results[i] = ToolResult(c.id, f"{type(e).__name__}: {e}", True)

    async with asyncio.TaskGroup() as tg:
        for i, c in enumerate(calls):
            tg.create_task(run_one(i, c))

    return [r for r in results if r is not None]
```

### 逐行解读

**类型定义**

- `from collections.abc import Awaitable, Callable`：标准库里描述“函数类型”的工具。
- `Tool = Callable[[str], Awaitable[str]]`：**类型别名**。读作：Tool 是一个可调用对象，参数列表是 `[str]`，返回一个可 await 的、最终给出 `str` 的东西。= TS 的 `type Tool = (args: string) => Promise<string>`。
  - 注意：**没有 signal / ctx 参数**。Python 靠异常注入取消，工具签名更干净。

**准备**

- `results: list[ToolResult | None] = [None] * len(calls)`：建一个长度为 N、全是 `None` 的列表。`[None] * 3` 得到 `[None, None, None]`。= Go 的 `make([]ToolResult, len(calls))`。
- `sem = asyncio.Semaphore(limit)`：并发上限。

**内部函数 `run_one`：处理一个工具调用**

- `async def run_one(i, c)` 定义在 `run_tools` 里面，能直接读写外层的 `results`、`sem`、`tools`（闭包，和 Go/TS 一样）。
- `tool = tools.get(c.name)`：查工具，查不到得到 `None`（不抛 KeyError）。
- `if tool is None: ... return`：未知工具直接记错误结果。放在 `async with sem` 之前，不占并发名额。
- `async with sem:`：领一个并发名额，最多 `limit` 个任务同时到达这里之后。
- `async with asyncio.timeout(per_tool_s):`：单工具超时。
- `output = await tool(c.args)`：真正调用工具。这里是让出点。
- `results[i] = ToolResult(c.id, output, False)`：按下标写结果，结果顺序和 `calls` 一致。和 Go/TS 一样，每个任务写自己的下标；又因为单线程，本来也不会有竞争。

**两种失败，分开处理**

- `except TimeoutError:`：**这个工具**超时了，记成错误结果回传给模型。
- `except Exception as e:`：工具自己抛了任何普通异常，同样记成结果。`type(e).__name__` 取异常类名（如 `ValueError`），让模型知道错在哪。
- **整体取消时会怎样？** 外层（比如整轮 60 秒超时）取消了任务，`await tool(...)` 处抛出的是 `CancelledError`。它**不是** `Exception` 的子类，所以上面两个 `except` 都不会接住它，它直接冒出 `run_one`，TaskGroup 取消其他任务并向上抛出。这就是 Go 版里的 `return err`、TS 版里的 `if (signal.aborted) throw err`，**Python 什么都不用写**。

**启动所有任务**

- `async with asyncio.TaskGroup() as tg:`：开任务组。
- `for i, c in enumerate(calls):`：`enumerate` 同时给出下标和元素，= Go 的 `for i, c := range calls`。
- `tg.create_task(run_one(i, c))`：每个调用一个任务。**一次性全部创建**，由 Semaphore 控制同时运行的数量。（TS 版是开 `limit` 个 worker 抢任务；两种写法都对，Python 里 Semaphore 更常见。）
- 离开 `async with` 时所有任务都结束了。

**返回**

- `return [r for r in results if r is not None]`：列表推导式过滤掉 `None`。走到这里时每个位置都已经填了，这一步主要是让类型从 `list[ToolResult | None]` 变成 `list[ToolResult]`。

### 三个版本对照

| | Go | TS | Python |
|---|---|---|---|
| 并发上限 | `g.SetLimit(4)` | 开 4 个 worker | `Semaphore(4)` |
| 启动 | `g.Go(...)` | `workers.push(worker())` | `tg.create_task(...)` |
| 等待 | `g.Wait()` | `await Promise.all(...)` | 离开 `async with TaskGroup` |
| 单工具超时 | `context.WithTimeout` | `AbortSignal.timeout` | `asyncio.timeout` |
| 工具怎么感知取消 | 读 `ctx.Done()` | 监听 `signal` | 自动：`await` 处抛 `CancelledError` |
| 单个失败 | `return nil` + 写 `Err` | `catch` 里写结果 | `except Exception` 里写结果 |
| 整体取消 | `return err` | `throw err` | 不用写，`CancelledError` 自动冒出 |

## 5. 异步生成器：流式读取 SSE

Python 的异步生成器和 TS 的几乎一模一样。HTTP 客户端用 **httpx**（OpenAI / Anthropic 官方 Python SDK 底层都是它）。

```python
from collections.abc import AsyncIterator

import httpx


async def sse_events(client: httpx.AsyncClient, url: str) -> AsyncIterator[str]:
    async with client.stream("GET", url) as resp:
        resp.raise_for_status()
        async for line in resp.aiter_lines():
            line = line.strip()
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if data == "[DONE]":
                return
            yield data
```

### 逐行解读

- `async def ... -> AsyncIterator[str]`：函数体里有 `yield`，所以它是**异步生成器**。= TS 的 `async function*(): AsyncGenerator<string>`。Python 不需要 `*` 标记，有 `yield` 就自动是生成器。
- `client: httpx.AsyncClient`：传入一个复用的 HTTP 客户端（内部有连接池），= Go 的 `*http.Client`。
- `async with client.stream("GET", url) as resp:`：发起**流式**请求。用 `stream` 而不是 `get`，响应体才不会一次性读进内存。`async with` 保证离开时**关闭响应、释放连接**，= Go 的 `defer resp.Body.Close()`。
- `resp.raise_for_status()`：状态码 4xx / 5xx 时抛异常，= Go 里 `if resp.StatusCode >= 400 { return err }`。
- `async for line in resp.aiter_lines():`：**逐行**异步读取。httpx 已经帮你做了 UTF-8 解码和按行切分，所以不需要 TS 版的 `TextDecoderStream` + `buf` + `indexOf("\n")`。= Go 的 `bufio.Scanner`。
- `line[5:]`：去掉前 5 个字符 `data:`。
- `return`：生成器结束（遇到 `[DONE]`）。
- `yield data`：产出一条，暂停，等调用方要下一条。

### 用法，以及一个和 JS 不同的坑

```python
from contextlib import aclosing

async with httpx.AsyncClient() as client:
    async with aclosing(sse_events(client, URL)) as events:
        async for data in events:
            print(data)
            if should_stop:
                break
```

- `async with httpx.AsyncClient() as client:`：创建客户端，离开时关闭连接池。
- `aclosing(...)`：**为什么要多包这一层？** 在 TS 里，`for await` 里 `break` 会立刻调用生成器的 `return()`，`finally` 马上执行。**Python 的 `async for` 里 `break` 不会立刻关闭异步生成器**，生成器只是被挂起，要等垃圾回收或事件循环关闭时才清理。于是生成器里的 `async with client.stream(...)` 迟迟不退出，**连接一直开着，上游可能还在生成、还在计费**。
- `aclosing` 保证离开这个 `async with` 时调用生成器的 `aclose()`，生成器在暂停的 `yield` 处收到 `GeneratorExit`，里面的 `async with` 正常退出，连接关闭。= TS 里 `break` 自动做的事。
- 规则：**`break` 可能提前退出的异步生成器，一律用 `aclosing` 包起来。**

取消整个流式读取，不需要 AbortController：

```python
async with asyncio.timeout(60):       # 60 秒后自动取消
    async with aclosing(sse_events(client, URL)) as events:
        async for data in events:
            ...
```

超时时，正在 `await` 下一行数据的地方抛 `CancelledError`，生成器里的 `async with client.stream` 退出、关闭连接，最后在外层变成 `TimeoutError`。

## 6. 四个高频坑

### 坑 1：在协程里调用阻塞函数

```python
async def bad_tool(args: str) -> str:
    time.sleep(5)                      # ❌ 阻塞整个线程，事件循环卡死 5 秒，所有任务都停
    return requests.get(URL).text      # ❌ requests 是同步库，同样阻塞
```

asyncio 只有一个线程，一个协程不 `await` 就霸占它，其他所有任务（包括超时计时）都动不了。Go 没有这个问题（goroutine 会被调度器抢占），JS 有同样的问题。

修正：用异步版本 `await asyncio.sleep(5)`、`httpx.AsyncClient`；实在要调同步库，丢到线程里：`await asyncio.to_thread(requests.get, URL)`。

### 坑 2：忘了 `await`

```python
async def main():
    asyncio.sleep(1)     # ❌ 协程对象被创建又丢掉，什么也没发生
```

比 TS 更隐蔽：TS 里没 await 至少还会执行，Python 里**根本不执行**。看到 `coroutine ... was never awaited` 警告就是它。

### 坑 3：`create_task` 后不保存引用

```python
asyncio.create_task(send_metrics())   # ❌ 返回值被丢弃
```

事件循环只对任务持有弱引用，你不保存它，任务可能执行到一半被垃圾回收。要么保存到变量 / 集合里，要么直接用 `TaskGroup`（推荐）。

### 坑 4：异步生成器 `break` 不关闭

见第 5 节：用 `aclosing`。

---

## 练习（今天做完）

### 环境准备

```bash
mkdir -p projects && cd projects
uv init --lib --name py-agent-core --python 3.12 02-py-agent-core
cd 02-py-agent-core
uv add pydantic httpx
uv add --dev pytest pytest-asyncio
```

在 `pyproject.toml` 末尾加上：

```toml
[tool.pytest.ini_options]
asyncio_mode = "auto"
```

`asyncio_mode = "auto"` 让 pytest 自动用事件循环运行 `async def test_...` 测试函数。

文件放在 `src/py_agent_core/` 下：`events.py`、`tools.py`、`sse.py`；测试放 `tests/` 下。

### 题目

1. 用 Pydantic 定义 `StreamEvent` 可辨识联合（text / tool_call / done / error 四种），写 `parse_event(data: str)`，返回 `(event, None)` 或 `(None, 错误文本)`，永远不抛异常
2. 用 `run_tools` 跑三个假工具（0.05s / 0.1s / 5s），`per_tool_s=0.2`，断言前两个成功、第三个 `is_error=True`、总耗时 < 0.4s
3. 写一个测试：外层套 `asyncio.timeout(0.05)` 调用 `run_tools`，断言抛出 `TimeoutError`（整体取消能穿透，没有被 `except Exception` 吞掉）
4. 用 `asyncio.start_server` 起一个假 SSE 服务，客户端读 3 条后 `break`，断言服务端感知到断开
5. `uv run pytest` 全绿

## 自测题

1. Python 的协程和 JS 的 Promise 有什么关键区别？
2. 为什么 Python 版 `run_tools` 不需要像 Go / TS 那样把 ctx / signal 传给每个工具？代价是什么？
3. `except Exception` 为什么不会把整体取消吞掉？
4. `TaskGroup` 和 `gather` 有什么区别？为什么新代码优先用 `TaskGroup`？
5. 为什么异步生成器要用 `aclosing` 包起来？TS 里为什么不需要？

---

# 参考答案与详解

## 自测题答案

### 1. 协程 vs Promise
JS 的 Promise 是“热”的：`doWork()` 一调用就开始执行，`await` 只是等结果。Python 的协程是“冷”的：`do_work()` 只创建一个协程对象，必须 `await` 它，或者用 `create_task` / `TaskGroup` 交给事件循环，才会开始执行。所以 Python 里忘了 `await` 的后果更严重：代码根本没跑。

### 2. 为什么不用传 ctx / signal
Python 的取消是“注入异常”：任务被取消（`task.cancel()`、`asyncio.timeout` 到期、TaskGroup 里别的任务失败）时，它正卡在哪个 `await` 上，那里就抛出 `CancelledError`。工具代码只要是正常的异步代码（`await httpx...`、`await asyncio.sleep`），天然就能被取消，不需要显式检查信号。

代价：没有 `await` 的代码取消不了，比如阻塞调用 `time.sleep`、同步库 `requests`、长时间纯计算循环。这和 Go 里 goroutine 不读 `ctx.Done()`、TS 里工具不监听 `signal` 是同一类问题，只是形式不同。

### 3. 为什么 `except Exception` 不吞取消
从 Python 3.8 起，`asyncio.CancelledError` 继承自 `BaseException`，不是 `Exception`。`except Exception` 只接普通错误，取消信号会直接穿过去，一路冒到 TaskGroup 和外层。练习 3 就是在验证这一点。反过来，写 `except BaseException:` 或裸 `except:` 却不重新 `raise`，就会把取消吞掉：这个任务收到了取消，却当成普通错误处理后继续往下执行。

### 4. `TaskGroup` vs `gather`
- `TaskGroup`：一个任务失败，**自动取消组内其他任务**，等它们都结束后抛出 `ExceptionGroup`。离开 `async with` 时保证没有任务还在后台跑。= `errgroup.WithContext`。
- `gather`（默认参数）：一个任务失败，异常立刻抛给你，但**其他任务继续在后台跑**，没人等它们，容易泄漏。

新代码优先用 `TaskGroup`，它把“每个任务怎么结束”这件事交给语言保证，和 0.1 里“每启动一个 goroutine，都要想清楚它怎么结束”是同一条原则。

### 5. 为什么要 `aclosing`
TS 的 `for await` 里 `break`，会立刻调用生成器的 `return()`，`finally` 马上执行。Python 的 `async for` 里 `break` 不会立刻关闭异步生成器，它只是被挂起，等垃圾回收或事件循环关闭时才清理。在这之前，生成器里的 `async with client.stream(...)` 不会退出，HTTP 连接一直占着。`aclosing` 在离开 `async with` 时显式调用 `aclose()`，保证立刻清理。

## 练习参考实现

以下代码均已在 Python 3.12、pydantic 2.14、httpx 0.28 下运行通过：`uv run pytest` → `6 passed`。

### 练习 1：`parse_event`（`src/py_agent_core/events.py`）

```python
from typing import Annotated, Literal

from pydantic import BaseModel, Field, TypeAdapter, ValidationError


class TextEvent(BaseModel):
    type: Literal["text"]
    delta: str


class ToolCallEvent(BaseModel):
    type: Literal["tool_call"]
    id: str
    name: str
    args: str


class Usage(BaseModel):
    input: int
    output: int


class DoneEvent(BaseModel):
    type: Literal["done"]
    usage: Usage


class ErrorEvent(BaseModel):
    type: Literal["error"]
    message: str


StreamEvent = Annotated[
    TextEvent | ToolCallEvent | DoneEvent | ErrorEvent,
    Field(discriminator="type"),
]

_adapter = TypeAdapter(StreamEvent)


def parse_event(data: str) -> tuple[StreamEvent | None, str | None]:
    """返回 (event, None) 或 (None, 错误文本)，永远不抛异常。"""
    try:
        return _adapter.validate_json(data), None
    except ValidationError as e:
        return None, str(e)
```

逐行解读：

- 四个 `BaseModel` 类，每个都有 `type: Literal[...]` 作为标签，和 TS 版 `z.object({ type: z.literal("text"), ... })` 一一对应。
- `Usage` 单独定义成一个 model，再作为 `DoneEvent.usage` 的类型：Pydantic 会递归校验嵌套对象。
- `StreamEvent = Annotated[A | B | C | D, Field(discriminator="type")]`：可辨识联合，= `z.discriminatedUnion("type", [...])`。
- `_adapter = TypeAdapter(StreamEvent)`：联合类型不是 `BaseModel`，要用 `TypeAdapter` 包一层才能校验。放在模块顶层只建一次，因为构建校验器有开销。下划线开头表示模块私有。
- `-> tuple[StreamEvent | None, str | None]`：返回 `(事件, 错误)`，Go 风格。
- `_adapter.validate_json(data)`：解析 + 校验一步完成，非法 JSON 也会变成 `ValidationError`。
- `"""..."""`：三引号字符串，放在函数第一行就是**文档字符串**（docstring），= Go 函数上方的注释。

测试（`tests/test_events.py`）：

```python
from py_agent_core.events import TextEvent, parse_event


def test_valid():
    ev, err = parse_event('{"type":"text","delta":"你好"}')
    assert err is None
    assert isinstance(ev, TextEvent)
    assert ev.delta == "你好"


def test_invalid():
    for bad in ['{"type":"text"}', '{"type":"unknown"}', "not json"]:
        ev, err = parse_event(bad)
        assert ev is None
        assert err
```

- pytest 会自动收集 `tests/` 下 `test_` 开头的文件里 `test_` 开头的函数，= Go 的 `TestXxx`。
- `assert 条件`：条件为假则测试失败，= `if !cond { t.Fatal() }`。pytest 会把失败时两边的值都打印出来，不需要写 `expect(...).toBe(...)`。
- `isinstance(ev, TextEvent)`：检查类型，= Go 的类型断言 `_, ok := ev.(TextEvent)`。
- `assert err`：空字符串和 `None` 都算假，这里断言“有错误文本”。

### 练习 2、3：并行工具（`src/py_agent_core/tools.py` 见第 4 节，测试 `tests/test_tools.py`）

```python
import asyncio
import time

import pytest

from py_agent_core.tools import ToolCall, run_tools


def fake_tool(seconds: float):
    async def tool(_args: str) -> str:
        await asyncio.sleep(seconds)
        return f"done {seconds}"

    return tool


async def test_slow_tool_times_out():
    start = time.monotonic()
    res = await run_tools(
        [ToolCall("a", "fast", "{}"), ToolCall("b", "mid", "{}"), ToolCall("c", "slow", "{}")],
        {"fast": fake_tool(0.05), "mid": fake_tool(0.1), "slow": fake_tool(5)},
        per_tool_s=0.2,
    )
    assert [r.is_error for r in res] == [False, False, True]
    assert time.monotonic() - start < 0.4


async def test_unknown_tool():
    res = await run_tools([ToolCall("x", "nope", "{}")], {})
    assert res[0].is_error


async def test_outer_cancel_propagates():
    with pytest.raises(TimeoutError):
        async with asyncio.timeout(0.05):
            await run_tools([ToolCall("a", "slow", "{}")], {"slow": fake_tool(5)})
```

逐行解读：

- `def fake_tool(seconds)`：普通函数，**返回一个协程函数**（工厂），和 TS 版 `fakeTool(ms)` 一样。内层 `async def tool(...)` 定义后直接 `return tool`。
- `time.monotonic()`：单调时钟，专门用来测耗时，不受系统改时间影响，= Go 的 `time.Since` 底层用的单调时钟。
- `ToolCall("a", "fast", "{}")`：dataclass 按位置传参。
- `per_tool_s=0.2`：关键字参数，跳过 `limit` 用默认值。
- 为什么约 0.2 秒就结束：`slow` 在 `asyncio.timeout(0.2)` 到期时，正卡在 `await asyncio.sleep(5)`，那里被注入取消，`timeout` 把它转成 `TimeoutError`，被 `except TimeoutError` 记成错误结果。**工具里什么取消逻辑都没写。** 对比 TS 版，`sleep` 要自己监听 `signal` 才能做到。
- `test_outer_cancel_propagates`：
  - `with pytest.raises(TimeoutError):`：断言块里会抛出 `TimeoutError`，= `expect(...).rejects.toThrow()`。
  - 外层 `asyncio.timeout(0.05)` 到期 → 取消 `run_tools` → TaskGroup 里的任务在 `await` 处收到 `CancelledError` → 它不是 `Exception`，`run_one` 里两个 `except` 都不接 → TaskGroup 取消其他任务并向外抛 → 外层 `timeout` 把它转成 `TimeoutError`。
  - 注意：这里外层被取消的是 `run_tools` 所在的任务本身，TaskGroup 退出时会把这个取消继续往外抛，所以即使子任务吞了 `CancelledError`，这个测试也能过。吞取消的真正危害在于**被吞的那个任务会继续往下执行**，比如继续调用下一个工具、继续计费。所以规则不变：不要用 `except BaseException` 或裸 `except`。

### 练习 4：服务端感知断开（`src/py_agent_core/sse.py` 见第 5 节，测试 `tests/test_sse.py`）

```python
import asyncio
from contextlib import aclosing

import httpx

from py_agent_core.sse import sse_events


async def test_client_break_closes_server():
    closed = asyncio.Event()

    async def handle(reader: asyncio.StreamReader, writer: asyncio.StreamWriter):
        await reader.readuntil(b"\r\n\r\n")  # 读掉请求头
        writer.write(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n")
        n = 0
        try:
            while True:
                writer.write(f'data: {{"type":"text","delta":"t{n}"}}\n\n'.encode())
                await writer.drain()  # 客户端断开后这里会抛 ConnectionResetError
                n += 1
                await asyncio.sleep(0.02)
        except (ConnectionResetError, BrokenPipeError):
            closed.set()
        finally:
            writer.close()

    server = await asyncio.start_server(handle, "127.0.0.1", 0)
    port = server.sockets[0].getsockname()[1]

    count = 0
    async with httpx.AsyncClient(trust_env=False) as client:
        async with aclosing(sse_events(client, f"http://127.0.0.1:{port}")) as events:
            async for _ in events:
                count += 1
                if count == 3:
                    break

    await asyncio.wait_for(closed.wait(), timeout=2)
    assert count == 3
    server.close()
```

逐行解读：

**服务端**

- `closed = asyncio.Event()`：一次性信号，`closed.set()` 触发，`await closed.wait()` 等待。= Go 的 `closed := make(chan struct{})` + `close(closed)` + `<-closed`，也是 TS 版里“把 resolve 拿到外面”的那个写法的标准库版本。
- `async def handle(reader, writer)`：每来一个 TCP 连接，`asyncio.start_server` 就调用一次它。`reader` / `writer` 是这条连接的读写两端，比 HTTP 框架更底层，这里手写 HTTP 响应。
- `await reader.readuntil(b"\r\n\r\n")`：读到请求头结束（HTTP 头以空行结尾）。`b"..."` 是**字节串**（= Go 的 `[]byte("...")`），网络读写用字节，不是字符串。
- `writer.write(b"HTTP/1.1 200 OK\r\n...")`：手写响应状态行和头。
- `f'data: {{"type":"text","delta":"t{n}"}}\n\n'.encode()`：f-string 里 `{{` `}}` 表示字面量的花括号，`{n}` 才是插值；`.encode()` 把字符串转成 UTF-8 字节。
- `await writer.drain()`：等待数据真正发出去。**客户端断开后，这里会抛 `ConnectionResetError` 或 `BrokenPipeError`**，这就是服务端“感知断开”的方式，= Go 里 `r.Context().Done()`、Node 里 `res.on("close")`。
- `finally: writer.close()`：无论怎样都关闭连接。
- `asyncio.start_server(handle, "127.0.0.1", 0)`：端口 0 = 系统随机分配。`server.sockets[0].getsockname()[1]` 取实际端口。

**客户端**

- `httpx.AsyncClient(trust_env=False)`：`trust_env=False` 表示不读取环境变量里的代理设置。如果你的机器设置了 `HTTP_PROXY`，访问 `127.0.0.1` 也可能被转到代理上，测试会莫名其妙 404。测试本地服务时建议总是加上。
- `async with aclosing(sse_events(...)) as events:`：见第 5 节，`break` 后立刻关闭生成器。
- `break`：读到第 3 条退出。退出 `aclosing` → 生成器 `aclose()` → 生成器里 `async with client.stream` 退出 → 连接关闭 → 服务端 `drain()` 抛错 → `closed.set()`。

**断言**

- `await asyncio.wait_for(closed.wait(), timeout=2)`：最多等 2 秒，等不到就抛 `TimeoutError`，测试失败。`wait_for` 是 `asyncio.timeout` 的老写法，作用相同。
- 和前两课一样，这个测试验证的是最值钱的那条链：**客户端不要了 → 上游连接真的断了 → 停止计费。**

> 说明：这个测试里，即使不用 `aclosing`，离开外层 `async with httpx.AsyncClient()` 时连接池关闭，连接也会断，所以测试同样能过。但在真实服务里，`AsyncClient` 通常是全局复用、不会关闭的，那时没有 `aclosing` 连接就会一直挂着。养成习惯比通过测试更重要。

### 练习 5

```bash
uv run pytest -q
# ......                                                    [100%]
# 6 passed in 0.51s
```

全部通过，这一课就算完成，可以在 ROADMAP 里勾上 Python。
