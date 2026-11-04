from __future__ import annotations

import re
from typing import Literal, NamedTuple


class SqlSummary(NamedTuple):
    """What a query line keeps of a SQL statement."""

    op: str
    tables: list[str]


class _Token(NamedTuple):
    # Identifier text without quotes, or the punctuation character.
    text: str
    kind: Literal["word", "quoted", "punct"]


class _CteScope(NamedTuple):
    """The range of tokens in which name refers to a common table expression."""

    name: str
    start: int
    stop: int


_TABLE_KEYWORDS = frozenset({"FROM", "JOIN", "INTO", "UPDATE"})

# FOR UPDATE and FOR NO KEY UPDATE lock rows. ON CONFLICT ... DO UPDATE updates the row
# of the INSERT. None of them is followed by a table name. The UPDATE action of MERGE
# follows THEN and updates the row of the MERGE.
_UPDATE_WITHOUT_TABLE = frozenset({"FOR", "KEY", "DO", "THEN"})

# A common table expression holds one of these statements. A column list after WITH
# ORDINALITY AS materialized has the same shape up to its first name.
_STATEMENT_KEYWORDS = frozenset(
    {"SELECT", "VALUES", "TABLE", "WITH", "INSERT", "UPDATE", "DELETE", "MERGE"}
)

# Postgres accepts letters with diacritical marks and non-Latin letters in a name and
# in a dollar quote tag. Every character from U+0080 up counts here.
_WORD = re.compile(r"[A-Za-z_\u0080-\U0010FFFF][A-Za-z0-9_$\u0080-\U0010FFFF]*")
_WORD_PART = re.compile(r"[A-Za-z0-9_$\u0080-\U0010FFFF]*")
_DOLLAR_QUOTE_TAG = re.compile(r"\$(?:[A-Za-z_\u0080-\U0010FFFF][A-Za-z0-9_\u0080-\U0010FFFF]*)?\$")
_LINE_END = re.compile(r"[\r\n]")
_BLOCK_COMMENT_MARK = re.compile(r"/\*|\*/")

# Postgres folds only ASCII letters when it looks up a keyword. str.upper turns the
# letters ı and ß of a name into I and SS.
_ASCII_UPPER = {code: code - 32 for code in range(ord("a"), ord("z") + 1)}
_ASCII_LOWER = {code: code + 32 for code in range(ord("A"), ord("Z") + 1)}


def summarize_sql(sql: str) -> SqlSummary:
    """Read the op and the table names of a statement. It never raises.

    Follows the query line rules of docs/log-format.md.
    """
    tokens = _tokenize(sql)
    after_close = _closing_parentheses(tokens)
    scopes: list[_CteScope] = []
    after_expressions: set[int] = set()
    statement_start = _after_leading_parentheses(tokens, 0)
    open_parentheses: list[int] = []
    for index, token in enumerate(tokens):
        if _is_punct(token, "("):
            open_parentheses.append(index)
        elif _is_punct(token, ")"):
            if open_parentheses:
                open_parentheses.pop()
        elif _is_keyword(token, "WITH"):
            scope_end = after_close[open_parentheses[-1]] if open_parentheses else len(tokens)
            after = _read_common_table_expressions(tokens, index, scope_end, after_close, scopes)
            after_expressions.add(after)
            if index == statement_start:
                statement_start = _after_leading_parentheses(tokens, after)
    return SqlSummary(
        op=_op_of(tokens, statement_start),
        tables=_table_names(tokens, scopes, after_expressions),
    )


def query_target(summary: SqlSummary) -> str:
    """Join op and tables into the target of a query line."""
    if not summary.tables:
        return summary.op
    return f"{summary.op} {', '.join(summary.tables)}"


# tokens[-1] in Python is the last token. A position before the first token holds
# nothing, as past the last one.
def _token_at(tokens: list[_Token], index: int) -> _Token | None:
    return tokens[index] if 0 <= index < len(tokens) else None


def _after_leading_parentheses(tokens: list[_Token], start: int) -> int:
    index = start
    while _is_punct(_token_at(tokens, index), "("):
        index += 1
    return index


