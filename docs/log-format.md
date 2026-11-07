# Log format

Specification version 0 (draft). The Node.js, Go and Python implementations follow this document and are tested against
the sample cases in [`spec/cases.json`](../spec/cases.json). Fields and phases may change before 1.0. Every change is
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

Arguments, return values, SQL text and SQL parameters are not written unless the application passes them as `params`
or `summary`. The exception is the `message` of an `Error`, which can repeat a value. See [Errors](#errors).

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
(`params`, `summary`) is compact, with object keys sorted by UTF-16 code unit at every level. An empty string in
`reqId`, `scope`, `method`, `target` or `op` counts as absent.

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
- A thrown object or function that is not an `Error` is rethrown unchanged. Its `name` is `ThrownValue`. Its `message`
  is the redacted JSON of the object, or `[Function {name}]` for a function, with `anonymous` when the function has no
  name.
- When reading the thrown value throws, its `name` is `UnreadableError` and its `message` is
  `[unreadable thrown value]`. An `Error` with a throwing getter falls under this case too.
- A failed SQL statement writes `FAILED` at warn level with `name` and `message`, without a stack. The frame that
  receives the error reports it.
- `message` is written as the `Error` carries it. A database error can repeat a parameter value, as Postgres does in
  `invalid input syntax for type integer: "..."`. The `errorMessage` option replaces the message of every `Error` on
  the lines and in `err.stack`. When that function throws, the description of an unreadable value stands in.
- With `errorMessage` in Node.js, the stack keeps its frames when its start holds the current message in one of these
  forms: `{name}: {message}`, `{other name}: {message}` where the other name has no `: ` (`err.name` was assigned after
  the stack was written), `{name}` alone when the message is empty, `{message}` alone when the name is empty. Any other
  stack keeps its replaced first line and no frames. Assigning `err.message` after `err.stack` was read leaves such a
  stack, and a line of the old message cannot be told apart from a frame. An `Error` with an empty message whose
  `err.name` was assigned after the stack was written loses its frames too.
- Known limit of `errorMessage`: when the value assigned to `err.message` after `err.stack` was read equals the leading
  lines of the old message, a later line of the old message that starts with whitespace and `at ` stays in the stack as
  a frame.

### Errors in Go

- An `error` has no `name`, so its line reads `FAILED 3ms stock not available`.
- `err.stack` is captured when `done` writes the first report of the error. It starts with the name and message and
  lists one frame per line as `at {function} ({file}:{line})`. A function that has already returned is not on it, so it
  shows where the error was seen, which can be above where it was created.
- A panic ends the call with `FAILED` and keeps going up with the same value. A panic value that is an `error`, a string,
  a number or a boolean is described by its text. Any other value has the `name` `ThrownValue` and the redacted JSON of
  the value as its `message`.
- `UnreadableError` covers `Error()` or `errorMessage` panicking and a panic value that `encoding/json` cannot encode.
- The same value seen again in the request counts as already reported, compared with `==` along its `Unwrap` chain. An
  error wrapped with `%w` on its way up is found this way. An `errors.Join` counts only when every branch was reported.
  `Is` methods play no part. The walk reads at most 32 errors, counting the failure itself, its wrappers and the branches
  of a `Join`, and a nil from `Unwrap` does not count. A reported error past that limit is not found, and a `Join` with a
  branch past it counts as new. A failure whose `Unwrap` panics counts as new.
- Known limits: a shared sentinel such as `ErrNotFound`, returned a second time in a request by an unrelated call, counts
  as already reported and has no stack. A value that cannot be compared, such as an error type that holds a slice, cannot
  be recognized and can be reported with its stack more than once. `runtime.Goexit` leaves `done` nothing to see, so with
  a nil error the call ends with `DONE`, and so does `panic(nil)` under `GODEBUG=panicnil=1`. Invalid UTF-8 in JSON becomes
  U+FFFD.
- Known limits of JSON text against Node.js: a `json.Number` or a custom `MarshalJSON` keeps its number as written, such
  as `1e3`. An integer above 2^53 keeps every digit in Go. A Node.js `number` has already rounded it to the nearest value
  it can hold, and a `BigInt` is written as a string.
- Known limits of `Render` called with a duration that no timer measures: from `1e21` up, Go writes every digit where
  Node.js writes `1e+21`, and an infinite duration reads `+Inf` where Node.js reads `Infinity`.
- Known limits of the `net/http` middleware: `ABORTED` reads the request context, which `net/http` cancels when the
  client closes the connection. A client that leaves while the request body is unread is not seen, and a client that
  leaves right after a complete response can end the request with `ABORTED`. A deadline on the context is not an abort.
  Register the middleware outside `http.TimeoutHandler`: inside it, the line shows what the handler wrote, and the 503 sent
  to the client stays out of sight. A handler that hijacks the connection writes past the middleware, and the line shows
  the last status it saw. A flush that fails counts as having sent the header, as `net/http` does. A custom writer that fails
  before sending it and then sends a later status leaves the line with the earlier status.
- `go/testdata/cases.json` is a copy of `spec/cases.json`, because the module zip holds only `go/`. A test fails when the
  two differ.

### Errors in Python

- `name` is the class of the exception, so its line reads `FAILED 3ms ValueError: stock not available`.
- `err.stack` starts with the name and the first line of the message and lists one frame per line as
  `at {qualified name} ({file}:{line})`, innermost first, at most 32. A Python traceback holds the frames from the function
  that caught the exception down to the raise, so the frames that called the wrapped method are not on it.
- Python raises only exceptions, so `ThrownValue` does not occur. `UnreadableError` covers `__str__` or `error_message`
  raising.
- Every `BaseException` ends the call with `FAILED` and keeps going up, `asyncio.CancelledError` included. A cancelled
  request ends with `ABORTED`.
- The same exception object seen again in the request counts as already reported. `raise New() from old` is a new object
  and is reported with its own stack.
- A SQLAlchemy `StatementError` uses the first line of the error of the driver as its `message`, because its own text holds
  the SQL and the parameters. `error_message` still replaces it.
- Known limits of JSON text against Node.js: an integer above 2^53 keeps every digit, as in Go. A `Render` duration of
  infinity reads `null` where Node.js reads `Infinity`.

## Query lines

The Go module has no query lines. The `sql` and `transactions` lists of [`spec/cases.json`](../spec/cases.json) are
tested in Node.js and Python.

- `op` is the first keyword of the statement in capitals. `START TRANSACTION` keeps both words. For a statement that
  starts with `WITH`, `op` is the keyword after the common table expressions. Parentheses before that keyword are
  skipped, both at the start of the statement and after the expressions.
- Keywords compare after folding `a`-`z` to capitals and nothing else, and `op` is capitalized the same way. A letter
  such as `ı` or `ß` stays as it is, even where a Unicode upper-case function would turn it into `I` or `SS`.
- `tables` lists every name that follows `FROM`, `JOIN`, `INTO` or `UPDATE`, in the order first seen, without
  duplicates. Double quotes are removed and a schema prefix is kept (`warehouse.inventory`). Subqueries are skipped.
  `ONLY`, `ONLY (` and `LATERAL` before a name are skipped.
- A name without a schema prefix is skipped when it refers to a common table expression. The expression is visible
  from the end of its own body to the end of the statement or subquery that holds its `WITH`. With `WITH RECURSIVE`
  it is visible from the `WITH` on. Names compare as Postgres compares them in a UTF-8 database: an unquoted name folds
  `A`-`Z` to lower case and keeps other letters, and a quoted name keeps its case. A `WITH` name counts as an
  expression only when a parenthesis follows it, after any `AS`, `NOT` and `MATERIALIZED`, and the first token inside
  is `(` or one of `SELECT`, `VALUES`, `TABLE`, `WITH`, `INSERT`, `UPDATE`, `DELETE`, `MERGE`. `SEARCH ... SET name` and
  `CYCLE ... USING name` after the body belong to the expression. Reading either stops at a parenthesis.
- The table written by `DELETE FROM`, `INSERT INTO`, `MERGE INTO` or `UPDATE` is always listed, because Postgres does
  not resolve it to a common table expression. `DELETE` before `FROM` counts as that command when it follows `(` or the
  list of expressions. Elsewhere it is a column name.
- `UPDATE` directly after `FOR` or `KEY` (a row lock), `DO` (the update of an upsert) or `THEN` (the action of a
  `MERGE`) does not name a table. `FROM` in `IS [NOT] DISTINCT FROM` does not name a table.
- String literals (`'...'`, `E'...'` with its continuation lines, `$tag$...$tag$`) and comments (`-- ...`, `/* ... */`)
  are skipped unread. A name or a tag can hold any character from U+0080 up.
- `target` is `{op}` followed by the tables joined with `, `, or `{op}` alone when there is no table.
- Known limits: `FROM some_function(...)` reports the function name, `FROM` inside a call such as
  `EXTRACT(YEAR FROM created_at)` reports the column, a name after a comma (`FROM a, b`) or first inside parentheses
  (`FROM (a JOIN b ON true)`) is not listed, a backslash escape outside `E'...'`, as in MySQL, can end a literal
  early, an unquoted column named `update` reads as the keyword (`SELECT update FROM orders` lists `FROM`), a column
  list after `WITH ORDINALITY AS materialized` that starts with a statement keyword reads as an expression, and
  statements joined by `;` read as one statement: `op` is that of the first, and a common table expression stays visible
  up to the end of the text.

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

[`spec/cases.json`](../spec/cases.json) holds three lists.

- `render`: an event and the expected `level`, `msg`, `fields` and readable `lines`. Each event carries `time`
  (ISO 8601), the raw duration in `durationMs`, and two inputs that are not output fields: `sameScope` for calls
  within one scope, and `firstReport` for the first report of an error in a request. `options.utcOffsetMinutes` sets
  the offset of the readable line.
- `sql`: a statement and the expected `op` and `tables`.
- `transactions`: statements run in order on one connection, and the expected `tables` of each `ROLLBACK` among them.
  The rules are in [Query lines](#query-lines).
