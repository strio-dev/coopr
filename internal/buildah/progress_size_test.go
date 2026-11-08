package buildah

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerResultSizeBoundIncludesLiveSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	for _, message := range []any{
		planWorkerResponse{ProgressStream: &workerProgressSummary{Events: 2, Bytes: 512}, ProgressDropped: 3},
		stageWorkerResponse{ProgressStream: &workerProgressSummary{Events: 2, Bytes: 512}, ProgressDropped: 3},
	} {
		if err := writeWorkerJSON(path, message); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeWorkerJSON(path, planWorkerResponse{Result: Result{Reference: strings.Repeat("x", workerMaxJSON)}}); err == nil {
		t.Fatal("oversized required payload was accepted")
	}
}
