import asyncio
import concurrent.futures

from spanlog import get_request_id
from support import make


def test_request_span_writes_once_and_keeps_the_id():
    log, sink = make()
    with log.with_request("r1"):
        span = log.start_request("GET /a")
        span.end(503)
        span.abort()
    assert [r.fields["phase"] for r in sink.rendered] == ["START", "FAILED"]
    assert sink.rendered[-1].fields["status"] == 503
    assert sink.rendered[-1].fields["reqId"] == "r1"


def test_request_id_absent_outside_a_request():
    assert get_request_id() is None


def test_to_thread_carries_the_request_and_a_plain_executor_does_not():
    log, _ = make()

    async def main():
        with log.with_request("r1"):
            carried = await asyncio.to_thread(get_request_id)
            with concurrent.futures.ThreadPoolExecutor() as pool:
                lost = pool.submit(get_request_id).result()
            return carried, lost

    assert asyncio.run(main()) == ("r1", None)
