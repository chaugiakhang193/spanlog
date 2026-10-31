# Node.js guide

Requires Node.js 22.13 or newer. The package is ESM. Express and TypeORM are optional peer dependencies, needed only by
`spanlog/express` and `spanlog/typeorm`.

## Quick start with Express

```js
import express from 'express';
import { createSpanlog } from 'spanlog';
import { logRequests } from 'spanlog/express';

class OrderService {
  async handleOrder() {
    return { id: 1 };
  }
}

const log = createSpanlog({ service: 'shop' });
const orders = log.traced(new OrderService(), 'OrderService');

const app = express();
app.use(logRequests(log));
app.post('/orders', async (_req, res) => {
  res.status(201).json(await orders.handleOrder());
});
app.listen(3020, '127.0.0.1');
```

A full example with query lines is in [`examples/express-orders`](../examples/express-orders/server.js).

## `createSpanlog(options)`

| Option | Default | Meaning |
|---|---|---|
| `service` | required | Name written on every line |
| `sink` | `prettySink()` | Destination of the lines |
| `level` | `'info'` | Lowest level written. Query lines and calls within one class appear at `'debug'` |
| `slowMs` | `500` | Calls longer than this end with `SLOW` |
| `utcOffsetMinutes` | offset of the host | UTC offset of the readable line |
| `redactKeys` | `[]` | Keys masked on top of `DEFAULT_REDACT_KEYS` |
| `errorMessage` | none | Replaces the message of every `Error`. See [Errors](log-format.md#errors) |

## Wrapping services

`log.traced(target, scope)` wraps every async method of `target`, inherited ones included, in place and returns it.
Each call writes a `START` line and an end line. A call through `this` goes through the wrapper too. Synchronous methods,
getters and setters stay as they are. Tracing the same object twice does nothing.

## Explicit calls

`log.call(target, fn, options)` wraps one piece of work, such as a call to a client that spanlog cannot wrap.

```js
const rows = await log.call('PriceClient.fetch', () => client.fetch(sku), {
  params: { sku },
  summarize: (result) => ({ count: result.length }),
});
```

`params` goes on the `START` line and `summarize` on the end line, both redacted. Without `summarize`, an array result is
summarised as `{"count":n}` and any other result has no summary.

## Requests

`logRequests(log, { header })` from `spanlog/express` runs each request inside `log.withRequest`. Register it before
the routes. An incoming `x-request-id` made of letters, digits, dot, underscore and hyphen, at most 64 characters, is
kept. Otherwise a new id is generated. The id is set on the response. `getRequestId()` returns it while the request runs.

Outside HTTP, `log.withRequest(jobId, fn)` gives every line inside `fn` the same id.

## Query lines

`logQueries(dataSource, log)` from `spanlog/typeorm` goes after `await dataSource.initialize()`. It returns a function
that removes the subscriber. SQL text and parameters are never written. The parser and its known limits are described
in [Query lines](log-format.md#query-lines). Statements of `QueryRunner.stream()` write no lines, because TypeORM fires
no query events for them.

## Sinks

- `prettySink(writeText)` writes the readable lines, to stdout by default.
- `jsonSink(writeText)` writes one JSON object per line, to stdout by default.
- `pinoSink(logger)` hands fields and message to a pino logger. Its tests use a stand-in with the same methods.
- A `SpanSink` is any object with `write(rendered)`.

## What stays out of the logs

Arguments, return values, SQL text and SQL parameters are not written unless passed as `params` or `summary`.

- Redaction matches keys, at any depth and without regard to case. It cannot see a sensitive value in a plain string,
  as in `reserve(email)` passed as `params: { customer: email }`.
- `target` of a request holds the real path, query string removed. A path such as `/users/alice@example.com` lands on
  the line as it is.
- The `message` of an `Error` is written as it is and can repeat a value. `errorMessage` replaces it.

## Warnings

spanlog never fails the call it describes. Its own problems reach `process.emitWarning` with one of these codes.

| Code | Cause | What happens to the line |
|---|---|---|
| `SPANLOG_SINK` | The sink threw | The line is lost |
| `SPANLOG_PARAMS` | Reading `params` threw | The line is written without `params`, and the call runs |
| `SPANLOG_SUMMARIZE` | `summarize` threw | The line is written without `summary` |

## Known limits

- Stack lines drop frames by function name. An application function named exactly `spanlogCall`, `spanlogEnter`,
  `spanlogEnterRequest`, `spanlogInvoke`, `spanlogMiddleware`, `spanlogRunInContext`, `spanlogTrack`,
  `spanlogWithRequest` or `spanlogWrapped` loses its frame on the readable line.
- `logQueries` called twice on one `DataSource` writes every line twice.

## Tested versions

Node.js 24.18.0, Express 5.3.0, TypeORM 1.1.2, pg 8.23.1, PostgreSQL 18.6. The peer ranges `express ^5.0.0` and
`typeorm ^1.1.0` are wider than what has been run. Node.js 22, other TypeORM drivers and real pino have not been run.
