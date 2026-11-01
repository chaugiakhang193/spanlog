from __future__ import annotations

import dataclasses
import enum
from collections.abc import Iterable, Mapping
from datetime import date, datetime, time
from decimal import Decimal
from pathlib import PurePath
from typing import Any
from uuid import UUID

REDACTED = "[REDACTED]"
CIRCULAR = "[CIRCULAR]"
TRUNCATED = "[TRUNCATED]"

DEFAULT_REDACT_KEYS = (
    "password",
    "token",
    "accessToken",
    "refreshToken",
    "apiKey",
    "secret",
    "authorization",
    "cookie",
    "email",
    "phone",
)

DEFAULT_MAX_DEPTH = 8


def redact(value: Any, keys: Iterable[str] = (), max_depth: int | None = None) -> Any:
    """Return a plain copy of value with every sensitive key replaced by REDACTED.

    Keys match without regard to case, at any depth. Only keys are matched. Sensitive
    text inside a string value is left as it is. The result holds only dicts, lists,
    strings, numbers, booleans and None. Sets, tuples, dataclasses, objects with
    model_dump, dates, Decimal, UUID, paths and enums are read. bytes becomes
    "[BYTES {length}]". Any other object becomes "[OBJECT {type}]", because its repr can
    hold any value.
    """
    sensitive = {key.lower() for key in (*DEFAULT_REDACT_KEYS, *keys)}
    depth = max_depth if max_depth is not None and max_depth >= 0 else DEFAULT_MAX_DEPTH
    return _redact_value(value, sensitive, depth, set())


# ancestors holds the ids of the containers on the path from the root to the current
# value. A container already on that path is a cycle. The same container reached
# through two sibling keys is copied twice.
def _redact_value(value: Any, sensitive: set[str], depth_left: int, ancestors: set[int]) -> Any:
    if value is None or isinstance(value, (str, bool, int, float)):
        return value
    if isinstance(value, enum.Enum):
        return _redact_value(value.value, sensitive, depth_left, ancestors)
    if isinstance(value, (datetime, date, time)):
        return value.isoformat()
    if isinstance(value, (Decimal, UUID, PurePath)):
        return str(value)
    if isinstance(value, (bytes, bytearray, memoryview)):
        return f"[BYTES {len(value)}]"
    contents = _contents_of(value)
    if contents is None:
        return f"[OBJECT {type(value).__name__}]"
    if id(value) in ancestors:
        return CIRCULAR
    if depth_left == 0:
        return TRUNCATED
    ancestors.add(id(value))
    try:
        if isinstance(contents, Mapping):
            return {
                str(key): REDACTED
                if str(key).lower() in sensitive
                else _redact_value(inner, sensitive, depth_left - 1, ancestors)
                for key, inner in contents.items()
            }
        return [_redact_value(item, sensitive, depth_left - 1, ancestors) for item in contents]
    finally:
        ancestors.discard(id(value))


def _contents_of(value: Any) -> Mapping[Any, Any] | Iterable[Any] | None:
    if isinstance(value, Mapping):
        return value
    if isinstance(value, (list, tuple)):
        return value
    if isinstance(value, (set, frozenset)):
        # A set has no order of its own, and the line must not change between runs.
        return sorted(value, key=repr)
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        return {field.name: getattr(value, field.name) for field in dataclasses.fields(value)}
    # A Pydantic model, read without a dependency on Pydantic.
    model_dump = getattr(type(value), "model_dump", None)
    if callable(model_dump):
        dumped: Mapping[Any, Any] = model_dump(value)
        return dumped
    return None
