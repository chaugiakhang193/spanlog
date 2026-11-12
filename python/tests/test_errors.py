import asyncio

import pytest

from support import make, traced_pair


def test_error_reported_once_with_stack_at_innermost_frame():
    log, sink = make()
    with pytest.raises(ValueError):
        traced_pair(log).handle_failing()
    failed = [r for r in sink.rendered if r.fields["phase"] == "FAILED"]
    assert [r.level for r in failed] == ["error", "info"]
    assert failed[0].msg.endswith("ValueError: stock not available")
    assert "stack" in failed[0].fields["err"]
    assert "stack" not in failed[1].fields["err"]
    readable = "\n".join(failed[0].lines[1:])
    assert "at Inventory.fail (" in readable
    assert "spanlog_wrapped" not in readable
    assert "spanlog_wrapped" in failed[0].fields["err"]["stack"]


def test_async_error_reported_once():
    log, sink = make()
    with pytest.raises(ValueError):
        asyncio.run(traced_pair(log).ahandle_failing())
    failed = [r for r in sink.rendered if r.fields["phase"] == "FAILED"]
    assert [r.level for r in failed] == ["error", "info"]
    assert "at Inventory.afail (" in "\n".join(failed[0].lines[1:])


def test_builtin_exception_reported_once_inside_request():
    log, sink = make()
    shared = ValueError("x")

    def raises():
        raise shared

    with log.with_request("r1"):
        for _ in range(2):
            with pytest.raises(ValueError):
                log.call("step", raises)
    assert [r.level for r in sink.rendered if r.fields["phase"] == "FAILED"] == ["error", "info"]


def test_same_error_in_another_request_is_new():
    log, sink = make()
    shared = ValueError("x")

    def raises():
        raise shared

    for req in ("r1", "r2"):
        with log.with_request(req), pytest.raises(ValueError):
            log.call("step", raises)
    assert [r.level for r in sink.rendered if r.fields["phase"] == "FAILED"] == ["error", "error"]


def test_cancelled_call_ends_failed_and_keeps_going():
    log, sink = make()

    class Slow:
        async def wait(self):
            await asyncio.sleep(10)

    slow = log.traced(Slow(), "Slow")

    async def main():
        task = asyncio.create_task(slow.wait())
        await asyncio.sleep(0.01)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(main())
    assert "CancelledError" in sink.rendered[-1].msg


def test_error_message_replaces_message_and_stack_header():
    log, sink = make(error_message=lambda failure: "hidden")
    with pytest.raises(ValueError):
        log.call("step", lambda: (_ for _ in ()).throw(ValueError("secret")))
    failed = sink.rendered[-1]
    assert failed.fields["err"]["message"] == "hidden"
    assert failed.fields["err"]["stack"].startswith("ValueError: hidden")
    assert "secret" not in failed.msg + failed.fields["err"]["stack"]


def test_unreadable_error():
    class BadStr(Exception):
        def __str__(self):
            raise RuntimeError("boom")

    log, sink = make()

    def raises():
        raise BadStr()

    with pytest.raises(BadStr):
        log.call("step", raises)
    assert sink.rendered[-1].msg.endswith("UnreadableError: [unreadable thrown value]")


def test_level_filter_marks_error_even_when_hidden():
    log, sink = make(level="error")
    with log.with_request("r1"):
        shared = ValueError("x")

        def raises():
            raise shared

        with pytest.raises(ValueError):
            log.call("first", raises)
        with pytest.raises(ValueError):
            log.call("second", raises)
    assert len(sink.rendered) == 1
