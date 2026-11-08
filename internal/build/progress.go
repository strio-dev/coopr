package build

import "github.com/strio-dev/coopr/internal/buildah"

func captureBuildProgress(enabled bool, sink buildah.OperationProgressFunc) (buildah.OperationProgressFunc, func() ([]buildah.OperationProgress, uint64)) {
	if !enabled {
		return sink, nil
	}
	capture, snapshot := buildah.CaptureOperationProgress()
	if sink == nil {
		return capture, snapshot
	}
	return func(event buildah.OperationProgress) {
		capture(event)
		sink(event)
	}, snapshot
}

func setBuildProgress(report *outputReport, snapshot func() ([]buildah.OperationProgress, uint64), builds []platformBuild) {
	if snapshot == nil {
		return
	}
	report.Progress, report.ProgressDropped = snapshot()
	for _, built := range builds {
		if ^uint64(0)-report.ProgressDropped < built.progressDropped {
			report.ProgressDropped = ^uint64(0)
		} else {
			report.ProgressDropped += built.progressDropped
		}
	}
}
