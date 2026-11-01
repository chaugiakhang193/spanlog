"""WSGI middleware for Flask and other WSGI applications."""

from __future__ import annotations

import contextvars
from collections.abc import Callable, Iterable, Iterator
from types import TracebackType
from typing import Any
from urllib.parse import quote

from ._context import RequestScope, SpanContext, current_context
from ._core import RequestSpan, Spanlog
from ._http import (
    DEFAULT_REQUEST_ID_HEADER,
    SAFE_PATH_CHARACTERS,
    new_request_id,
    safe_request_id,
)

Environ = dict[str, Any]
ExcInfo = tuple[type[BaseException], BaseException, TracebackType] | None
StartResponse = Callable[..., Callable[[bytes], object]]
WsgiApp = Callable[[Environ, StartResponse], Iterable[bytes]]


class LogRequests:
    """Run each request in its own context and write its START and end lines.

    Register with `app.wsgi_app = LogRequests(app.wsgi_app, log=log)`.
    """

    def __init__(self, app: WsgiApp, log: Spanlog, header: str = DEFAULT_REQUEST_ID_HEADER) -> None:
        self._app = app
        self._log = log
        self._header = header
        self._environ_key = "HTTP_" + header.upper().replace("-", "_")

    def __call__(self, environ: Environ, start_response: StartResponse) -> Iterable[bytes]:
        req_id = safe_request_id(environ.get(self._environ_key)) or new_request_id()
        # A server reads the response after this function returns. One Context carries the
        # request through the application call and every chunk of the body.
        context = contextvars.copy_context()
        context.run(current_context.set, SpanContext(RequestScope(req_id), None))
        target = f"{environ['REQUEST_METHOD']} {_path_of(environ)}"
        span = context.run(self._log.start_request, target)
        status: list[int] = []

        def recording_start(
            status_line: str, headers: list[tuple[str, str]], exc_info: ExcInfo = None
        ) -> Callable[[bytes], object]:
            # With exc_info an application replaces the status of a response not yet sent.
            status[:] = [int(status_line.split(" ", 1)[0])]
            if not any(name.lower() == self._header.lower() for name, _ in headers):
                headers = [*headers, (self._header, req_id)]
            if exc_info is None:
                return start_response(status_line, headers)
            return start_response(status_line, headers, exc_info)

        try:
            body = context.run(self._app, environ, recording_start)
        except BaseException:
            span.end(500)
            raise
        return _LoggedBody(body, context, span, status)


class _LoggedBody:
    """Iterates the response body in the context of its request.

    PEP 3333 has the server call close, also when it stops early because the client left.
    close before the last chunk therefore means ABORTED. The iterator is created on the
    first chunk. A failure there reaches the server after it holds this object, and the
    server can still call close.
    """

    def __init__(
        self,
        body: Iterable[bytes],
        context: contextvars.Context,
        span: RequestSpan,
        status: list[int],
    ) -> None:
        self._body = body
        self._context = context
        self._span = span
        self._status = status
        self._iterator: Iterator[bytes] | None = None

    def __iter__(self) -> Iterator[bytes]:
        return self

    def __next__(self) -> bytes:
        try:
            if self._iterator is None:
                self._iterator = self._context.run(iter, self._body)
            return self._context.run(next, self._iterator)
        except StopIteration:
            self._span.end(self._status[0] if self._status else 500)
            raise
        except BaseException:
            self._span.end(500)
            raise

    def close(self) -> None:
        self._span.abort()
        close = getattr(self._body, "close", None)
        if close is not None:
            self._context.run(close)


# PATH_INFO holds decoded text, where %0A is a line break. REQUEST_URI and RAW_URI are
# what the client sent, and they are optional in WSGI.
def _path_of(environ: Environ) -> str:
    raw = environ.get("REQUEST_URI") or environ.get("RAW_URI")
    if raw is not None:
        return str(raw).split("?", 1)[0]
    decoded = environ.get("SCRIPT_NAME", "") + environ.get("PATH_INFO", "")
    return quote(decoded.encode("latin-1"), safe=SAFE_PATH_CHARACTERS)
