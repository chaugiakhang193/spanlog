"""ASGI middleware for FastAPI, Starlette and other ASGI applications."""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable, MutableMapping
from typing import Any
from urllib.parse import quote

from ._core import RequestSpan, Spanlog
from ._http import (
    DEFAULT_REQUEST_ID_HEADER,
    SAFE_PATH_CHARACTERS,
    new_request_id,
    safe_request_id,
)

Scope = MutableMapping[str, Any]
Message = MutableMapping[str, Any]
Receive = Callable[[], Awaitable[Message]]
Send = Callable[[Message], Awaitable[None]]
App = Callable[[Scope, Receive, Send], Awaitable[None]]


class LogRequests:
    """Run each HTTP request inside log.with_request and write its START and end lines.

    Register with `app.add_middleware(LogRequests, log=log)`. Other scope types, such as
    websocket and lifespan, pass through untouched.
    """

    def __init__(self, app: App, log: Spanlog, header: str = DEFAULT_REQUEST_ID_HEADER) -> None:
        self._app = app
        self._log = log
        self._header = header.lower().encode("latin-1")

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self._app(scope, receive, send)
            return
        req_id = _incoming_request_id(scope, self._header) or new_request_id()
        with self._log.with_request(req_id):
            span = self._log.start_request(f"{scope['method']} {_path_of(scope)}")
            response = _ResponseState(receive, send, span, self._header, req_id)
            try:
                await self._app(scope, response.receive, response.send)
            except asyncio.CancelledError:
                span.abort()
                raise
            except BaseException:
                span.end(500)
                raise
            response.end_line()


class _ResponseState:
    def __init__(
        self, receive: Receive, send: Send, span: RequestSpan, header: bytes, req_id: str
    ) -> None:
        self._receive = receive
        self._send = send
        self._span = span
        self._header = header
        self._req_id = req_id.encode("latin-1")
        self._finished = False
        self.status: int | None = None
        self.client_left = False

    # A disconnect after the last body byte is the end of a complete response. Starlette
    # keeps listening for one while it streams.
    async def receive(self) -> Message:
        message = await self._receive()
        if message["type"] == "http.disconnect" and not self._finished:
            self.client_left = True
        return message

    # The line ends with the last body byte. Starlette runs background tasks after that
    # byte, inside the application call, and they are not part of the response.
    async def send(self, message: Message) -> None:
        is_last = message["type"] == "http.response.body" and not message.get("more_body", False)
        if message["type"] == "http.response.start":
            self.status = message["status"]
            headers = list(message.get("headers", ()))
            if not any(name.lower() == self._header for name, _ in headers):
                headers.append((self._header, self._req_id))
            message = {**message, "headers": headers}
        elif is_last:
            if not self.client_left:
                self.client_left = await self._disconnect_is_waiting()
            self._finished = True
        await self._send(message)
        if is_last:
            self.end_line()

    def end_line(self) -> None:
        if self.client_left:
            self._span.abort()
        elif self.status is None:
            # A server answers 500 to an application that returns without a response.
            self._span.end(500)
        else:
            self._span.end(self.status)

    # A handler that never reads the request gets no disconnect message, and a send to a
    # closed connection does nothing under uvicorn. One step of the event loop lets
    # receive return a disconnect that already happened. The request body it can take
    # instead has no reader left at this point. A server on another event loop, such as
    # trio, skips this step. The cancelled receive is not awaited, because a server that
    # ignores the cancel would hold the response back.
    async def _disconnect_is_waiting(self) -> bool:
        try:
            asyncio.get_running_loop()
        except RuntimeError:
            return False
        waiting = asyncio.ensure_future(self._receive())
        try:
            await asyncio.sleep(0)
            if not waiting.done() or waiting.cancelled() or waiting.exception() is not None:
                return False
            return bool(waiting.result()["type"] == "http.disconnect")
        finally:
            waiting.add_done_callback(_retrieve_outcome)
            waiting.cancel()


# asyncio reports a task failure that nothing read. A receive that fails while it is
# cancelled has no reader left, and the response is already decided.
def _retrieve_outcome(task: asyncio.Future[Message]) -> None:
    if not task.cancelled():
        task.exception()


def _incoming_request_id(scope: Scope, header: bytes) -> str | None:
    values = [value for name, value in scope.get("headers", ()) if name.lower() == header]
    if len(values) != 1:
        return None
    return safe_request_id(values[0].decode("latin-1"))


# scope["path"] holds decoded text, where %0A is a line break. raw_path keeps the target
# on one line and leaves out the query string. It is optional in the ASGI specification.
def _path_of(scope: Scope) -> str:
    raw_path = scope.get("raw_path")
    if raw_path is not None:
        return str(raw_path.decode("latin-1")).split("?", 1)[0]
    return quote(scope.get("path", ""), safe=SAFE_PATH_CHARACTERS)
