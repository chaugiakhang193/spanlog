from __future__ import annotations

import json
import math
import re
from collections.abc import Callable
from datetime import datetime, timedelta, timezone
from decimal import Decimal
from typing import Any

from ._model import ErrorInfo, Level, Rendered, SpanEvent

StackFilter = Callable[[list[str]], list[str]]

_LEVEL_WIDTH = 5
_STACK_INDENT = "    "
_LONE_SURROGATE = re.compile("[\ud800-\udfff]")
_SHORT_EXPONENT = re.compile(r"e-0(\d)$")


def render(
    event: SpanEvent, utc_offset_minutes: int, stack_filter: StackFilter | None = None
) -> Rendered:
    """Turn an event into its level, message, fields and readable lines.

    A pure function. spec/cases.json pins its output.
    """
    level = level_for(event)
    frame = _frame_of(event)
    tail = _tail_of(event)
    return Rendered(
        time=event.time,
        level=level,
        msg=tail if frame is None else f"{frame} - {tail}",
        fields=_fields_of(event),
        lines=[
            _readable_line(event, level, frame, tail, utc_offset_minutes),
            *_stack_lines(event, stack_filter),
        ],
    )


def level_for(event: SpanEvent) -> Level:
    if event.phase == "DONE":
        if event.action == "query" and event.op == "ROLLBACK":
            return "warn"
        return _base_level(event)
    if event.phase in ("SLOW", "ABORTED"):
        return "warn"
    if event.phase == "FAILED":
        if event.action in ("request", "query"):
            return "warn"
        return "error" if event.first_report else _base_level(event)
    return _base_level(event)


def _base_level(event: SpanEvent) -> Level:
    if event.action == "query":
        return "debug"
    if event.action == "call" and event.same_scope:
        return "debug"
    return "info"


def _frame_of(event: SpanEvent) -> str | None:
    if not event.scope or not event.method:
        return None
    return f"{event.scope}.{event.method}"


# _tail_of is the part that msg and the readable line share: body, phase and detail.
def _tail_of(event: SpanEvent) -> str:
    body = _body_of(event)
    detail = _detail_of(event)
    if detail == "":
        return f"{body} | {event.phase}"
    return f"{body} | {event.phase} {detail}"


def _body_of(event: SpanEvent) -> str:
    target = event.target or ""
    if event.action == "run":
        return "run"
    if event.action == "request":
        return target
    if event.action == "query" and not target:
        # A statement without a keyword has no target, and the body is the action alone.
        return "query"
    return f"{event.action} {target}"


def _detail_of(event: SpanEvent) -> str:
    ms = "" if event.duration_ms is None else f"{js_number(round_ms(event.duration_ms))}ms"
    if event.action == "request":
        if event.phase == "START":
            return ""
        if event.phase == "ABORTED":
            return ms
        return f"{'-' if event.status is None else event.status} {ms}"
    if event.phase == "START":
        return "" if event.params is None else stable_json(event.params)
    if event.phase in ("DONE", "SLOW"):
        return ms if event.summary is None else f"{ms} {stable_json(event.summary)}"
    if event.phase == "FAILED" and event.err is not None:
        return f"{ms} {_error_text(event.err)}"
    return ms


def _error_text(err: ErrorInfo) -> str:
    return f"{err.name}: {err.message}" if err.name else err.message


def _fields_of(event: SpanEvent) -> dict[str, Any]:
    fields: dict[str, Any] = {"service": event.service}
    if event.req_id:
        fields["reqId"] = event.req_id
    if event.scope:
        fields["scope"] = event.scope
    if event.method:
        fields["method"] = event.method
    fields["action"] = event.action
    if event.target:
        fields["target"] = event.target
    fields["phase"] = event.phase
    if event.duration_ms is not None:
        fields["ms"] = round_ms(event.duration_ms)
    if event.status is not None:
        fields["status"] = event.status
    if event.op:
        fields["op"] = event.op
    if event.tables is not None:
        fields["tables"] = event.tables
    if event.err is not None:
        fields["err"] = _error_fields(event, event.err)
    if event.params is not None:
        fields["params"] = event.params
    if event.summary is not None:
        fields["summary"] = event.summary
    return fields


def _error_fields(event: SpanEvent, err: ErrorInfo) -> dict[str, str]:
    fields = {"name": err.name, "message": err.message} if err.name else {"message": err.message}
    if _is_first_report_on_frame_line(event) and err.stack is not None:
        fields["stack"] = err.stack
    return fields


