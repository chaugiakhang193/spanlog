import json
import logging
from dataclasses import dataclass
from datetime import UTC, datetime
from decimal import Decimal

import pytest

from spanlog import (
    CIRCULAR,
    REDACTED,
    TRUNCATED,
    ErrorInfo,
    SpanEvent,
    json_sink,
    logging_sink,
    pretty_sink,
    redact,
    render,
)
from spanlog._render import js_number, round_ms, stable_json


def fixed_event(**rest):
    return SpanEvent(
        time=datetime(2026, 10, 24, 14, 15, 3, 120999, tzinfo=UTC),
        service="shop",
        action="call",
        phase="DONE",
        target="X.y",
        duration_ms=3.0,
        **rest,
    )


def test_pretty_sink_writes_one_text_per_event():
    written = []
    pretty_sink(written.append).write(render(fixed_event(), 420))
    assert written == ["2026-10-24 21:15:03.120 +07:00 INFO  [shop] req=- call X.y | DONE 3ms\n"]


def test_json_sink_writes_fields_in_spec_order():
    written = []
    json_sink(written.append).write(render(fixed_event(), 420))
    assert written[0] == (
        '{"time":"2026-10-24T14:15:03.120Z","level":"info","service":"shop","action":"call",'
        '"target":"X.y","phase":"DONE","ms":3,"msg":"call X.y | DONE 3ms"}\n'
    )
    json.loads(written[0])


def test_logging_sink_hands_fields_to_the_logger(caplog):
    logger = logging.getLogger("spanlog-test")
    err = ErrorInfo(message="x", name="ValueError", stack="ValueError: x\n    at f (a.py:1)")
    event = fixed_event(status=200, err=err, first_report=True, summary={"b": 2})
    with caplog.at_level(logging.INFO, logger="spanlog-test"):
        logging_sink(logger).write(render(event, 0))
    record = caplog.records[0]
    assert record.levelno == logging.INFO
    assert record.getMessage() == 'call X.y | DONE 3ms {"b":2}'
    assert (record.service, record.phase, record.ms, record.target) == ("shop", "DONE", 3, "X.y")


def test_percent_in_a_message_survives_logging(caplog):
    logger = logging.getLogger("spanlog-test")
    with caplog.at_level(logging.INFO, logger="spanlog-test"):
        event = SpanEvent(
            time=datetime(2026, 10, 24, tzinfo=UTC),
            service="shop",
            action="request",
            phase="START",
            target="GET /a%0Ab",
        )
        logging_sink(logger).write(render(event, 0))
    assert caplog.records[0].getMessage() == "GET /a%0Ab | START"


def test_redact_nested_and_case_insensitive():
    value = {"Password": "x", "a": [{"TOKEN": "y", "b": 1}]}
    assert redact(value) == {"Password": REDACTED, "a": [{"TOKEN": REDACTED, "b": 1}]}
    assert value["Password"] == "x"


def test_redact_extra_keys_cycle_and_depth():
    cycle = {}
    cycle["self"] = cycle
    assert redact(cycle) == {"self": CIRCULAR}
    assert redact({"card": 1}, keys=["CARD"]) == {"card": REDACTED}
    assert redact({"a": {"b": 1}}, max_depth=1) == {"a": TRUNCATED}


def test_redact_reads_known_types_and_hides_the_rest():
    @dataclass
    class Row:
        id: int
        email: str

    class Model:
        def model_dump(self):
            return {"token": "t", "n": 1}

    class Secretive:
        def __repr__(self):
            return "password=hunter2"

    result = redact(
        {
            "row": Row(1, "a@b.c"),
            "model": Model(),
            "other": Secretive(),
            "amount": Decimal("1.50"),
            "raw": b"abc",
            "tags": {"b", "a"},
            "at": datetime(2026, 1, 2, tzinfo=UTC),
        }
    )
    assert result == {
        "row": {"id": 1, "email": REDACTED},
        "model": {"token": REDACTED, "n": 1},
        "other": "[OBJECT Secretive]",
        "amount": "1.50",
        "raw": "[BYTES 3]",
        "tags": ["a", "b"],
        "at": "2026-01-02T00:00:00+00:00",
    }


# Expected text comes from Node.js 24.18.0: JSON.stringify and Math.round.
@pytest.mark.parametrize(
    ("value", "expected"),
    [
        (3.0, "3"),
        (0.1 + 0.2, "0.30000000000000004"),
        (1e21, "1e+21"),
        (1.5e300, "1.5e+300"),
        (1e-7, "1e-7"),
        (1.5e-7, "1.5e-7"),
        (0.000001, "0.000001"),
        (123456789012345680000.0, "123456789012345680000"),
        (-0.0, "0"),
        (float("nan"), "null"),
        (float("inf"), "null"),
    ],
)
def test_js_number(value, expected):
    assert js_number(value) == expected


@pytest.mark.parametrize(
    ("value", "expected"),
    [(0.25, 0.3), (0.35, 0.4), (2.96, 3), (800.96, 801), (0.04, 0), (-0.04, 0), (1.45, 1.5)],
)
def test_round_ms(value, expected):
    assert round_ms(value) == expected


def test_stable_json_sorts_by_utf16_and_keeps_text_as_node():
    # Node.js 24.18.0 prints this line for the same object.
    value = {"￿": 1, "\U0001f600": 2, "b": [True, None, 1.5], "a": "< \x1f\x7f\ud83d>"}
    assert (
        stable_json(value)
        == '{"a":"< \\u001f\x7f\\ud83d>","b":[true,null,1.5],"\U0001f600":2,"￿":1}'
    )
