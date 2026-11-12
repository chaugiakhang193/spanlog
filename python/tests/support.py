"""Sinks and services shared by the tests of the core API."""

import asyncio
import re

from spanlog import Spanlog


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
