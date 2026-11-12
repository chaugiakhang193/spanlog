package spanlog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
