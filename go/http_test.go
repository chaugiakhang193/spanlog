package spanlog

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func startServer(t *testing.T, logger *Logger, handler http.HandlerFunc, options ...MiddlewareOptions) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(logger.Middleware(handler, options...))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func get(t *testing.T, url string, header http.Header) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if header != nil {
		request.Header = header
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return response
}

func waitForLines(t *testing.T, sink *recordingSink, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.shapes()) >= count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("lines = %q, want %d", sink.shapes(), count)
}

func TestMiddlewareWritesRequestLinesWithoutTheQueryString(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	server := startServer(t, logger, func(http.ResponseWriter, *http.Request) {})

	get(t, server.URL+"/orders?token=secret", nil)

	waitForLines(t, sink, 2)
	assertShapes(t, sink, "info GET /orders | START", "info GET /orders | DONE 200 Nms")
}

func TestMiddlewareRecordsStatusFailedAndSlow(t *testing.T) {
	limit := 1.0
	cases := map[string]struct {
		options Options
		handler http.HandlerFunc
		want    string
	}{
		"created": {Options{}, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(201) }, "info GET / | DONE 201 Nms"},
		"body":    {Options{}, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) }, "info GET / | DONE 200 Nms"},
		"early hint": {Options{}, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(103)
			w.WriteHeader(204)
		}, "info GET / | DONE 204 Nms"},
		"second status": {Options{}, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(404)
			w.WriteHeader(500)
		}, "info GET / | DONE 404 Nms"},
		"server error": {Options{}, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }, "warn GET / | FAILED 503 Nms"},
		"slow": {Options{SlowMs: &limit}, func(http.ResponseWriter, *http.Request) {
			time.Sleep(5 * time.Millisecond)
		}, "warn GET / | SLOW 200 Nms"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			logger, sink := newTestLogger(testCase.options)
			server := startServer(t, logger, testCase.handler)

			get(t, server.URL+"/", nil)

			waitForLines(t, sink, 2)
			assertShapes(t, sink, "info GET / | START", testCase.want)
		})
	}
}

func TestMiddlewareKeepsASafeRequestIDAndReplacesOthers(t *testing.T) {
	generated := regexp.MustCompile(`^[0-9a-f]{8}$`)
	cases := map[string]struct {
		incoming string
		keep     bool
	}{
		"safe":     {"order-1_a.b", true},
		"at limit": {strings.Repeat("a", 64), true},
		"too long": {strings.Repeat("a", 65), false},
		"unsafe":   {"a b", false},
		"empty":    {"", false},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			logger, sink := newTestLogger(Options{})
			seen := ""
			server := startServer(t, logger, func(_ http.ResponseWriter, r *http.Request) {
				seen = RequestID(r.Context())
			})

			response := get(t, server.URL, http.Header{"X-Request-Id": {testCase.incoming}})

			waitForLines(t, sink, 2)
			got := response.Header.Get("X-Request-Id")
			if got != seen || got != sink.seen[0].Fields.ReqID {
				t.Errorf("response %q, handler %q, line %q", got, seen, sink.seen[0].Fields.ReqID)
			}
			if testCase.keep && got != testCase.incoming {
				t.Errorf("id = %q, want %q", got, testCase.incoming)
			}
			if !testCase.keep && !generated.MatchString(got) {
				t.Errorf("id = %q is not generated", got)
			}
		})
	}
}

func TestMiddlewareUsesAConfiguredHeader(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	server := startServer(t, logger, func(http.ResponseWriter, *http.Request) {}, MiddlewareOptions{Header: "X-Trace"})

	response := get(t, server.URL, http.Header{"X-Trace": {"abc"}})

	waitForLines(t, sink, 2)
	if response.Header.Get("X-Trace") != "abc" || response.Header.Get("X-Request-Id") != "" {
		t.Errorf("header = %v", response.Header)
	}
}

