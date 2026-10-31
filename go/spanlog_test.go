package spanlog

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var measured = regexp.MustCompile(`\d+(\.\d+)?ms`)

type recordingSink struct {
	mu   sync.Mutex
	seen []Rendered
}

func (s *recordingSink) Write(rendered Rendered) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, rendered)
	return nil
}

// shapes hides the measured duration, which changes from run to run.
func (s *recordingSink) shapes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.seen))
	for i, rendered := range s.seen {
		out[i] = string(rendered.Level) + " " + measured.ReplaceAllString(rendered.Msg, "Nms")
	}
	return out
}

func newTestLogger(options Options) (*Logger, *recordingSink) {
	sink := &recordingSink{}
	options.Service = "shop"
	options.Sink = sink
	if options.Level == "" {
		options.Level = LevelDebug
	}
	return New(options), sink
}

func assertShapes(t *testing.T, sink *recordingSink, want ...string) {
	t.Helper()
	got := sink.shapes()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

type orderService struct {
	log    *Logger
	failed error
}

func (s *orderService) reserve(ctx context.Context) (err error) {
	_, done := s.log.Start(ctx, "InventoryService", "Reserve")
	defer done(&err)
	return s.failed
}

func (s *orderService) validate(ctx context.Context) (err error) {
	_, done := s.log.Start(ctx, "OrderService", "Validate")
	defer done(&err)
	return nil
}

func (s *orderService) handle(ctx context.Context) (err error) {
	ctx, done := s.log.Start(ctx, "OrderService", "Handle")
	defer done(&err)
	if err := s.validate(ctx); err != nil {
		return err
	}
	return s.reserve(ctx)
}

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
	if !strings.Contains(stack[1], "(*orderService).reserve") || !strings.Contains(stack[1], "spanlog_test.go:") {
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

func TestCallWritesParamsAndSummaryRedacted(t *testing.T) {
	log, sink := newTestLogger(Options{})
	found := 0

	err := log.Call(context.Background(), "search.run", func(context.Context) error {
		found = 3
		return nil
	},
		WithParams(map[string]any{"query": "a", "token": "t"}),
		WithSummary(func() map[string]any { return map[string]any{"count": found, "apiKey": "k"} }),
	)

	if err != nil {
		t.Fatal(err)
	}
	assertShapes(t, sink,
		`info call search.run | START {"query":"a","token":"[REDACTED]"}`,
		`info call search.run | DONE Nms {"apiKey":"[REDACTED]","count":3}`,
	)
}

func TestCallInsideAFrameKeepsTheFrameAndTheRequest(t *testing.T) {
	log, sink := newTestLogger(Options{})
	ctx, done := log.Start(WithRequest(context.Background(), "r-4"), "A", "B")

	callErr := log.Call(ctx, "http.get", func(inner context.Context) error {
		if RequestID(inner) != "r-4" {
			t.Errorf("request id = %q", RequestID(inner))
		}
		_, innerDone := log.Start(inner, "C", "D")
		var err error
		innerDone(&err)
		return nil
	})
	var err error
	done(&err)

	if callErr != nil {
		t.Fatal(callErr)
	}
	assertShapes(t, sink,
		"info A.B - run | START",
		"info A.B - call http.get | START",
		"info A.B - call C.D | START",
		"info A.B - call C.D | DONE Nms",
		"info A.B - call http.get | DONE Nms",
		"info A.B - run | DONE Nms",
	)
}

func TestCallReturnsAndReportsTheError(t *testing.T) {
	log, sink := newTestLogger(Options{})
	failure := errors.New("x")

	err := log.Call(context.Background(), "t", func(context.Context) error { return failure })

	if err != failure {
		t.Errorf("err = %v", err)
	}
	readable := strings.Join(sink.seen[1].Lines, "\n")
	if strings.Contains(readable, "runCall") || strings.Contains(readable, "(*Logger).Call") {
		t.Errorf("own frames left in:\n%s", readable)
	}
}

func TestCallPanicKeepsGoingUp(t *testing.T) {
	log, sink := newTestLogger(Options{})
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = log.Call(context.Background(), "t", func(context.Context) error { panic("boom") })
	}()

	if recovered != "boom" {
		t.Errorf("recovered = %v", recovered)
	}
	assertShapes(t, sink, "info call t | START", "error call t | FAILED Nms boom")
}

func TestProblemsInParamsAndSummaryDoNotStopTheCall(t *testing.T) {
	warnings := []string{}
	log, sink := newTestLogger(Options{Warn: func(message string) { warnings = append(warnings, message) }})

	err := log.Call(context.Background(), "t", func(context.Context) error { return nil },
		WithParams(map[string]any{"c": make(chan int)}),
		WithSummary(func() map[string]any { panic("broken") }),
	)

	if err != nil {
		t.Fatal(err)
	}
	assertShapes(t, sink, "info call t | START", "info call t | DONE Nms")
	if len(warnings) != 2 {
		t.Errorf("warnings = %q", warnings)
	}
}

type failingSink struct{ panics bool }

func (s failingSink) Write(Rendered) error {
	if s.panics {
		panic("sink panic")
	}
	return errors.New("sink error")
}

func TestBrokenSinkDoesNotFailTheCall(t *testing.T) {
	for name, sink := range map[string]failingSink{"error": {}, "panic": {panics: true}} {
		t.Run(name, func(t *testing.T) {
			warnings := 0
			log := New(Options{Service: "s", Sink: sink, Warn: func(string) { warnings++ }})

			err := log.Call(context.Background(), "t", func(context.Context) error { return nil })

			if err != nil {
				t.Fatal(err)
			}
			if warnings != 2 {
				t.Errorf("warnings = %d, want 2", warnings)
			}
		})
	}
}

func TestRequestIDOutsideARequestIsEmpty(t *testing.T) {
	if got := RequestID(context.Background()); got != "" {
		t.Errorf("got %q", got)
	}
}

// holdsAny has a comparable type. Comparing two values panics at run time when the
// field holds a slice.
type holdsAny struct{ X any }

func (holdsAny) Error() string { return "holds any" }

type panickingIs struct{ error }

func (panickingIs) Is(error) bool { panic("is broke") }

type panickingUnwrap struct{ error }

func (panickingUnwrap) Unwrap() error { panic("unwrap broke") }

type matchesAll struct{ error }

func (matchesAll) Is(error) bool { return true }

type unwrapsToItself struct{ error }

func (e *unwrapsToItself) Unwrap() error { return e }

func TestUnwrapThatNeverEndsStillWritesFailed(t *testing.T) {
	log, sink := newTestLogger(Options{})

	_ = (&orderService{log: log, failed: &unwrapsToItself{errors.New("x")}}).reserve(context.Background())

	assertShapes(t, sink, "info InventoryService.Reserve - run | START", "error InventoryService.Reserve - run | FAILED Nms x")
}

func TestFailureThatPanicsWhenComparedStillWritesFailed(t *testing.T) {
	cases := map[string]error{
		"slice in an any field": holdsAny{X: []int{1}},
		"Is panics":             panickingIs{errors.New("x")},
		"Unwrap panics":         panickingUnwrap{errors.New("x")},
	}
	for name, failure := range cases {
		t.Run(name, func(t *testing.T) {
			log, sink := newTestLogger(Options{Warn: func(string) {}})
			service := &orderService{log: log, failed: failure}
			ctx := WithRequest(context.Background(), "r")

			_ = service.reserve(ctx)
			_ = service.reserve(ctx)

			phases := []Phase{}
			for _, rendered := range sink.seen {
				phases = append(phases, rendered.Fields.Phase)
			}
			if !slices.Equal(phases, []Phase{PhaseStart, PhaseFailed, PhaseStart, PhaseFailed}) {
				t.Errorf("phases = %v", phases)
			}
		})
	}
}

func TestOnlyTheSameFailureCountsAsReported(t *testing.T) {
	seen := errors.New("seen")
	fresh := errors.New("fresh")
	cases := map[string]struct {
		next error
		want Level
	}{
		"same value":            {seen, LevelInfo},
		"wrapped":               {fmt.Errorf("outer: %w", seen), LevelInfo},
		"joined, all reported":  {errors.Join(seen, fmt.Errorf("again: %w", seen)), LevelInfo},
		"joined with a new one": {errors.Join(seen, fresh), LevelError},
		"Is that matches all":   {matchesAll{fresh}, LevelError},
		"unrelated":             {fresh, LevelError},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			log, sink := newTestLogger(Options{})
			ctx := WithRequest(context.Background(), "r")

			_ = (&orderService{log: log, failed: seen}).reserve(ctx)
			_ = (&orderService{log: log, failed: testCase.next}).reserve(ctx)

			last := sink.seen[len(sink.seen)-1]
			if last.Level != testCase.want {
				t.Errorf("level = %s, want %s", last.Level, testCase.want)
			}
		})
	}
}

