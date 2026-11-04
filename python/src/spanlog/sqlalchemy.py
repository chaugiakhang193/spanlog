"""Query lines for SQLAlchemy. See docs/python.md."""

from __future__ import annotations

import threading
import weakref
from collections.abc import Callable
from typing import Any

from sqlalchemy import event
from sqlalchemy.engine import Connection, Engine, ExceptionContext
from sqlalchemy.engine.interfaces import DBAPICursor, ExecutionContext

from ._core import QuerySpan, Spanlog


class _RunningQueries:
    """Carries a span from the event before a statement to the event after it.

    ExecutionContext identifies one statement, and two connections on two threads share
    this object. A weak key lets a statement that never reached an end event go away.
    """

    def __init__(self, log: Spanlog) -> None:
        self._log = log
        self._spans: weakref.WeakKeyDictionary[ExecutionContext, QuerySpan] = (
            weakref.WeakKeyDictionary()
        )
        self._lock = threading.Lock()

    def _take(self, context: ExecutionContext) -> QuerySpan | None:
        with self._lock:
            return self._spans.pop(context, None)

    def before(
        self,
        conn: Connection,
        cursor: DBAPICursor,
        statement: str,
        parameters: Any,
        context: ExecutionContext,
        executemany: bool,
    ) -> None:
        span = self._log.start_query(statement)
        with self._lock:
            self._spans[context] = span

    def after(
        self,
        conn: Connection,
        cursor: DBAPICursor,
        statement: str,
        parameters: Any,
        context: ExecutionContext,
        executemany: bool,
    ) -> None:
        span = self._take(context)
        if span is not None:
            span.end()

    # execution_context is None when the connection itself fails, before any statement.
    # The exception of SQLAlchemy names the same class as the frame that receives it.
    def failed(self, exception_context: ExceptionContext) -> None:
        context = exception_context.execution_context
        span = None if context is None else self._take(context)
        if span is not None:
            span.fail(
                exception_context.sqlalchemy_exception or exception_context.original_exception
            )


def log_queries(engine: Any, log: Spanlog) -> Callable[[], None]:
    """Write a START line before each SQL statement and an end line after it.

    engine is an Engine or an AsyncEngine. The lines belong to the frame that runs the
    statement. It returns a function that removes the listeners, which must not run while
    a statement of the engine is running. A statement running at that moment keeps its
    START line without an end line.
    """
    target = getattr(engine, "sync_engine", engine)
    if not isinstance(target, Engine):
        raise TypeError(
            f"spanlog: log_queries needs an Engine or an AsyncEngine, and got "
            f"{type(engine).__name__}. Pass the result of create_engine or create_async_engine."
        )
    running = _RunningQueries(log)
    event.listen(target, "before_cursor_execute", running.before)
    event.listen(target, "after_cursor_execute", running.after)
    event.listen(target, "handle_error", running.failed)

    def detach() -> None:
        event.remove(target, "before_cursor_execute", running.before)
        event.remove(target, "after_cursor_execute", running.after)
        event.remove(target, "handle_error", running.failed)

    return detach
