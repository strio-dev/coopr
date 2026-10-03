package buildah

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/planner"
	"go.podman.io/image/v5/types"
)

func TestLowerGraphOperationsBindsNamedContextsBeforeStageAliases(t *testing.T) {
	platform := "linux/amd64"
	selected := map[ResolvedBaseKey]ResolvedImageSource{
		graphNamedContextKey("assets", platform): {ImageID: "context-image"},
	}
	aliases := map[string]string{"assets": "0"}
	images := map[string]stageState{"0": {storageImageID: "stage-image"}}
	copyOperation := planner.Operation{
		Instruction:  definition.Instruction{Name: "copy", Arguments: []string{"/payload", "/copied"}, Properties: map[string]string{"from": "assets"}},
		InputContext: "assets",
	}
	runOperation := planner.Operation{
		Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Children: []definition.Instruction{{
			Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"from": "assets", "target": "/input"},
		}}},
		MountContexts: map[int]string{0: "assets"},
	}
	lowered, err := lowerGraphOperationsWithSelectedImages([]planner.Operation{copyOperation, runOperation}, aliases, images, nil, []string{"/bin/sh", "-c"}, nil, selected)
	if err != nil {
		t.Fatal(err)
	}
	copyFrom, ok := lowered[0].(copyFromImageOperation)
	if !ok || copyFrom.imageID != "context-image" {
		t.Fatalf("named-context COPY lowering = %#v", lowered[0])
	}
	run, ok := lowered[1].(Run)
	if !ok || len(run.Mounts) != 1 || run.Mounts[0].Properties["from"] != "context-image" || !run.Mounts[0].BoundFrom {
		t.Fatalf("named-context RUN lowering = %#v", lowered[1])
	}
}

func TestMaterializePlanContextsReusesPlanningSelection(t *testing.T) {
	platform := "linux/" + runtime.GOARCH
	key := graphNamedContextKey("assets", platform)
	selected := ResolvedImageSource{ImageID: "already-selected"}
	executor := &graphExecutor{resolvedBases: map[ResolvedBaseKey]ResolvedImageSource{key: selected}}
	restore, err := executor.materializePlanContexts(context.Background(), &planner.Plan{
		Platform: platform,
		Contexts: []planner.ContextInput{{Spec: buildcontext.Spec{Name: "assets", Kind: buildcontext.Local, Path: "/does/not/need/to/be/read"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := executor.namedContext("assets", platform); !ok || got.ImageID != selected.ImageID {
		t.Fatalf("planning selection = %+v, %v", got, ok)
	}
	restore()
	if got, ok := executor.namedContext("assets", platform); !ok || got.ImageID != selected.ImageID {
		t.Fatalf("restored planning selection = %+v, %v", got, ok)
	}
}

func TestBuildPlanConsumesLocalNamedContextAsFromCopyAndRunMount(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless named-context graph build")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "assets")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), []byte("ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("named-context\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "ignored"), []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	contextSpec := buildcontext.Spec{Name: "assets", Kind: buildcontext.Local, Path: contextDir}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	policy := writeComponentTestPolicy(t, root)

	fromPlan := namedContextGraphPlan(t, `from "assets"`, contextSpec)
	fromLayout := filepath.Join(root, "from-layout")
	if _, err := BuildPlan(ctx, fromPlan, PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: fromLayout},
		SystemContext: namedContextSystemContext(root, policy),
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, fromLayout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("named-context FROM layers = %d, want 1", len(manifest.Layers))
	}
	if got := readLayerFile(t, filepath.Join(fromLayout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()), "payload"); got != "named-context\n" {
		t.Fatalf("named-context FROM payload = %q", got)
	}

	base := newLiveBusyBoxStorage(t, ctx, root, store)
	copyRunPlan := namedContextGraphPlan(t, `
from "`+base.reference+`"
copy "/payload" "/copied" from="assets"
run "cat /input/payload >/mounted" network="none" {
  mount "bind" from="assets" source="/" target="/input"
}
`, contextSpec)
	copyRunLayout := filepath.Join(root, "copy-run-layout")
	if _, err := BuildPlanSupervised(ctx, copyRunPlan, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: copyRunLayout},
		ImageStoreDir: base.imageStoreDir, BuildContexts: []buildcontext.Spec{contextSpec}, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ = readPlanImage(t, copyRunLayout)
	for _, proof := range []struct {
		layer int
		path  string
	}{{len(manifest.Layers) - 2, "copied"}, {len(manifest.Layers) - 1, "mounted"}} {
		if proof.layer < 0 {
			t.Fatalf("named-context build has too few layers: %d", len(manifest.Layers))
		}
		blob := filepath.Join(copyRunLayout, "blobs", "sha256", manifest.Layers[proof.layer].Digest.Encoded())
		if got := readLayerFile(t, blob, proof.path); got != "named-context\n" {
			t.Fatalf("named-context %s = %q", proof.path, got)
		}
	}

	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("changed-context\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	changedLayout := filepath.Join(root, "changed-layout")
	if _, err := BuildPlanSupervised(ctx, copyRunPlan, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: changedLayout},
		ImageStoreDir: base.imageStoreDir, BuildContexts: []buildcontext.Spec{contextSpec}, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ = readPlanImage(t, changedLayout)
	for _, proof := range []struct {
		layer int
		path  string
	}{{len(manifest.Layers) - 2, "copied"}, {len(manifest.Layers) - 1, "mounted"}} {
		blob := filepath.Join(changedLayout, "blobs", "sha256", manifest.Layers[proof.layer].Digest.Encoded())
		if got := readLayerFile(t, blob, proof.path); got != "changed-context\n" {
			t.Fatalf("changed named-context %s = %q", proof.path, got)
		}
	}
}

func namedContextGraphPlan(t *testing.T, source string, contextSpec buildcontext.Spec) *planner.Plan {
	t.Helper()
	parsed, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Create(parsed, planner.Options{
		Mode: planner.Build, Platform: "linux/" + runtime.GOARCH, BuildContexts: []buildcontext.Spec{contextSpec},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func namedContextSystemContext(root, policy string) *types.SystemContext {
	return &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
}
