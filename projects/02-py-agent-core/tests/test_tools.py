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
