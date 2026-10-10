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
