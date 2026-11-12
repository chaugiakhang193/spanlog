package spanlog

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestErrorIsReportedOnceThroughWrapping(t *testing.T) {
	log, sink := newTestLogger(Options{})
	base := errors.New("stock not available")
	run := func(ctx context.Context) (err error) {
		ctx, done := log.Start(ctx, "OrderService", "Handle")
		defer done(&err)
		inner := &orderService{log: log, failed: base}
		if err := inner.reserve(ctx); err != nil {
			return fmt.Errorf("handle: %w", err)
		}
		return nil
	}

	err := run(WithRequest(context.Background(), "r-2"))

	if !errors.Is(err, base) {
		t.Fatalf("err = %v", err)
	}
	assertShapes(t, sink,
		"info OrderService.Handle - run | START",
		"info OrderService.Handle - call InventoryService.Reserve | START",
		"error OrderService.Handle - call InventoryService.Reserve | FAILED Nms stock not available",
		"info OrderService.Handle - run | FAILED Nms handle: stock not available",
	)
	if sink.seen[2].Fields.Err.Stack == "" || len(sink.seen[2].Lines) < 2 {
		t.Errorf("first report has no stack: %+v", sink.seen[2])
	}
	if sink.seen[3].Fields.Err.Stack != "" || len(sink.seen[3].Lines) != 1 {
		t.Errorf("outer frame repeats the stack: %+v", sink.seen[3])
	}
}

func TestSameErrorInAnotherRequestIsReportedAgain(t *testing.T) {
	log, sink := newTestLogger(Options{})
	shared := errors.New("x")
	service := &orderService{log: log, failed: shared}

	_ = service.reserve(WithRequest(context.Background(), "a"))
	_ = service.reserve(WithRequest(context.Background(), "b"))

	assertShapes(t, sink,
		"info InventoryService.Reserve - run | START",
		"error InventoryService.Reserve - run | FAILED Nms x",
		"info InventoryService.Reserve - run | START",
		"error InventoryService.Reserve - run | FAILED Nms x",
	)
}

func TestStackNamesTheCallerAndLeavesOutSpanlogFrames(t *testing.T) {
	log, sink := newTestLogger(Options{})
	service := &orderService{log: log, failed: errors.New("x")}

	_ = service.handle(context.Background())

	got := sink.seen[len(sink.seen)-2]
	stack := strings.Split(got.Fields.Err.Stack, "\n")
	if stack[0] != "x" {
		t.Errorf("first line = %q", stack[0])
	}
	if !strings.Contains(stack[1], "(*orderService).reserve") || !strings.Contains(stack[1], "helpers_test.go:") {
		t.Errorf("first frame = %q", stack[1])
	}
	if readable := strings.Join(got.Lines, "\n"); strings.Contains(readable, "(*Logger).") || strings.Contains(readable, "newDone") {
		t.Errorf("own frames left in:\n%s", readable)
	}
}

func TestMultilineMessageStaysOutOfTheStackLines(t *testing.T) {
	log, sink := newTestLogger(Options{})
	service := &orderService{log: log, failed: errors.New("first\nsecond")}

	_ = service.reserve(context.Background())

	for _, line := range sink.seen[1].Lines[1:] {
		if !strings.HasPrefix(line, "    at ") {
			t.Errorf("stack line = %q", line)
		}
	}
}

type uncomparableError struct{ parts []string }

func (e uncomparableError) Error() string { return "uncomparable" }

type panickingError struct{}

func (panickingError) Error() string { panic("no text") }

func panicThrough(log *Logger, ctx context.Context, value any) (err error) {
	ctx, done := log.Start(ctx, "Outer", "Run")
	defer done(&err)
	func(ctx context.Context) (err error) {
		_, done := log.Start(ctx, "Inner", "Fail")
		defer done(&err)
		panic(value)
	}(ctx)
	return nil
}

