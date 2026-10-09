package buildah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/types"
)

func TestValidatePublicationGraphAcceptsMultiplePackageOutputs(t *testing.T) {
	plan := testPublicationPlan(t, `
from "scratch" as="producer"
copy "payload" "/payload"
package as="tools"
copy "/payload" "/tool" from="producer"
from "tools" as="combined"
package as="bundle"
copy "/tool" "/bundle" from="combined"
extend
copy "/tool" "/one" from="tools"
copy "/bundle" "/two" from="bundle"
`)
	root := t.TempDir()
	stages, outputs, err := validatePublicationGraph(plan, map[string]string{
		"tools": filepath.Join(root, "tools.tar"), "bundle": filepath.Join(root, "bundle.tar"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 4 || len(outputs) != 2 {
		t.Fatalf("publication stages=%v outputs=%v", stages, outputs)
	}
	if !reflect.DeepEqual(plan.Outputs, []string{"1", "3"}) || outputs["1"].key != "tools" || outputs["3"].key != "bundle" {
		t.Fatalf("publication output mapping: plan=%v outputs=%v", plan.Outputs, outputs)
	}
}

func TestValidatePublicationGraphIncludesRunMountStageDependencies(t *testing.T) {
	plan := testPublicationPlan(t, `
from "scratch" as="source"
from "scratch" as="producer"
run "true" {
  mount "bind" from="SOURCE" source="/input" target="/input"
  mount "cache" from="source" source="/cache" target="/cache"
}
package as="payload"
copy "/proof" "/proof" from="producer"
extend
copy "/proof" "/proof" from="payload"
`)
	stages, _, err := validatePublicationGraph(plan, map[string]string{"payload": filepath.Join(t.TempDir(), "payload.tar")})
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 3 || !reflect.DeepEqual(stages[1].Dependencies, []string{stages[0].ID}) {
		t.Fatalf("publication RUN mount dependencies: %+v", stages)
	}
}

func TestValidatePublicationGraphAcceptsExternalRunMountImageInputs(t *testing.T) {
	const source = "registry.example/tools:latest"
	plan := testPublicationPlan(t, `package as="payload"
run "true" {
  mount "bind" from="registry.example/tools:latest" source="/bin" target="/tools" readonly="true"
  mount "cache" from="registry.example/tools:latest" source="/var/cache" target="/cache" id="tools"
}
extend
copy "/proof" "/proof" from="payload"
`)
	stages, _, err := validatePublicationGraph(plan, map[string]string{"payload": filepath.Join(t.TempDir(), "payload.tar")})
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || len(stages[0].Dependencies) != 0 {
		t.Fatalf("external RUN mount became a stage dependency: %+v", stages)
	}
	if got, want := plan.Inputs, []planner.Input{{Kind: "image", Reference: source}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("publication inputs = %+v, want %+v", got, want)
	}
}

func TestPublicationGraphClassifiesNamedContextsWithoutImageResolution(t *testing.T) {
	contextSpec := buildcontext.Spec{Name: "assets", Kind: buildcontext.Local, Path: t.TempDir()}
	plan := testPublicationPlanWithContexts(t, `package as="payload"
copy "/copy" "/copy" from="assets"
run "true" network="none" {
  mount "bind" from="assets" source="/mount" target="/mount"
}
extend
copy "/copy" "/copy" from="payload"
`, contextSpec)
	stages, _, err := validatePublicationGraph(plan, map[string]string{"payload": filepath.Join(t.TempDir(), "payload.tar")})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectPublicationBases(context.Background(), PlanOptions{}, stages)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 0 {
		t.Fatalf("named contexts were selected as external images: %+v", selected)
	}
}

func TestValidatePublicationGraphRejectsMissingAndUnknownPackagePaths(t *testing.T) {
	plan := testPublicationPlan(t, `
package as="payload"
copy "payload" "/payload"
extend
copy "/payload" "/payload" from="payload"
`)
	_, _, err := validatePublicationGraph(plan, nil)
	if err == nil || !strings.Contains(err.Error(), "output path is required") {
		t.Fatalf("missing package path: %v", err)
	}
	_, _, err = validatePublicationGraph(plan, map[string]string{
		"payload": filepath.Join(t.TempDir(), "payload.tar"), "typo": filepath.Join(t.TempDir(), "typo.tar"),
	})
	if err == nil || !strings.Contains(err.Error(), "does not identify a required package") {
		t.Fatalf("unknown package path: %v", err)
	}
}

func TestPublishPlanRejectsAnyPackageOutputInsideCacheLayout(t *testing.T) {
	plan := testPublicationPlan(t, `
package as="first"
copy "first" "/first"
package as="second"
copy "second" "/second"
extend
copy "/first" "/first" from="first"
copy "/second" "/second" from="second"
`)
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	runRoot := filepath.Join(root, "run")
	graphRoot := filepath.Join(root, "graph")
	_, err := PublishPlan(context.Background(), plan, PublicationOptions{
		PlanOptions: PlanOptions{
			Store:      StoreOptions{RunRoot: runRoot, GraphRoot: graphRoot, GraphDriverName: "vfs"},
			ContextDir: root, Isolation: "rootless", CacheLocalDir: cacheDir,
		},
		PackagePaths: map[string]string{
			"first":  filepath.Join(root, "first.tar"),
			"second": filepath.Join(cacheDir, "index.json"),
		},
	})
	if err == nil || !strings.Contains(err.Error(), `package "second" cache location: component cache layout overlaps output`) {
		t.Fatalf("cache-overlapping package output = %v", err)
	}
	for _, path := range []string{runRoot, graphRoot, cacheDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid package output created %s: %v", path, err)
		}
	}
}

func TestPublishPlanSnapshotsMultiplePackages(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah component publication")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("shared package payload\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	plan := testPublicationPlan(t, `
from "scratch" as="producer"
copy "payload" "/payload"
package as="tools"
copy "/payload" "/tool" from="producer"
from "tools" as="combined"
package as="bundle"
copy "/tool" "/bundle" from="combined"
extend
copy "/tool" "/one" from="tools"
copy "/bundle" "/two" from="bundle"
`)
	toolsPath := filepath.Join(root, "packages", "tools.tar")
	bundlePath := filepath.Join(root, "packages", "bundle.tar")
	packages, err := PublishPlan(ctx, plan, PublicationOptions{
		PlanOptions: PlanOptions{
			Store: StoreOptions{
				RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
			},
			ContextDir: contextDir, Isolation: "rootless",
		},
		PackagePaths: map[string]string{"tools": toolsPath, "bundle": bundlePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 2 || packages["tools"].Stage != "tools" || packages["bundle"].Stage != "bundle" {
		t.Fatalf("published packages = %+v", packages)
	}
	assertPackageTarFile(t, toolsPath, "tool", "shared package payload\n")
	assertPackageTarFile(t, bundlePath, "bundle", "shared package payload\n")
}

func TestPublishPlanBuildsUnusedPackagesWithoutInvocationStages(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah component publication")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("unused package payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader(`package as="unused"
copy "payload" "/payload"
extend as="target"
run "exit 79"
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.CreateDemandDriven(def, planner.Options{
		Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH, Target: "target", BuildUnusedStages: true,
	}, func(source planner.FromSource) (planner.StageBind, error) {
		return planner.StageBind{}, fmt.Errorf("unexpected external source %+v", source)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Stages) != 1 || plan.Stages[0].ID != "0" || plan.Stages[0].Kind != "package" || len(plan.Outputs) != 0 {
		t.Fatalf("unused publication plan stages=%v outputs=%v", plan.Stages, plan.Outputs)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	packages, err := PublishPlan(ctx, plan, PublicationOptions{PlanOptions: PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", CacheLocalDir: filepath.Join(root, "cache"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 0 {
		t.Fatalf("unused producer became a published package: %+v", packages)
	}
	if count := instructionCacheRecordCount(t, store); count != 1 {
		t.Fatalf("unused package producer did not execute exactly once: cache records=%d", count)
	}
}

func TestPublishPlanOverlapsIndependentPackageProducers(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live parallel package coverage")
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(contextDir, "rendezvous"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "busybox"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := testPublicationPlan(t, `
package as="left"
copy "busybox" "/busybox"
run network="none" {
  exec "/busybox" "sh" "-ec" "/busybox timeout -s KILL 120 /busybox sh -ec \"$1\"; :" "rendezvous" "/busybox touch /rendezvous/left; until [ -f /rendezvous/right ]; do /busybox sleep 0.01; done; printf 'left\n' >/left"
  mount "bind" source="rendezvous" target="/rendezvous" rw="true"
}
package as="right"
copy "busybox" "/busybox"
run network="none" {
  exec "/busybox" "sh" "-ec" "/busybox timeout -s KILL 120 /busybox sh -ec \"$1\"; :" "rendezvous" "/busybox touch /rendezvous/right; until [ -f /rendezvous/left ]; do /busybox sleep 0.01; done; printf 'right\n' >/right"
  mount "bind" source="rendezvous" target="/rendezvous" rw="true"
}
extend
copy "/left" "/left" from="left"
copy "/right" "/right" from="right"
`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Each producer waits for the other to enter its RUN before finishing.
	// Serial scheduling cannot publish either output; timeout bounds each RUN
	// because context cancellation does not interrupt an in-flight Buildah RUN.
	// Keep the outer shell as namespace init so timeout can kill its child.
	leftPath, rightPath := filepath.Join(root, "left.tar"), filepath.Join(root, "right.tar")
	packages, err := PublishPlan(ctx, plan, PublicationOptions{
		PlanOptions: PlanOptions{
			Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
			ContextDir: contextDir, Isolation: "rootless", Jobs: 2,
		},
		PackagePaths: map[string]string{"left": leftPath, "right": rightPath},
	})
	if err != nil {
		t.Fatalf("parallel package publication: %v", err)
	}
	if len(packages) != 2 || packages["left"].Stage != "left" || packages["right"].Stage != "right" {
		t.Fatalf("published packages = %+v", packages)
	}
	assertPackageTarFile(t, leftPath, "left", "left\n")
	assertPackageTarFile(t, rightPath, "right", "right\n")
}

func TestPackageCacheEligibilityRejectsUndeclaredInputs(t *testing.T) {
	base := planner.Stage{Operations: []planner.Operation{{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"network": "none"}}}}}
	if _, eligible := packageCacheEligibility([]planner.Stage{base}, RunControls{}); !eligible {
		t.Fatal("network-isolated RUN was not package-cache eligible")
	}
	controls := RunControls{Devices: []string{"/dev/test-device"}}
	if _, eligible := packageCacheEligibility([]planner.Stage{base}, controls); eligible {
		t.Fatal("RUN with a global device was portable package-cache eligible")
	}
	eligible := []planner.Operation{
		{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"network": "default"}}},
		{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"network": "none"}, Children: []definition.Instruction{{Name: "mount", Arguments: []string{"secret"}, Properties: map[string]string{"id": "token"}}}}},
		{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"network": "none"}, Children: []definition.Instruction{{Name: "mount", Arguments: []string{"ssh"}}}}},
		{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"network": "none"}, Children: []definition.Instruction{{Name: "mount", Arguments: []string{"cache"}, Properties: map[string]string{"target": "/cache"}}}}},
		{Instruction: definition.Instruction{Name: "add", Arguments: []string{"https://example.invalid/payload", "/payload"}, Properties: map[string]string{"checksum": digest.FromString("payload").String()}}},
	}
	for index, operation := range eligible {
		if _, ok := packageCacheEligibility([]planner.Stage{{Operations: []planner.Operation{operation}}}, RunControls{}); !ok {
			t.Errorf("declared-input operation %d was not package-cache eligible", index)
		}
		if _, got := packageCacheEligibility([]planner.Stage{{Operations: []planner.Operation{operation}}}, controls); got != (operation.Name != "run") {
			t.Errorf("global-device operation %d eligibility = %v", index, got)
		}
	}
	tests := []planner.Operation{
		{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"network": "host"}}},
		{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"network": "none", "security": "insecure"}}},
		{Instruction: definition.Instruction{Name: "add", Arguments: []string{"https://example.invalid/payload", "/payload"}}},
	}
	for index, operation := range tests {
		if _, eligible := packageCacheEligibility([]planner.Stage{{Operations: []planner.Operation{operation}}}, RunControls{}); eligible {
			t.Errorf("undeclared-input operation %d was package-cache eligible", index)
		}
	}
}

func TestPublishPlanReusesPortablePackageAcrossBuildStores(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah package cache")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("portable package\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	plan := testPublicationPlan(t, `
package as="payload"
copy "payload" "/payload"
extend
copy "/payload" "/payload" from="payload"
`)
	cacheDir := filepath.Join(root, "cache")
	build := func(name, expected string) (oci.Package, StoreOptions) {
		store := StoreOptions{RunRoot: filepath.Join(root, name, "run"), GraphRoot: filepath.Join(root, name, "graph"), GraphDriverName: "vfs"}
		path := filepath.Join(root, name, "payload.tar")
		packages, err := PublishPlan(ctx, plan, PublicationOptions{
			PlanOptions:  PlanOptions{Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun", CacheLocalDir: cacheDir},
			PackagePaths: map[string]string{"payload": path},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertPackageTarFile(t, path, "payload", expected)
		return packages["payload"], store
	}
	first, _ := build("first", "portable package\n")
	second, secondStore := build("second", "portable package\n")
	if !reflect.DeepEqual(first.Descriptor, second.Descriptor) || !reflect.DeepEqual(first.Config, second.Config) {
		t.Fatalf("package cache changed package: first=%+v second=%+v", first, second)
	}
	lease, err := acquireStore(secondStore)
	if err != nil {
		t.Fatal(err)
	}
	images, imageErr := lease.store.Images()
	closeErr := lease.Close()
	if imageErr != nil || closeErr != nil {
		t.Fatalf("inspect second build store: %v", errors.Join(imageErr, closeErr))
	}
	if len(images) != 0 {
		t.Fatalf("warm package cache executed production into second store: %d images", len(images))
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("changed package\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	third, _ := build("third", "changed package\n")
	if third.Descriptor.Digest == first.Descriptor.Digest {
		t.Fatal("package cache reused a snapshot after the context input changed")
	}
}

func TestPackageCacheInvalidatesChangedFixedArgument(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah package cache")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	definition, err := definition.Parse(strings.NewReader(`
arg "flavor" "first"
package as="payload"
copy "flavor-$flavor" "/payload"
extend
copy "/payload" "/payload" from="payload"
`))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	build := func(flavor string) oci.Package {
		if err := os.WriteFile(filepath.Join(root, "flavor-"+flavor), []byte(flavor), 0o600); err != nil {
			t.Fatal(err)
		}
		plan, err := planner.Create(definition, planner.Options{
			Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH,
			Arguments: map[string]string{"flavor": flavor},
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, flavor, "payload.tar")
		packages, err := PublishPlan(ctx, plan, PublicationOptions{
			PlanOptions: PlanOptions{
				Store: StoreOptions{
					RunRoot: filepath.Join(root, flavor, "run"), GraphRoot: filepath.Join(root, flavor, "graph"), GraphDriverName: "vfs",
				},
				ContextDir: root, Isolation: "rootless", Runtime: "crun", CacheLocalDir: cacheDir,
			},
			PackagePaths: map[string]string{"payload": path},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertPackageTarFile(t, path, "payload", flavor)
		return packages["payload"]
	}
	first := build("first")
	second := build("second")
	if first.Descriptor.Digest == second.Descriptor.Digest {
		t.Fatal("package cache reused a snapshot after its fixed argument changed")
	}
}

func TestPublishPlanRunsProducerWithStageMount(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah component publication")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	resolver, err := oci.NewResolver(oci.Options{})
	if err != nil {
		t.Fatal(err)
	}
	policy := writeComponentTestPolicy(t, root)
	plan := testPublicationPlan(t, `
from "`+base.reference+`" as="source"
run "printf packaged-data >/tmp/seed" network="none"
from "`+base.reference+`" as="producer"
run "cat /input/seed >/proof" network="none" {
  mount "bind" from="source" source="/tmp" target="/input"
}
package as="payload"
copy "/proof" "/proof" from="producer"
extend
copy "/proof" "/proof" from="payload"
`)
	packagePath := filepath.Join(root, "payload.tar")
	packages, err := PublishPlan(ctx, plan, PublicationOptions{
		PlanOptions: PlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Resolver: resolver,
			SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
		},
		PackagePaths: map[string]string{"payload": packagePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 1 {
		t.Fatalf("published packages = %+v", packages)
	}
	assertPackageTarFile(t, packagePath, "proof", "packaged-data")
}

func TestPublishPlanConsumesLocalNamedContext(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless named-context publication")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "assets")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("named package\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	contextSpec := buildcontext.Spec{Name: "assets", Kind: buildcontext.Local, Path: contextDir}
	plan := testPublicationPlanWithContexts(t, `package as="payload"
copy "/payload" "/proof" from="assets"
extend
copy "/proof" "/proof" from="payload"
`, contextSpec)
	packagePath := filepath.Join(root, "payload.tar")
	packages, err := PublishPlan(ctx, plan, PublicationOptions{
		PlanOptions: PlanOptions{
			Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
			ContextDir: root, Isolation: "rootless",
			SystemContext: &types.SystemContext{SignaturePolicyPath: writeComponentTestPolicy(t, root), BigFilesTemporaryDir: root},
		},
		PackagePaths: map[string]string{"payload": packagePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 1 {
		t.Fatalf("published packages = %+v", packages)
	}
	assertPackageTarFile(t, packagePath, "proof", "named package\n")
}

func TestPublishDefinitionSupervisedConsumesPlanningSelectedNamedContext(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless named-context publication worker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "assets")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("worker named package\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	contextSpec := buildcontext.Spec{Name: "assets", Kind: buildcontext.Local, Path: contextDir}
	def := parseWorkerDefinition(t, `package as="payload"
copy "/payload" "/proof" from="assets"
extend
copy "/proof" "/proof" from="payload"
`)
	layout := filepath.Join(root, "component")
	result, err := PublishDefinitionSupervised(ctx, def, planner.Options{
		Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH, BuildContexts: []buildcontext.Spec{contextSpec},
	}, SupervisedPlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		BuildContexts: []buildcontext.Spec{contextSpec}, CacheLocalDir: filepath.Join(root, "cache"),
		SignaturePolicyPath: writeComponentTestPolicy(t, root), Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := readComponentMetadata(t, ctx, layout, result.Root)
	if len(metadata.Packages) != 1 || metadata.Packages[0].Stage != "payload" || metadata.Packages[0].Descriptor.Digest == "" {
		t.Fatalf("published named-context packages = %+v", metadata.Packages)
	}
}

func testPublicationPlan(t *testing.T, source string) *planner.Plan {
	return testPublicationPlanWithContexts(t, source)
}

func testPublicationPlanWithContexts(t *testing.T, source string, contexts ...buildcontext.Spec) *planner.Plan {
	t.Helper()
	parsed, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	options := planner.Options{Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH, BuildContexts: contexts}
	var plan *planner.Plan
	if len(contexts) == 0 {
		plan, err = planner.Create(parsed, options)
	} else {
		plan, err = planner.CreateDemandDriven(parsed, options, func(planner.FromSource) (planner.StageBind, error) {
			return planner.StageBind{}, nil
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
