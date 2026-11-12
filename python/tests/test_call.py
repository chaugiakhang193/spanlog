import asyncio
import warnings

import pytest

from spanlog import Spanlog, SpanlogWarning
from support import make


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
