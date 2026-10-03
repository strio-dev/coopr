package buildah

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"coopr/internal/planner"
	"github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
)

func TestBuildDefinitionCopiesFromSelectedExternalImage(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah external-image COPY test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	definition := parseWorkerDefinition(t, "from \"scratch\"\ncopy \"/bin/busybox\" \"/tool\" from=\""+base.reference+"\"\n")
	layout := filepath.Join(root, "result")
	_, err := BuildDefinitionSupervised(ctx, definition, planner.Options{
		Mode: planner.Build, Platform: "linux/" + runtime.GOARCH,
	}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		ImageStoreDir: base.imageStoreDir, Pull: false, SignaturePolicyPath: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("external-image COPY layers = %d, want 1", len(manifest.Layers))
	}
	if copied := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()), "tool"); copied == "" {
		t.Fatal("external-image COPY produced an empty tool")
	}
}

func TestBuildDefinitionMountsSelectedExternalImageForRun(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah external-image RUN mount test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	definition := parseWorkerDefinition(t, `from "`+base.reference+`"
run "cp /external-busybox /bind-proof && cp /external-cache/busybox /cache-proof" network="none" {
  mount "bind" from="`+base.reference+`" source="/bin/busybox" target="/external-busybox" readonly="true"
  mount "cache" from="`+base.reference+`" source="/bin" target="/external-cache" id="external-image-seed"
}
`)
	layout := filepath.Join(root, "result")
	_, err := BuildDefinitionSupervised(ctx, definition, planner.Options{
		Mode: planner.Build, Platform: "linux/" + runtime.GOARCH,
	}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		ImageStoreDir: base.imageStoreDir, Pull: false, SignaturePolicyPath: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	last := manifest.Layers[len(manifest.Layers)-1]
	for _, name := range []string{"bind-proof", "cache-proof"} {
		if proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), name); proof == "" {
			t.Fatalf("external-image RUN mount produced an empty %s", name)
		}
	}
}

func TestParallelStagesCopyFromSelectedExternalImage(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	definition := parseWorkerDefinition(t, `
from "scratch" as="left"
copy "/bin/busybox" "/left" from="fixture.local/coopr/busybox:latest"
from "scratch" as="right"
copy "/bin/busybox" "/right" from="fixture.local/coopr/busybox:latest"
from "left"
copy "/right" "/right" from="right"
`)
	layout := filepath.Join(root, "result")
	_, err := BuildDefinitionSupervised(ctx, definition, planner.Options{
		Mode: planner.Build, Platform: "linux/" + runtime.GOARCH,
	}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		ImageStoreDir: base.imageStoreDir, Pull: false, SignaturePolicyPath: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 2 {
		t.Fatalf("parallel external-image COPY layers = %d, want 2", len(manifest.Layers))
	}
	for index, name := range []string{"left", "right"} {
		layer := manifest.Layers[index]
		if copied := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", layer.Digest.Encoded()), name); copied == "" {
			t.Fatalf("parallel external-image COPY layer %d has empty %s", index, name)
		}
	}
}

func TestComponentInvocationCopiesFromSelectedExternalImage(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless component external-image COPY test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	resolver, _, _ := localComponentResolverWithDefinition(t, ctx, platform, `package as="pkg"
extend
copy "/artifact" "/artifact" from="pkg"
copy "/bin/busybox" "/tool" from="fixture.local/coopr/busybox:latest"
`, base.imageStoreDir)
	layout := filepath.Join(root, "result")
	_, err := BuildPlan(ctx, testPlan(t, "from \"scratch\"\ncomponent \"local:tool\"\n"), PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}, Resolver: resolver,
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("component external-image COPY layers = %d, want compact component layer", len(manifest.Layers))
	}
	if copied := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()), "tool"); copied == "" {
		t.Fatal("component external-image COPY produced an empty tool")
	}
}
