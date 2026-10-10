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
