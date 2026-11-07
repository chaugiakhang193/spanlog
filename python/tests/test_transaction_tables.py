import json
from pathlib import Path

import pytest

from spanlog._transaction_tables import TransactionTables

CASES = json.loads((Path(__file__).parents[2] / "spec" / "cases.json").read_text(encoding="utf-8"))


def rollbacks_of(statements):
    """Run statements the way an adapter does, statement_starting before each one and
    statement_done after it."""
    transaction = TransactionTables()
    rollbacks = []
    for sql in statements:
        undone = transaction.statement_starting(sql)
        if undone is not None:
            rollbacks.append(undone)
        transaction.statement_done(sql)
    return rollbacks


def test_loads_the_shared_cases():
    assert len(CASES["transactions"]) >= 25


@pytest.mark.parametrize("case", CASES["transactions"], ids=lambda case: case["name"])
def test_transaction_case(case):
    assert rollbacks_of(case["statements"]) == case["rollbacks"]


def test_events_of_a_driver_transaction_follow_the_same_rules():
    transaction = TransactionTables()
    transaction.begin()
    transaction.statement_done("INSERT INTO orders (sku) VALUES ($1)")
    transaction.end()
    transaction.begin()
    transaction.statement_done("UPDATE inventory SET available = 0")
    assert transaction.roll_back() == ["inventory"]
    assert transaction.roll_back() == []


def test_end_forgets_the_tables():
    transaction = TransactionTables()
    transaction.begin()
    transaction.statement_done("INSERT INTO orders (sku) VALUES ($1)")
    transaction.end()
    assert transaction.roll_back() == []


# A failed statement gets statement_starting and no statement_done, as the adapters call
# statement_done only after a statement without an error.
def test_a_failed_write_is_not_kept():
    transaction = TransactionTables()
    transaction.statement_starting("BEGIN")
    transaction.statement_done("BEGIN")
    transaction.statement_starting("INSERT INTO a (v) VALUES ('x')")
    assert transaction.statement_starting("ROLLBACK") == []


def test_a_failed_commit_and_chain_opens_no_transaction():
    transaction = TransactionTables()
    for sql in ["BEGIN", "INSERT INTO a (v) VALUES (1)"]:
        transaction.statement_starting(sql)
        transaction.statement_done(sql)
    transaction.statement_starting("COMMIT AND CHAIN")
    transaction.statement_starting("INSERT INTO b (v) VALUES (1)")
    transaction.statement_done("INSERT INTO b (v) VALUES (1)")
    assert transaction.statement_starting("ROLLBACK") == []
