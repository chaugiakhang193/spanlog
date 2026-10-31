package spanlog

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// DefaultSlowMs is the duration above which a call ends with SLOW.
const DefaultSlowMs = 500.0

var levelRank = map[Level]int{
	LevelDebug: 10,
	LevelInfo:  20,
	LevelWarn:  30,
	LevelError: 40,
}

// Options configures a Logger.
type Options struct {
	// Service is the name written on every line.
	Service string
	// Sink is the destination of the rendered lines. It defaults to PrettySink on
	// stdout.
	Sink Sink
	// Level is the lowest level written. It defaults to info.
	Level Level
	// SlowMs is the duration in milliseconds above which a call ends with SLOW. It
	// defaults to DefaultSlowMs.
	SlowMs *float64
	// UTCOffsetMinutes is the UTC offset of the readable line. It defaults to the
	// offset of the host time zone at the moment each line is written.
	UTCOffsetMinutes *int
	// RedactKeys are masked in params, summary and panic values on top of
	// DefaultRedactKeys.
	RedactKeys []string
	// ErrorMessage replaces the message of every error on the lines and in
	// err.stack. It defaults to the text of the error, which can repeat a value from
	// the application. When it panics, a fixed text stands in for the message.
	ErrorMessage func(error) string
	// Warn receives problems of spanlog itself, such as a sink that failed. It
	// defaults to a line on stderr.
	Warn func(message string)
}

// Logger writes the lines of calls.
type Logger struct {
	service      string
	sink         Sink
	minRank      int
	slowMs       float64
	offset       *int
	redactKeys   []string
	errorMessage func(error) string
	warn         func(string)
}

// New returns a Logger.
func New(options Options) *Logger {
	logger := &Logger{
		service:      options.Service,
		sink:         options.Sink,
		minRank:      levelRank[LevelInfo],
		slowMs:       DefaultSlowMs,
		offset:       options.UTCOffsetMinutes,
		redactKeys:   options.RedactKeys,
		errorMessage: options.ErrorMessage,
		warn:         options.Warn,
	}
	if logger.sink == nil {
		logger.sink = PrettySink(os.Stdout)
	}
	if rank, ok := levelRank[options.Level]; ok {
		logger.minRank = rank
	}
	if options.SlowMs != nil {
		logger.slowMs = *options.SlowMs
	}
	if logger.warn == nil {
		logger.warn = func(message string) { fmt.Fprintln(os.Stderr, message) }
	}
	logger.warn = withoutPanic(logger.warn)
	return logger
}

// withoutPanic stops a Warn callback that panics. The callback runs inside the
// guards of finish and emit, and a panic there would replace the panic or the result
// of the call being logged.
func withoutPanic(warn func(string)) func(string) {
	return func(message string) {
		defer func() { _ = recover() }()
		warn(message)
	}
}

// DoneFunc writes the end line of a call. Defer it directly, as in
//
//	defer done(&err)
//
// recover only sees a panic from the deferred function itself. Inside a closure it
// returns nil, and a call that panicked would end with DONE. Only the first call
// writes a line.
type DoneFunc func(errp *error)

// Start writes the START line of a call to scope.method and returns a context for
// the work inside it. The enclosing function needs a named error result:
//
//	func (s *OrderService) HandleOrder(ctx context.Context) (err error) {
//		ctx, done := s.log.Start(ctx, "OrderService", "HandleOrder")
//		defer done(&err)
//		return s.inventory.Reserve(ctx)
//	}
func (l *Logger) Start(ctx context.Context, scope, method string) (context.Context, DoneFunc) {
	request := requestOf(ctx)
	callee := &frame{scope: scope, method: method}
	line := ownerOf(ctx, callee)
	line.ReqID = request.reqID
	line.Phase = PhaseStart
	l.emit(line)
	child := context.WithValue(ctx, contextKey{}, spanContext{request: request, frame: callee})
	return child, l.newDone(line, request, nil)
}

// ownerOf decides whose line a call writes. Inside another call the line belongs to
// the caller and names the callee as target. Otherwise the callee writes a run line
// in its own scope.
func ownerOf(ctx context.Context, callee *frame) Event {
	current, ok := currentContext(ctx)
	if !ok || current.frame == nil {
		return Event{Scope: callee.scope, Method: callee.method, Action: ActionRun}
	}
	return Event{
		Scope:     current.frame.scope,
		Method:    current.frame.method,
		Action:    ActionCall,
		Target:    callee.scope + "." + callee.method,
		SameScope: current.frame.scope == callee.scope,
	}
}

type callConfig struct {
	params    map[string]any
	summarize func() map[string]any
}

// CallOption configures Call.
type CallOption func(*callConfig)

// WithParams writes params, redacted, on the START line.
func WithParams(params map[string]any) CallOption {
	return func(config *callConfig) { config.params = params }
}

