#!/usr/bin/env python3
"""Local imitation of proxy nodes for mihomo url-test / health-check scenarios.

* every listening port is one SOCKS5 "node" with its own, runtime adjustable
  behaviour: response latency, CONNECT delay and mode
  (ok | hang | close | down);
* an HTTP server that answers 204 to every request (keep-alive), used as the
  health-check target and as the target for test traffic;
* a control API: POST /ports {"<port>": {...}, ...} merges settings,
  GET /stats returns the number of accepted connections per port.

Only the standard library is used.
"""
import asyncio
import json
import socket
import sys
from collections import defaultdict

PORTS = {}            # port -> {"latency": ms, "connect_delay": ms, "mode": str}
SERVERS = {}          # port -> asyncio.Server
STATS = defaultdict(int)
DEFAULT = {"latency": 0, "connect_delay": 0, "mode": "ok"}


async def pipe(reader, writer, latency_of=None):
    try:
        while True:
            data = await reader.read(65536)
            if not data:
                break
            if latency_of is not None:
                ms = latency_of()
                if ms:
                    await asyncio.sleep(ms / 1000)
            writer.write(data)
            await writer.drain()
    except (ConnectionError, asyncio.IncompleteReadError, OSError):
        pass
    finally:
        try:
            writer.close()
        except Exception:
            pass


async def handle_socks(reader, writer, port):
    STATS[str(port)] += 1
    cfg = PORTS[port]
    try:
        if cfg["mode"] == "close":
            writer.close()
            return
        if cfg["mode"] == "hang":  # accepts TCP, never answers
            await asyncio.sleep(3600)
            return
        head = await reader.readexactly(2)
        await reader.readexactly(head[1])
        writer.write(b"\x05\x00")
        await writer.drain()
        req = await reader.readexactly(4)
        atyp = req[3]
        if atyp == 1:
            host = socket.inet_ntoa(await reader.readexactly(4))
        elif atyp == 3:
            n = (await reader.readexactly(1))[0]
            host = (await reader.readexactly(n)).decode()
        else:
            host = socket.inet_ntop(socket.AF_INET6, await reader.readexactly(16))
        dport = int.from_bytes(await reader.readexactly(2), "big")
        if cfg["connect_delay"]:
            await asyncio.sleep(cfg["connect_delay"] / 1000)
        try:
            tr, tw = await asyncio.open_connection(host, dport)
        except OSError:
            writer.write(b"\x05\x05\x00\x01" + bytes(6))
            await writer.drain()
            writer.close()
            return
        writer.write(b"\x05\x00\x00\x01" + bytes(6))
        await writer.drain()
        await asyncio.gather(
            pipe(reader, tw),
            pipe(tr, writer, lambda: PORTS[port]["latency"]),
        )
    except (ConnectionError, asyncio.IncompleteReadError, OSError):
        try:
            writer.close()
        except Exception:
            pass


async def ensure_server(port):
    cfg = PORTS[port]
    if cfg["mode"] == "down":
        srv = SERVERS.pop(port, None)
        if srv is not None:
            srv.close()
        return
    if port not in SERVERS:
        SERVERS[port] = await asyncio.start_server(
            lambda r, w, p=port: handle_socks(r, w, p), "127.0.0.1", port, reuse_address=True)


async def apply(update):
    for port, cfg in update.items():
        port = int(port)
        PORTS.setdefault(port, dict(DEFAULT)).update(cfg)
        await ensure_server(port)


async def handle_target(reader, writer):
    try:
        while True:
            buf = b""
            while b"\r\n\r\n" not in buf:
                chunk = await reader.read(4096)
                if not chunk:
                    writer.close()
                    return
                buf += chunk
            writer.write(b"HTTP/1.1 204 No Content\r\nConnection: keep-alive\r\n\r\n")
            await writer.drain()
    except (ConnectionError, OSError):
        pass


async def handle_control(reader, writer):
    try:
        head = b""
        while b"\r\n\r\n" not in head:
            head += await reader.read(4096)
        header, _, body = head.partition(b"\r\n\r\n")
        lines = header.decode().split("\r\n")
        method, path, _ = lines[0].split(" ", 2)
        length = 0
        for line in lines[1:]:
            if line.lower().startswith("content-length:"):
                length = int(line.split(":", 1)[1])
        while len(body) < length:
            body += await reader.read(4096)
        if method == "POST" and path == "/ports":
            await apply(json.loads(body or b"{}"))
            out = {"ok": True}
        elif path == "/stats":
            out = dict(STATS)
        else:
            out = {"ports": {str(k): v for k, v in PORTS.items()}}
        data = json.dumps(out).encode()
        writer.write(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n" % len(data) + data)
        await writer.drain()
    finally:
        writer.close()


async def main():
    target_port, control_port = int(sys.argv[1]), int(sys.argv[2])
    initial = json.loads(sys.argv[3]) if len(sys.argv) > 3 else {}
    await asyncio.start_server(handle_target, "127.0.0.1", target_port, reuse_address=True)
    await asyncio.start_server(handle_control, "127.0.0.1", control_port, reuse_address=True)
    await apply(initial)
    print("sim ready", flush=True)
    await asyncio.Event().wait()


if __name__ == "__main__":
    asyncio.run(main())