# _tokenize keeps words, double-quoted identifiers and the punctuation that shapes table
# names and subqueries. String literals and comments are dropped unread because they can
# hold application values. Everything else, such as $1 or a comparison operator, is
# dropped too.
def _tokenize(sql: str) -> list[_Token]:
    tokens: list[_Token] = []
    index = 0
    while index < len(sql):
        char = sql[index]
        word = _WORD.match(sql, index)
        if word is not None:
            end = word.end()
            if sql[end : end + 1] == "'" and word.group() in ("E", "e"):
                index = _after_escape_string(sql, end)
            else:
                tokens.append(_Token(word.group(), "word"))
                index = end
        elif char == "'":
            index = _after_string_literal(sql, index, False)
        elif sql.startswith("--", index):
            index = _after_line_comment(sql, index)
        elif sql.startswith("/*", index):
            index = _after_block_comment(sql, index)
        elif char == '"':
            text, index = _scan_quoted(sql, index)
            tokens.append(_Token(text, "quoted"))
        elif char in "(),.":
            tokens.append(_Token(char, "punct"))
            index += 1
        elif char == "$":
            index = _after_dollar_token(sql, index)
        elif "0" <= char <= "9":
            # 42 must not leave a trailing word behind.
            index = _end_of_word_part(sql, index + 1)
        else:
            index += 1
    return tokens


def _end_of_word_part(sql: str, start: int) -> int:
    found = _WORD_PART.match(sql, start)
    return start if found is None else found.end()


# A doubled quote inside a quoted identifier stands for one quote character.
def _scan_quoted(sql: str, start: int) -> tuple[str, int]:
    parts: list[str] = []
    index = start + 1
    while True:
        close = sql.find('"', index)
        if close == -1:
            parts.append(sql[index:])
            return "".join(parts), len(sql)
        parts.append(sql[index:close])
        if sql[close + 1 : close + 2] != '"':
            return "".join(parts), close + 1
        parts.append('"')
        index = close + 2


# A doubled quote stands for one quote character. A backslash escapes the next character
# only in an E'...' string. Postgres keeps it literal elsewhere.
def _after_string_literal(sql: str, opening: int, backslash_escapes: bool) -> int:
    index = opening + 1
    while index < len(sql):
        char = sql[index]
        if backslash_escapes and char == "\\":
            index += 2
        elif char != "'":
            index += 1
        elif sql[index + 1 : index + 2] == "'":
            index += 2
        else:
            return index + 1
    return len(sql)


# _after_escape_string skips an E'...' string and its continuations. Only the first part
# carries the E, and every part reads a backslash as an escape.
def _after_escape_string(sql: str, opening: int) -> int:
    end = _after_string_literal(sql, opening, True)
    following = _continuation_quote(sql, end)
    while following is not None:
        end = _after_string_literal(sql, following, True)
        following = _continuation_quote(sql, end)
    return end


# _continuation_quote returns the index of the quote that continues the string ending at
# start. Whitespace and line comments may stand between the two parts. Postgres rejects
# two parts with no newline between them.
def _continuation_quote(sql: str, start: int) -> int | None:
    index = start
    while index < len(sql):
        char = sql[index]
        if char in "\n\r \t\f\v":
            index += 1
        elif sql.startswith("--", index):
            index = _after_line_comment(sql, index)
        else:
            return index if char == "'" else None
    return None


# Postgres ends a line comment at a carriage return as well as a line feed.
def _after_line_comment(sql: str, opening: int) -> int:
    found = _LINE_END.search(sql, opening)
    return len(sql) if found is None else found.start()


# Postgres block comments nest.
def _after_block_comment(sql: str, opening: int) -> int:
    depth = 0
    for mark in _BLOCK_COMMENT_MARK.finditer(sql, opening):
        if mark.group() == "/*":
            depth += 1
        else:
            depth -= 1
            if depth == 0:
                return mark.end()
    return len(sql)


# _after_dollar_token skips a $tag$...$tag$ string or a parameter such as $1.
def _after_dollar_token(sql: str, opening: int) -> int:
    tag = _DOLLAR_QUOTE_TAG.match(sql, opening)
    if tag is None:
        return _end_of_word_part(sql, opening + 1)
    close = sql.find(tag.group(), tag.end())
    return len(sql) if close == -1 else close + len(tag.group())


