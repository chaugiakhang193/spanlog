"""Real uvicorn and waitress over real sockets. Lines are read from a list sink."""

import asyncio
import gc
import socket
import sys
import threading
import time

import pytest
import uvicorn
from fastapi import BackgroundTasks, FastAPI, Request
from fastapi.responses import StreamingResponse
from flask import Flask, Response
from waitress.server import create_server

from spanlog import Spanlog, get_request_id
from spanlog.asgi import LogRequests as AsgiLogRequests
from spanlog.asgi import _path_of as asgi_path_of
from spanlog.asgi import _ResponseState
from spanlog.wsgi import LogRequests as WsgiLogRequests
from spanlog.wsgi import _path_of as wsgi_path_of


class ListSink:
    def __init__(self):
        self.lines = []

    def write(self, rendered):
        self.lines.append(rendered.msg + (" req=" + rendered.fields.get("reqId", "-")))


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def fetch(port, path, method="GET", body=b"", headers="", leave_after=None):
    with socket.create_connection(("127.0.0.1", port), timeout=10) as sock:
        head = f"{method} {path} HTTP/1.1\r\nHost: x\r\nConnection: close\r\n"
        head += f"Content-Length: {len(body)}\r\n{headers}\r\n"
        sock.sendall(head.encode() + body)
        if leave_after is not None:
            time.sleep(leave_after)
            return None
        data = b""
        while chunk := sock.recv(65536):
            data += chunk
        return data


def wait_for(predicate, timeout=5.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.02)
    return False


class Inventory:
    async def reserve(self):
        await asyncio.sleep(0)
        raise ValueError("stock not available")

    def reserve_sync(self):
        raise ValueError("stock not available")


class Orders:
    def __init__(self, inventory):
        self.inventory = inventory

    async def handle(self):
        return await self.inventory.reserve()

    def handle_sync(self):
        return self.inventory.reserve_sync()


@pytest.fixture
def asgi_server():
    sink = ListSink()
    log = Spanlog(service="shop", sink=sink, utc_offset_minutes=0)
    orders = log.traced(Orders(log.traced(Inventory(), "Inventory")), "Orders")
    app = FastAPI()
    app.add_middleware(AsgiLogRequests, log=log)

    @app.get("/fast")
    async def fast():
        return {"id": get_request_id()}

    @app.get("/worker")
    def worker():
        return {"id": get_request_id()}

    @app.get("/slow")
    async def slow():
        await asyncio.sleep(0.5)
        return {"ok": True}

    @app.get("/slowsync")
    def slowsync():
        time.sleep(0.5)
        return {"ok": True}

    @app.get("/stream")
    async def stream():
        async def body():
            for _ in range(50):
                yield b"x" * 1024
                await asyncio.sleep(0.05)

        return StreamingResponse(body())

    @app.get("/short-stream")
    async def short_stream():
        async def body():
            yield b"a"
            yield b"b"

        return StreamingResponse(body())

    @app.post("/unread")
    async def unread():
        return {"ok": True}

    @app.post("/read")
    async def read(request: Request):
        return {"n": len(await request.body())}

    @app.get("/background")
    async def background(tasks: BackgroundTasks):
        tasks.add_task(asyncio.sleep, 0.6)
        return {"ok": True}

    @app.get("/background-fails")
    async def background_fails(tasks: BackgroundTasks):
        async def fail_later():
            await asyncio.sleep(0.6)
            raise ValueError("after the response")

        tasks.add_task(fail_later)
        return {"ok": True}

    @app.get("/boom")
    async def boom():
        await orders.handle()

    @app.get("/boom-worker")
    def boom_worker():
        orders.handle_sync()

    port = free_port()
    server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=port, log_level="critical"))
    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()
    assert wait_for(lambda: server.started)
    yield port, sink
    server.should_exit = True
    thread.join(5)


def lines_end(sink, text):
    return wait_for(lambda: any(text in line for line in sink.lines))


def test_asgi_complete(asgi_server):
    port, sink = asgi_server
    reply = fetch(port, "/fast?token=1", headers="X-Request-Id: abc\r\n")
    assert b"x-request-id: abc" in reply.lower() and b'"id":"abc"' in reply
    assert lines_end(sink, "GET /fast | DONE 200")
    assert sink.lines[0] == "GET /fast | START req=abc"


def test_asgi_worker_thread_sees_request(asgi_server):
    port, _ = asgi_server
    reply = fetch(port, "/worker", headers="X-Request-Id: abc\r\n")
    assert b'"id":"abc"' in reply


def test_asgi_line_break_in_path_stays_escaped(asgi_server):
    port, sink = asgi_server
    fetch(port, "/a%0Ab?token=1")
    assert lines_end(sink, "GET /a%0Ab | DONE 404")


