from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Any, Literal

Level = Literal["debug", "info", "warn", "error"]
Action = Literal["request", "run", "call", "query"]
Phase = Literal["START", "DONE", "SLOW", "FAILED", "ABORTED"]


@dataclass(frozen=True)
class ErrorInfo:
    message: str
    name: str | None = None
    stack: str | None = None


@dataclass(frozen=True)
class SpanEvent:
    """One line before rendering. Fields follow docs/log-format.md."""

    time: datetime
    service: str
    action: Action
    phase: Phase
    req_id: str | None = None
    scope: str | None = None
    method: str | None = None
    target: str | None = None
    same_scope: bool = False
    duration_ms: float | None = None
    status: int | None = None
    op: str | None = None
    tables: list[str] | None = None
    err: ErrorInfo | None = None
    first_report: bool = False
    params: dict[str, Any] | None = None
    summary: dict[str, Any] | None = None


@dataclass(frozen=True)
class Rendered:
    time: datetime
    level: Level
    msg: str
    fields: dict[str, Any]
    lines: list[str]
