package buildah

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestValidateStandaloneGraphPreservesSelectedNamedStageClosure(t *testing.T) {
	plan := testPlanWithTarget(t, `
from "scratch" as="Source"
env SOURCE="yes"
from "source" as="Final"
env FINAL="yes"
from "scratch" as="unused"
env UNUSED="yes"
`, "fInAl")
	stages, output, err := validateStandaloneGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	if output != "1" || len(stages) != 2 {
		t.Fatalf("selected graph output=%q stages=%+v", output, stages)
	}
	if stages[0].Name != "Source" || stages[1].Name != "Final" || !reflect.DeepEqual(stages[1].Dependencies, []string{"0"}) {
		t.Fatalf("selected graph stages=%+v", stages)
	}
}

func TestValidateStandaloneGraphRejectsStageAfterSelectedOutput(t *testing.T) {
	plan := testPlan(t, "from \"scratch\"\n")
	plan.Stages = append(plan.Stages, planner.Stage{ID: "later", Kind: "from", Source: "scratch", Platform: plan.Platform})
	if _, _, err := validateStandaloneGraph(plan); err == nil || !strings.Contains(err.Error(), "selected output must be the final stage") {
		t.Fatalf("post-output stage error = %v", err)
	}
}

func TestGraphLowersCacheAndTmpfsMountsWithoutContainerfile(t *testing.T) {
	plan := testPlan(t, "from \"scratch\"\nrun \"true\" { mount \"cache\" target=\"/cache\"; mount \"tmpfs\" target=\"/tmp\" }\n")
	stages, _, err := validateStandaloneGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	resolve, err := graphCacheMountIDResolver(plan, stages[0])
	if err != nil {
		t.Fatal(err)
	}
	operations, err := lowerGraphOperations(stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, resolve)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := operations[0].(Run)
	if !ok || len(run.Mounts) != 2 || run.Mounts[0].Type != "cache" || run.Mounts[0].Properties["id"] == "" || run.Mounts[1].Type != "tmpfs" {
		t.Fatalf("lowered RUN mounts: %#v", operations)
	}
}

func TestGraphLowersContextBindMountForFilteredExecution(t *testing.T) {
	plan := testPlan(t, "from \"scratch\"\nrun \"true\" { mount \"bind\" source=\"src\" target=\"/src\" }\n")
	stages, _, err := validateStandaloneGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	resolve, err := graphCacheMountIDResolver(plan, stages[0])
	if err != nil {
		t.Fatal(err)
	}
	operations, err := lowerGraphOperations(stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, resolve)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := operations[0].(Run)
	if !ok || len(run.Mounts) != 1 || run.Mounts[0].Type != "bind" {
		t.Fatalf("lowered context bind: %#v", operations)
	}
}

func TestValidateStandaloneGraphIncludesCopyFromDependency(t *testing.T) {
	plan := testPlan(t, "from \"scratch\" as=\"source\"\ncopy \"payload\" \"/payload\"\nfrom \"scratch\"\ncopy \"/payload\" \"/copied\" from=\"SOURCE\"\n")
	stages, _, err := validateStandaloneGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 2 || !reflect.DeepEqual(stages[1].Dependencies, []string{stages[0].ID}) {
		t.Fatalf("COPY --from stage dependency: %+v", stages)
	}
}

func TestValidateStandaloneGraphIncludesRunMountDependencies(t *testing.T) {
	plan := testPlan(t, `
from "scratch" as="source"
from "scratch"
run "true" {
  mount "bind" from="SOURCE" source="/input" target="/input"
  mount "cache" from="source" source="/cache" target="/cache"
}
`)
	stages, _, err := validateStandaloneGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 2 || !reflect.DeepEqual(stages[1].Dependencies, []string{stages[0].ID}) {
		t.Fatalf("RUN mount stage dependencies: %+v", stages)
	}
}