// samePanicValue checks identity for a map or channel, which == rejects or compares
// by reference, and equality for the other values.
func samePanicValue(got, want any) bool {
	wantValue := reflect.ValueOf(want)
	if wantValue.Kind() == reflect.Map {
		gotValue := reflect.ValueOf(got)
		return gotValue.Kind() == reflect.Map && gotValue.Pointer() == wantValue.Pointer()
	}
	return got == want
}

func TestPanicEndsWithFailedAndKeepsItsValue(t *testing.T) {
	sentinel := errors.New("broken")
	cases := map[string]struct {
		value      any
		wantName   string
		wantDetail string
	}{
		"error":          {sentinel, "", "broken"},
		"string":         {"boom", "", "boom"},
		"number":         {42, "", "42"},
		"object":         {map[string]any{"code": "E1", "token": "t"}, nameThrownValue, `{"code":"E1","token":"[REDACTED]"}`},
		"struct":         {struct{ Code string }{"E1"}, nameThrownValue, `{"Code":"E1"}`},
		"channel":        {make(chan int), nameUnreadableError, unreadableMessage},
		"Error() panics": {panickingError{}, nameUnreadableError, unreadableMessage},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			log, sink := newTestLogger(Options{})
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				_ = panicThrough(log, context.Background(), testCase.value)
			}()

			if !samePanicValue(recovered, testCase.value) {
				t.Errorf("recovered %#v, want %#v", recovered, testCase.value)
			}
			got := sink.seen[2]
			if got.Fields.Phase != PhaseFailed || got.Level != LevelError {
				t.Errorf("inner line = %s %s", got.Level, got.Msg)
			}
			if got.Fields.Err.Name != testCase.wantName || got.Fields.Err.Message != testCase.wantDetail {
				t.Errorf("err = %+v", got.Fields.Err)
			}
		})
	}
}

func TestPanicIsReportedOnceAcrossFrames(t *testing.T) {
	log, sink := newTestLogger(Options{})
	func() {
		defer func() { _ = recover() }()
		_ = panicThrough(log, context.Background(), "boom")
	}()

	assertShapes(t, sink,
		"info Outer.Run - run | START",
		"info Outer.Run - call Inner.Fail | START",
		"error Outer.Run - call Inner.Fail | FAILED Nms boom",
		"info Outer.Run - run | FAILED Nms boom",
	)
}

func TestStackOfAPanicLeavesOutRuntimePanicFrames(t *testing.T) {
	log, sink := newTestLogger(Options{})
	func() {
		defer func() { _ = recover() }()
		_ = panicThrough(log, context.Background(), "boom")
	}()

	readable := strings.Join(sink.seen[2].Lines, "\n")
	if strings.Contains(readable, "runtime.gopanic") || !strings.Contains(readable, "panicThrough") {
		t.Errorf("readable lines:\n%s", readable)
	}
}

func TestUncomparableErrorDoesNotPanic(t *testing.T) {
	log, sink := newTestLogger(Options{})
	service := &orderService{log: log, failed: uncomparableError{parts: []string{"a"}}}

	if err := service.handle(context.Background()); err == nil {
		t.Fatal("want the error")
	}

	assertShapes(t, sink,
		"info OrderService.Handle - run | START",
		"debug OrderService.Handle - call OrderService.Validate | START",
		"debug OrderService.Handle - call OrderService.Validate | DONE Nms",
		"info OrderService.Handle - call InventoryService.Reserve | START",
		"error OrderService.Handle - call InventoryService.Reserve | FAILED Nms uncomparable",
		"error OrderService.Handle - run | FAILED Nms uncomparable",
	)
}

func TestUncomparablePanicValueDoesNotPanicInDone(t *testing.T) {
	log, sink := newTestLogger(Options{})
	func() {
		defer func() {
			if _, ok := recover().(map[string]any); !ok {
				t.Error("panic value changed")
			}
		}()
		_ = panicThrough(log, context.Background(), map[string]any{"a": 1})
	}()

	if len(sink.seen) != 4 {
		t.Errorf("wrote %d lines, want 4", len(sink.seen))
	}
}

