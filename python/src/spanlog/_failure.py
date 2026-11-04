from __future__ import annotations

import os
from collections.abc import Callable
from types import TracebackType

from ._model import ErrorInfo

_UNREADABLE = ErrorInfo(name="UnreadableError", message="[unreadable thrown value]")
_MAX_STACK_FRAMES = 32
_OWN_DIRECTORY = os.path.dirname(os.path.abspath(__file__)) + os.sep


def describe_thrown(value: object) -> str:
    """Return str(value), or a fixed text when str() raises."""
    try:
        return str(value)
    except Exception:
        return "[unprintable value]"


def error_info(
    failure: BaseException, error_message: Callable[[BaseException], str] | None
) -> ErrorInfo:
    """Read a failure into its name, message and stack.

    str() and error_message run code of the application. When either raises, the
    unreadable description stands in.
    """
    try:
        name = type(failure).__name__
        message = _message_of(failure) if error_message is None else error_message(failure)
        return ErrorInfo(name=name, message=message, stack=_stack_text(name, message, failure))
    except Exception:
        return _UNREADABLE


# str() of a SQLAlchemy StatementError holds the SQL text and the parameters, and
# hide_parameters=True still leaves the SQL text. SQLAlchemy appends both after the text of
# the driver error. The message is the first line of that text. psycopg and asyncpg add
# later lines, such as DETAIL, that can repeat a value. Without a driver error the SQL text
# starts on the second line of str().
def _message_of(failure: BaseException) -> str:
    if not _is_statement_error(failure):
        return str(failure)
    driver_error = getattr(failure, "orig", None)
    source = failure if driver_error is None else driver_error
    return str(source).split("\n", 1)[0]


# The core package does not import SQLAlchemy. The class is matched by module and name in
# the MRO, which also covers DBAPIError and its subclasses.
def _is_statement_error(failure: BaseException) -> bool:
    return any(
        cls.__module__ == "sqlalchemy.exc" and cls.__name__ == "StatementError"
        for cls in type(failure).__mro__
    )


# A traceback lists the frames from the catching function down to the raise. The stack
# starts at the raise, as in the Node.js package. The walk reads no source file.
def _stack_text(name: str, message: str, failure: BaseException) -> str:
    first_line = message.split("\n", 1)[0]
    lines = [f"{name}: {first_line}"]
    lines.extend(f"    at {frame}" for frame in reversed(_frames(failure.__traceback__)))
    return "\n".join(lines)


def _frames(traceback: TracebackType | None) -> list[str]:
    frames: list[str] = []
    while traceback is not None:
        code = traceback.tb_frame.f_code
        frames.append(f"{code.co_qualname} ({code.co_filename}:{traceback.tb_lineno})")
        traceback = traceback.tb_next
    return frames[-_MAX_STACK_FRAMES:]


def without_own_frames(lines: list[str]) -> list[str]:
    """Drop the stack lines whose file belongs to the spanlog package."""
    return [line for line in lines if f"({_OWN_DIRECTORY}" not in line]
