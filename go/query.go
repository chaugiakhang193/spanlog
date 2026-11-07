package spanlog

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

type queryConfig struct {
	rolledBackTables []string
	rolledBackGiven  bool
}

// QueryOption configures StartQuery.
type QueryOption func(*queryConfig)

// WithRolledBackTables names the tables that a ROLLBACK undoes. They replace
// the tables read from the statement, which a ROLLBACK does not name. Other
// statements ignore them.
func WithRolledBackTables(tables []string) QueryOption {
	return func(config *queryConfig) {
		config.rolledBackTables = tables
		config.rolledBackGiven = true
	}
}

// QuerySpan ends the query lines opened by StartQuery. Only the first call to
// End or Fail writes a line.
type QuerySpan struct {
	logger    *Logger
	line      Event
	startedAt time.Time
	ended     atomic.Bool
}

// StartQuery writes the START line of a SQL statement in the frame running in
// ctx. The statement is read for its op and tables. Its text is never written.
// Database adapters call it.
func (l *Logger) StartQuery(ctx context.Context, sql string, options ...QueryOption) *QuerySpan {
	config := queryConfig{}
	for _, option := range options {
		option(&config)
	}
	summary := summarizeSQL(sql)
	if summary.Op == "ROLLBACK" && config.rolledBackGiven {
		summary.Tables = append([]string{}, config.rolledBackTables...)
	}
	line := Event{
		ReqID:  requestOf(ctx).reqID,
		Action: ActionQuery,
		Target: queryTarget(summary),
		Op:     summary.Op,
		Tables: summary.Tables,
	}
	if current, ok := currentContext(ctx); ok && current.frame != nil {
		line.Scope = current.frame.scope
		line.Method = current.frame.method
	}
	start := line
	start.Phase = PhaseStart
	l.emit(start)
	return &QuerySpan{logger: l, line: line, startedAt: time.Now()}
}

// End writes DONE or SLOW.
func (s *QuerySpan) End() {
	s.Fail(nil)
}

// Fail writes FAILED with the message of err, without a stack. A nil err writes
// DONE or SLOW. The error stays unreported in the request, and the call that
// returns it writes it with its stack.
func (s *QuerySpan) Fail(err error) {
	if !s.ended.CompareAndSwap(false, true) {
		return
	}
	defer func() {
		if problem := recover(); problem != nil {
			s.logger.warn(fmt.Sprintf("spanlog failed to write the end of %s: %v", s.line.Target, problem))
		}
	}()
	line := s.line
	durationMs := float64(time.Since(s.startedAt)) / float64(time.Millisecond)
	line.DurationMs = &durationMs
	switch {
	case err != nil:
		line.Phase = PhaseFailed
		info := describe(err, s.logger.redactKeys, s.logger.errorMessage)
		line.Err = &info
	case durationMs > s.logger.slowMs:
		line.Phase = PhaseSlow
	default:
		line.Phase = PhaseDone
	}
	s.logger.emit(line)
}