func TestGraphBindsRunMountsToStageImages(t *testing.T) {
	plan := testPlan(t, `
from "scratch" as="source"
from "scratch"
run "true" {
  mount "bind" from="SOURCE" source="/input" target="/input"
  mount "cache" from="source" source="/cache" target="/cache" id="stage-cache"
}
`)
	const imageID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	operations, err := lowerGraphOperations(plan.Stages[1].Operations,
		map[string]string{"source": plan.Stages[0].ID},
		map[string]stageState{plan.Stages[0].ID: {storageImageID: imageID}},
		nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := operations[0].(Run)
	if !ok || len(run.Mounts) != 2 {
		t.Fatalf("lowered RUN = %#v", operations)
	}
	mounts, err := serializeRunMounts(run.Mounts, "")
	if err != nil {
		t.Fatal(err)
	}
	for index, mount := range mounts {
		if !strings.Contains(mount, "from="+imageID) {
			t.Fatalf("bound mount %d = %q, want storage image ID", index, mount)
		}
	}
}

func TestBuildPlanRequiresResolverForExternalBaseWithCopyDependency(t *testing.T) {
	plan := testPlan(t, "from \"scratch\" as=\"source\"\nfrom \"alpine:3.20\"\ncopy \"/a\" \"/b\" from=\"source\"\n")
	_, err := BuildPlan(context.Background(), plan, PlanOptions{})
	if err == nil || !strings.Contains(err.Error(), "requires a Coopr image resolver") {
		t.Fatalf("external base with COPY --from dependency: %v", err)
	}
}

func TestBuildPlanRequiresCooprResolverForExternalBase(t *testing.T) {
	plan := testPlan(t, `from "alpine:3.20"`)
	if _, _, err := validateStandaloneGraph(plan); err != nil {
		t.Fatal(err)
	}
	_, err := BuildPlan(context.Background(), plan, PlanOptions{})
	if err == nil || !strings.Contains(err.Error(), "requires a Coopr image resolver") {
		t.Fatalf("external base without resolver: %v", err)
	}
	plan.Inputs = nil
	if _, _, err := validateStandaloneGraph(plan); err == nil || !strings.Contains(err.Error(), "lacks a declared image input") {
		t.Fatalf("external base without declared input: %v", err)
	}
}

func TestBuildPlanNamedStageTargetCreatesOneLayerPerCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah graph build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"base": "base\n", "second": "second\n", "third": "third\n"} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan := testPlanWithTarget(t, `
from "scratch" as="source"
copy "base" "/base"
from "SOURCE" as="final"
copy "second" "/second"
env COOPR_GRAPH="yes"
copy "third" "/third"
from "scratch" as="unused"
copy "base" "/unused"
`, "FINAL")
	layout := filepath.Join(root, "layout")
	result, err := BuildPlan(ctx, plan, PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: layout, Reference: "planned-graph"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ManifestDigest == "" {
		t.Fatal("graph build returned no manifest digest")
	}

	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) != 3 {
		t.Fatalf("OCI layer count = %d, want 3", len(manifest.Layers))
	}
	if !reflect.DeepEqual(image.Config.Env, []string{"COOPR_GRAPH=yes"}) {
		t.Fatalf("OCI image env = %#v", image.Config.Env)
	}
}

func TestBuildPlanCopiesFromIndependentNamedStage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah graph build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("copied from a stage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, "from \"scratch\" as=\"source\"\ncopy \"payload\" \"/payload\"\nfrom \"scratch\"\ncopy \"/payload\" \"/copied\" from=\"source\"\n")
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, plan, PlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: layout},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("final stage layers = %d, want 1", len(manifest.Layers))
	}
	layer, err := os.Open(filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := layer.Close(); err != nil {
			t.Errorf("close output layer: %v", err)
		}
	}()
	buffered := bufio.NewReader(layer)
	header, err := buffered.Peek(2)
	if err != nil {
		t.Fatal(err)
	}
	var contents io.Reader = buffered
	if header[0] == 0x1f && header[1] == 0x8b {
		decoded, err := gzip.NewReader(buffered)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := decoded.Close(); err != nil {
				t.Errorf("close decoded output layer: %v", err)
			}
		}()
		contents = decoded
	}
	archive := tar.NewReader(contents)
	for {
		entry, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(entry.Name, "./") != "copied" {
			continue
		}
		copied, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		if string(copied) != "copied from a stage\n" {
			t.Fatalf("copied content = %q", copied)
		}
		return
	}
	t.Fatal("COPY --from output missing from final layer")
}