type uncomparableWrapper struct {
	parts []string
	inner error
}

func (e uncomparableWrapper) Error() string { return "wrapper" }

func (e uncomparableWrapper) Unwrap() error { return e.inner }

func TestUncomparableWrapperStillFindsTheReportedError(t *testing.T) {
	log, sink := newTestLogger(Options{})
	ctx := WithRequest(context.Background(), "r")
	seen := errors.New("seen")

	_ = (&orderService{log: log, failed: seen}).reserve(ctx)
	_ = (&orderService{log: log, failed: uncomparableWrapper{inner: errors.New("other")}}).reserve(ctx)
	_ = (&orderService{log: log, failed: uncomparableWrapper{inner: seen}}).reserve(ctx)

	if last := sink.seen[len(sink.seen)-1]; last.Level != LevelInfo {
		t.Errorf("level = %s, want info", last.Level)
	}
}

// unwrapCallsLogger runs a failing Call while its chain is being read.
type unwrapCallsLogger struct {
	error
	log *Logger
	ctx context.Context
}

func (e unwrapCallsLogger) Unwrap() error {
	_ = e.log.Call(e.ctx, "inner", func(context.Context) error { return errors.New("inner") })
	return nil
}

func TestUnwrapThatCallsTheLoggerDoesNotBlockTheRequest(t *testing.T) {
	log, sink := newTestLogger(Options{})
	ctx := WithRequest(context.Background(), "r")
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		_ = (&orderService{log: log, failed: unwrapCallsLogger{errors.New("outer"), log, ctx}}).reserve(ctx)
	}()

	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("the call did not end")
	}
	assertShapes(t, sink,
		"info InventoryService.Reserve - run | START",
		"info call inner | START",
		"error call inner | FAILED Nms inner",
		"error InventoryService.Reserve - run | FAILED Nms outer",
	)
}

