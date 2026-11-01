import json
from datetime import datetime
from pathlib import Path

import pytest

from spanlog import ErrorInfo, SpanEvent, render

CASES = json.loads((Path(__file__).parents[2] / "spec" / "cases.json").read_text(encoding="utf-8"))


def event_from(raw: dict) -> SpanEvent:
    err = raw.get("err")
    return SpanEvent(
        time=datetime.fromisoformat(raw["time"]),
        service=raw["service"],
        action=raw["action"],
        phase=raw["phase"],
        req_id=raw.get("reqId"),
        scope=raw.get("scope"),
        method=raw.get("method"),
        target=raw.get("target"),
        same_scope=raw.get("sameScope", False),
        duration_ms=raw.get("durationMs"),
        status=raw.get("status"),
        op=raw.get("op"),
        tables=raw.get("tables"),
        err=None
        if err is None
        else ErrorInfo(message=err["message"], name=err.get("name"), stack=err.get("stack")),
        first_report=raw.get("firstReport", False),
        params=raw.get("params"),
        summary=raw.get("summary"),
    )


def test_loads_the_shared_cases():
    assert len(CASES["render"]) >= 20


@pytest.mark.parametrize("case", CASES["render"], ids=lambda case: case["name"])
def test_render_case(case):
    rendered = render(event_from(case["event"]), case["options"]["utcOffsetMinutes"])
    assert rendered.level == case["expect"]["level"]
    assert rendered.msg == case["expect"]["msg"]
    assert rendered.fields == case["expect"]["fields"]
    assert rendered.lines == case["expect"]["lines"]
