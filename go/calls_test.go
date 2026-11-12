package spanlog

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStartWritesRunAndCallLines(t *testing.T) {
	log, sink := newTestLogger(Options{})
	service := &orderService{log: log}

	if err := service.handle(WithRequest(context.Background(), "r-1")); err != nil {
		t.Fatal(err)
	}

	assertShapes(t, sink,
		"info OrderService.Handle - run | START",
		"debug OrderService.Handle - call OrderService.Validate | START",
		"debug OrderService.Handle - call OrderService.Validate | DONE Nms",
		"info OrderService.Handle - call InventoryService.Reserve | START",
		"info OrderService.Handle - call InventoryService.Reserve | DONE Nms",
		"info OrderService.Handle - run | DONE Nms",
	)
	for _, rendered := range sink.seen {
		if rendered.Fields.ReqID != "r-1" {
			t.Errorf("reqId = %q on %q", rendered.Fields.ReqID, rendered.Msg)
		}
	}
}

func TestDefaultLevelHidesCallsWithinOneScope(t *testing.T) {
	log, sink := newTestLogger(Options{Level: LevelInfo})
	service := &orderService{log: log}

	if err := service.handle(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, shape := range sink.shapes() {
		if strings.Contains(shape, "Validate") {
			t.Errorf("debug line written: %s", shape)
		}
	}
}

func TestSlowCallEndsWithSlowAtWarn(t *testing.T) {
	limit := 1.0
	log, sink := newTestLogger(Options{SlowMs: &limit})
	ctx, done := log.Start(context.Background(), "A", "B")
	_ = ctx
	time.Sleep(5 * time.Millisecond)

	var err error
	done(&err)

	assertShapes(t, sink, "info A.B - run | START", "warn A.B - run | SLOW Nms")
}

func TestDoneWritesOnlyOnce(t *testing.T) {
	log, sink := newTestLogger(Options{})
	_, done := log.Start(context.Background(), "A", "B")

	var err error
	done(&err)
	done(&err)

	if len(sink.seen) != 2 {
		t.Errorf("wrote %d lines, want 2", len(sink.seen))
	}
}

func TestDoneAcceptsANilPointer(t *testing.T) {
	log, sink := newTestLogger(Options{})
	_, done := log.Start(context.Background(), "A", "B")

	done(nil)

	assertShapes(t, sink, "info A.B - run | START", "info A.B - run | DONE Nms")
}

func TestGoexitEndsWithDoneAsTheSpecStates(t *testing.T) {
	log, sink := newTestLogger(Options{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		_, done := log.Start(context.Background(), "A", "B")
		var err error
		defer done(&err)
		runtime.Goexit()
	}()
	wait.Wait()

	assertShapes(t, sink, "info A.B - run | START", "info A.B - run | DONE Nms")
}

func TestRequestIDOutsideARequestIsEmpty(t *testing.T) {
	if got := RequestID(context.Background()); got != "" {
		t.Errorf("got %q", got)
	}
}
