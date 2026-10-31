package spanlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"
)

const defaultRequestIDHeader = "X-Request-Id"

// The request id lands on every line of the request. An id from a client is kept
// only when it cannot break a line or flood it.
var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// MiddlewareOptions configures Middleware.
type MiddlewareOptions struct {
	// Header carries the request id. An incoming id made of letters, digits, dot,
	// underscore and hyphen, at most 64 characters, is kept. Otherwise a new id is
	// generated. The id is set on the response under the same header. It defaults to
	// X-Request-Id.
	Header string
}

// Middleware runs each request inside WithRequest and writes its START and end
// lines. A client that closed the connection ends the request with ABORTED. A panic
// other than http.ErrAbortHandler ends it with FAILED and status 500, and it keeps
// going up to net/http. Register it outside http.TimeoutHandler, which answers 503
// from a writer that Middleware sees only from the outside.
func (l *Logger) Middleware(next http.Handler, options ...MiddlewareOptions) http.Handler {
	header := defaultRequestIDHeader
	if len(options) > 0 && options[0].Header != "" {
		header = options[0].Header
	}
	return &requestMiddleware{logger: l, next: next, header: header}
}

// requestMiddleware is a named type because the stack filter matches function names
// and the name of a closure changes with the function it is inlined into.
type requestMiddleware struct {
	logger *Logger
	next   http.Handler
	header string
}

func (m *requestMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := incomingRequestID(r.Header.Values(m.header))
	if reqID == "" {
		reqID = newRequestID()
	}
	w.Header().Set(m.header, reqID)
	ctx := WithRequest(r.Context(), reqID)
	// URL.Path holds decoded text, where %0A is a line break. EscapedPath keeps the
	// target on one line and leaves out the query string.
	span := m.logger.startRequest(ctx, r.Method+" "+r.URL.EscapedPath())
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	var writer http.ResponseWriter = recorder
	if _, ok := w.(http.Flusher); ok {
		writer = flushingRecorder{recorder}
	}
	defer func() {
		thrown := recover()
		span.finish(r.Context(), recorder.status, thrown)
		if thrown != nil {
			panic(thrown)
		}
	}()
	m.next.ServeHTTP(writer, r.WithContext(ctx))
}

type requestSpan struct {
	logger    *Logger
	line      Event
	startedAt time.Time
	ended     atomic.Bool
}

func (l *Logger) startRequest(ctx context.Context, target string) *requestSpan {
	line := Event{Action: ActionRequest, Target: target, ReqID: RequestID(ctx)}
	start := line
	start.Phase = PhaseStart
	l.emit(start)
	return &requestSpan{logger: l, line: line, startedAt: time.Now()}
}

// finish reads the context of the server, which net/http cancels when the client
// closes the connection. A deadline, as set by http.TimeoutHandler, leaves the client
// connected and is not an abort.
func (s *requestSpan) finish(serverContext context.Context, status int, thrown any) {
	if !s.ended.CompareAndSwap(false, true) {
		return
	}
	line := s.line
	durationMs := float64(time.Since(s.startedAt)) / float64(time.Millisecond)
	line.DurationMs = &durationMs
	switch {
	case thrown == http.ErrAbortHandler, thrown == nil && errors.Is(serverContext.Err(), context.Canceled):
		line.Phase = PhaseAborted
	case thrown != nil:
		status = http.StatusInternalServerError
		line.Status = &status
		line.Phase = PhaseFailed
	default:
		line.Status = &status
		line.Phase = endPhase(status, durationMs, s.logger.slowMs)
	}
	s.logger.emit(line)
}

func endPhase(status int, durationMs, slowMs float64) Phase {
	switch {
	case status >= http.StatusInternalServerError:
		return PhaseFailed
	case durationMs > slowMs:
		return PhaseSlow
	default:
		return PhaseDone
	}
}

// incomingRequestID returns the id from a client, or an empty string. A repeated
// header holds no single id.
func incomingRequestID(values []string) string {
	if len(values) == 1 && safeRequestID.MatchString(values[0]) {
		return values[0]
	}
	return ""
}

func newRequestID() string {
	id := make([]byte, 4)
	_, _ = rand.Read(id)
	return hex.EncodeToString(id)
}

// statusRecorder keeps the first final status. net/http treats 101 as final, as it
// does every status from 200. Unwrap gives http.ResponseController the original
// writer, for Hijack and deadlines. A hijacked connection writes past the recorder.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader && (status >= http.StatusOK || status == http.StatusSwitchingProtocols) {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(data)
}

// FlushError sits in front of Unwrap for http.ResponseController. net/http in Go 1.26
// sends the header before a flush can fail, and a later WriteHeader no longer changes
// the status of the response. Any failure other than a writer without flush counts as
// sent, because the recorder cannot see the connection.
func (r *statusRecorder) FlushError() error {
	err := http.NewResponseController(r.ResponseWriter).Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		r.wroteHeader = true
	}
	return err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// flushingRecorder serves a handler that asserts http.Flusher. It stands in only for
// a writer that has the method, and a handler sees the same answer as without the
// middleware.
type flushingRecorder struct {
	*statusRecorder
}

func (r flushingRecorder) Flush() {
	_ = r.FlushError()
}
