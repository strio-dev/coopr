package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildPlanCopyFromLinkProducesIndependentLayer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah linked COPY build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "producer"), []byte("from-stage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "local"), []byte("from-context\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	plan := testPlan(t, `
from "scratch" as="producer"
copy "producer" "/producer"
from "scratch"
copy "local" "/local" link="false"
copy "/producer" "/linked" from="producer" link="true"
`)
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, plan, PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: layout, Reference: "coopr-copy-link"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 2 {
		t.Fatalf("linked COPY build layers = %d, want local layer plus independent linked layer", len(manifest.Layers))
	}
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()), "local"); got != "from-context\n" {
		t.Fatalf("ordinary COPY contents = %q", got)
	}
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded()), "linked"); got != "from-stage\n" {
		t.Fatalf("linked COPY --from contents = %q", got)
	}
}

func TestBuildPlanLinkedCopyIsVisibleToFollowingRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah linked COPY and RUN build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	writeLinkedRunContext(t, contextDir)
	plan := testPlan(t, `
from "scratch"
copy "busybox" "proof" "/input/" link="true"
run network="none" { exec "/input/busybox" "sh" "-c" "read value </input/proof; test \"$value\" = linked-run; echo \"$value\" >/observed" }
`)
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, plan, PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
		Output: Output{Path: layout, Reference: "coopr-copy-link-run"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 2 {
		t.Fatalf("linked COPY followed by RUN layers = %d, want linked layer plus RUN layer", len(manifest.Layers))
	}
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded()), "observed"); got != "linked-run\n" {
		t.Fatalf("RUN read linked COPY contents = %q", got)
	}
}

func TestDirectBuildLinkedCopyIsVisibleToFollowingRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live direct Buildah linked COPY and RUN build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	writeLinkedRunContext(t, contextDir)
	request, err := RequestFromPlan(testPlan(t, `
from "scratch"
copy "busybox" "proof" "/input/" link="true"
run network="none" { exec "/input/busybox" "sh" "-c" "read value </input/proof; test \"$value\" = linked-run; echo \"$value\" >/observed" }
`), PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
		Output: Output{Path: filepath.Join(root, "layout"), Reference: "coopr-direct-copy-link-run"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, request); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, request.Output.Path)
	if len(manifest.Layers) != 2 {
		t.Fatalf("direct linked COPY followed by RUN layers = %d, want linked layer plus RUN layer", len(manifest.Layers))
	}
	if got := readLayerFile(t, filepath.Join(request.Output.Path, "blobs", "sha256", manifest.Layers[1].Digest.Encoded()), "observed"); got != "linked-run\n" {
		t.Fatalf("direct RUN read linked COPY contents = %q", got)
	}
}

func TestDirectBuildFinalLinkedCopyPreservesEarlierCopyLayer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live direct Buildah final linked COPY build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "ordinary"), []byte("ordinary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "linked"), []byte("linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request, err := RequestFromPlan(testPlan(t, `
from "scratch"
copy "ordinary" "/ordinary"
copy "linked" "/linked" link="true"
`), PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: filepath.Join(root, "layout"), Reference: "coopr-direct-final-copy-link"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, request); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, request.Output.Path)
	if len(manifest.Layers) != 2 {
		t.Fatalf("ordinary COPY followed by final linked COPY layers = %d, want two", len(manifest.Layers))
	}
	if got := readLayerFile(t, filepath.Join(request.Output.Path, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()), "ordinary"); got != "ordinary\n" {
		t.Fatalf("ordinary COPY contents = %q", got)
	}
	if got := readLayerFile(t, filepath.Join(request.Output.Path, "blobs", "sha256", manifest.Layers[1].Digest.Encoded()), "linked"); got != "linked\n" {
		t.Fatalf("final linked COPY contents = %q", got)
	}
}

func TestDirectBuildConfigBeforeLinkedCopyDoesNotAddLayer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live direct Buildah linked COPY build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request, err := RequestFromPlan(testPlan(t, `
from "scratch"
env build_marker="preserved"
copy "payload" "/payload" link="true"
`), PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: filepath.Join(root, "layout"), Reference: "coopr-direct-config-copy-link"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, request); err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, request.Output.Path)
	if len(manifest.Layers) != 1 {
		t.Fatalf("configuration followed by linked COPY layers = %d, want one", len(manifest.Layers))
	}
	if got := image.Config.Env; len(got) != 1 || got[0] != "build_marker=preserved" {
		t.Fatalf("configuration before linked COPY was lost: %v", got)
	}
	if got := readLayerFile(t, filepath.Join(request.Output.Path, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()), "payload"); got != "linked\n" {
		t.Fatalf("linked COPY contents = %q", got)
	}
}

func TestBuildPlanLinkedAddExtractsArchiveIntoIndependentLayer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah linked ADD build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	contents := []byte("linked-add\n")
	if err := w.WriteHeader(&tar.Header{Name: "inside", Mode: 0o644, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload.tar"), archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, `
from "scratch"
add "payload.tar" "/unpacked/" link="true"
`)
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, plan, PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: layout, Reference: "coopr-add-link"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("linked ADD layers = %d, want one independent linked layer", len(manifest.Layers))
	}
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()), "unpacked/inside"); got != "linked-add\n" {
		t.Fatalf("linked ADD archive contents = %q", got)
	}
}

func writeLinkedRunContext(t *testing.T, contextDir string) {
	t.Helper()
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal("live Buildah tests require the Nix dev shell's static busybox binary")
	}
	binary, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "busybox"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("linked-run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
