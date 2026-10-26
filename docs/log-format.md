# Log format

Specification version 0 (draft). The Node.js and Go implementations follow this document and are tested against the
sample cases in [`spec/cases.json`](../spec/cases.json). Fields and phases may change before 1.0. Every change is
listed in the release notes.

## Events

spanlog writes one line per event. An event is the start or the end of one of four actions.

| Action | Written when | `target` |
|---|---|---|
| `request` | An HTTP request arrives, and when its response ends | `POST /orders` (method and path, query string removed) |
| `run` | A wrapped method starts or ends with no wrapped method above it | none |
| `call` | A wrapped method starts or ends inside another wrapped method, or an explicit `call` starts or ends | `InventoryService.reserve` |
| `query` | The database adapter sees a SQL statement start or end | `UPDATE warehouse.inventory` |

A **frame** is the wrapped method that is running: a scope (class or component name) and a method, written
`{scope}.{method}`. A `call` line and a `query` line belong to the frame that made the call. They carry the caller's
`scope` and `method`, and the callee in `target`. A method wrapped by spanlog writes one pair of lines per call: a
`run` pair when nothing wrapped is above it, a `call` pair in its caller's frame otherwise.

`service` names the application process. Classes that run in one process share it, so `call` lines describe calls
inside one process. Another process writes its own lines under its own `service` name.

## Phases

| Phase | Meaning |
|---|---|
| `START` | The action began |
| `DONE` | The action finished within `slowMs` (default 500) |
| `SLOW` | The action finished after more than `slowMs` |
| `FAILED` | The action threw or rejected. For `request`: the response status is 500 or higher |
| `ABORTED` | `request` only. The client closed the connection before the response finished |

## Fields