func TestMiddlewareLinesOfTheHandlerShareTheRequestID(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	server := startServer(t, logger, func(_ http.ResponseWriter, r *http.Request) {
		service := &orderService{log: logger}
		_ = service.reserve(r.Context())
	})

	get(t, server.URL, http.Header{"X-Request-Id": {"r-5"}})

	waitForLines(t, sink, 4)
	for _, rendered := range sink.seen {
		if rendered.Fields.ReqID != "r-5" {
			t.Errorf("reqId = %q on %q", rendered.Fields.ReqID, rendered.Msg)
		}
	}
}

func TestMiddlewareWritesAbortedWhenTheClientCloses(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	entered := make(chan struct{})
	server := startServer(t, logger, func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	})
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "GET /slow HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	<-entered

	_ = conn.Close()

	waitForLines(t, sink, 2)
	assertShapes(t, sink, "info GET /slow | START", "warn GET /slow | ABORTED Nms")
}

// net/http watches the connection only after the request body has been read. The
// limit is stated in docs/log-format.md.
func TestMiddlewareCannotSeeAClientThatLeftBeforeTheBodyWasRead(t *testing.T) {
	neverSlow := 60000.0
	logger, sink := newTestLogger(Options{SlowMs: &neverSlow})
	entered := make(chan struct{})
	server := startServer(t, logger, func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
	})
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "POST /upload HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\n\r\nab"); err != nil {
		t.Fatal(err)
	}
	<-entered

	_ = conn.Close()

	waitForLines(t, sink, 2)
	assertShapes(t, sink, "info POST /upload | START", "info POST /upload | DONE 200 Nms")
}

func TestMiddlewareKeepsAPanicGoingToTheServer(t *testing.T) {
	cases := map[string]struct {
		value any
		want  string
	}{
		"panic": {"boom", "warn GET / | FAILED 500 Nms"},
		"abort": {http.ErrAbortHandler, "warn GET / | ABORTED Nms"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			logger, sink := newTestLogger(Options{})
			var outer any
			handler := logger.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				panic(testCase.value)
			}))
			func() {
				defer func() { outer = recover() }()
				request := httptest.NewRequest(http.MethodGet, "/", nil)
				handler.ServeHTTP(httptest.NewRecorder(), request)
			}()

			if outer != testCase.value {
				t.Errorf("recovered %v, want %v", outer, testCase.value)
			}
			assertShapes(t, sink, "info GET / | START", testCase.want)
		})
	}
}

func TestMiddlewareResponseWriterUnwrapsForTheController(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	var flushErr error
	server := startServer(t, logger, func(w http.ResponseWriter, _ *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
	})

	get(t, server.URL, nil)

	waitForLines(t, sink, 2)
	if flushErr != nil {
		t.Errorf("flush: %v", flushErr)
	}
}

func TestMiddlewareCanceledByTheHandlerContextIsNotAborted(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	server := startServer(t, logger, func(_ http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		<-ctx.Done()
	})

	get(t, server.URL, nil)

	waitForLines(t, sink, 2)
	assertShapes(t, sink, "info GET / | START", "info GET / | DONE 200 Nms")
}

func TestMiddlewareTargetKeepsThePathEncoded(t *testing.T) {
	for _, path := range []string{"/a%0Ab", "/a%2Fb", "/caf%C3%A9"} {
		t.Run(path, func(t *testing.T) {
			logger, sink := newTestLogger(Options{})
			server := startServer(t, logger, func(http.ResponseWriter, *http.Request) {})

			get(t, server.URL+path, nil)

			waitForLines(t, sink, 2)
			assertShapes(t, sink, "info GET "+path+" | START", "info GET "+path+" | DONE 200 Nms")
		})
	}
}

