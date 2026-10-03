package buildah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	upstream "go.podman.io/buildah"
)

func TestLifecycleCancellationRetainsStoppedBuilderWhenSelected(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for native lifecycle builds")
	}
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, context.Background(), root, store)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, fmt.Sprintf("from %q\nrun \"trap '' TERM; sleep 60\" network=\"none\"\n", base.reference))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output := filepath.Join(root, "layout")
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: output},
		ImageStoreDir: base.imageStoreDir, SignaturePolicyPath: policy, Lifecycle: LifecycleControls{KeepFailed: true}, Stdout: io.Discard, Stderr: io.Discard,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel error=%v", err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled output exists: %v", err)
	}
	lease, err := acquireStore(store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	containers, err := lease.store.Containers()
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) == 0 {
		t.Fatal("cancellation retained no builder with force-rm=false")
	}
	for _, container := range containers {
		builder, err := upstream.OpenBuilder(lease.store, container.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := builder.Delete(); err != nil {
			t.Fatal(err)
		}
	}
}
