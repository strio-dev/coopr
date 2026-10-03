package buildah

import (
	"context"
	"path/filepath"

	"coopr/internal/storeactivity"
)

func acquirePlanActivity(ctx context.Context, options PlanOptions) (*storeactivity.Lease, error) {
	roots := activityRoots(options.Store.GraphRoot, "", "")
	if options.Store.ImageStore != "" {
		roots = append(roots, filepath.Dir(filepath.Clean(options.Store.ImageStore)))
	}
	if options.Resolver != nil {
		roots = append(roots, options.Resolver.ImageStoreDir(), options.Resolver.ComponentStoreDir())
	}
	return storeactivity.AcquireShared(ctx, roots...)
}

func acquireSupervisedActivity(ctx context.Context, options SupervisedPlanOptions) (*storeactivity.Lease, error) {
	roots := activityRoots(options.Store.GraphRoot, options.ImageStoreDir, options.ComponentStoreDir)
	if options.Store.ImageStore != "" {
		roots = append(roots, filepath.Dir(filepath.Clean(options.Store.ImageStore)))
	}
	return storeactivity.AcquireShared(ctx, roots...)
}

func activityRoots(graphRoot, imageStore, componentStore string) []string {
	roots := make([]string, 0, 3)
	if graphRoot != "" {
		roots = append(roots, filepath.Dir(filepath.Clean(graphRoot)))
	}
	if imageStore != "" {
		roots = append(roots, imageStore)
	}
	if componentStore != "" {
		roots = append(roots, componentStore)
	}
	return roots
}
