package spanlog

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"sync"
	"testing"
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

func openLoggedDB(t *testing.T, backend *fakeBackend, minimal bool) (*sql.DB, *recordingSink, *Logger) {
	t.Helper()
	log, sink := newTestLogger(Options{})
	db := sql.OpenDB(log.Connector(&fakeConnector{backend: backend, minimal: minimal}))
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db, sink, log
}
