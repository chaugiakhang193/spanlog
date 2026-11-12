package spanlog

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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
