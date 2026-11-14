# spanlog

Readable call-boundary logs for Node.js, Go and Python.

```text
2026-11-01 01:49:38.070 +07:00 INFO  [shop] req=89e32c2e POST /orders | START
2026-11-01 01:49:38.071 +07:00 INFO  [shop] [OrderService.handleOrder] req=89e32c2e run | START
2026-11-01 01:49:38.071 +07:00 INFO  [shop] [OrderService.handleOrder] req=89e32c2e call InventoryService.reserve | START
2026-11-01 01:49:38.072 +07:00 DEBUG [shop] [InventoryService.reserve] req=89e32c2e query UPDATE inventory | START
2026-11-01 01:49:38.074 +07:00 DEBUG [shop] [InventoryService.reserve] req=89e32c2e query UPDATE inventory | DONE 2.5ms
2026-11-01 01:49:38.075 +07:00 ERROR [shop] [OrderService.handleOrder] req=89e32c2e call InventoryService.reserve | FAILED 3.6ms Error: stock not available
    at InventoryService.reserve (file:///app/server.js:43:13)
    at process.processTicksAndRejections (node:internal/process/task_queues:104:5)
    at async OrderService.handleOrder (file:///app/server.js:63:5)
    at async file:///app/server.js:110:17
2026-11-01 01:49:38.076 +07:00 INFO  [shop] [OrderService.handleOrder] req=89e32c2e run | FAILED 5.1ms Error: stock not available
2026-11-01 01:49:38.077 +07:00 WARN  [shop] req=89e32c2e POST /orders | FAILED 500 6.9ms
```

This is one request from [`examples/express-orders`](examples/express-orders/server.js), without the lines of one
internal call. Stack paths are shortened.

spanlog writes one line when a call starts and one line when it ends, with its duration. The lines go to stdout as
readable text or JSON, or into the logger the application already uses: pino, slog or `logging`. There is no
collector, dashboard or account to set up.

## Install

```bash
npm install spanlog
```

For Go:

```bash
go get github.com/chaugiakhang193/spanlog/go
```

For Python:

```bash
pip install spanlog
```

## Use

Express, once per application and once per service:

```js
const log = createSpanlog({ service: 'shop' });
const orders = log.traced(new OrderService(), 'OrderService');
app.use(logRequests(log));
```

Go, once per application and once per method:

```go
logger := spanlog.New(spanlog.Options{Service: "shop"})
http.ListenAndServe(address, logger.Middleware(mux))

func (s *OrderService) HandleOrder(ctx context.Context) (err error) {
	ctx, done := s.log.Start(ctx, "OrderService", "HandleOrder")
	defer done(&err)
	return s.inventory.Reserve(ctx)
}
```

FastAPI, once per application and once per service. Flask has the same two lines through `spanlog.wsgi`:

```python
log = Spanlog(service="shop")
orders = log.traced(OrderService(), "OrderService")
app.add_middleware(LogRequests, log=log)
```

To keep the logger you already have, pass it as the sink. Its own level filters the lines too:

```js
const log = createSpanlog({ service: 'shop', sink: pinoSink(pino()) });
```

```go
logger := spanlog.New(spanlog.Options{Service: "shop", Sink: spanlog.SlogSink(slog.Default())})
```

```python
log = Spanlog(service="shop", sink=logging_sink(logging.getLogger("shop")))
```

- [Node.js guide](docs/node.md)
- [Go guide](docs/go.md)
- [Python guide](docs/python.md)
- [Log format](docs/log-format.md)
- [Reading the logs](docs/reading-logs.md)
- [Examples](examples/README.md)

## What it covers

| | Node.js | Go | Python |
|---|---|---|---|
| Incoming requests | Express 5 | `net/http` | ASGI (FastAPI, Starlette), WSGI (Flask) |
| Calls | `traced` wraps a whole object, `call` wraps one piece of work | `Start` in each method, `Call` wraps one piece of work | `traced` wraps a whole object, `call` and `acall` wrap one piece of work |
| Database queries | TypeORM 1.1, with the operation and table names | `database/sql`, any driver, with the operation and table names | SQLAlchemy 2, with the operation and table names |
| Destinations | Readable text, JSON, pino | Readable text, JSON, slog | Readable text, JSON, `logging` |

All implementations follow one [specification](docs/log-format.md) and are tested against the same
[sample lines](spec/cases.json).

## Safe defaults

- Arguments, results and SQL text stay out of the logs unless passed as `params` or `summary`.
- Those two are redacted by key. A sensitive value in a plain string is not seen.
- Each error is logged once per request, with its stack.
- An error's message is written as it is and can repeat a value, as a database error can repeat a SQL parameter. The
  `errorMessage` option replaces it.
- The request line holds the real path without its query string. A path can hold personal data.

## Other tools

spanlog is not a logger. It decides which lines to write and hands them to a sink, and `pinoSink`, `SlogSink` and
`logging_sink` keep pino, slog or `logging` as the logger. Each tool below does more than spanlog in its own area. Pick spanlog when plain lines in your
own logger are what you want.

| Tool | Output | Setup | Scope |
|---|---|---|---|
| spanlog | Lines in the application's logger | Middleware, one wrapper per service | Express, `net/http`, ASGI and WSGI, TypeORM and SQLAlchemy queries |
| [`pino`](https://www.npmjs.com/package/pino) 10.4.0 with [`pino-http`](https://www.npmjs.com/package/pino-http) 11.0.0 | JSON lines, one per finished request with `responseTime` | A logger and a middleware | Calls and queries are logged by hand through `req.log` |
| [`@nestjs/observe`](https://www.npmjs.com/package/@nestjs/observe) 0.3.21 | Traces in a hosted dashboard, with an app key and secret | One module and one option in `NestFactory.create`. Every provider is instrumented by default | NestJS, with queries of `pg`, `mysql2`, `mssql`, `mongodb` |
| [OpenTelemetry](https://opentelemetry.io) | Spans sent to the backend of your choice | SDK, instrumentations and an exporter | Many languages and libraries. `@opentelemetry/instrumentation-typeorm` 0.24.0 records the statement text |
| [`nestlogged`](https://www.npmjs.com/package/nestlogged) 3.7.3 | Lines through the NestJS logger | A decorator on each class | NestJS |

## Status

Early development. Fields and options can change before 1.0. Each release is listed in
[docs/CHANGELOG.md](docs/CHANGELOG.md).

## License

[MIT](LICENSE)
