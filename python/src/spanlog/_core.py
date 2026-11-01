from __future__ import annotations

import functools
import inspect
import time
import warnings
from collections.abc import Awaitable, Callable, Iterable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass, replace
from datetime import UTC, datetime
from typing import Any, TypeVar, cast

from ._context import Frame, RequestScope, SpanContext, current_context
from ._failure import describe_thrown, error_info, without_own_frames
from ._model import Action, Level, Phase, SpanEvent
from ._redact import redact
from ._render import level_for, render
from ._sinks import SpanSink, pretty_sink

T = TypeVar("T")

DEFAULT_SLOW_MS = 500.0

_LEVEL_RANK: dict[Level, int] = {"debug": 10, "info": 20, "warn": 30, "error": 40}
_TRACED_MARK = "__spanlog_traced__"


class SpanlogWarning(RuntimeWarning):
    """A sink, params or summarize failed. The call it describes goes on."""


@dataclass(frozen=True)
class _Owner:
    """Who writes a line. For a nested call, scope and method name the caller."""

    action: Action
    scope: str | None = None
    method: str | None = None
    target: str | None = None
    same_scope: bool = False


class RequestSpan:
    """Ends the lines opened by start_request. Only the first end or abort writes."""

    def __init__(self, write_end: Callable[..., None]) -> None:
        self._write_end = write_end

    def end(self, status: int) -> None:
        if status >= 500:
            self._write_end(phase="FAILED", status=status)
        else:
            self._write_end(status=status)

    def abort(self) -> None:
        self._write_end(phase="ABORTED")


def get_request_id() -> str | None:
    """Return the reqId of the request running now, if any."""
    context = current_context.get()
    return None if context is None else context.request.req_id


