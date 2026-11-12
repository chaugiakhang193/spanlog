import json
import logging
from datetime import UTC, datetime

from spanlog import ErrorInfo, SpanEvent, json_sink, logging_sink, pretty_sink, render


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
