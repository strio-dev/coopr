package buildah

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSupervisedBuildLogsResourceUsagePerInstruction(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	logPath := filepath.Join(root, "rusage.log")
	_, err := BuildPlanSupervised(ctx, testPlan(t, "from \"scratch\"\nenv ONE=\"1\"\nlabel two=\"2\"\n"), SupervisedPlanOptions{
		Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Runtime: "crun",
		SignaturePolicyPath: writeComponentTestPolicy(t, root), Output: Output{Path: filepath.Join(root, "layout")},
		LogRusage: true, RusageLogFile: logPath, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 3 {
		t.Fatalf("resource usage lines = %d, want at least 3: %q", len(lines), data)
	}
	for _, line := range lines {
		if !strings.Contains(line, "(elapsed)") {
			t.Fatalf("non-native resource usage line %q", line)
		}
	}
}
