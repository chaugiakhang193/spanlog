package spanlog

import (
	"context"
	"reflect"
	"sync"
)

const maxChainErrors = 32

type contextKey struct{}

// frame is the wrapped function that is running.
type frame struct {
	scope  string
	method string
}

// requestScope is shared by every goroutine of one request. reported holds the
// failures already written with their stack.
type requestScope struct {
	reqID    string
	mu       sync.Mutex
	reported []any
}

// spanContext is the value stored in a context. frame is nil outside a Start.
type spanContext struct {
	request *requestScope
	frame   *frame
}

func currentContext(ctx context.Context) (spanContext, bool) {
	current, ok := ctx.Value(contextKey{}).(spanContext)
	return current, ok
}

// requestOf returns the request running in ctx, or a new request without an id.
func requestOf(ctx context.Context) *requestScope {
	if current, ok := currentContext(ctx); ok {
		return current.request
	}
	return &requestScope{}
}

// WithRequest returns a context that runs as one request. Every line written with
// it carries reqID, and each error is reported with its stack once.
func WithRequest(ctx context.Context, reqID string) context.Context {
	return context.WithValue(ctx, contextKey{}, spanContext{request: &requestScope{reqID: reqID}})
}

// RequestID returns the id of the request running in ctx, or an empty string.
func RequestID(ctx context.Context) string {
	if current, ok := currentContext(ctx); ok {
		return current.request.reqID
	}
	return ""
}

// failureChain holds a failure and the errors its Unwrap method returned.
type failureChain struct {
	failure  any
	branches []failureChain
}

// markReported marks failure as reported. It returns true when failure was not
// marked before. A failure whose Unwrap panics counts as new and keeps its FAILED
// line and its stack. The chain is read before the lock because Unwrap runs code of
// the application, and that code can write a line of the same request.
func (r *requestScope) markReported(failure any) bool {
	chain, readable := readChain(failure)
	if !readable {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isReported(chain) {
		return false
	}
	r.reported = append(r.reported, failure)
	return true
}

func readChain(failure any) (chain failureChain, readable bool) {
	defer func() {
		if recover() != nil {
			readable = false
		}
	}()
	errorsLeft := maxChainErrors
	return readBranches(failure, &errorsLeft), true
}

// readBranches stops at maxChainErrors, the limit for an Unwrap that returns its own
// error. A chain cut there has no branches and cannot count as reported.
func readBranches(failure any, errorsLeft *int) failureChain {
	*errorsLeft--
	chain := failureChain{failure: failure}
	for _, branch := range unwrapOnce(failure) {
		if branch == nil {
			continue
		}
		if *errorsLeft == 0 {
			chain.branches = nil
			return chain
		}
		chain.branches = append(chain.branches, readBranches(branch, errorsLeft))
	}
	return chain
}

func unwrapOnce(failure any) []error {
	switch wrapper := failure.(type) {
	case interface{ Unwrap() error }:
		return []error{wrapper.Unwrap()}
	case interface{ Unwrap() []error }:
		return wrapper.Unwrap()
	default:
		return nil
	}
}

// isReported compares by value along the chain. errors.Is would also run Is methods,
// which can match an unrelated error, and it accepts an errors.Join that holds one
// reported error next to a new one. Every branch of a Join must be reported.
func (r *requestScope) isReported(chain failureChain) bool {
	for _, seen := range r.reported {
		if sameValue(chain.failure, seen) {
			return true
		}
	}
	for _, branch := range chain.branches {
		if !r.isReported(branch) {
			return false
		}
	}
	return len(chain.branches) > 0
}

// sameValue checks first because == panics at run time on a value that holds a
// slice or a map, even when its type is comparable.
func sameValue(a, b any) bool {
	return reflect.ValueOf(a).Comparable() && reflect.ValueOf(b).Comparable() && a == b
}
