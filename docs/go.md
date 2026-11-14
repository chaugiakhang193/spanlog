# Go guide

Requires Go 1.26. The package has no dependency outside the standard library.

```bash
go get github.com/chaugiakhang193/spanlog/go
```

```go
import "github.com/chaugiakhang193/spanlog/go"
```

The package name is `spanlog`.

## Quick start with net/http

```go
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/chaugiakhang193/spanlog/go"
)

type OrderService struct {
	log *spanlog.Logger
}

func (s *OrderService) HandleOrder(ctx context.Context) (err error) {
	_, done := s.log.Start(ctx, "OrderService", "HandleOrder")
	defer done(&err)
	return nil
}

func main() {
	logger := spanlog.New(spanlog.Options{Service: "shop"})
	orders := &OrderService{log: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		if err := orders.HandleOrder(r.Context()); err != nil {
			http.Error(w, "order failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:3021", logger.Middleware(mux)))
}
```

A full example is in [`examples/go-http`](../examples/go-http/main.go).

## `spanlog.New(options)`

| Field | Default | Meaning |
|---|---|---|
| `Service` | empty | Name written on every line |
| `Sink` | `PrettySink(os.Stdout)` | Destination of the lines |
| `Level` | `LevelInfo` | Lowest level written. Query lines and calls within one scope appear at `LevelDebug` |
| `SlowMs` | `DefaultSlowMs` (500) | Calls longer than this end with `SLOW` |
| `UTCOffsetMinutes` | offset of the host | UTC offset of the readable line |
| `RedactKeys` | none | Keys masked on top of `DefaultRedactKeys` |
| `ErrorMessage` | none | Replaces the text of every error. See [Errors](log-format.md#errors) |
| `Warn` | a line on stderr | Receives problems of spanlog itself, such as a sink that failed |

## `Start`

`Start` needs a named error result and `defer done(&err)`, written directly. Go requires a name on every result once
one has a name, so a function with another result reads `(id int64, err error)`. `recover` sees a panic only from the
deferred function itself, so inside a closure a call that panicked would end with `DONE`.

Pass the returned context to the calls inside. A call that receives the outer context is written as if it were not
nested.

## `Call`

`Call` wraps one piece of work under a name. `WithSummary` reads what `fn` stored.

```go
var count int
err := logger.Call(ctx, "PriceClient.Fetch", func(ctx context.Context) error {
	rows, err := client.Fetch(ctx, sku)
	count = len(rows)
	return err
},
	spanlog.WithParams(map[string]any{"sku": sku}),
	spanlog.WithSummary(func() map[string]any { return map[string]any{"count": count} }),
)
```

## Requests

`logger.Middleware(next)` runs each request inside `WithRequest` and sets the id on the response under `X-Request-Id`.
`logger.Middleware(next, spanlog.MiddlewareOptions{Header: "X-Correlation-Id"})` uses another header. An incoming id
made of letters, digits, dot, underscore and hyphen, at most 64 characters, is kept. Otherwise a new id is generated.
`spanlog.RequestID(ctx)` returns it.

Outside HTTP, `spanlog.WithRequest(ctx, jobID)` gives every line under that context the same id.

## Query lines

`Connector` wraps the `driver.Connector` of any `database/sql` driver. With pgx:

    connector := stdlib.GetConnector(*config) // config from pgx.ParseConfig
    db := sql.OpenDB(logger.Connector(connector))

A driver without its own connector gives one through `driver.DriverContext`:
`sql.Open` once, then `db.Driver().(driver.DriverContext).OpenConnector(dsn)`. For a driver with neither, a
`driver.Connector` whose `Connect` returns `Open(dsn)` of the driver does the same.

The `START` and `DONE` lines of a statement are at debug level and appear with `Level: spanlog.LevelDebug`. A slow or
failed statement writes at warn, and so does the `DONE` line of a `ROLLBACK`.

- A statement writes its pair around the call to the driver. A query ends when its rows are closed, which
  `database/sql` does after the last row, on an error and on `Rows.Close`. Rows left open keep the query without an end
  line. The duration of a query includes the time the application spends between rows.
- A query fails when reading a row fails. Postgres sends an error such as a division by zero with the row that causes
  it, after the rows before it.
- `BeginTx` writes a `BEGIN` pair, `Commit` a `COMMIT` pair and `Rollback` a `ROLLBACK` pair that names the tables
  written in the transaction. The three belong to the frame of the context given to `BeginTx`, because `driver.Tx` has
  no context. A `Rollback` after `Commit` writes nothing, because `database/sql` returns `sql.ErrTxDone` without calling
  the driver.
- When the context of a transaction ends, `database/sql` rolls it back on a goroutine of its own. pgx closes a
  connection whose statement is cancelled. That `ROLLBACK` ends with `FAILED conn closed`, and Postgres undoes the
  transaction as the session ends.
- A statement that the driver answers with `driver.ErrSkip` runs again as a prepared statement, and writes one pair.
  Goroutines that share a `Tx` or a `Conn` can run statements between the two calls, and each statement still keeps
  its own pair.

`StartQuery` writes the START line of a SQL statement in the frame of `ctx` and returns a `QuerySpan`. `End` writes
`DONE` or `SLOW`, and `Fail(err)` writes `FAILED` with the text of `err` and no stack. The frame that receives the error
writes it with its stack. The statement is read for its operation and tables, and its text is not written.

    span := logger.StartQuery(ctx, query)
    _, err := conn.Exec(ctx, query, args...)
    span.Fail(err)

`WithRolledBackTables` gives a `ROLLBACK` the tables that it undoes. The rules are in [Query lines](log-format.md#query-lines).

## Sinks

- `PrettySink(w)` writes the readable lines.
- `JSONSink(w)` writes one JSON object per line.
- `SlogSink(logger)` hands fields and message to a `*slog.Logger`.
- A `Sink` is any type with `Write(Rendered) error`.
- With `SlogSink`, the handler of the `*slog.Logger` filters too. The default handlers write Info and above, so query
  lines need `LevelDebug` on both spanlog and the handler.

## What stays out of the logs

Arguments and results stay out unless passed through `WithParams` or `WithSummary`, which are redacted by key. A
sensitive value in a plain string is not seen. `target` of a request holds the real path, query string removed. The text
of an error is written as it is, and `ErrorMessage` replaces it.

## Known limits

Limits of errors, panics and the middleware are listed in [Errors in Go](log-format.md#errors-in-go).

`sql.Conn.Raw` hands over the connection of `Connector`, not the one of the driver. `db.Driver()` returns the driver of
the wrapped connector, and a connection opened from it writes no query lines.

- A statement that the driver answers with `driver.ErrSkip` through `sql.Conn.Raw` is never prepared by `database/sql`.
  It ends with `FAILED driver: skip fast-path; continue as if unimplemented` when the connection closes.
- A query whose context ends while its rows are read ends with what the driver returns when the rows close: `FAILED`
  with that error, or `DONE` when it returns none. `database/sql` closes the rows without passing the error of the
  context to the driver, and keeps that error for `rows.Err()`.
- On a `DB`, when the driver answers `driver.ErrBadConn`, `database/sql` runs the statement again. The first attempt
  ends with `FAILED driver: bad connection` and the next one writes its own pair.
- Interfaces that `database/sql/driver` gains after Go 1.26 are not passed through by the connections, statements and
  rows of `Connector`.

## Tested versions

Go 1.26.5 on Linux, with pgx 5.12.0 against Postgres 18.6. Older versions have not been run.
