package buildah

import (
	"context"

	"github.com/strio-dev/coopr/api"
)

func withOperationProgress(ctx context.Context, sink OperationProgressFunc) context.Context {
	return api.WithProgressObserver(ctx, sink)
}

// ObserveOperationPhase measures one phase using the invocation's observer.
func ObserveOperationPhase(ctx context.Context, attributes OperationProgress) (context.Context, func(string, error)) {
	return startOperationProgress(ctx, api.ProgressObserver(ctx), attributes)
}
