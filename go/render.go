package spanlog

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const stackIndent = "    "

// RenderOptions configures Render.
type RenderOptions struct {
	// UTCOffsetMinutes is the offset of the readable line from UTC, positive east
	// of UTC.
	UTCOffsetMinutes int
	// StackFilter picks the stack lines shown under the readable line. It receives
	// the trimmed, non-empty lines after the first one. err.stack in the fields
	// keeps every line.
	StackFilter func(lines []string) []string
}

// Render turns an event into its level, message, fields and readable lines. It
// is a pure function. spec/cases.json pins its output.
func Render(event Event, options RenderOptions) Rendered {
	level := levelFor(event)
	frame := frameOf(event)
	tail := tailOf(event)
	msg := tail
	if frame != "" {
		msg = frame + " - " + tail
	}
	lines := []string{readableLine(event, level, frame, tail, options.UTCOffsetMinutes)}
	lines = append(lines, stackLines(event, options.StackFilter)...)
	return Rendered{
		Time:   event.Time,
		Level:  level,
		Msg:    msg,
		Fields: fieldsOf(event),
		Lines:  lines,
	}
}

func levelFor(event Event) Level {
	switch event.Phase {
	case PhaseDone:
		if event.Action == ActionQuery && event.Op == "ROLLBACK" {
			return LevelWarn
		}
		return baseLevel(event)
	case PhaseSlow, PhaseAborted:
		return LevelWarn
	case PhaseFailed:
		if event.Action == ActionRequest || event.Action == ActionQuery {
			return LevelWarn
		}
		if event.FirstReport {
			return LevelError
		}
		return baseLevel(event)
	default:
		return baseLevel(event)
	}
}

func baseLevel(event Event) Level {
	if event.Action == ActionQuery {
		return LevelDebug
	}
	if event.Action == ActionCall && event.SameScope {
		return LevelDebug
	}
	return LevelInfo
}

func frameOf(event Event) string {
	if event.Scope == "" || event.Method == "" {
		return ""
	}
	return event.Scope + "." + event.Method
}

// tailOf is the part that the message and the readable line share: body, phase
// and detail. The message puts the frame in front of it, the readable line puts
// the frame in brackets.
func tailOf(event Event) string {
	detail := detailOf(event)
	if detail == "" {
		return bodyOf(event) + " | " + string(event.Phase)
	}
	return bodyOf(event) + " | " + string(event.Phase) + " " + detail
}

func bodyOf(event Event) string {
	switch event.Action {
	case ActionRun:
		return "run"
	case ActionCall:
		return "call " + event.Target
	case ActionQuery:
		return "query " + event.Target
	default:
		return event.Target
	}
}

func detailOf(event Event) string {
	ms := ""
	if event.DurationMs != nil {
		ms = formatMs(*event.DurationMs) + "ms"
	}
	if event.Action == ActionRequest {
		return requestDetail(event, ms)
	}
	switch event.Phase {
	case PhaseStart:
		if event.Params == nil {
			return ""
		}
		return stableJSON(event.Params)
	case PhaseDone, PhaseSlow:
		if event.Summary == nil {
			return ms
		}
		return ms + " " + stableJSON(event.Summary)
	case PhaseFailed:
		if event.Err == nil {
			return ms
		}
		return ms + " " + errorText(event.Err)
	default:
		return ms
	}
}

func requestDetail(event Event, ms string) string {
	switch event.Phase {
	case PhaseStart:
		return ""
	case PhaseAborted:
		return ms
	default:
		status := "-"
		if event.Status != nil {
			status = strconv.Itoa(*event.Status)
		}
		return status + " " + ms
	}
}

func errorText(err *ErrorInfo) string {
	if err.Name == "" {
		return err.Message
	}
	return err.Name + ": " + err.Message
}

func fieldsOf(event Event) Fields {
	fields := Fields{
		Service: event.Service,
		ReqID:   event.ReqID,
		Scope:   event.Scope,
		Method:  event.Method,
		Action:  event.Action,
		Target:  event.Target,
		Phase:   event.Phase,
		Status:  event.Status,
		Op:      event.Op,
		Tables:  event.Tables,
		Params:  event.Params,
		Summary: event.Summary,
	}
	if event.DurationMs != nil {
		ms := roundMs(*event.DurationMs)
		fields.Ms = &ms
	}
	if event.Err != nil {
		fields.Err = errorFields(event)
	}
	return fields
}

func errorFields(event Event) *ErrorInfo {
	fields := &ErrorInfo{Name: event.Err.Name, Message: event.Err.Message}
	if isFirstReportOnFrameLine(event) {
		fields.Stack = event.Err.Stack
	}
	return fields
}

// Only the first report of an error on a frame line carries the stack. Frames
// further out and failed SQL statements repeat the name and message only.
func isFirstReportOnFrameLine(event Event) bool {
	return event.FirstReport && (event.Action == ActionRun || event.Action == ActionCall)
}

func readableLine(event Event, level Level, frame, tail string, utcOffsetMinutes int) string {
	owner := "[" + event.Service + "]"
	if frame != "" {
		owner += " [" + frame + "]"
	}
	reqID := event.ReqID
	if reqID == "" {
		reqID = "-"
	}
	label := fmt.Sprintf("%-5s", strings.ToUpper(string(level)))
	return formatTime(event.Time, utcOffsetMinutes) + " " + label + " " + owner + " req=" + reqID + " " + tail
}

// The first line of a stack repeats the name and message, which the main line
// already shows.
func stackLines(event Event, stackFilter func([]string) []string) []string {
	if event.Err == nil || event.Err.Stack == "" || !isFirstReportOnFrameLine(event) {
		return nil
	}
	lines := []string{}
	for i, line := range strings.Split(event.Err.Stack, "\n") {
		trimmed := strings.TrimSpace(line)
		if i > 0 && trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if stackFilter != nil {
		lines = stackFilter(lines)
	}
	indented := make([]string, len(lines))
	for i, line := range lines {
		indented[i] = stackIndent + line
	}
	return indented
}

func formatTime(t time.Time, utcOffsetMinutes int) string {
	zone := time.FixedZone("", utcOffsetMinutes*60)
	return t.In(zone).Format("2006-01-02 15:04:05.000 -07:00")
}

// roundMs rounds a half up, as Math.round does in the Node.js package. math.Round
// rounds a half away from zero. A zero result drops its sign, because -0 prints with
// a minus sign in Go and without one in Node.js.
func roundMs(ms float64) float64 {
	tenths := ms * 10
	rounded := math.Floor(tenths)
	if tenths-rounded >= 0.5 {
		rounded++
	}
	if rounded == 0 {
		return 0
	}
	return rounded / 10
}

// formatMs uses the 'f' format with the shortest digits, which never writes an
// exponent.
func formatMs(ms float64) string {
	return strconv.FormatFloat(roundMs(ms), 'f', -1, 64)
}
