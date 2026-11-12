"""Sinks, services and HTTP helpers shared by the ASGI and WSGI tests."""

import asyncio
import socket
import time


class ListSink:
    def __init__(self):
        self.lines = []

    def write(self, rendered):
        self.lines.append(rendered.msg + (" req=" + rendered.fields.get("reqId", "-")))


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def fetch(port, path, method="GET", body=b"", headers="", leave_after=None):
    with socket.create_connection(("127.0.0.1", port), timeout=10) as sock:
        head = f"{method} {path} HTTP/1.1\r\nHost: x\r\nConnection: close\r\n"
        head += f"Content-Length: {len(body)}\r\n{headers}\r\n"
        sock.sendall(head.encode() + body)
        if leave_after is not None:
            time.sleep(leave_after)
            return None
        data = b""
        while chunk := sock.recv(65536):
            data += chunk
        return data


def wait_for(predicate, timeout=5.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.02)
    return False


class Inventory:
    async def reserve(self):
        await asyncio.sleep(0)
        raise ValueError("stock not available")

    def reserve_sync(self):
        raise ValueError("stock not available")


class Orders:
    def __init__(self, inventory):
        self.inventory = inventory

    async def handle(self):
        return await self.inventory.reserve()

    def handle_sync(self):
        return self.inventory.reserve_sync()


def lines_end(sink, text):
    return wait_for(lambda: any(text in line for line in sink.lines))
