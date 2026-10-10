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