func TestMiddlewareRecordsTheStatusThatReachedTheClient(t *testing.T) {
	cases := map[string]struct {
		handler    http.HandlerFunc
		wantStatus int
		want       string
	}{
		"controller flush": {func(w http.ResponseWriter, _ *http.Request) {
			_ = http.NewResponseController(w).Flush()
			w.WriteHeader(503)
		}, 200, "info GET / | DONE 200 Nms"},
		"http.Flusher": {func(w http.ResponseWriter, _ *http.Request) {
			w.(http.Flusher).Flush()
			w.WriteHeader(503)
		}, 200, "info GET / | DONE 200 Nms"},
		"switching protocols": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(101)
			w.WriteHeader(503)
		}, 101, "info GET / | DONE 101 Nms"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			logger, sink := newTestLogger(Options{})
			server := startServer(t, logger, testCase.handler)
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
				t.Fatal(err)
			}

			response, err := http.ReadResponse(bufio.NewReader(conn), nil)

			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != testCase.wantStatus {
				t.Errorf("client status = %d, want %d", response.StatusCode, testCase.wantStatus)
			}
			waitForLines(t, sink, 2)
			assertShapes(t, sink, "info GET / | START", testCase.want)
		})
	}
}

// writerWithoutFlush hides every optional method of the writer it holds.
type writerWithoutFlush struct{ http.ResponseWriter }

func TestMiddlewareOffersFlushOnlyWhenTheWriterHasIt(t *testing.T) {
	cases := map[string]struct {
		writer      http.ResponseWriter
		wantFlusher bool
		wantErr     error
	}{
		"with flush":    {httptest.NewRecorder(), true, nil},
		"without flush": {writerWithoutFlush{httptest.NewRecorder()}, false, http.ErrNotSupported},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			logger, _ := newTestLogger(Options{})
			var isFlusher bool
			var flushErr error
			handler := logger.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, isFlusher = w.(http.Flusher)
				flushErr = http.NewResponseController(w).Flush()
			}))

			handler.ServeHTTP(testCase.writer, httptest.NewRequest(http.MethodGet, "/", nil))

			if isFlusher != testCase.wantFlusher {
				t.Errorf("http.Flusher = %v, want %v", isFlusher, testCase.wantFlusher)
			}
			if !errors.Is(flushErr, testCase.wantErr) {
				t.Errorf("flush error = %v, want %v", flushErr, testCase.wantErr)
			}
		})
	}
}

func TestMiddlewareKeepsTheStatusWhenFlushIsNotSupported(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	handler := logger.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = http.NewResponseController(w).Flush()
		w.WriteHeader(404)
	}))

	handler.ServeHTTP(writerWithoutFlush{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/", nil))

	assertShapes(t, sink, "info GET / | START", "info GET / | DONE 404 Nms")
}

// flushSendsThenFails sends the header and then fails, as a connection that breaks
// during a flush does.
type flushSendsThenFails struct {
	http.ResponseWriter
	failure error
}

func (w flushSendsThenFails) FlushError() error {
	w.WriteHeader(http.StatusOK)
	return w.failure
}

// flushFailsBeforeTheHeader fails without sending anything. The limit is stated in
// docs/log-format.md.
type flushFailsBeforeTheHeader struct{ http.ResponseWriter }

func (flushFailsBeforeTheHeader) FlushError() error { return errors.New("not sent") }

func TestMiddlewareCannotSeeAFlushThatFailedBeforeTheHeader(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	handler := logger.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = http.NewResponseController(w).Flush()
		w.WriteHeader(503)
	}))
	writer := httptest.NewRecorder()

	handler.ServeHTTP(flushFailsBeforeTheHeader{writer}, httptest.NewRequest(http.MethodGet, "/", nil))

	if writer.Code != http.StatusServiceUnavailable {
		t.Errorf("writer status = %d", writer.Code)
	}
	assertShapes(t, sink, "info GET / | START", "info GET / | DONE 200 Nms")
}

