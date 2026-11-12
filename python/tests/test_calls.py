import asyncio

import pytest

from spanlog import get_request_id
from support import Inventory, make, traced_pair


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


def test_slow_phase():
    log, sink = make(slow_ms=-1)
    log.call("step", lambda: 1)
    assert sink.rendered[-1].fields["phase"] == "SLOW"
    assert sink.rendered[-1].level == "warn"
