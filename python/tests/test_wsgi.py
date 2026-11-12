import sys
import threading
import time

import pytest
from flask import Flask, Response
from waitress.server import create_server

from server_support import Inventory, ListSink, Orders, fetch, free_port, lines_end, wait_for
from spanlog import Spanlog, get_request_id
from spanlog.wsgi import LogRequests as WsgiLogRequests
from spanlog.wsgi import _path_of as wsgi_path_of


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


def test_wsgi_path_without_raw_uri_is_escaped():
    environ = {"SCRIPT_NAME": "/app", "PATH_INFO": "/a\nb"}
    assert wsgi_path_of(environ) == "/app/a%0Ab"


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


def test_wsgi_close_before_the_first_chunk_is_aborted():
    body = ClosableBody([b"a"])

    def app(environ, start_response):
        start_response("200 OK", [])
        return body

    wrapper, sink, _ = wsgi_call(app)
    wrapper.close()
    assert body.closed
    assert sink.lines[1].startswith("GET /x | ABORTED")
