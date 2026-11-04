import json
import time
from pathlib import Path

import pytest

from spanlog._sql import SqlSummary, query_target, summarize_sql

CASES = json.loads((Path(__file__).parents[2] / "spec" / "cases.json").read_text(encoding="utf-8"))


def test_loads_the_shared_cases():
    assert len(CASES["sql"]) >= 75


@pytest.mark.parametrize("case", CASES["sql"], ids=lambda case: case["name"])
def test_sql_case(case):
    assert summarize_sql(case["sql"]) == SqlSummary(case["op"], case["tables"])


def test_keeps_a_doubled_quote_inside_a_quoted_name():
    assert summarize_sql('SELECT 1 FROM "odd""name"').tables == ['odd"name']


def test_lists_a_table_once_when_it_appears_twice():
    sql = "SELECT a.id FROM orders a JOIN orders b ON b.parent_id = a.id"
    assert summarize_sql(sql).tables == ["orders"]


def test_returns_an_empty_op_for_text_that_is_not_a_statement():
    assert summarize_sql("") == SqlSummary("", [])
    assert summarize_sql("  $1 ") == SqlSummary("", [])


def test_survives_an_unclosed_quote_and_parenthesis():
    assert summarize_sql('SELECT * FROM "orders') == SqlSummary("SELECT", ["orders"])
    assert summarize_sql("WITH x AS (SELECT 1").op == ""


def test_survives_an_unclosed_string_comment_and_dollar_quote():
    assert summarize_sql("SELECT 'from alice FROM orders").tables == []
    assert summarize_sql("SELECT /* from alice FROM orders").tables == []
    assert summarize_sql("SELECT $$ from alice FROM orders").tables == []


def test_keeps_a_name_that_holds_a_dollar_sign():
    assert summarize_sql("SELECT 1 FROM a$b$c").tables == ["a$b$c"]


def test_does_not_read_a_quoted_name_as_a_parenthesis():
    sql = 'WITH a AS (SELECT "(" FROM items) SELECT * FROM a'
    assert summarize_sql(sql) == SqlSummary("SELECT", ["items"])


# A negative index reads the last token in Python, and these keywords look one and two
# tokens back.
@pytest.mark.parametrize(
    ("sql", "tables"),
    [
        ("UPDATE orders SET a = 1 FOR", ["orders"]),
        ("FROM orders DISTINCT IS", ["orders"]),
        ("DELETE FROM x", ["x"]),
        ("FROM x DELETE (", ["x"]),
    ],
)
def test_the_first_tokens_have_nothing_before_them(sql, tables):
    assert summarize_sql(sql).tables == tables


# Expected values come from Node.js 24.18.0. str.upper turns ı into I, which would read
# these names as WITH and IS.
def test_a_dotless_i_does_not_make_a_keyword():
    assert summarize_sql("wıth a AS (SELECT 1) SELECT * FROM a") == SqlSummary("WıTH", ["a"])
    sql = "SELECT 1 FROM x WHERE a ıS DISTINCT FROM b"
    assert summarize_sql(sql) == SqlSummary("SELECT", ["x", "b"])


def test_reads_a_character_above_u_ffff_as_part_of_a_name():
    assert summarize_sql("SELECT 1 FROM t\U0001f600x").tables == ["t\U0001f600x"]


# A scan of every scope for each name takes minutes at this depth with one name. The
# limit leaves room for a loaded machine.
@pytest.mark.parametrize(
    "name", [lambda level: f"c{level}", lambda level: "c"], ids=["different names", "one name"]
)
def test_reads_deeply_nested_common_table_expressions(name):
    depth = 32_000
    heads = [f"WITH {name(level)} AS (" for level in range(depth)]
    tails = [f") SELECT * FROM {name(level)}" for level in range(depth)]
    sql = "".join(reversed(heads)) + "SELECT * FROM orders" + "".join(tails)

    start = time.perf_counter()
    assert summarize_sql(sql).tables == ["orders"]
    assert time.perf_counter() - start < 10


def test_reads_a_statement_that_names_many_different_tables():
    names = [f"t{index}" for index in range(32_000)]
    sql = f"SELECT * FROM {' JOIN '.join(names)}"

    start = time.perf_counter()
    assert summarize_sql(sql).tables == names
    assert time.perf_counter() - start < 10


def test_query_target_joins_op_and_tables():
    assert query_target(SqlSummary("SELECT", ["orders", "items"])) == "SELECT orders, items"


def test_query_target_is_the_op_alone_without_tables():
    assert query_target(SqlSummary("COMMIT", [])) == "COMMIT"
