import json
import threading
from types import SimpleNamespace

import pytest
from sqlalchemy import (
    Column,
    Integer,
    MetaData,
    String,
    Table,
    create_engine,
    event,
    insert,
    select,
    text,
)
from sqlalchemy.exc import IntegrityError, OperationalError, StatementError
from sqlalchemy.orm import DeclarativeBase, Mapped, Session, mapped_column
from sqlalchemy.pool import StaticPool

from spanlog import Spanlog, json_sink
from spanlog.sqlalchemy import _autocommits, log_queries

meta = MetaData()
items = Table(
    "items",
    meta,
    Column("id", Integer, primary_key=True),
    Column("email", String, unique=True),
)
SECRET = "alice@example.com"


class Recording:
    def __init__(self, level="debug", **options):
        self.written = []
        self.log = Spanlog(
            service="shop", sink=json_sink(self.written.append), level=level, **options
        )

    @property
    def lines(self):
        return [json.loads(text) for text in self.written]

    def queries(self):
        return [line for line in self.lines if line["action"] == "query"]


@pytest.fixture
def engine():
    created = create_engine(
        "sqlite://", poolclass=StaticPool, connect_args={"check_same_thread": False}
    )
    meta.create_all(created)
    yield created
    created.dispose()


def duplicate_insert(engine):
    with engine.begin() as conn:
        conn.execute(insert(items).values(email=SECRET))
        conn.execute(insert(items).values(email=SECRET))


def test_writes_op_and_tables_and_keeps_the_sql_out(engine):
    recording = Recording()
    log_queries(engine, recording.log)
    with engine.connect() as conn:
        conn.execute(select(items).where(items.c.email == SECRET))
    assert [(q["phase"], q["op"], q["tables"], q["target"]) for q in recording.queries()] == [
        ("START", "SELECT", ["items"], "SELECT items"),
        ("DONE", "SELECT", ["items"], "SELECT items"),
    ]
    assert SECRET not in "".join(recording.written)
    assert "WHERE" not in "".join(recording.written)


def test_query_lines_are_debug_and_hidden_at_info(engine):
    recording = Recording(level="info")
    log_queries(engine, recording.log)
    with engine.connect() as conn:
        conn.execute(text("SELECT 1"))
    assert recording.queries() == []


def test_a_slow_statement_ends_with_slow_at_info(engine):
    recording = Recording(level="info", slow_ms=-1)
    log_queries(engine, recording.log)
    with engine.connect() as conn:
        conn.execute(text("SELECT 1"))
    assert [(q["level"], q["phase"]) for q in recording.queries()] == [("warn", "SLOW")]


def test_lines_belong_to_the_frame_and_the_request(engine):
    recording = Recording()
    log_queries(engine, recording.log)

    class Repo:
        def load(self):
            with engine.connect() as conn:
                return conn.execute(select(items)).all()

    repo = recording.log.traced(Repo(), "Repo")
    with recording.log.with_request("r1"):
        repo.load()
    query = recording.queries()[0]
    assert (query["reqId"], query["scope"], query["method"]) == ("r1", "Repo", "load")


def test_two_threads_keep_their_own_request(tmp_path):
    engine = create_engine(f"sqlite:///{tmp_path / 'two.db'}")
    meta.create_all(engine)
    recording = Recording()
    log_queries(engine, recording.log)
    barrier = threading.Barrier(2)

    def work(req_id):
        with recording.log.with_request(req_id), engine.connect() as conn:
            barrier.wait()
            conn.execute(select(items))

    threads = [threading.Thread(target=work, args=(name,)) for name in ("a", "b")]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    engine.dispose()
    by_request = {}
    for query in recording.queries():
        by_request.setdefault(query["reqId"], []).append(query["phase"])
    assert by_request == {"a": ["START", "DONE"], "b": ["START", "DONE"]}


def test_failed_statement_writes_warn_without_stack_or_sql(engine):
    recording = Recording()
    log_queries(engine, recording.log)
    with pytest.raises(IntegrityError):
        duplicate_insert(engine)
    failed = [q for q in recording.queries() if q["phase"] == "FAILED"]
    assert len(failed) == 1
    assert failed[0]["level"] == "warn"
    assert failed[0]["err"] == {
        "name": "IntegrityError",
        "message": "UNIQUE constraint failed: items.email",
    }


def test_frame_line_of_a_statement_error_holds_no_sql_or_parameters(engine):
    recording = Recording()

    class Repo:
        def add(self):
            duplicate_insert(engine)

    repo = recording.log.traced(Repo(), "Repo")
    with pytest.raises(IntegrityError) as raised:
        repo.add()
    assert SECRET in str(raised.value)
    text_written = "".join(recording.written)
    assert SECRET not in text_written
    assert "INSERT" not in text_written
    failed = [line for line in recording.lines if line["phase"] == "FAILED"][0]
    assert failed["err"]["name"] == "IntegrityError"
    assert failed["err"]["message"] == "UNIQUE constraint failed: items.email"
    assert failed["err"]["stack"].startswith(
        "IntegrityError: UNIQUE constraint failed: items.email\n"
    )


