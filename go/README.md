# spanlog for Go

spanlog writes one line when a call starts and one line when it ends, with its duration, into the logger the application
already uses.

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

The module has no release. Build it from a clone:

```bash
git clone https://github.com/chaugiakhang193/spanlog.git
```

Then, in your `go.mod`:

```
require github.com/chaugiakhang193/spanlog/go v0.0.0

replace github.com/chaugiakhang193/spanlog/go => ./spanlog/go
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

## Documentation

- [Go guide](https://github.com/chaugiakhang193/spanlog/blob/main/docs/go.md)
- [Log format](https://github.com/chaugiakhang193/spanlog/blob/main/docs/log-format.md)
- [Examples](https://github.com/chaugiakhang193/spanlog/blob/main/examples/README.md)

## License

[MIT](LICENSE)
