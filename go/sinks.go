package spanlog

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// Sink receives every rendered event that passes the level filter. Goroutines of
// one request can call Write at the same time.
type Sink interface {
	Write(rendered Rendered) error
}

type prettySink struct {
	mu sync.Mutex
	w  io.Writer
}

// PrettySink writes the readable lines to w, one event per Write call on w.
func PrettySink(w io.Writer) Sink {
	return &prettySink{w: w}
}

func (s *prettySink) Write(rendered Rendered) error {
	text := strings.Join(rendered.Lines, "\n") + "\n"
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := io.WriteString(s.w, text)
	return err
}

type jsonSink struct {
	mu sync.Mutex
	w  io.Writer
}

// JSONSink writes one JSON object per line to w: time in UTC, level, the fields
// of docs/log-format.md, then msg.
func JSONSink(w io.Writer) Sink {
	return &jsonSink{w: w}
}

// jsonRecord embeds Fields. encoding/json writes promoted fields at the position
// of the embedded struct, between level and msg.
type jsonRecord struct {
	Time  string `json:"time"`
	Level Level  `json:"level"`
	Fields
	Msg string `json:"msg"`
}

func (s *jsonSink) Write(rendered Rendered) error {
	encoded, err := encodeJSON(jsonRecord{
		Time:   rendered.Time.UTC().Format("2006-01-02T15:04:05.000Z"),
		Level:  rendered.Level,
		Fields: rendered.Fields,
		Msg:    rendered.Msg,
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(append(encoded, '\n'))
	return err
}

type slogSink struct {
	handler slog.Handler
}

// SlogSink hands the fields and the message to a slog logger. Time, level and
// message encoding then follow its handler.
func SlogSink(logger *slog.Logger) Sink {
	return &slogSink{handler: logger.Handler()}
}

func (s *slogSink) Write(rendered Rendered) error {
	level := slogLevel(rendered.Level)
	ctx := context.Background()
	if !s.handler.Enabled(ctx, level) {
		return nil
	}
	record := slog.NewRecord(rendered.Time, level, rendered.Msg, 0)
	record.AddAttrs(slogAttrs(rendered.Fields)...)
	return s.handler.Handle(ctx, record)
}

func slogLevel(level Level) slog.Level {
	switch level {
	case LevelDebug:
		return slog.LevelDebug
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func slogAttrs(fields Fields) []slog.Attr {
	attrs := []slog.Attr{slog.String("service", fields.Service)}
	addString := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}
	addString("reqId", fields.ReqID)
	addString("scope", fields.Scope)
	addString("method", fields.Method)
	attrs = append(attrs, slog.String("action", string(fields.Action)))
	addString("target", fields.Target)
	attrs = append(attrs, slog.String("phase", string(fields.Phase)))
	if fields.Ms != nil {
		attrs = append(attrs, slog.Float64("ms", *fields.Ms))
	}
	if fields.Status != nil {
		attrs = append(attrs, slog.Int("status", *fields.Status))
	}
	addString("op", fields.Op)
	if fields.Tables != nil {
		attrs = append(attrs, slog.Any("tables", fields.Tables))
	}
	if fields.Err != nil {
		attrs = append(attrs, slog.Any("err", slogError(fields.Err)))
	}
	if fields.Params != nil {
		attrs = append(attrs, slog.Any("params", fields.Params))
	}
	if fields.Summary != nil {
		attrs = append(attrs, slog.Any("summary", fields.Summary))
	}
	return attrs
}

func slogError(err *ErrorInfo) slog.Value {
	attrs := []slog.Attr{}
	if err.Name != "" {
		attrs = append(attrs, slog.String("name", err.Name))
	}
	attrs = append(attrs, slog.String("message", err.Message))
	if err.Stack != "" {
		attrs = append(attrs, slog.String("stack", err.Stack))
	}
	return slog.GroupValue(attrs...)
}
