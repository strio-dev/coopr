package buildah

import (
	"context"

	"coopr/internal/storeactivity"
)

func acquirePlanActivity(ctx context.Context, options PlanOptions) (*storeactivity.Lease, error) {
	roots := ActivityRoots(options.Store, "")
	if options.Resolver != nil {
		roots = append(roots, options.Resolver.ComponentStoreDir())
	}
	return storeactivity.AcquireShared(ctx, roots...)
}

func acquireSupervisedActivity(ctx context.Context, options SupervisedPlanOptions) (*storeactivity.Lease, error) {
	roots := ActivityRoots(options.Store, options.ComponentStoreDir)
	return storeactivity.AcquireShared(ctx, roots...)
}

// ActivityRoots returns the native storage and optional component boundaries
// shared by build, copy and maintenance activity leases.
func ActivityRoots(store StoreOptions, componentStore string) []string {
	roots := make([]string, 0, 4)
	for _, root := range []string{store.GraphRoot, store.RunRoot, store.ImageStore, componentStore} {
		if root != "" {
			roots = append(roots, root)
		}
	}
	return roots
}