// WithSummary writes the result of summarize, redacted, on the DONE or SLOW line.
// fn can store what summarize needs in a variable both of them close over.
func WithSummary(summarize func() map[string]any) CallOption {
	return func(config *callConfig) { config.summarize = summarize }
}

// Call runs fn as an explicit call named target and writes its lines. A panic in fn
// ends the call with FAILED and keeps going up.
func (l *Logger) Call(ctx context.Context, target string, fn func(context.Context) error, options ...CallOption) error {
	return l.runCall(ctx, target, fn, options)
}

func (l *Logger) runCall(ctx context.Context, target string, fn func(context.Context) error, options []CallOption) (err error) {
	config := callConfig{}
	for _, option := range options {
		option(&config)
	}
	request := requestOf(ctx)
	line := Event{Action: ActionCall, Target: target, ReqID: request.reqID}
	child := spanContext{request: request}
	if current, ok := currentContext(ctx); ok && current.frame != nil {
		line.Scope = current.frame.scope
		line.Method = current.frame.method
		child.frame = current.frame
	}
	start := line
	start.Phase = PhaseStart
	start.Params = l.redactRecord("params", target, config.params)
	l.emit(start)
	done := l.newDone(line, request, config.summarize)
	defer done(&err)
	return fn(context.WithValue(ctx, contextKey{}, child))
}

// newDone returns the function that writes the end line of line. recover sits
// directly in the returned function because the caller defers that function.
func (l *Logger) newDone(line Event, request *requestScope, summarize func() map[string]any) DoneFunc {
	startedAt := time.Now()
	var ended atomic.Bool
	return func(errp *error) {
		thrown := recover()
		if ended.CompareAndSwap(false, true) {
			l.finish(line, request, startedAt, errp, thrown, summarize)
		}
		if thrown != nil {
			panic(thrown)
		}
	}
}

// finish must never replace the panic or the result of the call it describes.
func (l *Logger) finish(line Event, request *requestScope, startedAt time.Time, errp *error, thrown any, summarize func() map[string]any) {
	defer func() {
		if problem := recover(); problem != nil {
			l.warn(fmt.Sprintf("spanlog failed to write the end of %s: %v", nameOf(line), problem))
		}
	}()
	durationMs := float64(time.Since(startedAt)) / float64(time.Millisecond)
	line.DurationMs = &durationMs
	failure := thrown
	if failure == nil && errp != nil && *errp != nil {
		failure = *errp
	}
	if failure != nil {
		line.Phase = PhaseFailed
		info := describe(failure, l.redactKeys, l.errorMessage)
		line.FirstReport = request.markReported(failure)
		if line.FirstReport {
			info.Stack = stackText(info, 2)
		}
		line.Err = &info
		l.emit(line)
		return
	}
	line.Phase = PhaseDone
	if durationMs > l.slowMs {
		line.Phase = PhaseSlow
	}
	if summarize != nil {
		line.Summary = l.summaryOf(line, summarize)
	}
	l.emit(line)
}

func (l *Logger) summaryOf(line Event, summarize func() map[string]any) (summary map[string]any) {
	defer func() {
		if problem := recover(); problem != nil {
			l.warn(fmt.Sprintf("spanlog summary failed for %s: %v", nameOf(line), problem))
			summary = nil
		}
	}()
	return l.redactRecord("summary", nameOf(line), summarize())
}

// redactRecord runs before the call on the START line. Encoding runs MarshalJSON of
// the application, and a panic there must not stop the call.
func (l *Logger) redactRecord(kind, target string, record map[string]any) (out map[string]any) {
	if record == nil {
		return nil
	}
	defer func() {
		if problem := recover(); problem != nil {
			l.warn(fmt.Sprintf("spanlog %s failed for %s: %v", kind, target, problem))
			out = nil
		}
	}()
	redacted, err := Redact(record, RedactOptions{Keys: l.redactKeys})
	if err != nil {
		l.warn(fmt.Sprintf("spanlog %s failed for %s: %v", kind, target, err))
		return nil
	}
	return redacted.(map[string]any)
}

func nameOf(line Event) string {
	if line.Target != "" {
		return line.Target
	}
	return line.Scope + "." + line.Method
}

func (l *Logger) emit(event Event) {
	defer func() {
		if problem := recover(); problem != nil {
			l.warn(fmt.Sprintf("spanlog sink failed: %v", problem))
		}
	}()
	event.Time = time.Now()
	event.Service = l.service
	if levelRank[levelFor(event)] < l.minRank {
		return
	}
	rendered := Render(event, RenderOptions{UTCOffsetMinutes: l.offsetAt(event.Time), StackFilter: withoutOwnFrames})
	if err := l.sink.Write(rendered); err != nil {
		l.warn(fmt.Sprintf("spanlog sink failed: %v", err))
	}
}

func (l *Logger) offsetAt(t time.Time) int {
	if l.offset != nil {
		return *l.offset
	}
	_, seconds := t.Zone()
	return seconds / 60
}
