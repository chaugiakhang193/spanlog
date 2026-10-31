package spanlog

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
)

const (
	nameThrownValue     = "ThrownValue"
	nameUnreadableError = "UnreadableError"
	unreadableMessage   = "[unreadable thrown value]"
	maxStackFrames      = 32
	modulePrefix        = "github.com/chaugiakhang193/spanlog/go."
)

// ownFrames are the functions left out of the readable stack lines. An exact list
// leaves functions of the same package in its own tests on those lines.
var ownFrames = map[string]bool{
	modulePrefix + "(*Logger).Call":                 true,
	modulePrefix + "(*Logger).runCall":              true,
	modulePrefix + "(*requestMiddleware).ServeHTTP": true,
	"runtime.gopanic":                               true,
	"runtime.goexit":                                true,
}

// withoutOwnFrames is the StackFilter of a Logger. err.stack keeps every frame.
func withoutOwnFrames(lines []string) []string {
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		function, _, _ := strings.Cut(strings.TrimPrefix(line, "at "), " (")
		if !ownFrames[function] {
			kept = append(kept, line)
		}
	}
	return kept
}

// describe reads a failure into its name and message. Reading runs code of the
// application: Error() and errorMessage. Either can panic, and the unreadable
// description stands in.
func describe(failure any, redactKeys []string, errorMessage func(error) string) (info ErrorInfo) {
	defer func() {
		if recover() != nil {
			info = ErrorInfo{Name: nameUnreadableError, Message: unreadableMessage}
		}
	}()
	err, ok := asError(failure)
	if ok {
		if errorMessage != nil {
			return ErrorInfo{Message: errorMessage(err)}
		}
		return ErrorInfo{Message: err.Error()}
	}
	redacted, redactErr := Redact(failure, RedactOptions{Keys: redactKeys})
	if redactErr != nil {
		return ErrorInfo{Name: nameUnreadableError, Message: unreadableMessage}
	}
	return ErrorInfo{Name: nameThrownValue, Message: stableJSON(redacted)}
}

// asError returns a failure as an error. A string, number or boolean from a panic
// has no structure to encode and becomes an error with the same text.
func asError(failure any) (error, bool) {
	if err, ok := failure.(error); ok {
		return err, true
	}
	switch reflect.ValueOf(failure).Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return errors.New(fmt.Sprint(failure)), true
	default:
		return nil, false
	}
}

// stackText writes a stack in the shape err.stack has in docs/log-format.md. The
// first line holds one line of the message, since rendering drops only the first
// line. skip counts the frames left out, starting at the caller of stackText.
func stackText(info ErrorInfo, skip int) string {
	pcs := make([]uintptr, maxStackFrames)
	count := runtime.Callers(skip+2, pcs)
	var builder strings.Builder
	builder.WriteString(stackHeader(info))
	frames := runtime.CallersFrames(pcs[:count])
	for {
		frame, more := frames.Next()
		if frame.Function != "" {
			fmt.Fprintf(&builder, "\n    at %s (%s:%d)", frame.Function, frame.File, frame.Line)
		}
		if !more {
			return builder.String()
		}
	}
}

func stackHeader(info ErrorInfo) string {
	message, _, _ := strings.Cut(info.Message, "\n")
	if info.Name == "" {
		return message
	}
	return info.Name + ": " + message
}