| Field | Type | Present on |
|---|---|---|
| `service` | string | every line. Name of the application process, given in the configuration |
| `reqId` | string | lines written inside a request |
| `scope` | string | lines that belong to a frame |
| `method` | string | lines that belong to a frame |
| `action` | string | every line: `request`, `run`, `call`, `query` |
| `target` | string | `request`, `call`, `query` |
| `phase` | string | every line: `START`, `DONE`, `SLOW`, `FAILED`, `ABORTED` |
| `ms` | number | every phase except `START`. Duration in milliseconds, rounded to one decimal |
| `status` | number | `request` lines with `DONE`, `SLOW` or `FAILED` |
| `op` | string | `query`. See [Query lines](#query-lines) |
| `tables` | string[] | `query`. Can be empty |
| `err` | object | `FAILED` on `run`, `call`, `query`. `name` (optional), `message`, `stack` (see [Errors](#errors)) |
| `params` | object | `START` of an explicit `call`, when parameters are given. Redacted |
| `summary` | object | `DONE` and `SLOW` of an explicit `call`, when a summary is given. Redacted |

The built-in JSON writer adds `time` (ISO 8601, UTC), `level` and `msg`. With pino or slog as the destination, the
encoding of time, level and message follows that logger.

Arguments, return values, SQL text and SQL parameters are never written unless the application passes them as
`params` or `summary`.

## Levels

| Action | `START` | `DONE` | `SLOW` | `FAILED`, first report of the error | `FAILED`, error already reported | `ABORTED` |
|---|---|---|---|---|---|---|
| `request` | info | info | warn | warn (status 500 or higher, no `err`) | — | warn |
| `run` | info | info | warn | error | info | — |
| `call` to another scope | info | info | warn | error | info | — |
| `call` within the same scope | debug | debug | warn | error | debug | — |
| `query` | debug | debug, warn for `ROLLBACK` | warn | warn | warn | — |

The default minimum level is info. Query lines and calls within one scope appear at debug.

## Message

    {frame} - {body} | {PHASE} {detail}

`{frame} - ` is left out when the line belongs to no frame. ` {detail}` is left out when the detail is empty.

| Action | `{body}` |
|---|---|
| `request` | `{target}` |
| `run` | `run` |
| `call` | `call {target}` |
| `query` | `query {target}` |

Detail:

| Phase | `request` | Other actions |
|---|---|---|
| `START` | empty | `params` as JSON when present, otherwise empty |
| `DONE`, `SLOW` | `{status} {ms}ms` | `{ms}ms`, then `summary` as JSON when present |
| `FAILED` | `{status} {ms}ms` | `{ms}ms`, then `{name}: {message}`, or `{message}` when the error has no name |
| `ABORTED` | `{ms}ms` | — |

`{ms}` is the duration rounded to one decimal, without a trailing `.0`: `3ms`, `0.4ms`, `801ms`. JSON in a message
(`params`, `summary`) is compact, with object keys sorted at every level.

## Readable line

    {time} {LEVEL} [{service}] [{frame}] req={reqId} {body} | {PHASE} {detail}

The readable line moves the frame into its own brackets. `msg` keeps it in front, so a log viewer that shows only
`msg` still names the class and the method.

- `{time}` is `YYYY-MM-DD HH:mm:ss.SSS ±HH:MM` in the configured UTC offset. The default is the host's local offset.
- `{LEVEL}` is the level in capitals, padded with spaces to 5 characters.
- ` [{frame}]` is left out when the line belongs to no frame.
- `{reqId}` is `-` outside a request.
- On the first report of an error that has a stack, the lines of the stack after its first line follow the main line,
  each trimmed and indented by 4 spaces. Frames of spanlog's own functions are left out of these lines. `err.stack`
  in the fields keeps every frame.

## Errors

- Each error is reported once per request with its stack, at error level, on the line of the innermost frame that
  sees it. Frames further out end with `FAILED` at their usual level, with the error's `name` and `message` only.
- The same error object thrown again in another request is reported once in that request too.
- A thrown value that is not an object, such as a string, is wrapped in an `Error`. The wrapper is rethrown, and code
  further up receives the wrapper.
- A failed SQL statement writes `FAILED` at warn level with `name` and `message`, without a stack. The frame that
  receives the error reports it.

## Query lines

- `op` is the first keyword of the statement in capitals. `START TRANSACTION` keeps both words. For a statement that
  starts with `WITH`, `op` is the keyword after the common table expressions.
- `tables` lists every name that follows `FROM`, `JOIN`, `INTO` or `UPDATE`, in the order first seen, without
  duplicates. Double quotes are removed and a schema prefix is kept (`warehouse.inventory`). Names of common table
  expressions and subqueries are skipped.
- `target` is `{op}` followed by the tables joined with `, `, or `{op}` alone when there is no table.
- Known limits: a keyword inside a string literal can produce a wrong table name, and `FROM some_function(...)`
  reports the function name.

## Example

One process named `shop` takes two orders at debug level. `OrderService` and `InventoryService` are classes of that
process. The first order succeeds with a slow payment. The second fails because the stock is gone.

    2026-10-24 21:15:03.118 +07:00 INFO  [shop] req=7f3a9c POST /orders | START
    2026-10-24 21:15:03.119 +07:00 INFO  [shop] [OrderService.handleOrder] req=7f3a9c run | START
    2026-10-24 21:15:03.119 +07:00 DEBUG [shop] [OrderService.handleOrder] req=7f3a9c call OrderService.validate | START
    2026-10-24 21:15:03.119 +07:00 DEBUG [shop] [OrderService.handleOrder] req=7f3a9c call OrderService.validate | DONE 0.1ms
    2026-10-24 21:15:03.120 +07:00 DEBUG [shop] [OrderService.handleOrder] req=7f3a9c query INSERT orders | START
    2026-10-24 21:15:03.122 +07:00 DEBUG [shop] [OrderService.handleOrder] req=7f3a9c query INSERT orders | DONE 2ms
    2026-10-24 21:15:03.122 +07:00 INFO  [shop] [OrderService.handleOrder] req=7f3a9c call InventoryService.reserve | START
    2026-10-24 21:15:03.123 +07:00 DEBUG [shop] [InventoryService.reserve] req=7f3a9c query UPDATE warehouse.inventory | START
    2026-10-24 21:15:03.125 +07:00 DEBUG [shop] [InventoryService.reserve] req=7f3a9c query UPDATE warehouse.inventory | DONE 1.5ms
    2026-10-24 21:15:03.125 +07:00 INFO  [shop] [OrderService.handleOrder] req=7f3a9c call InventoryService.reserve | DONE 3ms
    2026-10-24 21:15:03.125 +07:00 INFO  [shop] [OrderService.handleOrder] req=7f3a9c call PaymentService.charge | START
    2026-10-24 21:15:03.926 +07:00 WARN  [shop] [OrderService.handleOrder] req=7f3a9c call PaymentService.charge | SLOW 801ms
    2026-10-24 21:15:03.927 +07:00 WARN  [shop] [OrderService.handleOrder] req=7f3a9c run | SLOW 808ms
    2026-10-24 21:15:03.928 +07:00 WARN  [shop] req=7f3a9c POST /orders | SLOW 201 810ms
    2026-10-24 21:15:04.300 +07:00 INFO  [shop] req=b81e44 POST /orders | START
    2026-10-24 21:15:04.300 +07:00 INFO  [shop] [OrderService.handleOrder] req=b81e44 run | START
    2026-10-24 21:15:04.301 +07:00 INFO  [shop] [OrderService.handleOrder] req=b81e44 call InventoryService.reserve | START
    2026-10-24 21:15:04.302 +07:00 DEBUG [shop] [InventoryService.reserve] req=b81e44 query UPDATE warehouse.inventory | START
    2026-10-24 21:15:04.303 +07:00 DEBUG [shop] [InventoryService.reserve] req=b81e44 query UPDATE warehouse.inventory | DONE 1.2ms
    2026-10-24 21:15:04.304 +07:00 ERROR [shop] [OrderService.handleOrder] req=b81e44 call InventoryService.reserve | FAILED 3ms Error: stock not available
        at InventoryService.reserve (file:///app/src/inventory.service.js:12:11)
        at async OrderService.handleOrder (file:///app/src/order.service.js:20:5)
    2026-10-24 21:15:04.304 +07:00 INFO  [shop] [OrderService.handleOrder] req=b81e44 run | FAILED 4ms Error: stock not available
    2026-10-24 21:15:04.305 +07:00 WARN  [shop] req=b81e44 POST /orders | FAILED 500 5ms

The `call ... | FAILED` line of the second order, written by the JSON writer:

```json
{"time":"2026-10-24T14:15:04.304Z","level":"error","service":"shop","reqId":"b81e44","scope":"OrderService","method":"handleOrder","action":"call","target":"InventoryService.reserve","phase":"FAILED","ms":3,"err":{"name":"Error","message":"stock not available","stack":"Error: stock not available\n    at InventoryService.reserve (file:///app/src/inventory.service.js:12:11)\n    at async OrderService.handleOrder (file:///app/src/order.service.js:20:5)"},"msg":"OrderService.handleOrder - call InventoryService.reserve | FAILED 3ms Error: stock not available"}
```

## Sample cases

[`spec/cases.json`](../spec/cases.json) holds two lists.

- `render`: an event and the expected `level`, `msg`, `fields` and readable `lines`. Each event carries `time`
  (ISO 8601), the raw duration in `durationMs`, and two inputs that are not output fields: `sameScope` for calls
  within one scope, and `firstReport` for the first report of an error in a request. `options.utcOffsetMinutes` sets
  the offset of the readable line.
- `sql`: a statement and the expected `op` and `tables`.
