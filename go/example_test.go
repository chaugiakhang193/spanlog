package spanlog_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/chaugiakhang193/spanlog/go"
)

// fieldsSink prints the parts of a line that stay the same from one run to the
// next. Time, duration and request id change on every run and stay out.
type fieldsSink struct{}

func (fieldsSink) Write(rendered spanlog.Rendered) error {
	fields := rendered.Fields
	parts := []string{string(rendered.Level), string(fields.Action)}
	if fields.Target != "" {
		parts = append(parts, fields.Target)
	}
	parts = append(parts, string(fields.Phase))
	if fields.Status != nil {
		parts = append(parts, strconv.Itoa(*fields.Status))
	}
	if fields.Err != nil {
		if fields.Err.Name != "" {
			parts = append(parts, fields.Err.Name+":")
		}
		parts = append(parts, fields.Err.Message)
	}
	fmt.Println(strings.Join(parts, " "))
	return nil
}

type orderService struct {
	log *spanlog.Logger
}

func (s *orderService) HandleOrder(ctx context.Context) (err error) {
	ctx, done := s.log.Start(ctx, "OrderService", "HandleOrder")
	defer done(&err)
	return s.log.Call(ctx, "InventoryService.Reserve", func(ctx context.Context) error {
		return errors.New("stock not available")
	})
}

func (s *orderService) ConfirmOrder(ctx context.Context) (err error) {
	_, done := s.log.Start(ctx, "OrderService", "ConfirmOrder")
	defer done(&err)
	return nil
}

// Example shows a method that writes its lines with Start and an explicit Call
// that fails inside it.
func Example() {
	logger := spanlog.New(spanlog.Options{Service: "shop", Level: spanlog.LevelDebug, Sink: fieldsSink{}})
	orders := &orderService{log: logger}
	_ = orders.HandleOrder(context.Background())
	// Output:
	// info run START
	// info call InventoryService.Reserve START
	// error call InventoryService.Reserve FAILED stock not available
	// info run FAILED stock not available
}

// ExampleRender shows the readable line of one event.
func ExampleRender() {
	ms := 3.0
	rendered := spanlog.Render(spanlog.Event{
		Time:       time.Date(2026, 10, 24, 14, 15, 3, 125_000_000, time.UTC),
		Service:    "shop",
		ReqID:      "7f3a9c",
		Scope:      "OrderService",
		Method:     "handleOrder",
		Action:     spanlog.ActionCall,
		Target:     "InventoryService.reserve",
		Phase:      spanlog.PhaseDone,
		DurationMs: &ms,
	}, spanlog.RenderOptions{UTCOffsetMinutes: 420})
	fmt.Println(rendered.Level)
	fmt.Println(rendered.Msg)
	for _, line := range rendered.Lines {
		fmt.Println(line)
	}
	// Output:
	// info
	// OrderService.handleOrder - call InventoryService.reserve | DONE 3ms
	// 2026-10-24 21:15:03.125 +07:00 INFO  [shop] [OrderService.handleOrder] req=7f3a9c call InventoryService.reserve | DONE 3ms
}

// ExampleLogger_Middleware shows the lines of one request and the request id set
// on the response.
func ExampleLogger_Middleware() {
	logger := spanlog.New(spanlog.Options{Service: "shop", Sink: fieldsSink{}})
	orders := &orderService{log: logger}
	handler := logger.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := orders.ConfirmOrder(r.Context()); err != nil {
			http.Error(w, "order failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/orders", nil))
	fmt.Println(recorder.Header().Get("X-Request-Id") != "")
	// Output:
	// info request POST /orders START
	// info run START
	// info run DONE
	// info request POST /orders DONE 201
	// true
}
