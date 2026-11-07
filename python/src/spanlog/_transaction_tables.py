"""The tables written by the open transaction of one connection."""

from __future__ import annotations

from ._sql import chains_transaction, rolls_back_to_savepoint, summarize_sql

_BEGIN_OPS = frozenset({"BEGIN", "START TRANSACTION"})
# ABORT is a ROLLBACK in Postgres. Its line keeps the op ABORT. That line stays at debug and
# names no table.
_END_OPS = frozenset({"COMMIT", "END", "ABORT"})
# A write inside a common table expression of a SELECT is not seen, because the op of that
# statement is SELECT.
_WRITE_OPS = frozenset({"INSERT", "UPDATE", "DELETE", "MERGE"})


class TransactionTables:
    """Follows the statements of one connection and keeps the tables its open transaction wrote.

    A ROLLBACK line names them. The rules are those of the query lines in docs/log-format.md.
    begin, end and roll_back serve a driver that runs a transaction without a statement.
    """

    def __init__(self) -> None:
        # _levels[0] holds the writes of the transaction, and each later entry those of one
        # savepoint, innermost last. ROLLBACK TO and RELEASE act on the innermost savepoint,
        # whatever name they give, as TypeORM and SQLAlchemy nest them.
        self._levels: list[list[str]] = []

    def statement_starting(self, sql: str) -> list[str] | None:
        """Run before a statement. For a ROLLBACK, return the tables the rollback undoes.

        Any other statement returns None. A statement that ends the transaction ends it here,
        because Postgres also ends a transaction whose COMMIT fails.
        """
        op = summarize_sql(sql).op
        if op in _BEGIN_OPS:
            self.begin()
        elif op in _END_OPS:
            self.end()
        elif op == "SAVEPOINT":
            self._open_savepoint()
        elif op == "RELEASE":
            self._release_savepoint()
        elif op == "ROLLBACK":
            if rolls_back_to_savepoint(sql):
                return self._roll_back_to_savepoint()
            return self.roll_back()
        return None

    def statement_done(self, sql: str) -> None:
        """Run after a statement that ended without an error.

        A statement with AND CHAIN opens the next transaction here, because Postgres opens it
        only when that statement succeeds.
        """
        summary = summarize_sql(sql)
        if summary.op in _WRITE_OPS and self._levels:
            _add_new(self._levels[-1], summary.tables)
        elif (summary.op in _END_OPS or summary.op == "ROLLBACK") and chains_transaction(sql):
            self.begin()

    def begin(self) -> None:
        self._levels = [[]]

    def end(self) -> None:
        self._levels = []

    def roll_back(self) -> list[str]:
        undone: list[str] = []
        for level in self._levels:
            _add_new(undone, level)
        self._levels = []
        return undone

    # Postgres keeps the savepoint open after ROLLBACK TO, with nothing written since.
    def _roll_back_to_savepoint(self) -> list[str]:
        if len(self._levels) < 2:
            return []
        undone = self._levels[-1]
        self._levels[-1] = []
        return undone

    def _open_savepoint(self) -> None:
        if self._levels:
            self._levels.append([])

    def _release_savepoint(self) -> None:
        if len(self._levels) < 2:
            return
        released = self._levels.pop()
        _add_new(self._levels[-1], released)


def _add_new(into: list[str], tables: list[str]) -> None:
    for table in tables:
        if table not in into:
            into.append(table)