func TestErrorMessageReplacesTheText(t *testing.T) {
	log, sink := newTestLogger(Options{ErrorMessage: func(error) string { return "hidden" }})
	service := &orderService{log: log, failed: errors.New("value 12345")}

	_ = service.reserve(context.Background())

	got := sink.seen[1]
	if got.Fields.Err.Message != "hidden" || strings.Contains(got.Fields.Err.Stack, "12345") {
		t.Errorf("err = %+v", got.Fields.Err)
	}
}

func TestErrorMessageThatPanicsHidesTheText(t *testing.T) {
	log, sink := newTestLogger(Options{ErrorMessage: func(error) string { panic("broken") }})
	service := &orderService{log: log, failed: errors.New("value 12345")}

	if err := service.reserve(context.Background()); err == nil {
		t.Fatal("want the error")
	}

	got := sink.seen[1].Fields.Err
	if got.Name != nameUnreadableError || strings.Contains(got.Stack, "12345") {
		t.Errorf("err = %+v", got)
	}
}

func TestGoroutinesOfOneRequestShareItsReports(t *testing.T) {
	log, sink := newTestLogger(Options{})
	shared := errors.New("x")
	service := &orderService{log: log, failed: shared}
	ctx := WithRequest(context.Background(), "r-3")

	var wait sync.WaitGroup
	for range 20 {
		wait.Go(func() { _ = service.reserve(ctx) })
	}
	wait.Wait()

	errorLines := 0
	for _, rendered := range sink.seen {
		if rendered.Level == LevelError {
			errorLines++
		}
	}
	if errorLines != 1 {
		t.Errorf("error lines = %d, want 1", errorLines)
	}
}

func TestThrownValueJSONSortsKeysByUTF16(t *testing.T) {
	log, sink := newTestLogger(Options{})
	low, high := string(rune(0xE000)), string(rune(0x10000))
	func() {
		defer func() { _ = recover() }()
		_ = log.Call(context.Background(), "t", func(context.Context) error {
			panic(map[string]int{low: 1, high: 2})
		})
	}()

	got := sink.seen[1].Fields.Err
	if want := `{"` + high + `":2,"` + low + `":1}`; got.Name != nameThrownValue || got.Message != want {
		t.Errorf("err = %+v, want message %s", got, want)
	}
}

func TestPanicNilEndsWithFailed(t *testing.T) {
	log, sink := newTestLogger(Options{})
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = log.Call(context.Background(), "t", func(context.Context) error { panic(nil) })
	}()

	if _, ok := recovered.(*runtime.PanicNilError); !ok {
		t.Errorf("recovered = %T", recovered)
	}
	if got := sink.seen[1].Fields.Phase; got != PhaseFailed {
		t.Errorf("phase = %s", got)
	}
}

func TestUintptrPanicIsDescribedByItsText(t *testing.T) {
	log, sink := newTestLogger(Options{})
	func() {
		defer func() { _ = recover() }()
		_ = log.Call(context.Background(), "t", func(context.Context) error { panic(uintptr(42)) })
	}()

	got := sink.seen[1].Fields.Err
	if got.Name != "" || got.Message != "42" {
		t.Errorf("err = %+v", got)
	}
}

func TestFieldsKeepOwnFramesAndReadableLinesLeaveThemOut(t *testing.T) {
	log, sink := newTestLogger(Options{})

	_ = log.Call(context.Background(), "t", func(context.Context) error { return errors.New("x") })

	got := sink.seen[1]
	if !strings.Contains(got.Fields.Err.Stack, "(*Logger).runCall") {
		t.Errorf("err.stack lost an own frame:\n%s", got.Fields.Err.Stack)
	}
	if readable := strings.Join(got.Lines, "\n"); strings.Contains(readable, "(*Logger).") {
		t.Errorf("readable lines keep an own frame:\n%s", readable)
	}
}
