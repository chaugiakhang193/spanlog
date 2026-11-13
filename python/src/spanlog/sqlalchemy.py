"""Query lines for SQLAlchemy. See docs/python.md."""

from __future__ import annotations

import threading
import weakref
from collections.abc import Callable
from typing import Any

try:
    from sqlalchemy import event
    from sqlalchemy.engine import Connection, Engine, ExceptionContext
    from sqlalchemy.engine.interfaces import DBAPICursor, Dialect, ExecutionContext
except ModuleNotFoundError as error:
    # SQLAlchemy is the optional extra of this module.
    # Errors from other missing modules pass unchanged.
    if error.name != "sqlalchemy":
        raise
    raise ModuleNotFoundError(
        'spanlog.sqlalchemy needs SQLAlchemy. Install it with: pip install "spanlog[sqlalchemy]"',
        name="sqlalchemy",
    ) from error

from ._core import QuerySpan, Spanlog
from ._transaction_tables import TransactionTables, ends_transaction


class _RunningQueries:
    """Carries a span from the event before a statement to the event after it.

    ExecutionContext identifies one statement, and two connections on two threads share
    this object. A weak key lets a statement that never reached an end event go away. The
    tables written by the open transaction of each Connection are kept the same way.
    """

    def __init__(self, log: Spanlog) -> None:
        self._log = log
        self._spans: weakref.WeakKeyDictionary[ExecutionContext, QuerySpan] = (
            weakref.WeakKeyDictionary()
        )
        self._transactions: weakref.WeakKeyDictionary[Connection, TransactionTables] = (
            weakref.WeakKeyDictionary()
        )
        self._lock = threading.Lock()
        # The rollback event arms this slot, and the wrapper of do_rollback that SQLAlchemy
        # calls next on the same thread takes it.
        self._armed = threading.local()

    def _take(self, context: ExecutionContext) -> QuerySpan | None:
        with self._lock:
            return self._spans.pop(context, None)

    def _transaction_of(self, conn: Connection) -> TransactionTables:
        with self._lock:
            transaction = self._transactions.get(conn)
            if transaction is None:
                transaction = TransactionTables()
                self._transactions[conn] = transaction
            return transaction

    def before(
        self,
        conn: Connection,
        cursor: DBAPICursor,
        statement: str,
        parameters: Any,
        context: ExecutionContext,
        executemany: bool,
    ) -> None:
        # A slot still armed here belongs to a rollback that never reached do_rollback, as when
        # another listener stops it. Its writes stay in the transaction of the driver, and a
        # statement that ends that transaction on the same connection leaves the slot nothing
        # to time. Any other statement keeps it for the rollback that undoes those writes.
        armed = getattr(self._armed, "rollback", None)
        if armed is not None and armed[0] is conn.connection and ends_transaction(statement):
            self._armed.rollback = None
        rolled_back = self._transaction_of(conn).statement_starting(statement)
        span = self._log.start_query(statement, rolled_back_tables=rolled_back)
        with self._lock:
            self._spans[context] = span

    def after(
        self,
        conn: Connection,
        cursor: DBAPICursor,
        statement: str,
        parameters: Any,
        context: ExecutionContext,
        executemany: bool,
    ) -> None:
        self._transaction_of(conn).statement_done(statement)
        span = self._take(context)
        if span is not None:
            span.end()

    # execution_context is None when the connection itself fails, before any statement.
    # The exception of SQLAlchemy names the same class as the frame that receives it.
    def failed(self, exception_context: ExceptionContext) -> None:
        context = exception_context.execution_context
        span = None if context is None else self._take(context)
        if span is not None:
            span.fail(
                exception_context.sqlalchemy_exception or exception_context.original_exception
            )

    # SQLAlchemy runs BEGIN, COMMIT and the ROLLBACK of a transaction through the driver,
    # without a statement and without an event after the call.
    def begun(self, conn: Connection) -> None:
        self._transaction_of(conn).begin()

    # A rollback that another listener stopped never reaches do_rollback, and its writes stay
    # in the open transaction until a later rollback undoes them. A commit keeps them, and
    # the slot armed for them must not time the rollback of the pool reset after it.
    def committed(self, conn: Connection) -> None:
        self._armed.rollback = None
        self._transaction_of(conn).end()

    # SQLAlchemy rolls back every connection that closes without a commit, reads included.
    # Only a rollback that undoes a write is armed. SQLAlchemy skips do_rollback on a closed
    # or invalidated connection, and a connection in autocommit has nothing to undo.
    def rolling_back(self, conn: Connection) -> None:
        undone = self._transaction_of(conn).roll_back()
        if not undone or conn.closed or conn.invalidated:
            return
        pooled = conn.connection
        if _autocommits(conn.dialect, pooled.dbapi_connection):
            return
        self._armed.rollback = (pooled, undone)

    # SQLAlchemy also calls do_rollback on the first connect and to reset a connection that
    # goes back to the pool. Those calls find the slot empty or armed for another connection.
    def time_rollback(self, original: Callable[[Any], None], pooled: Any) -> None:
        armed = getattr(self._armed, "rollback", None)
        self._armed.rollback = None
        if armed is None or armed[0] is not pooled:
            original(pooled)
            return
        span = self._log.start_query("ROLLBACK", rolled_back_tables=armed[1])
        try:
            original(pooled)
        except BaseException as failure:
            span.fail(failure)
            raise
        span.end()


