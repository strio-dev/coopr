package client

import (
	"errors"
	"testing"
)

func TestExecutionErrorUnwrapContainsOnlyPresentCauses(t *testing.T) {
	primary, cleanup := errors.New("primary"), errors.New("cleanup")
	for _, test := range []struct {
		name           string
		cause, cleanup error
	}{
		{name: "none"},
		{name: "primary", cause: primary},
		{name: "cleanup", cleanup: cleanup},
		{name: "both", cause: primary, cleanup: cleanup},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := &ExecutionError{ID: "execution", Cause: test.cause, CleanupError: test.cleanup}
			if errors.Is(err, primary) != (test.cause != nil) || errors.Is(err, cleanup) != (test.cleanup != nil) {
				t.Fatal("error causes were lost")
			}
			var visit func(error)
			visit = func(value error) {
				if wrapped, ok := value.(interface{ Unwrap() []error }); ok {
					for _, child := range wrapped.Unwrap() {
						if child == nil {
							t.Fatal("error tree contains a nil child")
						}
						visit(child)
					}
				} else if child := errors.Unwrap(value); child != nil {
					visit(child)
				}
			}
			visit(err)
		})
	}
}
