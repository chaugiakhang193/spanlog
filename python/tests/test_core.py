import asyncio
import concurrent.futures
import re
import warnings

import pytest

from spanlog import Spanlog, SpanlogWarning, get_request_id


class ListSink:
    def __init__(self):
        self.rendered = []

    def write(self, rendered):
        self.rendered.append(rendered)

    # msgs leaves out the duration, which changes between runs.
    @property
    def msgs(self):
        return [re.sub(r" [\d.]+ms", "", rendered.msg) for rendered in self.rendered]


def make(**options):
    sink = ListSink()
    options.setdefault("level", "debug")
    return Spanlog(service="shop", sink=sink, utc_offset_minutes=0, **options), sink


class Inventory:
    def reserve(self):
        return "reserved"

    async def areserve(self):
        await asyncio.sleep(0)
        return "reserved"

    def fail(self):
        raise ValueError("stock not available")

    async def afail(self):
        await asyncio.sleep(0)
        raise ValueError("stock not available")

    @property
    def explosive(self):
        raise AssertionError("a property must not run")

    def items(self):
        yield 1

    async def aitems(self):
        yield 1


class Orders:
    def __init__(self, inventory):
        self.inventory = inventory

    def handle(self):
        return self.inventory.reserve()

    async def ahandle(self):
        return await self.inventory.areserve()

    def handle_failing(self):
        return self.inventory.fail()

    async def ahandle_failing(self):
        return await self.inventory.afail()

    def local(self):
        return self.helper()

    def helper(self):
        return 1


def traced_pair(log):
    return log.traced(Orders(log.traced(Inventory(), "Inventory")), "Orders")


def test_sync_lines():
    log, sink = make()
    assert traced_pair(log).handle() == "reserved"
    assert sink.msgs == [
        "Orders.handle - run | START",
        "Orders.handle - call Inventory.reserve | START",
        "Orders.handle - call Inventory.reserve | DONE",
        "Orders.handle - run | DONE",
    ]


def test_async_lines_match_sync_lines():
    log, sink = make()
    assert asyncio.run(traced_pair(log).ahandle()) == "reserved"
    assert sink.msgs == [
        "Orders.ahandle - run | START",
        "Orders.ahandle - call Inventory.areserve | START",
        "Orders.ahandle - call Inventory.areserve | DONE",
        "Orders.ahandle - run | DONE",
    ]


def test_same_scope_call_is_debug():
    log, sink = make()
    traced_pair(log).local()
    levels = {r.msg.split(" | ")[0]: r.level for r in sink.rendered}
    assert levels["Orders.local - call Orders.helper"] == "debug"
    assert levels["Orders.local - run"] == "info"


def test_side_by_side_tasks_keep_their_own_frame():
    log, sink = make()
    orders = traced_pair(log)

    async def main():
        with log.with_request("r1"):
            await asyncio.gather(orders.ahandle(), orders.ahandle())

    asyncio.run(main())
    assert all(r.fields["reqId"] == "r1" for r in sink.rendered)
    assert (
        len([m for m in sink.msgs if m == "Orders.ahandle - call Inventory.areserve | DONE"]) == 2
    )


def test_side_by_side_callers_of_one_method_keep_their_own_parent():
    class Shared:
        async def work(self):
            await asyncio.sleep(0.01)

    class First:
        def __init__(self, shared):
            self.shared = shared

        async def run(self):
            await self.shared.work()

    class Second(First):
        pass

    log, sink = make()
    shared = log.traced(Shared(), "Shared")
    first = log.traced(First(shared), "First")
    second = log.traced(Second(shared), "Second")

    async def main():
        await asyncio.gather(first.run(), second.run())

    asyncio.run(main())
    assert "First.run - call Shared.work | DONE" in sink.msgs
    assert "Second.run - call Shared.work | DONE" in sink.msgs


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


def test_decorated_async_method_runs_in_its_frame_and_ends_with_the_coroutine():
    log, sink = make()

    def decorator(fn):
        def inner(*args):
            return fn(*args)

        return inner

    class Hidden:
        @decorator
        async def run(self):
            await asyncio.sleep(0)
            return await self.step()

        async def step(self):
            return get_request_id()

    hidden = log.traced(Hidden(), "Hidden")

    async def main():
        with log.with_request("r1"):
            return await hidden.run()

    assert asyncio.run(main()) == "r1"
    assert sink.msgs == [
        "Hidden.run - run | START",
        "Hidden.run - call Hidden.step | START",
        "Hidden.run - call Hidden.step | DONE",
        "Hidden.run - run | DONE",
    ]


def test_traced_leaves_properties_and_generators():
    log, sink = make()
    inventory = log.traced(Inventory(), "Inventory")
    assert list(inventory.items()) == [1]
    assert sink.rendered == []


def test_traced_twice_writes_once():
    log, sink = make()
    inventory = Inventory()
    log.traced(inventory, "Inventory")
    log.traced(inventory, "Inventory")
    inventory.reserve()
    assert len(sink.rendered) == 2