class Spanlog:
    def __init__(
        self,
        service: str,
        sink: SpanSink | None = None,
        level: Level = "info",
        slow_ms: float = DEFAULT_SLOW_MS,
        utc_offset_minutes: int | None = None,
        redact_keys: Iterable[str] = (),
        error_message: Callable[[BaseException], str] | None = None,
    ) -> None:
        self._service = service
        self._sink = pretty_sink() if sink is None else sink
        self._min_rank = _LEVEL_RANK[level]
        self._slow_ms = slow_ms
        self._utc_offset_minutes = utc_offset_minutes
        self._redact_keys = tuple(redact_keys)
        self._error_message = error_message

    def _emit(self, event: SpanEvent) -> None:
        if _LEVEL_RANK[level_for(event)] < self._min_rank:
            return
        try:
            offset = self._utc_offset_minutes
            if offset is None:
                offset = _host_offset_minutes(event.time)
            self._sink.write(render(event, offset, without_own_frames))
        except Exception as failure:
            # A broken sink must never fail the call it describes.
            _warn(f"spanlog sink failed: {describe_thrown(failure)}")

    def _event(self, owner: _Owner, req_id: str | None, phase: Phase, **rest: Any) -> SpanEvent:
        return SpanEvent(
            time=datetime.now(UTC),
            service=self._service,
            action=owner.action,
            phase=phase,
            req_id=req_id,
            scope=owner.scope,
            method=owner.method,
            target=owner.target,
            same_scope=owner.same_scope,
            **rest,
        )

    def _phase_after(self, duration_ms: float) -> Phase:
        return "SLOW" if duration_ms > self._slow_ms else "DONE"

    def _redacted(self, value: dict[str, Any] | None) -> dict[str, Any] | None:
        return None if value is None else cast("dict[str, Any]", redact(value, self._redact_keys))

    def _params_for_event(
        self, owner: _Owner, params: dict[str, Any] | None
    ) -> dict[str, Any] | None:
        try:
            return self._redacted(params)
        except Exception as failure:
            # Reading params runs code of the application, which must not stop the call.
            name = owner.target or owner.method
            _warn(f"spanlog params failed for {name}: {describe_thrown(failure)}")
            return None

    def _summary_for_event(
        self,
        owner: _Owner,
        result: Any,
        summarize: Callable[[Any], dict[str, Any]] | None,
        explicit: bool,
    ) -> dict[str, Any] | None:
        if not explicit:
            return None
        try:
            return self._redacted(_summary_of(result, summarize))
        except Exception as failure:
            # A summarize function that raises must not fail a call that succeeded.
            name = owner.target or owner.method
            _warn(f"spanlog summarize failed for {name}: {describe_thrown(failure)}")
            return None

    def _begin(
        self, owner: _Owner, request: RequestScope, params: dict[str, Any] | None = None
    ) -> float:
        params_for_event = self._params_for_event(owner, params)
        self._emit(self._event(owner, request.req_id, "START", params=params_for_event))
        return time.perf_counter()

    def _finish(
        self,
        owner: _Owner,
        request: RequestScope,
        started_at: float,
        result: Any,
        summarize: Callable[[Any], dict[str, Any]] | None = None,
        explicit: bool = False,
    ) -> None:
        duration_ms = (time.perf_counter() - started_at) * 1000
        self._emit(
            self._event(
                owner,
                request.req_id,
                self._phase_after(duration_ms),
                duration_ms=duration_ms,
                summary=self._summary_for_event(owner, result, summarize, explicit),
            )
        )

    # BaseException includes CancelledError. A cancelled call ends with FAILED and the
    # exception keeps going up.
    def _fail(
        self, owner: _Owner, request: RequestScope, started_at: float, failure: BaseException
    ) -> None:
        duration_ms = (time.perf_counter() - started_at) * 1000
        self._emit(
            self._event(
                owner,
                request.req_id,
                "FAILED",
                duration_ms=duration_ms,
                err=error_info(failure, self._error_message),
                first_report=request.mark_reported(failure),
            )
        )

    def _wrap_async(
        self, scope: str, method: str, original: Callable[..., Awaitable[Any]]
    ) -> Callable[..., Any]:
        @functools.wraps(original)
        async def spanlog_wrapped(*args: Any, **kwargs: Any) -> Any:
            owner, context = _enter(scope, method)
            started_at = self._begin(owner, context.request)
            return await self._await_in_frame(owner, context, started_at, original(*args, **kwargs))

        return spanlog_wrapped

    def _wrap_sync(
        self, scope: str, method: str, original: Callable[..., Any]
    ) -> Callable[..., Any]:
        @functools.wraps(original)
        def spanlog_wrapped(*args: Any, **kwargs: Any) -> Any:
            owner, context = _enter(scope, method)
            started_at = self._begin(owner, context.request)
            token = current_context.set(context)
            try:
                result = original(*args, **kwargs)
            except BaseException as failure:
                self._fail(owner, context.request, started_at, failure)
                raise
            finally:
                current_context.reset(token)
            # A decorator around an async def hides it from inspect. The line ends when
            # the coroutine ends, and the coroutine runs in the frame of this method.
            if inspect.iscoroutine(result):
                return self._await_in_frame(owner, context, started_at, result)
            self._finish(owner, context.request, started_at, result)
            return result

        return spanlog_wrapped

    async def _await_in_frame(
        self,
        owner: _Owner,
        context: SpanContext,
        started_at: float,
        awaitable: Awaitable[Any],
        summarize: Callable[[Any], dict[str, Any]] | None = None,
        explicit: bool = False,
    ) -> Any:
        token = current_context.set(context)
        try:
            result = await awaitable
        except BaseException as failure:
            self._fail(owner, context.request, started_at, failure)
            raise
        finally:
            current_context.reset(token)
        self._finish(owner, context.request, started_at, result, summarize, explicit)
        return result

    def traced(self, target: T, scope: str) -> T:
        """Wrap the methods of an object. Each call writes a START line and an end line.

        Every def and async def method is wrapped, inherited ones too, apart from names
        with two leading and trailing underscores. Properties, generators and async
        generators are left untouched. target is changed in place and returned. Tracing
        the same object again does nothing.
        """
        if isinstance(target, type):
            raise TypeError(
                f"spanlog: traced needs an instance, and {target.__name__} is a class. "
                f"Pass {target.__name__}(...)."
            )
        if getattr(target, _TRACED_MARK, False) is True:
            return target
        try:
            for name, original in _methods_of(target):
                is_async = inspect.iscoroutinefunction(original)
                wrap = self._wrap_async if is_async else self._wrap_sync
                object.__setattr__(target, name, wrap(scope, name, original))
            object.__setattr__(target, _TRACED_MARK, True)
        except AttributeError as failure:
            raise TypeError(
                f"spanlog: traced cannot set attributes on {type(target).__name__}: {failure}. "
                "A class with __slots__ needs a __dict__ slot."
            ) from failure
        return target

    def call(
        self,
        target: str,
        fn: Callable[[], T],
        *,
        params: dict[str, Any] | None = None,
        summarize: Callable[[T], dict[str, Any]] | None = None,
    ) -> T:
        """Run fn as an explicit call named target and write its lines."""
        if inspect.iscoroutinefunction(fn):
            raise TypeError(f"spanlog: {target} is async. Use `await log.acall(...)`.")
        owner, context = _call_owner(target)
        started_at = self._begin(owner, context.request, params)
        token = current_context.set(context)
        try:
            result = fn()
        except BaseException as failure:
            self._fail(owner, context.request, started_at, failure)
            raise
        finally:
            current_context.reset(token)
        self._finish(owner, context.request, started_at, result, summarize, True)
        return result

    async def acall(
        self,
        target: str,
        fn: Callable[[], Awaitable[T]],
        *,
        params: dict[str, Any] | None = None,
        summarize: Callable[[T], dict[str, Any]] | None = None,
    ) -> T:
        """Await fn() as an explicit call named target and write its lines."""
        owner, context = _call_owner(target)
        started_at = self._begin(owner, context.request, params)
        token = current_context.set(context)
        try:
            awaitable = fn()
        except BaseException as failure:
            self._fail(owner, context.request, started_at, failure)
            raise
        finally:
            current_context.reset(token)
        result: T = await self._await_in_frame(
            owner, context, started_at, awaitable, summarize, True
        )
        return result

    @contextmanager
    def with_request(self, req_id: str | None) -> Iterator[None]:
        """Run the block as one request.

        Every line written inside carries req_id, and each error is reported with its
        stack once. A context manager works for def and async def alike. A coroutine
        built inside a function and awaited after it returns would run outside.
        """
        token = current_context.set(SpanContext(RequestScope(req_id), None))
        try:
            yield
        finally:
            current_context.reset(token)

    # _end_once returns the writer of the end line of a span. An adapter can end the
    # same span from more than one event.
    def _end_once(self, start_line: SpanEvent) -> Callable[..., None]:
        started_at = time.perf_counter()
        ended = False

        def write_end(**end: Any) -> None:
            nonlocal ended
            if ended:
                return
            ended = True
            duration_ms = (time.perf_counter() - started_at) * 1000
            end.setdefault("phase", self._phase_after(duration_ms))
            now = datetime.now(UTC)
            self._emit(replace(start_line, time=now, duration_ms=duration_ms, **end))

        return write_end

    def start_request(self, target: str) -> RequestSpan:
        """Write the START line of an incoming request. Framework adapters call it."""
        context = current_context.get()
        req_id = None if context is None else context.request.req_id
        start_line = self._event(_Owner(action="request", target=target), req_id, "START")
        self._emit(start_line)
        return RequestSpan(self._end_once(start_line))