def test_asgi_repeated_or_unsafe_id_is_replaced(asgi_server):
    port, sink = asgi_server
    fetch(port, "/fast", headers="X-Request-Id: a\r\nX-Request-Id: b\r\n")
    fetch(port, "/fast", headers="X-Request-Id: a b\r\n")
    assert wait_for(lambda: len(sink.lines) >= 4)
    assert not [line for line in sink.lines if line.endswith((" req=a", " req=a b", " req=a, b"))]


@pytest.mark.parametrize("path", ["/slow", "/slowsync"])
def test_asgi_client_left_during_handler(asgi_server, path):
    port, sink = asgi_server
    fetch(port, path, leave_after=0.1)
    assert lines_end(sink, f"GET {path} | ABORTED")


def test_asgi_slow_handler_client_stays(asgi_server):
    port, sink = asgi_server
    fetch(port, "/slow")
    assert lines_end(sink, "GET /slow | DONE 200") or lines_end(sink, "GET /slow | SLOW 200")


def test_asgi_client_left_during_stream(asgi_server):
    port, sink = asgi_server
    fetch(port, "/stream", leave_after=0.2)
    assert lines_end(sink, "GET /stream | ABORTED")


def test_asgi_complete_stream_is_not_aborted(asgi_server):
    port, sink = asgi_server
    fetch(port, "/short-stream")
    assert lines_end(sink, "GET /short-stream | DONE 200")


@pytest.mark.parametrize("path", ["/unread", "/read"])
def test_asgi_body_read_or_unread_is_done(asgi_server, path):
    port, sink = asgi_server
    fetch(port, path, method="POST", body=b"abc")
    assert lines_end(sink, f"POST {path} | DONE 200")


def test_asgi_handler_reads_the_whole_body(asgi_server):
    port, _ = asgi_server
    assert b'{"n":3}' in fetch(port, "/read", method="POST", body=b"abc")


@pytest.mark.parametrize("path", ["/background", "/background-fails"])
def test_asgi_background_task_stays_out_of_the_request_line(asgi_server, path):
    port, sink = asgi_server
    reply = fetch(port, path)
    assert b"200" in reply.split(b"\r\n", 1)[0]
    assert lines_end(sink, f"GET {path} | DONE 200")
    time.sleep(0.7)
    assert len([line for line in sink.lines if line.startswith(f"GET {path} | ")]) == 2


def test_asgi_exception_reports_once(asgi_server):
    port, sink = asgi_server
    reply = fetch(port, "/boom", headers="X-Request-Id: abc\r\n")
    assert b"500" in reply.split(b"\r\n", 1)[0]
    assert lines_end(sink, "GET /boom | FAILED 500")
    joined = "\n".join(sink.lines)
    assert "Orders.handle - call Inventory.reserve | FAILED" in joined
    assert "Orders.handle - run | FAILED" in joined


def test_asgi_exception_in_worker_thread(asgi_server):
    port, sink = asgi_server
    fetch(port, "/boom-worker")
    assert lines_end(sink, "GET /boom-worker | FAILED 500")
    assert "Orders.handle_sync - call Inventory.reserve_sync | FAILED" in "\n".join(sink.lines)


@pytest.fixture
def wsgi_server():
    sink = ListSink()
    log = Spanlog(service="shop", sink=sink, utc_offset_minutes=0)
    orders = log.traced(Orders(log.traced(Inventory(), "Inventory")), "Orders")
    app = Flask(__name__)
    seen = {}

    @app.get("/fast")
    def fast():
        return {"id": get_request_id()}

    @app.get("/boom")
    def boom():
        orders.handle_sync()

    @app.get("/stream")
    def stream():
        def body():
            for index in range(200):
                seen[index] = get_request_id()
                yield b"x" * 1024 * 256
                time.sleep(0.02)

        return Response(body())

    @app.get("/short-stream")
    def short_stream():
        def body():
            seen["short"] = get_request_id()
            yield b"a"

        return Response(body())

    app.wsgi_app = WsgiLogRequests(app.wsgi_app, log=log)
    port = free_port()
    server = create_server(app, host="127.0.0.1", port=port, threads=4)
    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()
    time.sleep(0.2)
    yield port, sink, seen
    # close of a single-socket server leaves the worker threads of its dispatcher running.
    server.close()
    server.task_dispatcher.shutdown()
    thread.join(5)
    assert not thread.is_alive()


def test_wsgi_complete(wsgi_server):
    port, sink, _ = wsgi_server
    reply = fetch(port, "/fast?token=1", headers="X-Request-Id: abc\r\n")
    assert b"x-request-id: abc" in reply.lower() and b'"id":"abc"' in reply
    assert lines_end(sink, "GET /fast | DONE 200 ")
    assert sink.lines[0] == "GET /fast | START req=abc"


