# 02-py-agent-core · 0.3 Python 练习

对应课件：[`notes/00-foundations/03-python.md`](../../notes/00-foundations/03-python.md)

| 文件 | 内容 |
|---|---|
| `src/py_agent_core/events.py` | Pydantic 可辨识联合解析流式事件 |
| `src/py_agent_core/tools.py` | asyncio TaskGroup 并行工具调用、单工具超时、取消传播 |
| `src/py_agent_core/sse.py` | 异步生成器读 SSE，服务端感知客户端断开 |
| `tests/` | 6 个 pytest 用例 |

```bash
uv sync
uv run pytest -q
```