def _is_keyword(token: _Token | None, keyword: str) -> bool:
    return token is not None and token.kind == "word" and _upper_ascii(token.text) == keyword


def _is_punct(token: _Token | None, char: str) -> bool:
    return token is not None and token.kind == "punct" and token.text == char


def _upper_ascii(text: str) -> str:
    return text.translate(_ASCII_UPPER)


# Postgres keeps a quoted name as is. In a UTF-8 database it folds A-Z of an unquoted
# name and keeps every other letter. É and é are two names there.
def _folded_name(token: _Token) -> str:
    return token.text if token.kind == "quoted" else token.text.translate(_ASCII_LOWER)


# _closing_parentheses gives each "(" the index after its closing ")", or the token count
# when it never closes.
def _closing_parentheses(tokens: list[_Token]) -> list[int]:
    after_close = [len(tokens)] * len(tokens)
    still_open: list[int] = []
    for index, token in enumerate(tokens):
        if _is_punct(token, "("):
            still_open.append(index)
        elif _is_punct(token, ")") and still_open:
            after_close[still_open.pop()] = index + 1
    return after_close


# _read_common_table_expressions walks WITH [RECURSIVE] name [(columns)] AS [NOT]
# [MATERIALIZED] (...) [SEARCH ...] [CYCLE ...], ... from the WITH at with_index and
# returns the index of the keyword after the last expression. Each name goes into scopes
# up to scope_end, the end of the statement or subquery that holds the WITH. A plain
# expression becomes visible after its own body. Inside that body its name still reads a
# table. RECURSIVE makes every name of the list visible from the WITH on. A name without
# a statement in parentheses is left out, as in WITH TIME ZONE and WITH ORDINALITY AS t.
def _read_common_table_expressions(
    tokens: list[_Token],
    with_index: int,
    scope_end: int,
    after_close: list[int],
    scopes: list[_CteScope],
) -> int:
    recursive = _is_keyword(_token_at(tokens, with_index + 1), "RECURSIVE")
    index = with_index + (2 if recursive else 1)
    while True:
        name = _token_at(tokens, index)
        if name is None or name.kind == "punct":
            return index
        if _is_punct(_token_at(tokens, index + 1), "("):
            index = after_close[index + 1]
        else:
            index += 1
        while _is_cte_modifier(_token_at(tokens, index)):
            index += 1
        opening = _token_at(tokens, index)
        if not _is_punct(opening, "(") or not _opens_statement(_token_at(tokens, index + 1)):
            return index
        index = after_close[index]
        index = _after_clause(tokens, index, "SEARCH", "SET")
        index = _after_clause(tokens, index, "CYCLE", "USING")
        scopes.append(_CteScope(_folded_name(name), with_index if recursive else index, scope_end))
        if not _is_punct(_token_at(tokens, index), ","):
            return index
        index += 1


def _is_cte_modifier(token: _Token | None) -> bool:
    return (
        _is_keyword(token, "AS") or _is_keyword(token, "NOT") or _is_keyword(token, "MATERIALIZED")
    )


def _opens_statement(token: _Token | None) -> bool:
    if _is_punct(token, "("):
        return True
    return (
        token is not None
        and token.kind == "word"
        and _upper_ascii(token.text) in _STATEMENT_KEYWORDS
    )


# _after_clause skips a SEARCH or CYCLE clause of a common table expression. Each ends
# with its last keyword and one column name. A clause holds no parenthesis in the forms
# it reads, and stopping at one keeps text without last_keyword from being scanned again
# by every WITH.
def _after_clause(tokens: list[_Token], start: int, first_keyword: str, last_keyword: str) -> int:
    if not _is_keyword(_token_at(tokens, start), first_keyword):
        return start
    index = start + 1
    while (
        index < len(tokens)
        and not _is_keyword(tokens[index], last_keyword)
        and not _is_punct(tokens[index], "(")
    ):
        index += 1
    return index + 2 if _is_keyword(_token_at(tokens, index), last_keyword) else index


# _scope_changes lists the scopes that open (+1) or close (-1) at each token. _table_names
# walks the tokens once and keeps a count per name. Many scopes of one name then cost no
# scan at each table keyword.
def _scope_changes(scopes: list[_CteScope]) -> dict[int, list[tuple[str, int]]]:
    changes: dict[int, list[tuple[str, int]]] = {}
    for scope in scopes:
        changes.setdefault(scope.start, []).append((scope.name, 1))
        changes.setdefault(scope.stop, []).append((scope.name, -1))
    return changes