def test_wsgi_line_break_in_path_stays_escaped(wsgi_server):
    port, sink, _ = wsgi_server
    fetch(port, "/a%0Ab?token=1")
    assert lines_end(sink, "GET /a%0Ab | DONE 404")


def test_wsgi_repeated_id_is_replaced(wsgi_server):
    port, sink, _ = wsgi_server
    fetch(port, "/fast", headers="X-Request-Id: a\r\nX-Request-Id: b\r\n")
    assert wait_for(lambda: len(sink.lines) >= 2)
    assert not [line for line in sink.lines if line.endswith((" req=a", " req=a, b"))]


def test_wsgi_body_runs_in_request_context(wsgi_server):
    port, sink, seen = wsgi_server
    fetch(port, "/short-stream", headers="X-Request-Id: abc\r\n")
    assert lines_end(sink, "GET /short-stream | DONE 200")
    assert seen["short"] == "abc"


def test_wsgi_handled_exception_reports_and_ends_500(wsgi_server):
    port, sink, _ = wsgi_server
    fetch(port, "/boom")
    assert lines_end(sink, "GET /boom | FAILED 500")
    assert "Orders.handle_sync - call Inventory.reserve_sync | FAILED" in "\n".join(sink.lines)


def test_wsgi_client_left_during_stream(wsgi_server):
    port, sink, _ = wsgi_server
    fetch(port, "/stream", leave_after=0.2)
    assert lines_end(sink, "GET /stream | ABORTED")


def test_asgi_path_without_raw_path_is_escaped():
    assert asgi_path_of({"path": "/a\nb"}) == "/a%0Ab"


def test_wsgi_path_without_raw_uri_is_escaped():
    environ = {"SCRIPT_NAME": "/app", "PATH_INFO": "/a\nb"}
    assert wsgi_path_of(environ) == "/app/a%0Ab"


def poll_state(receive):
    async def never_sent(message):
        raise AssertionError("send must not run")

    return _ResponseState(receive, never_sent, None, b"x-request-id", "abc")


def test_asgi_poll_is_skipped_without_an_asyncio_loop():
    async def never_called():
        raise AssertionError("receive must not run")

    with pytest.raises(StopIteration) as stopped:
        poll_state(never_called)._disconnect_is_waiting().send(None)
    assert stopped.value.value is False


def test_asgi_poll_does_not_wait_for_a_receive_that_ignores_cancel():
    async def main():
        release = asyncio.Event()

        async def stubborn():
            while not release.is_set():
                try:
                    await release.wait()
                except asyncio.CancelledError:
                    pass
            return {"type": "http.request"}

        poll = asyncio.ensure_future(poll_state(stubborn)._disconnect_is_waiting())
        done, _ = await asyncio.wait({poll}, timeout=1)
        release.set()
        assert done and poll.result() is False

    asyncio.run(main())


def test_asgi_poll_cancelled_leaves_no_task_behind():
    async def main():
        async def forever():
            await asyncio.Event().wait()

        poll = asyncio.ensure_future(poll_state(forever)._disconnect_is_waiting())
        await asyncio.sleep(0)
        poll.cancel()
        with pytest.raises(asyncio.CancelledError):
            await poll
        await asyncio.sleep(0)
        assert asyncio.all_tasks() == {asyncio.current_task()}

    asyncio.run(main())


def test_asgi_poll_keeps_a_failing_receive_out_of_send():
    async def main():
        async def failing():
            raise OSError("receive failed")

        assert await poll_state(failing)._disconnect_is_waiting() is False

    asyncio.run(main())


def test_asgi_error_after_the_last_byte_keeps_the_response_line():
    late = ValueError("after the response")

    async def app(scope, receive, send):
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send({"type": "http.response.body", "body": b"ok"})
        raise late

    async def receive():
        await asyncio.Event().wait()

    async def send(message):
        pass

    sink = ListSink()
    log = Spanlog(service="shop", sink=sink, utc_offset_minutes=0)
    scope = {"type": "http", "method": "GET", "path": "/x", "headers": []}
    with pytest.raises(ValueError) as raised:
        asyncio.run(AsgiLogRequests(app, log=log)(scope, receive, send))
    assert raised.value is late
    assert len(sink.lines) == 2 and sink.lines[1].startswith("GET /x | DONE 200 ")


class ClosableBody:
    def __init__(self, chunks, fail_on_iter=False):
        self.chunks = chunks
        self.fail_on_iter = fail_on_iter
        self.closed = False

    def __iter__(self):
        if self.fail_on_iter:
            raise ValueError("body failed")
        return iter(self.chunks)

    def close(self):
        self.closed = True