# Only the first report of an error on a frame line carries the stack. Frames further
# out and failed SQL statements repeat the name and message only.
def _is_first_report_on_frame_line(event: SpanEvent) -> bool:
    return event.first_report and event.action in ("run", "call")


def _readable_line(
    event: SpanEvent, level: Level, frame: str | None, tail: str, utc_offset_minutes: int
) -> str:
    owner = f"[{event.service}]" if frame is None else f"[{event.service}] [{frame}]"
    label = level.upper().ljust(_LEVEL_WIDTH)
    time_text = format_time(event.time, utc_offset_minutes)
    return f"{time_text} {label} {owner} req={event.req_id or '-'} {tail}"


# The first line of a stack repeats name and message, which the main line shows.
def _stack_lines(event: SpanEvent, stack_filter: StackFilter | None) -> list[str]:
    stack = None if event.err is None else event.err.stack
    if stack is None or not _is_first_report_on_frame_line(event):
        return []
    lines = [line.strip() for line in stack.split("\n")[1:]]
    lines = [line for line in lines if line != ""]
    shown = lines if stack_filter is None else stack_filter(lines)
    return [f"{_STACK_INDENT}{line}" for line in shown]


def format_time(time: datetime, utc_offset_minutes: int) -> str:
    shifted = time.astimezone(timezone(timedelta(minutes=utc_offset_minutes)))
    clock = shifted.strftime("%Y-%m-%d %H:%M:%S")
    return f"{clock}.{shifted.microsecond // 1000:03d} {_format_offset(utc_offset_minutes)}"


def _format_offset(minutes: int) -> str:
    sign = "-" if minutes < 0 else "+"
    hours, rest = divmod(abs(minutes), 60)
    return f"{sign}{hours:02d}:{rest:02d}"


def round_ms(ms: float) -> int | float:
    """Round a half up, as Math.round does in the Node.js package.

    round() in Python rounds a half to even. A whole result is an int, which the JSON
    encoder writes without ".0".
    """
    if not math.isfinite(ms):
        return ms
    tenths = ms * 10
    rounded = math.floor(tenths)
    if tenths - rounded >= 0.5:
        rounded += 1
    if rounded % 10 == 0:
        return rounded // 10
    return rounded / 10


def js_number(value: int | float) -> str:
    """Write a number as JSON.stringify does in the Node.js package.

    Python and JavaScript both pick the shortest digits of a float. They differ in
    where the exponent starts and how it is written. A float that is not finite is
    null in JSON.
    """
    if isinstance(value, int):
        return str(value)
    if not math.isfinite(value):
        return "null"
    if value == 0:
        return "0"
    magnitude = abs(value)
    if 1e-6 <= magnitude < 1e21:
        text = format(Decimal(repr(value)), "f")
        return text[:-2] if text.endswith(".0") else text
    return _SHORT_EXPONENT.sub(r"e-\1", repr(value))


def stable_json(value: Any) -> str:
    """Encode a value as compact JSON with object keys sorted at every level."""
    return _encode(value, sort_keys=True)


def plain_json(value: Any) -> str:
    """Encode a value as compact JSON with object keys in insertion order."""
    return _encode(value, sort_keys=False)


def _encode(value: Any, sort_keys: bool) -> str:
    if value is None:
        return "null"
    if value is True:
        return "true"
    if value is False:
        return "false"
    if isinstance(value, (int, float)):
        return js_number(value)
    if isinstance(value, str):
        return _encode_string(value)
    if isinstance(value, (list, tuple)):
        return "[" + ",".join(_encode(item, sort_keys) for item in value) + "]"
    if isinstance(value, dict):
        keys = sorted(value, key=_utf16_key) if sort_keys else list(value)
        parts = [f"{_encode_string(key)}:{_encode(value[key], sort_keys)}" for key in keys]
        return "{" + ",".join(parts) + "}"
    return _encode_string(str(value))


# sort() in the Node.js package compares UTF-16 code units. Python compares code
# points, which differ above U+FFFF.
def _utf16_key(key: str) -> bytes:
    return key.encode("utf-16-be", "surrogatepass")


# JSON.stringify escapes a lone surrogate, and such text cannot be encoded as UTF-8.
def _encode_string(text: str) -> str:
    encoded = json.dumps(text, ensure_ascii=False)
    return _LONE_SURROGATE.sub(lambda found: f"\\u{ord(found.group()):04x}", encoded)