def _op_of(tokens: list[_Token], index: int) -> str:
    first = _token_at(tokens, index)
    if first is None or first.kind != "word":
        return ""
    op = _upper_ascii(first.text)
    if op == "START" and _is_keyword(_token_at(tokens, index + 1), "TRANSACTION"):
        return "START TRANSACTION"
    return op


def _table_names(
    tokens: list[_Token], scopes: list[_CteScope], after_expressions: set[int]
) -> list[str]:
    # A dict keeps the order first seen, which a set does not.
    tables: dict[str, None] = {}
    changes = _scope_changes(scopes)
    visible_scopes: dict[str, int] = {}
    for index in range(len(tokens)):
        for name, delta in changes.get(index, ()):
            visible_scopes[name] = visible_scopes.get(name, 0) + delta
        if not _is_table_keyword(tokens, index):
            continue
        parts = _qualified_name(tokens, index + 1)
        if not parts:
            continue
        if (
            len(parts) == 1
            and not _is_write_target(tokens, index, after_expressions)
            and visible_scopes.get(_folded_name(parts[0]), 0) > 0
        ):
            continue
        tables[".".join(part.text for part in parts)] = None
    return list(tables)


# Postgres looks up the table of DELETE FROM, INSERT INTO, MERGE INTO and UPDATE without
# looking at common table expressions.
def _is_write_target(tokens: list[_Token], keyword_index: int, after_expressions: set[int]) -> bool:
    keyword = tokens[keyword_index]
    if _is_keyword(keyword, "FROM"):
        return _is_delete_command(tokens, keyword_index - 1, after_expressions)
    return not _is_keyword(keyword, "JOIN")


# _is_delete_command tells the DELETE command from a column named delete. With a common
# table expression in sight, the command opens the body of an expression or follows the
# list of expressions.
def _is_delete_command(tokens: list[_Token], index: int, after_expressions: set[int]) -> bool:
    return _is_keyword(_token_at(tokens, index), "DELETE") and (
        _is_punct(_token_at(tokens, index - 1), "(") or index in after_expressions
    )


def _is_table_keyword(tokens: list[_Token], index: int) -> bool:
    token = tokens[index]
    if token.kind != "word":
        return False
    keyword = _upper_ascii(token.text)
    if keyword not in _TABLE_KEYWORDS:
        return False
    if keyword == "FROM":
        return not _is_distinct_from_operator(tokens, index)
    if keyword != "UPDATE":
        return True
    previous = _token_at(tokens, index - 1)
    return not (
        previous is not None
        and previous.kind == "word"
        and _upper_ascii(previous.text) in _UPDATE_WITHOUT_TABLE
    )


# a IS [NOT] DISTINCT FROM b compares two values.
def _is_distinct_from_operator(tokens: list[_Token], from_index: int) -> bool:
    before = _token_at(tokens, from_index - 2)
    return _is_keyword(_token_at(tokens, from_index - 1), "DISTINCT") and (
        _is_keyword(before, "IS") or _is_keyword(before, "NOT")
    )


# _first_name_index skips what can sit between a table keyword and the name it introduces:
# LATERAL, ONLY, and the parenthesis of ONLY (name).
def _first_name_index(tokens: list[_Token], index: int) -> int:
    if _is_keyword(_token_at(tokens, index), "LATERAL"):
        return index + 1
    if not _is_keyword(_token_at(tokens, index), "ONLY"):
        return index
    return index + 2 if _is_punct(_token_at(tokens, index + 1), "(") else index + 1


# _qualified_name reads name or schema.name starting at index. A subquery, which starts
# with a parenthesis, has no name.
def _qualified_name(tokens: list[_Token], index: int) -> list[_Token]:
    parts: list[_Token] = []
    position = _first_name_index(tokens, index)
    while True:
        part = _token_at(tokens, position)
        if part is None or part.kind == "punct":
            return parts
        parts.append(part)
        if not _is_punct(_token_at(tokens, position + 1), "."):
            return parts
        position += 2