func TestMiddlewareKeepsTheStatusOfAFlushThatFailed(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	failure := errors.New("connection lost")
	var flushErr error
	handler := logger.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
		w.WriteHeader(503)
	}))
	writer := httptest.NewRecorder()

	handler.ServeHTTP(flushSendsThenFails{writer, failure}, httptest.NewRequest(http.MethodGet, "/", nil))

	if flushErr != failure {
		t.Errorf("flush error = %v", flushErr)
	}
	if writer.Code != http.StatusOK {
		t.Errorf("writer status = %d", writer.Code)
	}
	assertShapes(t, sink, "info GET / | START", "info GET / | DONE 200 Nms")
}

func TestMiddlewareCannotSeeWhatAHijackedConnectionWrites(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	server := startServer(t, logger, func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = fmt.Fprint(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_ = conn.Close()
	})

	response := get(t, server.URL, nil)

	waitForLines(t, sink, 2)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("client status = %d", response.StatusCode)
	}
	assertShapes(t, sink, "info GET / | START", "info GET / | DONE 200 Nms")
}

func TestMiddlewareWithTimeoutHandler(t *testing.T) {
	cases := map[string]struct {
		wrap func(*Logger, http.Handler) http.Handler
		want string
	}{
		"outside": {func(logger *Logger, handler http.Handler) http.Handler {
			return logger.Middleware(http.TimeoutHandler(handler, 10*time.Millisecond, ""))
		}, "warn GET / | FAILED 503 Nms"},
		"inside": {func(logger *Logger, handler http.Handler) http.Handler {
			return http.TimeoutHandler(logger.Middleware(handler), 10*time.Millisecond, "")
		}, "info GET / | DONE 200 Nms"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			logger, sink := newTestLogger(Options{})
			// The handler outlives the response. http.TimeoutHandler picks at random when
			// the handler and the deadline finish together.
			clientAnswered := make(chan struct{})
			releaseHandler := sync.OnceFunc(func() { close(clientAnswered) })
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-clientAnswered })
			server := httptest.NewServer(testCase.wrap(logger, handler))
			defer server.Close()
			defer releaseHandler()

			response := get(t, server.URL, nil)
			releaseHandler()

			waitForLines(t, sink, 2)
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("client status = %d", response.StatusCode)
			}
			assertShapes(t, sink, "info GET / | START", testCase.want)
		})
	}
}

func TestMiddlewareReplacesARepeatedRequestIDHeader(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	server := startServer(t, logger, func(http.ResponseWriter, *http.Request) {})

	response := get(t, server.URL, http.Header{"X-Request-Id": {"first", "second"}})

	waitForLines(t, sink, 2)
	if got := response.Header.Get("X-Request-Id"); got == "first" || got == "second" {
		t.Errorf("id = %q", got)
	}
}

func TestMiddlewarePanicAgainstARealServer(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	server := startServer(t, logger, func(http.ResponseWriter, *http.Request) { panic("boom") })

	_, err := http.Get(server.URL)

	if err == nil {
		t.Error("want the server to close the connection")
	}
	waitForLines(t, sink, 2)
	assertShapes(t, sink, "info GET / | START", "warn GET / | FAILED 500 Nms")
}

func TestReadableStackOfAHandlerLeavesOutTheMiddleware(t *testing.T) {
	logger, sink := newTestLogger(Options{})
	server := startServer(t, logger, func(_ http.ResponseWriter, r *http.Request) {
		_ = (&orderService{log: logger, failed: errors.New("x")}).reserve(r.Context())
	})

	get(t, server.URL, nil)

	waitForLines(t, sink, 4)
	got := sink.seen[2]
	if !strings.Contains(got.Fields.Err.Stack, "(*requestMiddleware).ServeHTTP") {
		t.Errorf("err.stack lost the middleware frame:\n%s", got.Fields.Err.Stack)
	}
	if readable := strings.Join(got.Lines, "\n"); strings.Contains(readable, "requestMiddleware") {
		t.Errorf("readable lines keep an own frame:\n%s", readable)
	}
}
