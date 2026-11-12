# Reading the logs

Every line puts its action and phase in a fixed place, and the lines of a request carry its id, so plain `grep` finds
most answers. The examples below come from [`examples/express-orders`](../examples/express-orders/server.js) and
[`examples/fastapi-orders`](../examples/fastapi-orders/server.py). `app.log` holds readable lines and `app.jsonl` holds
lines of the built-in JSON writer. Stack paths are shortened.

## One request

The request id is set on the response, under `x-request-id` in Node.js and Python and `X-Request-Id` in Go by default.
With that id:

```bash
grep 'req=4abd9c49' app.log
```

```text
2026-11-12 21:04:32.897 +07:00 INFO  [shop] req=4abd9c49 POST /orders | START
2026-11-12 21:04:32.898 +07:00 INFO  [shop] [OrderService.handleOrder] req=4abd9c49 run | START
2026-11-12 21:04:32.898 +07:00 DEBUG [shop] [OrderService.handleOrder] req=4abd9c49 call OrderService.validate | START
2026-11-12 21:04:32.898 +07:00 DEBUG [shop] [OrderService.handleOrder] req=4abd9c49 call OrderService.validate | DONE 0.1ms
2026-11-12 21:04:32.898 +07:00 INFO  [shop] [OrderService.handleOrder] req=4abd9c49 call InventoryService.reserve | START
2026-11-12 21:04:32.899 +07:00 DEBUG [shop] [InventoryService.reserve] req=4abd9c49 query UPDATE inventory | START
2026-11-12 21:04:32.902 +07:00 DEBUG [shop] [InventoryService.reserve] req=4abd9c49 query UPDATE inventory | DONE 2.7ms
2026-11-12 21:04:32.904 +07:00 ERROR [shop] [OrderService.handleOrder] req=4abd9c49 call InventoryService.reserve | FAILED 3.9ms Error: stock not available
2026-11-12 21:04:32.905 +07:00 INFO  [shop] [OrderService.handleOrder] req=4abd9c49 run | FAILED 6.6ms Error: stock not available
2026-11-12 21:04:32.906 +07:00 WARN  [shop] req=4abd9c49 POST /orders | FAILED 500 8.6ms
```

The stack lines under a readable line carry no `req=`, so this search leaves them out. `grep -A` shows them, as in the
next section. The JSON writer prints each object on one line with no spaces, and `err.stack` sits inside the line it
belongs to:

```bash
grep '"reqId":"a1cee763"' app.jsonl
```

## The failure

```bash
grep ' | FAILED' app.log
```

```text
2026-11-12 21:04:32.904 +07:00 ERROR [shop] [OrderService.handleOrder] req=4abd9c49 call InventoryService.reserve | FAILED 3.9ms Error: stock not available
2026-11-12 21:04:32.905 +07:00 INFO  [shop] [OrderService.handleOrder] req=4abd9c49 run | FAILED 6.6ms Error: stock not available
2026-11-12 21:04:32.906 +07:00 WARN  [shop] req=4abd9c49 POST /orders | FAILED 500 8.6ms
```

- The first report of an error is on the innermost frame that sees it, at error level, with the stack.
- Frames further out end with `FAILED` at their usual level and repeat only `name: message`.
- The request line ends with `FAILED` and the status when the status is 500 or higher. It has no `err`.

The line at error level, with its stack:

```bash
grep -A 4 ' ERROR \[' app.log
```

```text
2026-11-12 21:04:32.904 +07:00 ERROR [shop] [OrderService.handleOrder] req=4abd9c49 call InventoryService.reserve | FAILED 3.9ms Error: stock not available
    at InventoryService.reserve (file:///app/server.js:43:13)
    at process.processTicksAndRejections (node:internal/process/task_queues:104:5)
    at async OrderService.handleOrder (file:///app/server.js:63:5)
    at async file:///app/server.js:110:17
```

In JSON, `grep '"phase":"FAILED"' app.jsonl` finds the same lines and `grep '"level":"error"' app.jsonl` finds the first
report.

A failed SQL statement ends with `FAILED` at warn level, with `name` and `message` and no stack. The frame that receives
the error reports it with the stack. From the FastAPI example:

```bash
grep 'req=9632761e.* | FAILED' app.log
```

```text
2026-11-12 21:04:45.118 +07:00 WARN  [shop] [InventoryService.reserve] req=9632761e query UPDATE inventory | FAILED 5.6ms DBAPIError: invalid input for query argument $1: 2147483648 (value out of int32 range)
2026-11-12 21:04:45.121 +07:00 ERROR [shop] [OrderService.handle_order] req=9632761e call InventoryService.reserve | FAILED 9.1ms DBAPIError: invalid input for query argument $1: 2147483648 (value out of int32 range)
2026-11-12 21:04:45.121 +07:00 INFO  [shop] [OrderService.handle_order] req=9632761e run | FAILED 9.6ms DBAPIError: invalid input for query argument $1: 2147483648 (value out of int32 range)
2026-11-12 21:04:45.121 +07:00 WARN  [shop] req=9632761e POST /orders | FAILED 500 10.5ms
```

The message of this error repeats a parameter value. See [Errors](log-format.md#errors).

## Slow work

A call that takes longer than `slowMs` (`SlowMs` in Go, `slow_ms` in Python, 500 by default) ends with `SLOW` at warn
level:

```bash
grep ' | SLOW' app.log
```

```text
2026-11-12 21:04:32.615 +07:00 WARN  [shop] [OrderService.handleOrder] req=0c185e30 call PaymentService.charge | SLOW 615.1ms
2026-11-12 21:04:32.626 +07:00 WARN  [shop] [OrderService.handleOrder] req=0c185e30 run | SLOW 637.4ms
2026-11-12 21:04:32.631 +07:00 WARN  [shop] req=0c185e30 POST /orders | SLOW 201 688.9ms
```

The innermost `SLOW` line names the call that took the time. In JSON, `grep '"phase":"SLOW"' app.jsonl`.

## What stays stable

| Stable within specification version 0, every change listed in the release notes | Not stable |
|---|---|
| Field names | Values of `ms` |
| Values of `action` and `phase` | Stack contents, which depend on the runtime and the build |
| The shape of `msg` | The format of a generated request id |
| The layout of the readable line | How pino, slog and `logging` encode time and level |

## JSON Schema

[`spec/log-line.schema.json`](../spec/log-line.schema.json) describes a line of the built-in JSON writer. A tool can
check a line against it before reading its fields.