func wrappedTimes(failure error, times int) error {
	for range times {
		failure = fmt.Errorf("w: %w", failure)
	}
	return failure
}

func TestReportedErrorIsFoundWithinTheLimitOfTheChain(t *testing.T) {
	cases := map[string]struct {
		wrappers int
		want     Level
	}{
		"last error inside the limit": {maxChainErrors - 1, LevelInfo},
		"first error past the limit":  {maxChainErrors, LevelError},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			log, sink := newTestLogger(Options{})
			ctx := WithRequest(context.Background(), "r")
			seen := errors.New("seen")

			_ = (&orderService{log: log, failed: seen}).reserve(ctx)
			_ = (&orderService{log: log, failed: wrappedTimes(seen, testCase.wrappers)}).reserve(ctx)

			last := sink.seen[len(sink.seen)-1]
			if last.Level != testCase.want {
				t.Errorf("level = %s, want %s", last.Level, testCase.want)
			}
		})
	}
}

func TestJoinPastTheLimitOfTheChainCountsAsNew(t *testing.T) {
	log, sink := newTestLogger(Options{})
	ctx := WithRequest(context.Background(), "r")
	seen := errors.New("seen")
	branches := make([]error, maxChainErrors)
	for i := range branches {
		branches[i] = seen
	}

	_ = (&orderService{log: log, failed: seen}).reserve(ctx)
	_ = (&orderService{log: log, failed: errors.Join(branches...)}).reserve(ctx)

	if last := sink.seen[len(sink.seen)-1]; last.Level != LevelError {
		t.Errorf("level = %s, want error", last.Level)
	}
}

type wrapsNothing struct{ error }

func (wrapsNothing) Unwrap() error { return nil }