def _enter(scope: str, method: str) -> tuple[_Owner, SpanContext]:
    parent = current_context.get()
    request = RequestScope(None) if parent is None else parent.request
    callee = Frame(scope, method)
    return _owner_of(parent, callee), SpanContext(request, callee)


def _call_owner(target: str) -> tuple[_Owner, SpanContext]:
    parent = current_context.get()
    request = RequestScope(None) if parent is None else parent.request
    frame = None if parent is None else parent.frame
    owner = _Owner(
        action="call",
        scope=None if frame is None else frame.scope,
        method=None if frame is None else frame.method,
        target=target,
    )
    return owner, SpanContext(request, frame)


# Inside another wrapped method the line belongs to the caller and names the callee as
# target. Otherwise the callee writes a run line in its own scope.
def _owner_of(parent: SpanContext | None, callee: Frame) -> _Owner:
    caller = None if parent is None else parent.frame
    if caller is None:
        return _Owner(action="run", scope=callee.scope, method=callee.method)
    return _Owner(
        action="call",
        scope=caller.scope,
        method=caller.method,
        target=f"{callee.scope}.{callee.method}",
        same_scope=caller.scope == callee.scope,
    )


def _summary_of(
    result: Any, summarize: Callable[[Any], dict[str, Any]] | None
) -> dict[str, Any] | None:
    if summarize is not None:
        return summarize(result)
    return {"count": len(result)} if isinstance(result, (list, tuple)) else None


# getattr_static reads a property without running it. Generators stay as they are,
# because a wrapper would end its line before the first item is read.
def _methods_of(target: object) -> list[tuple[str, Callable[..., Any]]]:
    methods: list[tuple[str, Callable[..., Any]]] = []
    for name in dir(target):
        if name.startswith("__") and name.endswith("__"):
            continue
        static = inspect.getattr_static(target, name)
        if isinstance(static, (staticmethod, classmethod)):
            static = static.__func__
        if not inspect.isfunction(static):
            continue
        if inspect.isgeneratorfunction(static) or inspect.isasyncgenfunction(static):
            continue
        methods.append((name, getattr(target, name)))
    return methods


def _host_offset_minutes(moment: datetime) -> int:
    offset = moment.astimezone().utcoffset()
    return 0 if offset is None else int(offset.total_seconds() // 60)


# warnings.warn raises under a filter set to error, and a replaced warnings.showwarning
# can raise anything. A warning must not fail the call.
def _warn(message: str) -> None:
    try:
        warnings.warn(message, SpanlogWarning, stacklevel=2)
    except Exception:
        pass
