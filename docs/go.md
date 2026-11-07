# Go guide

Requires Go 1.26. The package has no dependency outside the standard library.

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
| `Level` | `LevelInfo` | Lowest level written. Calls within one scope appear at `LevelDebug` |
| `SlowMs` | `DefaultSlowMs` (500) | Calls longer than this end with `SLOW` |
| `UTCOffsetMinutes` | offset of the host | UTC offset of the readable line |
| `RedactKeys` | none | Keys masked on top of `DefaultRedactKeys` |
| `ErrorMessage` | none | Replaces the text of every error. See [Errors](log-format.md#errors) |
| `Warn` | a line on stderr | Receives problems of spanlog itself, such as a sink that failed |

## `Start`

`Start` needs a named error result and `defer done(&err)`, written directly. `recover` sees a panic only from the
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

`logger.Middleware(next, spanlog.MiddlewareOptions{Header: "X-Request-Id"})` runs each request inside `WithRequest` and
sets the id on the response. An incoming id made of letters, digits, dot, underscore and hyphen, at most 64 characters,
is kept. Otherwise a new id is generated. `spanlog.RequestID(ctx)` returns it.

Outside HTTP, `spanlog.WithRequest(ctx, jobID)` gives every line under that context the same id.

## Query lines

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

## What stays out of the logs

Arguments and results stay out unless passed through `WithParams` or `WithSummary`, which are redacted by key. A
sensitive value in a plain string is not seen. `target` of a request holds the real path, query string removed. The text
of an error is written as it is, and `ErrorMessage` replaces it.

## Known limits

Limits of errors, panics and the middleware are listed in [Errors in Go](log-format.md#errors-in-go).

## Tested versions

Go 1.26.5 on Linux. Older versions have not been run.
