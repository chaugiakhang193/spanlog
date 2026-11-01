from __future__ import annotations

import logging
import sys
import threading
from collections.abc import Callable
from datetime import UTC
from typing import Protocol

from ._model import Level, Rendered
from ._render import plain_json

TextWriter = Callable[[str], object]


class SpanSink(Protocol):
    """Receives every rendered event that passes the level filter."""

    def write(self, rendered: Rendered) -> None: ...


# sys.stdout is read at each write. A test runner or the application can replace it.
def _write_stdout(text: str) -> None:
    sys.stdout.write(text)


class _TextSink:
    def __init__(self, write_text: TextWriter, format_text: Callable[[Rendered], str]) -> None:
        self._write_text = write_text
        self._format_text = format_text
        self._lock = threading.Lock()

    # The lock keeps the lines of worker threads from mixing inside one event.
    def write(self, rendered: Rendered) -> None:
        text = self._format_text(rendered)
        with self._lock:
            self._write_text(text)


def _pretty_text(rendered: Rendered) -> str:
    return "\n".join(rendered.lines) + "\n"


def _json_text(rendered: Rendered) -> str:
    utc = rendered.time.astimezone(UTC)
    record = {
        "time": f"{utc.strftime('%Y-%m-%dT%H:%M:%S')}.{utc.microsecond // 1000:03d}Z",
        "level": rendered.level,
        **rendered.fields,
        "msg": rendered.msg,
    }
    return plain_json(record) + "\n"


def pretty_sink(write_text: TextWriter = _write_stdout) -> SpanSink:
    """Write the readable lines, one event per write."""
    return _TextSink(write_text, _pretty_text)


def json_sink(write_text: TextWriter = _write_stdout) -> SpanSink:
    """Write one JSON object per line: time in UTC, level, the fields, then msg."""
    return _TextSink(write_text, _json_text)


_LOGGING_LEVELS: dict[Level, int] = {
    "debug": logging.DEBUG,
    "info": logging.INFO,
    "warn": logging.WARNING,
    "error": logging.ERROR,
}


class _LoggingSink:
    def __init__(self, logger: logging.Logger) -> None:
        self._logger = logger

    def write(self, rendered: Rendered) -> None:
        self._logger.log(_LOGGING_LEVELS[rendered.level], rendered.msg, extra=rendered.fields)


def logging_sink(logger: logging.Logger) -> SpanSink:
    """Hand the message and the fields to a standard library logger.

    The fields arrive as attributes of the LogRecord. Time, level and message encoding
    follow the handlers of that logger.
    """
    return _LoggingSink(logger)
