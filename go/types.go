// Package spanlog writes readable call-boundary logs: one line when a call
// starts and one line when it ends, with its duration. The line format follows
// docs/log-format.md and is shared with the Node.js package.
package spanlog

import "time"

// Level is the severity of a line.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Action is the kind of event a line describes.
type Action string

const (
	ActionRequest Action = "request"
	ActionRun     Action = "run"
	ActionCall    Action = "call"
	ActionQuery   Action = "query"
)

// Phase is the point in an action that a line describes.
type Phase string

const (
	PhaseStart   Phase = "START"
	PhaseDone    Phase = "DONE"
	PhaseSlow    Phase = "SLOW"
	PhaseFailed  Phase = "FAILED"
	PhaseAborted Phase = "ABORTED"
)

// ErrorInfo describes a failure on a FAILED line.
type ErrorInfo struct {
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
}

// Event is one line before rendering. An empty string means the field is absent.
type Event struct {
	Time    time.Time
	Service string
	ReqID   string
	// Scope and Method name the frame that owns the line.
	Scope  string
	Method string
	Action Action
	Target string
	// SameScope is true for a call whose callee has the scope of the caller.
	SameScope bool
	Phase     Phase
	// DurationMs is the raw duration. It is nil on START.
	DurationMs *float64
	Status     *int
	Op         string
	Tables     []string
	Err        *ErrorInfo
	// FirstReport is true when this line is the first report of Err in the request.
	FirstReport bool
	Params      map[string]any
	Summary     map[string]any
}

// Fields holds the structured fields of a line. The field order is the order of
// the JSON writer. omitzero keeps an empty tables list and an empty params object.
type Fields struct {
	Service string         `json:"service"`
	ReqID   string         `json:"reqId,omitempty"`
	Scope   string         `json:"scope,omitempty"`
	Method  string         `json:"method,omitempty"`
	Action  Action         `json:"action"`
	Target  string         `json:"target,omitempty"`
	Phase   Phase          `json:"phase"`
	Ms      *float64       `json:"ms,omitempty"`
	Status  *int           `json:"status,omitempty"`
	Op      string         `json:"op,omitempty"`
	Tables  []string       `json:"tables,omitzero"`
	Err     *ErrorInfo     `json:"err,omitempty"`
	Params  map[string]any `json:"params,omitzero"`
	Summary map[string]any `json:"summary,omitzero"`
}

// Rendered is an event turned into its level, message, fields and readable lines.
type Rendered struct {
	Time   time.Time
	Level  Level
	Msg    string
	Fields Fields
	// Lines holds the readable line, followed by stack lines on the first report
	// of an error.
	Lines []string
}
