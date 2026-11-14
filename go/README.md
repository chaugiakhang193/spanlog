# spanlog for Go

spanlog writes one line when a call starts and one line when it ends, with its duration. The lines go to an
`io.Writer` as readable text or JSON, or into a `*slog.Logger` the application already uses.

```text
2026-11-12 21:04:38.163 +07:00 INFO  [shop] req=b1965b3d POST /orders | START
2026-11-12 21:04:38.163 +07:00 INFO  [shop] [OrderService.HandleOrder] req=b1965b3d run | START
2026-11-12 21:04:38.163 +07:00 INFO  [shop] [OrderService.HandleOrder] req=b1965b3d call InventoryService.Reserve | START
2026-11-12 21:04:38.163 +07:00 ERROR [shop] [OrderService.HandleOrder] req=b1965b3d call InventoryService.Reserve | FAILED 0ms stock not available
    at main.(*InventoryService).Reserve (/app/main.go:39)
    at main.(*OrderService).HandleOrder (/app/main.go:68)
2026-11-12 21:04:38.163 +07:00 INFO  [shop] [OrderService.HandleOrder] req=b1965b3d run | FAILED 0.1ms stock not available
2026-11-12 21:04:38.163 +07:00 WARN  [shop] req=b1965b3d POST /orders | FAILED 500 0.2ms
```

This is one failed request from
[`examples/go-http`](https://github.com/chaugiakhang193/spanlog/blob/main/examples/go-http/main.go), without the lines
of one internal call. The stack is cut to its first two frames and its paths are shortened.

## Install

```bash
go get github.com/chaugiakhang193/spanlog/go
```

```go
import "github.com/chaugiakhang193/spanlog/go"
```

The package name is `spanlog`.

## Quick start

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

## Keep your logger

```go
logger := spanlog.New(spanlog.Options{
	Service: "shop",
	Sink:    spanlog.SlogSink(slog.Default()),
})
```

The handler of the `*slog.Logger` filters the lines too. Query lines are at `LevelDebug`.

## Documentation

- [Go guide](https://github.com/chaugiakhang193/spanlog/blob/main/docs/go.md)
- [Log format](https://github.com/chaugiakhang193/spanlog/blob/main/docs/log-format.md)
- [Examples](https://github.com/chaugiakhang193/spanlog/blob/main/examples/README.md)

## License

[MIT](https://github.com/chaugiakhang193/spanlog/blob/main/go/LICENSE)