func TestBuildPlanRunsWithStageBindAndCacheMounts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless stage mount build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := writeComponentTestPolicy(t, root)
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "caller"), []byte("caller-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, `
from "`+base.reference+`" as="source"
run "printf source-data >/proof && printf cache-data >/tmp/seed" network="none"
from "`+base.reference+`"
run "cat /input/seed >/bound" network="none" {
  mount "bind" from="SOURCE" source="/tmp" target="/input"
}
run "cat /single /local >/file-bound" network="none" {
  mount "bind" from="source" source="/proof" target="/single"
  mount "bind" source="caller" target="/local"
}
run "printf changed >/input/seed && cat /input/seed >/mutated" network="none" {
  mount "bind" from="source" source="/tmp" target="/input" readonly="false"
}
run "cat /cache/seed >/cached" network="none" {
  mount "cache" from="source" source="/tmp" target="/cache" id="stage-cache"
}
`)
	layout := filepath.Join(root, "layout")
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		Pull: false, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 5 {
		t.Fatalf("final image layers = %d, want base and four RUN layers", len(manifest.Layers))
	}
	for _, proof := range []struct {
		layer         int
		path, content string
	}{
		{1, "bound", "cache-data"}, {2, "file-bound", "source-datacaller-data"},
		{3, "mutated", "changed"}, {4, "cached", "cache-data"},
	} {
		blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[proof.layer].Digest.Encoded())
		if got := readLayerFile(t, blob, proof.path); got != proof.content {
			t.Fatalf("%s = %q, want %q", proof.path, got, proof.content)
		}
	}
	writable := testPlan(t, `
from "`+base.reference+`" as="source"
run "printf source-data >/proof" network="none"
from "`+base.reference+`"
run "printf changed >/input && cat /input >/written" network="none" {
  mount "bind" from="source" source="/proof" target="/input" readonly="false"
}
run "cat /input >/original" network="none" {
  mount "bind" from="source" source="/proof" target="/input"
}
`)
	writableLayout := filepath.Join(root, "writable-layout")
	_, err = BuildPlanSupervised(ctx, writable, SupervisedPlanOptions{
		Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: writableLayout},
		Pull: false, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("writable stage file bind: %v", err)
	}
	manifest, _ = readPlanImage(t, writableLayout)
	for _, layer := range manifest.Layers {
		blob := filepath.Join(writableLayout, "blobs", "sha256", layer.Digest.Encoded())
		if layerContainsPath(t, blob, "input") {
			t.Fatal("writable stage file bind leaked its mount target into the image")
		}
	}
	for _, proof := range []struct {
		layer         int
		path, content string
	}{
		{1, "written", "changed"}, {2, "original", "source-data"},
	} {
		blob := filepath.Join(writableLayout, "blobs", "sha256", manifest.Layers[proof.layer].Digest.Encoded())
		if got := readLayerFile(t, blob, proof.path); got != proof.content {
			t.Fatalf("writable stage file bind %s = %q, want %q", proof.path, got, proof.content)
		}
	}
}

func TestBuildPlanMetadataOnlyFinalStageKeepsBaseLayers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah graph build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, "from \"scratch\" as=\"source\"\ncopy \"proof\" \"/proof\"\nfrom \"source\"\nenv FLAG=\"yes\"\n")
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, plan, PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: layout},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 || len(image.RootFS.DiffIDs) != 1 {
		t.Fatalf("metadata-only final stage added a filesystem layer: layers=%d diffIDs=%d", len(manifest.Layers), len(image.RootFS.DiffIDs))
	}
	if !reflect.DeepEqual(image.Config.Env, []string{"FLAG=yes"}) {
		t.Fatalf("OCI image env = %#v", image.Config.Env)
	}
}

func TestBuildPlanUsesSelectedLocalImageAsBase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah graph build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	source, err := orasoci.NewWithContext(ctx, sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	selected, _ := sourceTestImage(t, ctx, source, platform, "selected")
	const reference = "registry.example/team/base:latest"
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	resolver, err := oci.NewResolver(oci.Options{})
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
	importTestImageToNative(t, ctx, store, sourceDir, reference, selected, system)
	plan := testPlan(t, "from \""+reference+"\"\nenv COOPR_GRAPH=\"yes\"\n")
	layout := filepath.Join(root, "layout")
	_, err = BuildPlan(ctx, plan, PlanOptions{
		Store:      store,
		ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}, Resolver: resolver,
		SystemContext: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) != 0 {
		t.Fatalf("metadata-only image gained %d filesystem layers", len(manifest.Layers))
	}
	if !reflect.DeepEqual(image.Config.Env, []string{"SOURCE=selected", "COOPR_GRAPH=yes"}) {
		t.Fatalf("OCI image env = %#v", image.Config.Env)
	}
}

func readPlanImage(t *testing.T, layout string) (v1.Manifest, v1.Image) {
	t.Helper()
	indexData, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("OCI layout index: %v, %+v", err, index.Manifests)
	}
	manifestData, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", index.Manifests[0].Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	configData, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", manifest.Config.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var image v1.Image
	if err := json.Unmarshal(configData, &image); err != nil {
		t.Fatal(err)
	}
	return manifest, image
}

func testPlanWithTarget(t *testing.T, source, target string) *planner.Plan {
	t.Helper()
	parsed, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Create(parsed, planner.Options{Mode: planner.Build, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
