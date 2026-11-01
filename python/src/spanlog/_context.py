from __future__ import annotations

import threading
from contextvars import ContextVar
from dataclasses import dataclass, field


@dataclass(frozen=True)
class Frame:
    """The wrapped method that is running."""

    scope: str
    method: str


@dataclass
class RequestScope:
    """Shared by every line written while one request runs.

    reported keeps the errors already reported, by identity. The scope holds them until
    it ends. Built-in exceptions such as ValueError refuse a weak reference.
    """

    req_id: str | None
    reported: dict[int, BaseException] = field(default_factory=dict)
    lock: threading.Lock = field(default_factory=threading.Lock)

    def mark_reported(self, failure: BaseException) -> bool:
        """Mark failure as reported. Return True when it was not marked before."""
        with self.lock:
            if id(failure) in self.reported:
                return False
            self.reported[id(failure)] = failure
            return True


@dataclass(frozen=True)
class SpanContext:
    request: RequestScope
    frame: Frame | None


# Each wrapped call sets a new SpanContext. asyncio copies the context into each task
# and FastAPI into each worker thread. Two calls side by side see their own frame and
# share one RequestScope.
current_context: ContextVar[SpanContext | None] = ContextVar("spanlog_context", default=None)
