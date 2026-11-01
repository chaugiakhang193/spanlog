# Python guide

Requires Python 3.11 or newer. The package has no dependency outside the standard library. The package is not on PyPI, so
install it from a clone:

```bash
git clone https://github.com/chaugiakhang193/spanlog.git
pip install ./spanlog/python
```

## Quick start with FastAPI

```python
from fastapi import FastAPI
from spanlog import Spanlog
from spanlog.asgi import LogRequests


class OrderService:
    async def handle_order(self, sku: str) -> dict[str, str]:
        return {"sku": sku}


log = Spanlog(service="shop")
orders = log.traced(OrderService(), "OrderService")

app = FastAPI()
app.add_middleware(LogRequests, log=log)


@app.post("/orders/{sku}")
async def create_order(sku: str) -> dict[str, str]:
    return await orders.handle_order(sku)
```

## Quick start with Flask

```python
from flask import Flask
from spanlog import Spanlog
from spanlog.wsgi import LogRequests


class OrderService:
    def handle_order(self, sku: str) -> dict[str, str]:
        return {"sku": sku}


log = Spanlog(service="shop")
orders = log.traced(OrderService(), "OrderService")

app = Flask(__name__)
app.wsgi_app = LogRequests(app.wsgi_app, log=log)


@app.post("/orders/<sku>")
def create_order(sku: str) -> dict[str, str]:
    return orders.handle_order(sku)
```

## `Spanlog(...)`

| Argument | Default | Meaning |
|---|---|---|
| `service` | required | Name written on every line |
| `sink` | `pretty_sink()` | Destination of the lines |
| `level` | `"info"` | Lowest level written. Calls within one scope appear at `"debug"` |
| `slow_ms` | `DEFAULT_SLOW_MS` (500) | Calls longer than this end with `SLOW` |
| `utc_offset_minutes` | offset of the host | UTC offset of the readable line |
| `redact_keys` | none | Keys masked on top of `DEFAULT_REDACT_KEYS` |
| `error_message` | none | Replaces the text of every exception. See [Errors](log-format.md#errors) |

Problems of spanlog itself, such as a sink that failed, arrive as a `SpanlogWarning` through the `warnings` module.

## `traced`

`log.traced(instance, "OrderService")` wraps every `def` and `async def` method of the instance, inherited ones too, and
returns the same instance. Names with two leading and trailing underscores, properties and generators are left as they
are. The class itself does not change, and a class is refused.

A `def` that returns a coroutine, as a decorator around an `async def` does, ends its line when the coroutine ends. A `def`
that returns a `Task` or a `Future` ends its line when it returns. A class with `__slots__` needs a `__dict__` slot.

## `call` and `acall`

Both wrap one piece of work under a name. Use `call` for a plain function and `await log.acall(...)` for one that returns
an awaitable.

```python
rows = await log.acall(
    "PriceClient.fetch",
    lambda: client.fetch(sku),
    params={"sku": sku},
    summarize=lambda result: {"count": len(result)},
)
```

Without `summarize`, a list or a tuple is summarized as its length.

## Requests

`LogRequests(app, log=log, header="x-request-id")` runs each request in its own context and sets the id on the response. An
incoming id made of letters, digits, dot, underscore and hyphen, at most 64 characters, is kept. Otherwise a new id is
generated. `spanlog.get_request_id()` returns it.

Under ASGI the end line of a request is written when the server has taken the last body message. A background task of
Starlette or FastAPI runs after it, so its time and its failure stay out of that line. Lines written by the task still carry
the request id. Under WSGI the end line is written when the body iterator ends, so work a generator does after its last
`yield` counts.

Outside HTTP, `with log.with_request(job_id):` gives every line in the block the same id.

The request travels in a `contextvars.ContextVar`. asyncio tasks, `asyncio.to_thread` and the worker threads of FastAPI
carry it. `threading.Thread` and `ThreadPoolExecutor.submit` start with an empty context, so their lines have no id. Pass
`contextvars.copy_context().run` to carry it there.

## Sinks

- `pretty_sink()` writes the readable lines to `sys.stdout`.
- `json_sink()` writes one JSON object per line.
- `logging_sink(logger)` hands the message to a `logging.Logger`, with the fields as attributes of the record.
- A sink is any object with `write(rendered)`.

## What stays out of the logs

Arguments and results stay out unless passed as `params` or through `summarize`, which are redacted by key. A sensitive
value in a plain string is not seen. These two read dicts, lists, tuples, sets, dataclasses, objects with `model_dump`,
dates, `Decimal`, `UUID`, paths and enums. `bytes` is written as `[BYTES n]` and any other object as `[OBJECT {type}]`,
because its `repr` can hold anything. `target` of a request holds the real path, query string removed. The text of an
exception is written as it is, and `error_message` replaces it.

## Known limits

- The package writes no query lines.
- ASGI: the middleware sees a client that left when the application reads a disconnect or when one is waiting right before
  the last byte of the response. A client that leaves after that point is not seen. The check needs asyncio, and it can take
  a part of the request body that nothing has read. A server on trio skips it. Other scope types, such as websockets, pass
  through without lines.
- WSGI: a client that leaves while the handler runs is not seen, and the request ends with its status. A server calling
  `close` before the last chunk ends the request with `ABORTED`. The wrapper around the body hides `wsgi.file_wrapper` and
  the length of the body from the server.
- Limits of errors are listed in [Errors in Python](log-format.md#errors-in-python).

## Tested versions

Python 3.14.6 on Windows, FastAPI 0.162.0, Starlette 1.10.0, uvicorn 0.54.0, Flask 3.1.3, waitress 3.0.2. Older versions,
other operating systems and other servers have not been run.
