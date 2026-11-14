# spanlog for Python

spanlog writes one line when a call starts and one line when it ends, with its duration. The lines go to stdout as
readable text or JSON, or into a `logging.Logger` the application already uses.

```text
2026-11-12 21:04:44.830 +07:00 INFO  [shop] req=e5e0891a POST /orders | START
2026-11-12 21:04:44.831 +07:00 INFO  [shop] [OrderService.handle_order] req=e5e0891a run | START
2026-11-12 21:04:44.831 +07:00 INFO  [shop] [OrderService.handle_order] req=e5e0891a call InventoryService.reserve | START
2026-11-12 21:04:44.832 +07:00 DEBUG [shop] [InventoryService.reserve] req=e5e0891a query UPDATE inventory | START
2026-11-12 21:04:44.836 +07:00 DEBUG [shop] [InventoryService.reserve] req=e5e0891a query UPDATE inventory | DONE 3.6ms
2026-11-12 21:04:44.838 +07:00 ERROR [shop] [OrderService.handle_order] req=e5e0891a call InventoryService.reserve | FAILED 6.6ms ValueError: stock not available
    at InventoryService.reserve (/app/server.py:52)
2026-11-12 21:04:44.838 +07:00 INFO  [shop] [OrderService.handle_order] req=e5e0891a run | FAILED 7ms ValueError: stock not available
2026-11-12 21:04:44.838 +07:00 WARN  [shop] req=e5e0891a POST /orders | FAILED 500 8.2ms
```

This is one failed request from
[`examples/fastapi-orders`](https://github.com/chaugiakhang193/spanlog/blob/main/examples/fastapi-orders/server.py),
without the lines of one internal call. Stack paths are shortened.

## Install

```bash
pip install spanlog
```

Query lines for SQLAlchemy need the extra: `pip install "spanlog[sqlalchemy]"`.

## Quick start

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

Flask has the same two lines through `spanlog.wsgi`.

## Keep your logger

```python
import logging

from spanlog import Spanlog, logging_sink

logging.basicConfig(level=logging.INFO)
log = Spanlog(service="shop", sink=logging_sink(logging.getLogger("shop")))
```

The level of the `logging.Logger` filters the lines too. Without a level set, `logging` keeps `WARNING` and drops
`START` and `DONE`.

## Documentation

- [Python guide](https://github.com/chaugiakhang193/spanlog/blob/main/docs/python.md)
- [Log format](https://github.com/chaugiakhang193/spanlog/blob/main/docs/log-format.md)
- [Examples](https://github.com/chaugiakhang193/spanlog/blob/main/examples/README.md)

## License

[MIT](https://github.com/chaugiakhang193/spanlog/blob/main/python/LICENSE)
