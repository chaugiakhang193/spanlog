from dataclasses import dataclass
from datetime import UTC, datetime
from decimal import Decimal

from spanlog import CIRCULAR, REDACTED, TRUNCATED, redact


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
