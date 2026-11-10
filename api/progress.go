package api

import "context"

type progressContextKey struct{}

// WithProgressObserver attaches an optional observer to a backend invocation.
// The caller owns synchronization when several executions share an observer.
func WithProgressObserver(ctx context.Context, observer func(OperationProgress)) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, progressContextKey{}, observer)
}

// ProgressObserver returns the observer selected by the invocation owner.
func ProgressObserver(ctx context.Context) func(OperationProgress) {
	observer, _ := ctx.Value(progressContextKey{}).(func(OperationProgress))
	return observer
}
