import pytest

from spanlog._render import js_number, round_ms, stable_json


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