def wsgi_call(app):
    sink = ListSink()
    log = Spanlog(service="shop", sink=sink, utc_offset_minutes=0)
    started = []

    def start_response(status, headers, exc_info=None):
        started.append(status)
        return lambda data: None

    environ = {"REQUEST_METHOD": "GET", "PATH_INFO": "/x"}
    return WsgiLogRequests(app, log=log)(environ, start_response), sink, started


def test_wsgi_body_that_fails_to_iterate_ends_failed_and_is_closed():
    body = ClosableBody([], fail_on_iter=True)

    def app(environ, start_response):
        start_response("200 OK", [])
        return body

    wrapper, sink, _ = wsgi_call(app)
    with pytest.raises(ValueError, match="body failed"):
        list(wrapper)
    wrapper.close()
    assert body.closed
    assert sink.lines[1].startswith("GET /x | FAILED 500 ")


def test_wsgi_close_after_the_last_chunk_closes_the_body_and_keeps_the_status():
    body = ClosableBody([b"a"])

    def app(environ, start_response):
        start_response("201 Created", [])
        return body

    wrapper, sink, _ = wsgi_call(app)
    assert list(wrapper) == [b"a"]
    wrapper.close()
    assert body.closed
    assert len(sink.lines) == 2 and sink.lines[1].startswith("GET /x | DONE 201 ")


def test_wsgi_exc_info_replaces_the_status():
    def app(environ, start_response):
        start_response("200 OK", [])
        try:
            raise ValueError("late")
        except ValueError:
            start_response("503 Service Unavailable", [], sys.exc_info())
        return [b"no"]

    wrapper, sink, started = wsgi_call(app)
    list(wrapper)
    assert started == ["200 OK", "503 Service Unavailable"]
    assert sink.lines[1].startswith("GET /x | FAILED 503 ")


def test_wsgi_request_stays_out_of_the_thread_context():
    seen = []

    def app(environ, start_response):
        seen.append(get_request_id())
        start_response("200 OK", [])
        return [b"a"]

    wrapper, _, _ = wsgi_call(app)
    list(wrapper)
    wrapper.close()
    assert seen[0] is not None
    assert get_request_id() is None


def test_asgi_disconnect_during_the_last_send_is_a_complete_response():
    gone = asyncio.Event()

    async def receive():
        await gone.wait()
        return {"type": "http.disconnect"}

    async def send(message):
        if message["type"] == "http.response.body":
            gone.set()
            await asyncio.sleep(0.01)

    async def app(scope, receive, send):
        listener = asyncio.ensure_future(receive())
        await asyncio.sleep(0)
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send({"type": "http.response.body", "body": b"ok"})
        await listener

    sink = ListSink()
    log = Spanlog(service="shop", sink=sink, utc_offset_minutes=0)
    scope = {"type": "http", "method": "GET", "path": "/x", "headers": []}
    asyncio.run(AsgiLogRequests(app, log=log)(scope, receive, send))
    assert sink.lines[1].startswith("GET /x | DONE 200 ")


def test_asgi_poll_retrieves_a_failure_raised_while_receive_is_cancelled():
    reported = []

    async def main():
        asyncio.get_running_loop().set_exception_handler(
            lambda loop, context: reported.append(context["message"])
        )

        async def fails_on_cancel():
            try:
                await asyncio.Event().wait()
            finally:
                raise OSError("receive failed")

        assert await poll_state(fails_on_cancel)._disconnect_is_waiting() is False
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        gc.collect()

    asyncio.run(main())
    assert reported == []


def test_asgi_failure_of_the_last_send_ends_failed():
    async def app(scope, receive, send):
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send({"type": "http.response.body", "body": b"ok"})

    async def receive():
        await asyncio.Event().wait()

    async def send(message):
        if message["type"] == "http.response.body":
            raise OSError("connection lost")

    sink = ListSink()
    log = Spanlog(service="shop", sink=sink, utc_offset_minutes=0)
    scope = {"type": "http", "method": "GET", "path": "/x", "headers": []}
    with pytest.raises(OSError, match="connection lost"):
        asyncio.run(AsgiLogRequests(app, log=log)(scope, receive, send))
    assert len(sink.lines) == 2 and sink.lines[1].startswith("GET /x | FAILED 500 ")


def test_wsgi_close_before_the_first_chunk_is_aborted():
    body = ClosableBody([b"a"])

    def app(environ, start_response):
        start_response("200 OK", [])
        return body

    wrapper, sink, _ = wsgi_call(app)
    wrapper.close()
    assert body.closed
    assert sink.lines[1].startswith("GET /x | ABORTED")
