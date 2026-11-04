import asyncio
import json

import pytest
from sqlalchemy import Column, Integer, MetaData, String, Table, create_engine, insert, select, text
from sqlalchemy.exc import IntegrityError
from sqlalchemy.ext.asyncio import create_async_engine
from sqlalchemy.orm import DeclarativeBase, Mapped, Session, mapped_column

from spanlog import Spanlog, json_sink
from spanlog.sqlalchemy import log_queries

pytestmark = pytest.mark.pg

ASYNC_URL = "postgresql+asyncpg://spanlog:spanlog@127.0.0.1:5438/spanlog"
SYNC_URL = "postgresql+psycopg://spanlog:spanlog@127.0.0.1:5438/spanlog"
SECRET = "alice@example.com"
UNIQUE_MESSAGE = 'duplicate key value violates unique constraint "pg_people_email_key"'

meta = MetaData()
people = Table(
    "pg_people",
    meta,
    Column("id", Integer, primary_key=True),
    Column("email", String, unique=True),
)


class Base(DeclarativeBase):
    pass


class Row(Base):
    __tablename__ = "pg_rows"
    id: Mapped[int] = mapped_column(primary_key=True)
    name: Mapped[str] = mapped_column(String)


class Recording:
    def __init__(self):
        self.written = []
        self.log = Spanlog(service="shop", sink=json_sink(self.written.append), level="debug")

    @property
    def lines(self):
        return [json.loads(line) for line in self.written]

    def queries(self):
        return [line for line in self.lines if line["action"] == "query"]


def assert_statement_error_is_clean(recording):
    written = "".join(recording.written)
    assert SECRET not in written
    assert "VALUES" not in written
    failed = [line for line in recording.lines if line["phase"] == "FAILED"]
    assert [(line["err"]["name"], line["err"]["message"]) for line in failed] == [
        ("IntegrityError", failed[0]["err"]["message"])
    ] * len(failed)
    assert UNIQUE_MESSAGE in failed[0]["err"]["message"]
    assert "\n" not in failed[0]["err"]["message"]


def in_async_tables(body):
    async def run():
        engine = create_async_engine(ASYNC_URL)
        async with engine.begin() as conn:
            await conn.run_sync(meta.drop_all)
            await conn.run_sync(meta.create_all)
        try:
            await body(engine)
        finally:
            async with engine.begin() as conn:
                await conn.run_sync(meta.drop_all)
            await engine.dispose()

    asyncio.run(run())


def test_asyncpg_two_tasks_keep_their_own_frame():
    recording = Recording()

    class Repo:
        def __init__(self, engine):
            self.engine = engine

        async def first(self):
            await self.load()

        async def second(self):
            await self.load()

        async def load(self):
            async with self.engine.connect() as conn:
                await asyncio.sleep(0)
                await conn.execute(select(people))

    async def body(engine):
        log_queries(engine, recording.log)
        repo = recording.log.traced(Repo(engine), "Repo")

        async def one(req_id, method):
            with recording.log.with_request(req_id):
                await method()

        await asyncio.gather(one("a", repo.first), one("b", repo.second))

    in_async_tables(body)
    # The tables are dropped through the same engine after the two requests.
    queries = [q for q in recording.queries() if "reqId" in q]
    assert sorted((q["reqId"], q["phase"]) for q in queries) == [
        ("a", "DONE"),
        ("a", "START"),
        ("b", "DONE"),
        ("b", "START"),
    ]
    assert {q["method"] for q in queries} == {"load"}
    assert {q["target"] for q in queries} == {"SELECT pg_people"}


def test_asyncpg_failed_statement_holds_no_sql_or_parameters():
    recording = Recording()

    class Repo:
        def __init__(self, engine):
            self.engine = engine

        async def add(self):
            async with self.engine.begin() as conn:
                await conn.execute(insert(people).values(email=SECRET))
                await conn.execute(insert(people).values(email=SECRET))

    async def body(engine):
        log_queries(engine, recording.log)
        with pytest.raises(IntegrityError):
            await recording.log.traced(Repo(engine), "Repo").add()

    in_async_tables(body)
    assert_statement_error_is_clean(recording)


def test_asyncpg_cancelled_statement_ends_with_failed():
    recording = Recording()

    async def body(engine):
        log_queries(engine, recording.log)

        async def long():
            async with engine.connect() as conn:
                await conn.execute(text("SELECT pg_sleep(5)"))

        task = asyncio.create_task(long())
        await asyncio.sleep(0.3)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    in_async_tables(body)
    failed = [q for q in recording.queries() if q["phase"] == "FAILED"]
    assert [(q["target"], q["err"]["name"]) for q in failed] == [("SELECT", "CancelledError")]


def test_asyncpg_orm_insert_batches_each_write_a_closed_pair():
    recording = Recording()

    async def body(engine):
        async with engine.begin() as conn:
            await conn.run_sync(Base.metadata.drop_all)
            await conn.run_sync(Base.metadata.create_all)
        log_queries(engine, recording.log)

        def add_rows(sync):
            with Session(bind=sync) as session:
                session.add_all([Row(name=str(n)) for n in range(2500)])
                session.flush()

        async with engine.begin() as conn:
            await conn.run_sync(add_rows)
        async with engine.begin() as conn:
            await conn.run_sync(Base.metadata.drop_all)

    in_async_tables(body)
    inserts = [q["phase"] for q in recording.queries() if q["op"] == "INSERT"]
    assert inserts.count("START") == inserts.count("DONE") >= 2


@pytest.fixture
def sync_engine():
    engine = create_engine(SYNC_URL)
    meta.drop_all(engine)
    meta.create_all(engine)
    yield engine
    meta.drop_all(engine)
    engine.dispose()


def test_psycopg_failed_statement_holds_no_sql_or_parameters(sync_engine):
    recording = Recording()
    log_queries(sync_engine, recording.log)

    class Repo:
        def add(self):
            with sync_engine.begin() as conn:
                conn.execute(insert(people).values(email=SECRET))
                conn.execute(insert(people).values(email=SECRET))

    with pytest.raises(IntegrityError) as raised:
        recording.log.traced(Repo(), "Repo").add()
    assert "DETAIL" in str(raised.value.orig)
    assert_statement_error_is_clean(recording)