# SQLAlchemy 2.0.43 added detect_autocommit_setting. A dialect without it, or one that raises
# NotImplementedError, is taken as not in autocommit.
def _autocommits(dialect: Dialect, dbapi_connection: Any) -> bool:
    detect = getattr(dialect, "detect_autocommit_setting", None)
    if detect is None or dbapi_connection is None:
        return False
    try:
        return bool(detect(dbapi_connection))
    except NotImplementedError:
        return False


class _RollbackTimer:
    """The do_rollback that log_queries puts in the instance dictionary of a dialect.

    previous is what the dictionary held before, or None when the method of the class served.
    A detached timer that another one still calls passes every call through.
    """

    def __init__(self, running: _RunningQueries, original: Callable[[Any], None], previous: Any):
        self.running = running
        self.original = original
        self.previous = previous
        self.detached = False

    def __call__(self, pooled: Any) -> None:
        if self.detached:
            self.original(pooled)
        else:
            self.running.time_rollback(self.original, pooled)


def _time_rollbacks(dialect: Dialect, running: _RunningQueries) -> Callable[[], None]:
    """Wrap do_rollback of dialect and return the function that removes the wrapper.

    The wrapper goes into the instance dictionary of dialect, and the method of its class
    stays as it is. The pool and every engine made by execution_options share dialect.
    """
    own = vars(dialect)
    timer = _RollbackTimer(running, dialect.do_rollback, own.get("do_rollback"))
    own["do_rollback"] = timer

    # A timer added over this one keeps calling it until that timer is removed too. The
    # removal of the top timer also drops every detached timer under it.
    def unwrap() -> None:
        timer.detached = True
        if own.get("do_rollback") is not timer:
            return
        restore = timer.previous
        while isinstance(restore, _RollbackTimer) and restore.detached:
            restore = restore.previous
        if restore is None:
            del own["do_rollback"]
        else:
            own["do_rollback"] = restore

    return unwrap


def log_queries(engine: Any, log: Spanlog) -> Callable[[], None]:
    """Write a START line before each SQL statement and an end line after it.

    engine is an Engine or an AsyncEngine. The lines belong to the frame that runs the
    statement. A ROLLBACK names the tables its transaction wrote. A rollback that SQLAlchemy
    runs through the driver writes its lines only when the transaction wrote a table, and
    log_queries wraps do_rollback of the dialect of engine to time it. It returns a function
    that removes the listeners and the wrapper, which must not run while a statement of the
    engine is running. A statement running at that moment keeps its START line without an
    end line.
    """
    target = getattr(engine, "sync_engine", engine)
    if not isinstance(target, Engine):
        raise TypeError(
            f"spanlog: log_queries needs an Engine or an AsyncEngine, and got "
            f"{type(engine).__name__}. Pass the result of create_engine or create_async_engine."
        )
    running = _RunningQueries(log)
    listeners: list[tuple[str, Callable[..., None]]] = [
        ("before_cursor_execute", running.before),
        ("after_cursor_execute", running.after),
        ("handle_error", running.failed),
        ("begin", running.begun),
        ("commit", running.committed),
        ("rollback", running.rolling_back),
    ]
    for name, listener in listeners:
        event.listen(target, name, listener)
    unwrap = _time_rollbacks(target.dialect, running)

    def detach() -> None:
        for name, listener in listeners:
            event.remove(target, name, listener)
        unwrap()

    return detach