func TestUnwrapThatReturnsNilTakesNoPlaceInTheChain(t *testing.T) {
	log, sink := newTestLogger(Options{})
	ctx := WithRequest(context.Background(), "r")
	branches := make([]error, 17)
	for i := range branches {
		branches[i] = wrapsNothing{fmt.Errorf("leaf %d", i)}
		_ = (&orderService{log: log, failed: branches[i]}).reserve(ctx)
	}

	_ = (&orderService{log: log, failed: errors.Join(branches...)}).reserve(ctx)

	if last := sink.seen[len(sink.seen)-1]; last.Level != LevelInfo {
		t.Errorf("level = %s, want info", last.Level)
	}
}

func TestSameFailureAboveTheLimitOfTheChainIsFoundByItsOwnValue(t *testing.T) {
	log, sink := newTestLogger(Options{})
	ctx := WithRequest(context.Background(), "r")
	failure := wrappedTimes(errors.New("x"), maxChainErrors)

	_ = (&orderService{log: log, failed: failure}).reserve(ctx)
	_ = (&orderService{log: log, failed: failure}).reserve(ctx)

	if last := sink.seen[len(sink.seen)-1]; last.Level != LevelInfo {
		t.Errorf("level = %s, want info", last.Level)
	}
}

// joinsItself branches twice at every level. A walk without a total limit would
// call Unwrap about 2^32 times.
type joinsItself struct {
	error
	calls *atomic.Int64
}

func (e joinsItself) Unwrap() []error {
	e.calls.Add(1)
	return []error{e, e}
}

func TestUnwrapThatBranchesForeverIsCalledWithinTheLimit(t *testing.T) {
	log, sink := newTestLogger(Options{})
	calls := &atomic.Int64{}

	_ = (&orderService{log: log, failed: joinsItself{errors.New("x"), calls}}).reserve(context.Background())

	assertShapes(t, sink, "info InventoryService.Reserve - run | START", "error InventoryService.Reserve - run | FAILED Nms x")
	if calls.Load() > maxChainErrors {
		t.Errorf("Unwrap ran %d times", calls.Load())
	}
}

func TestSameFailureFromManyGoroutinesIsReportedOnce(t *testing.T) {
	log, sink := newTestLogger(Options{})
	ctx := WithRequest(context.Background(), "r")
	failure := fmt.Errorf("w: %w", errors.New("x"))
	var running sync.WaitGroup

	for range 32 {
		running.Go(func() {
			_ = (&orderService{log: log, failed: failure}).reserve(ctx)
		})
	}
	running.Wait()

	firstReports := 0
	for _, rendered := range sink.seen {
		if rendered.Level == LevelError {
			firstReports++
		}
	}
	if firstReports != 1 {
		t.Errorf("first reports = %d, want 1", firstReports)
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

func TestPanickingWarnChangesNeitherResultNorPanic(t *testing.T) {
	breakWarn := func(string) { panic("warn broke") }
	sink := failingSink{panics: true}

	t.Run("result", func(t *testing.T) {
		log := New(Options{Service: "s", Sink: sink, Warn: breakWarn})
		ran := false
		failure := errors.New("x")

		err := log.Call(context.Background(), "t", func(context.Context) error {
			ran = true
			return failure
		})

		if !ran || err != failure {
			t.Errorf("ran = %v, err = %v", ran, err)
		}
	})
	t.Run("panic", func(t *testing.T) {
		log := New(Options{Service: "s", Sink: sink, Warn: breakWarn})
		failure := errors.New("x")
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_ = log.Call(context.Background(), "t", func(context.Context) error { panic(failure) })
		}()

		if recovered != failure {
			t.Errorf("recovered = %v", recovered)
		}
	})
}

type panickingJSON struct{}

func (panickingJSON) MarshalJSON() ([]byte, error) { panic("json broke") }

func TestParamsThatPanicWhenEncodedDoNotStopTheCall(t *testing.T) {
	warnings := 0
	log, sink := newTestLogger(Options{Warn: func(string) { warnings++ }})
	ran := false

	err := log.Call(context.Background(), "t", func(context.Context) error {
		ran = true
		return nil
	}, WithParams(map[string]any{"x": panickingJSON{}}))

	if err != nil || !ran {
		t.Fatalf("ran = %v, err = %v", ran, err)
	}
	assertShapes(t, sink, "info call t | START", "info call t | DONE Nms")
	if warnings != 1 {
		t.Errorf("warnings = %d", warnings)
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
