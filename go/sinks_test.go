package spanlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func failedCallEvent() Event {
	ms := 3.04
	return Event{
		Time:        time.Date(2026, 10, 24, 14, 15, 4, 304_000_000, time.UTC),
		Service:     "shop",
		ReqID:       "b81e44",
		Scope:       "OrderService",
		Method:      "handleOrder",
		Action:      ActionCall,
		Target:      "InventoryService.reserve",
		Phase:       PhaseFailed,
		DurationMs:  &ms,
		FirstReport: true,
		Err: &ErrorInfo{
			Name:    "Error",
			Message: "stock not available",
			Stack:   "Error: stock not available\n    at InventoryService.reserve (file:///app/src/inventory.service.js:12:11)\n    at async OrderService.handleOrder (file:///app/src/order.service.js:20:5)",
		},
	}
}

func TestPrettySinkWritesLinesOfOneEventInOneWrite(t *testing.T) {
	var buffer bytes.Buffer
	rendered := Render(failedCallEvent(), RenderOptions{UTCOffsetMinutes: 420})

	if err := PrettySink(&buffer).Write(rendered); err != nil {
		t.Fatal(err)
	}

	want := "2026-10-24 21:15:04.304 +07:00 ERROR [shop] [OrderService.handleOrder] req=b81e44 call InventoryService.reserve | FAILED 3ms Error: stock not available\n" +
		"    at InventoryService.reserve (file:///app/src/inventory.service.js:12:11)\n" +
		"    at async OrderService.handleOrder (file:///app/src/order.service.js:20:5)\n"
	if buffer.String() != want {
		t.Errorf("got %q, want %q", buffer.String(), want)
	}
}

// The expected text is the example in docs/log-format.md.
func TestJSONSinkWritesTheLineOfTheSpecExample(t *testing.T) {
	var buffer bytes.Buffer
	rendered := Render(failedCallEvent(), RenderOptions{UTCOffsetMinutes: 420})

	if err := JSONSink(&buffer).Write(rendered); err != nil {
		t.Fatal(err)
	}

	want := `{"time":"2026-10-24T14:15:04.304Z","level":"error","service":"shop","reqId":"b81e44","scope":"OrderService","method":"handleOrder","action":"call","target":"InventoryService.reserve","phase":"FAILED","ms":3,"err":{"name":"Error","message":"stock not available","stack":"Error: stock not available\n    at InventoryService.reserve (file:///app/src/inventory.service.js:12:11)\n    at async OrderService.handleOrder (file:///app/src/order.service.js:20:5)"},"msg":"OrderService.handleOrder - call InventoryService.reserve | FAILED 3ms Error: stock not available"}` + "\n"
	if buffer.String() != want {
		t.Errorf("got  %s\nwant %s", buffer.String(), want)
	}
}

func TestSlogSinkHandsFieldsToTheLogger(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	rendered := Render(failedCallEvent(), RenderOptions{})

	if err := SlogSink(logger).Write(rendered); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["level"] != "ERROR" || got["time"] != "2026-10-24T14:15:04.304Z" {
		t.Errorf("level or time follows the wrong source: %v", got)
	}
	if got["msg"] != rendered.Msg || got["reqId"] != "b81e44" || got["ms"] != float64(3) {
		t.Errorf("fields lost: %v", got)
	}
	errGroup, ok := got["err"].(map[string]any)
	if !ok || errGroup["name"] != "Error" || !strings.Contains(errGroup["stack"].(string), "reserve") {
		t.Errorf("err group = %v", got["err"])
	}
}

func TestSlogSinkLeavesFilteringToTheHandler(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelError}))
	sink := SlogSink(logger)

	if err := sink.Write(Render(Event{Service: "shop", Action: ActionRun, Phase: PhaseStart}, RenderOptions{})); err != nil {
		t.Fatal(err)
	}

	if buffer.Len() != 0 {
		t.Errorf("info line passed an error-level handler: %s", buffer.String())
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestSinksReturnTheWriterError(t *testing.T) {
	rendered := Render(failedCallEvent(), RenderOptions{})

	for name, sink := range map[string]Sink{"pretty": PrettySink(brokenWriter{}), "json": JSONSink(brokenWriter{})} {
		t.Run(name, func(t *testing.T) {
			if err := sink.Write(rendered); err == nil {
				t.Error("want an error")
			}
		})
	}
}