def test_traced_rejects_a_class():
    log, _ = make()
    with pytest.raises(TypeError, match=r"Pass Inventory\(\.\.\.\)"):
        log.traced(Inventory, "Inventory")


def test_traced_rejects_slots_with_a_way_out():
    log, _ = make()

    class Slotted:
        __slots__ = ()

        def run(self):
            return 1

    with pytest.raises(TypeError, match="__slots__"):
        log.traced(Slotted(), "Slotted")


def test_call_params_redacted_and_summary_counted():
    log, sink = make()
    assert log.call("load", lambda: [1, 2], params={"id": 7, "email": "a@b.c"}) == [1, 2]
    assert sink.rendered[0].msg == 'call load | START {"email":"[REDACTED]","id":7}'
    assert sink.rendered[1].msg.endswith('ms {"count":2}')


def test_acall_summary():
    log, sink = make()

    async def load():
        return {"n": 1}

    asyncio.run(log.acall("load", load, summarize=lambda result: {"n": result["n"], "token": "t"}))
    assert sink.rendered[1].msg.endswith('ms {"n":1,"token":"[REDACTED]"}')


def test_call_rejects_async_with_a_way_out():
    log, _ = make()

    async def load():
        return 1

    with pytest.raises(TypeError, match="acall"):
        log.call("load", load)


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


def test_broken_sink_warns_and_the_call_goes_on():
    class Broken:
        def write(self, rendered):
            raise OSError("disk full")

    log = Spanlog(service="shop", sink=Broken())
    with pytest.warns(SpanlogWarning, match="spanlog sink failed: disk full"):
        assert log.call("step", lambda: 1) == 1


def test_warning_filter_set_to_error_does_not_fail_the_call():
    class Broken:
        def write(self, rendered):
            raise OSError("disk full")

    log = Spanlog(service="shop", sink=Broken())
    with warnings.catch_warnings():
        warnings.simplefilter("error")
        assert log.call("step", lambda: 1) == 1


def test_warning_hook_that_raises_does_not_fail_the_call_or_hide_its_error(monkeypatch):
    class Broken:
        def write(self, rendered):
            raise OSError("disk full")

    def raising_hook(*args, **kwargs):
        raise RuntimeError("hook failed")

    original = ValueError("stock not available")

    def fail():
        raise original

    log = Spanlog(service="shop", sink=Broken())
    with warnings.catch_warnings():
        warnings.simplefilter("always")
        monkeypatch.setattr(warnings, "showwarning", raising_hook)
        assert log.call("step", lambda: 1) == 1
        with pytest.raises(ValueError) as raised:
            log.call("step", fail)
    assert raised.value is original


def test_broken_summarize_warns_and_the_call_goes_on():
    log, sink = make()
    with pytest.warns(SpanlogWarning, match="summarize failed for step"):
        assert log.call("step", lambda: 1, summarize=lambda result: 1 / 0) == 1
    assert sink.rendered[-1].fields["phase"] == "DONE"


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


def test_slow_phase():
    log, sink = make(slow_ms=-1)
    log.call("step", lambda: 1)
    assert sink.rendered[-1].fields["phase"] == "SLOW"
    assert sink.rendered[-1].level == "warn"


def test_request_span_writes_once_and_keeps_the_id():
    log, sink = make()
    with log.with_request("r1"):
        span = log.start_request("GET /a")
        span.end(503)
        span.abort()
    assert [r.fields["phase"] for r in sink.rendered] == ["START", "FAILED"]
    assert sink.rendered[-1].fields["status"] == 503
    assert sink.rendered[-1].fields["reqId"] == "r1"


def test_request_id_absent_outside_a_request():
    assert get_request_id() is None


def test_to_thread_carries_the_request_and_a_plain_executor_does_not():
    log, _ = make()

    async def main():
        with log.with_request("r1"):
            carried = await asyncio.to_thread(get_request_id)
            with concurrent.futures.ThreadPoolExecutor() as pool:
                lost = pool.submit(get_request_id).result()
            return carried, lost

    assert asyncio.run(main()) == ("r1", None)


def test_start_query_names_the_tables_given_to_a_rollback():
    log, sink = make()
    log.start_query("ROLLBACK", rolled_back_tables=("orders", "warehouse.inventory")).end()
    assert sink.msgs == [
        "query ROLLBACK orders, warehouse.inventory | START",
        "query ROLLBACK orders, warehouse.inventory | DONE",
    ]
    assert [rendered.level for rendered in sink.rendered] == ["debug", "warn"]
    assert sink.rendered[1].fields["tables"] == ["orders", "warehouse.inventory"]


def test_start_query_keeps_the_tables_of_other_statements():
    log, sink = make()
    log.start_query("COMMIT", rolled_back_tables=["orders"]).end()
    assert sink.msgs == ["query COMMIT | START", "query COMMIT | DONE"]