def test_statement_error_without_a_driver_error_holds_no_sql_or_parameters():
    recording = Recording()

    class Repo:
        def run(self):
            raise StatementError("no driver error", "SELECT secret", {"email": SECRET}, None)

    with pytest.raises(StatementError) as raised:
        recording.log.traced(Repo(), "Repo").run()
    assert "SELECT secret" in str(raised.value)
    failed = [line for line in recording.lines if line["phase"] == "FAILED"][0]
    assert failed["err"]["message"] == "no driver error"
    assert "SELECT secret" not in "".join(recording.written)


def test_an_application_error_with_statement_and_orig_keeps_its_text():
    recording = Recording()

    class AppError(Exception):
        def __init__(self, message):
            super().__init__(message)
            self.statement = "business"
            self.orig = ValueError("private-token")

    class Repo:
        def run(self):
            raise AppError("public message")

    with pytest.raises(AppError):
        recording.log.traced(Repo(), "Repo").run()
    failed = [line for line in recording.lines if line["phase"] == "FAILED"][0]
    assert failed["err"]["message"] == "public message"
    assert failed["err"]["stack"].startswith("AppError: public message\n")
    assert "private-token" not in "".join(recording.written)


def test_error_message_still_replaces_the_text(engine):
    recording = Recording(error_message=lambda failure: "hidden")
    log_queries(engine, recording.log)
    with pytest.raises(IntegrityError):
        duplicate_insert(engine)
    failed = [q for q in recording.queries() if q["phase"] == "FAILED"]
    assert failed[0]["err"]["message"] == "hidden"


def test_rollback_written_as_a_statement_names_the_tables(engine):
    recording = Recording()
    log_queries(engine, recording.log)
    with engine.connect() as conn:
        conn.exec_driver_sql("BEGIN")
        conn.execute(insert(items).values(email="a"))
        conn.exec_driver_sql("ROLLBACK")
    done = [q for q in recording.queries() if q["phase"] == "DONE"]
    assert [(q["op"], q["level"], q["tables"]) for q in done] == [
        ("BEGIN", "debug", []),
        ("INSERT", "debug", ["items"]),
        ("ROLLBACK", "warn", ["items"]),
    ]


def test_a_transaction_without_a_write_writes_no_rollback_line(engine):
    recording = Recording()
    log_queries(engine, recording.log)
    with engine.begin() as conn:
        conn.execute(text("SELECT 1"))
    with engine.connect() as conn:
        conn.execute(select(items))
    assert [q["op"] for q in recording.queries()] == ["SELECT"] * 4


def test_a_rolled_back_transaction_names_the_tables_it_wrote(engine):
    recording = Recording()
    log_queries(engine, recording.log)
    with pytest.raises(RuntimeError), engine.begin() as conn:
        conn.execute(insert(items).values(email="a"))
        conn.execute(select(items))
        raise RuntimeError("payment declined")
    rollback = [q for q in recording.queries() if q["op"] == "ROLLBACK"]
    assert [(q["phase"], q["level"], q["target"], q["tables"]) for q in rollback] == [
        ("START", "debug", "ROLLBACK items", ["items"]),
        ("DONE", "warn", "ROLLBACK items", ["items"]),
    ]
    assert rollback[1]["ms"] >= 0


def test_session_rollback_after_a_flush_names_the_table(engine):
    recording = Recording()
    log_queries(engine, recording.log)
    with Session(engine) as session:
        session.execute(insert(items).values(email="a"))
        session.rollback()
    rollback = [(q["phase"], q["tables"]) for q in recording.queries() if q["op"] == "ROLLBACK"]
    assert rollback == [("START", ["items"]), ("DONE", ["items"])]


def test_a_committed_transaction_writes_no_rollback_line(engine):
    recording = Recording()
    log_queries(engine, recording.log)
    with engine.begin() as conn:
        conn.execute(insert(items).values(email="a"))
    with engine.connect() as conn:
        conn.execute(select(items))
    assert "ROLLBACK" not in [q["op"] for q in recording.queries()]


def test_autocommit_writes_no_rollback_line(engine):
    recording = Recording()
    log_queries(engine, recording.log)
    with engine.connect().execution_options(isolation_level="AUTOCOMMIT") as conn:
        conn.execute(insert(items).values(email="kept"))
    with engine.connect() as conn:
        assert conn.execute(select(items.c.email)).scalars().all() == ["kept"]
    assert "ROLLBACK" not in [q["op"] for q in recording.queries()]


def test_detach_gives_the_dialect_its_own_do_rollback(engine):
    recording = Recording()
    detach = log_queries(engine, recording.log)
    assert "do_rollback" in vars(engine.dialect)
    detach()
    assert "do_rollback" not in vars(engine.dialect)
    with pytest.raises(RuntimeError), engine.begin() as conn:
        conn.execute(insert(items).values(email="a"))
        raise RuntimeError("after detach")
    assert recording.written == []


def test_a_failing_connection_writes_no_line(tmp_path):
    engine = create_engine(f"sqlite:///{tmp_path / 'missing' / 'x.db'}")
    recording = Recording()
    log_queries(engine, recording.log)
    with pytest.raises(OperationalError), engine.connect():
        pass
    assert recording.written == []


def test_executemany_writes_one_pair(engine):
    recording = Recording()
    log_queries(engine, recording.log)
    with engine.begin() as conn:
        conn.execute(insert(items), [{"email": "a"}, {"email": "b"}, {"email": "c"}])
    assert [q["phase"] for q in recording.queries()] == ["START", "DONE"]


def test_orm_insert_batches_each_write_a_closed_pair(engine):
    class Base(DeclarativeBase):
        pass

    class Row(Base):
        __tablename__ = "rows"
        id: Mapped[int] = mapped_column(primary_key=True)

    Base.metadata.create_all(engine)
    recording = Recording()
    log_queries(engine, recording.log)
    with Session(engine) as session:
        session.add_all([Row() for _ in range(5)])
        session.commit()
    phases = [q["phase"] for q in recording.queries()]
    assert phases.count("START") == phases.count("DONE") >= 1


def test_detach_stops_the_lines(engine):
    recording = Recording()
    detach = log_queries(engine, recording.log)
    detach()
    with engine.connect() as conn:
        conn.execute(text("SELECT 1"))
    assert recording.written == []


def test_two_listeners_write_each_line_twice(engine):
    recording = Recording()
    log_queries(engine, recording.log)
    log_queries(engine, recording.log)
    with engine.connect() as conn:
        conn.execute(text("SELECT 1"))
    assert [q["phase"] for q in recording.queries()] == ["START", "START", "DONE", "DONE"]


def test_a_broken_sink_does_not_fail_the_statement(engine):
    class Broken:
        def write(self, rendered):
            raise RuntimeError("sink down")

    log_queries(engine, Spanlog(service="shop", sink=Broken(), level="debug"))
    with pytest.warns(RuntimeWarning), engine.connect() as conn:
        assert conn.execute(text("SELECT 1")).scalar() == 1


def test_refuses_what_is_not_an_engine():
    with pytest.raises(TypeError, match="Engine or an AsyncEngine"):
        log_queries(object(), Recording().log)


# The insert stays in the open transaction of the driver after the stopped rollback, and the
# commit keeps it. Connection.begin and commit run no statement in between.
@pytest.mark.parametrize("commit_with", ["Connection.begin and commit", "a COMMIT statement"])
def test_a_rollback_stopped_by_another_listener_leaves_nothing_armed(engine, commit_with):
    recording = Recording()
    log_queries(engine, recording.log)

    def stop(conn):
        raise RuntimeError("stopped")

    event.listen(engine, "rollback", stop)
    with engine.connect() as conn:
        conn.execute(insert(items).values(email="a"))
        with pytest.raises(RuntimeError):
            conn.rollback()
        event.remove(engine, "rollback", stop)
        if commit_with == "Connection.begin and commit":
            conn.begin()
            conn.commit()
        else:
            conn.exec_driver_sql("COMMIT")
    with engine.connect() as conn:
        assert conn.execute(select(items.c.email)).scalars().all() == ["a"]
    assert "ROLLBACK" not in [q["op"] for q in recording.queries()]


# A statement that does not end the transaction leaves the insert in it, and the rollback of
# the close undoes the insert.
def test_a_rollback_after_a_stopped_one_and_a_read_names_the_tables_it_undoes(engine):
    recording = Recording()
    log_queries(engine, recording.log)

    def stop(conn):
        raise RuntimeError("stopped")

    event.listen(engine, "rollback", stop)
    with engine.connect() as conn:
        conn.execute(insert(items).values(email="a"))
        with pytest.raises(RuntimeError):
            conn.rollback()
        event.remove(engine, "rollback", stop)
        conn.execute(text("SELECT 1"))
    with engine.connect() as conn:
        assert conn.execute(select(items.c.email)).scalars().all() == []
    rollbacks = [q for q in recording.queries() if q["op"] == "ROLLBACK"]
    assert [q["phase"] for q in rollbacks] == ["START", "DONE"]
    assert all(q["tables"] == ["items"] for q in rollbacks)


@pytest.mark.parametrize("order", ["first attached first", "last attached first"])
def test_detach_in_any_order_gives_the_dialect_its_own_do_rollback(engine, order):
    first = log_queries(engine, Recording().log)
    second = log_queries(engine, Recording().log)
    detaches = [first, second] if order == "first attached first" else [second, first]
    for detach in detaches:
        detach()
    assert "do_rollback" not in vars(engine.dialect)


def test_a_dialect_that_cannot_detect_autocommit_is_taken_as_not_in_autocommit():
    assert _autocommits(SimpleNamespace(), object()) is False
