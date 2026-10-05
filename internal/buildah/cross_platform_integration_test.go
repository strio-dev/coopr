package buildah

import (
	"context"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/componentstore"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestBuildPlanWritesForeignPlatformToOCIOutput(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	platform := foreignLinuxPlatform(t)
	layout := filepath.Join(root, "layout")

	buildScratchCopyForPlatform(t, ctx, cacheTestStore(root), root, layout, platform)

	_, image := readPlanImage(t, layout)
	if image.OS != platform.OS || image.Architecture != platform.Architecture {
		t.Fatalf("OCI config platform = %s/%s, want %s/%s", image.OS, image.Architecture, platform.OS, platform.Architecture)
	}
}

func TestBuildPlanSeparatesInstructionCacheByTargetPlatform(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	foreign := foreignLinuxPlatform(t)
	native := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}

	buildScratchCopyForPlatform(t, ctx, store, root, filepath.Join(root, "foreign"), foreign)
	if records := instructionCacheRecordCount(t, store); records != 1 {
		t.Fatalf("foreign-platform cache records = %d, want 1", records)
	}
	buildScratchCopyForPlatform(t, ctx, store, root, filepath.Join(root, "native"), native)
	if records := instructionCacheRecordCount(t, store); records != 2 {
		t.Fatalf("cache records after native and foreign builds = %d, want 2", records)
	}
}

func TestBuildPlanUsesEachStagesSelectedPlatform(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("mixed-platform\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := foreignLinuxPlatform(t)
	native := "linux/" + runtime.GOARCH
	plan := planForPlatform(t, fmt.Sprintf(`
from "scratch" as="producer" platform=%q
copy "proof" "/proof"
from "scratch" platform=%q
copy "/proof" "/proof" from="producer"
`, native, foreign.OS+"/"+foreign.Architecture), foreign)
	layout := filepath.Join(root, "layout")
	if _, err := BuildPlan(ctx, plan, PlanOptions{
		Store: cacheTestStore(root), ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: layout, Reference: "mixed-platform"},
	}); err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	if image.OS != foreign.OS || image.Architecture != foreign.Architecture {
		t.Fatalf("mixed-platform output config = %s/%s, want %s/%s", image.OS, image.Architecture, foreign.OS, foreign.Architecture)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("mixed-platform output layers = %d, want 1", len(manifest.Layers))
	}
	layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, layer, "proof"); got != "mixed-platform\n" {
		t.Fatalf("mixed-platform stage copy = %q", got)
	}
}

func TestBuildPlanExecutesForeignBinaryThroughRegisteredBinfmt(t *testing.T) {
	requireLiveInstructionCache(t)
	platform := foreignLinuxPlatform(t)
	requireBinfmtFixBinary(t, platform.Architecture)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := buildForeignProofBinary(t, contextDir, platform.Architecture)
	assertELFArchitecture(t, binary, platform.Architecture)
	plan := planForPlatform(t, `
from "scratch"
copy "foreign-proof" "/foreign-proof" chmod="0755"
run network="none" { exec "/foreign-proof" }
`, platform)
	layout := filepath.Join(root, "layout")
	if _, err := BuildPlan(ctx, plan, PlanOptions{
		Store: cacheTestStore(root), ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
		Output: Output{Path: layout, Reference: "foreign-run"},
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 2 {
		t.Fatalf("foreign RUN image layers = %d, want COPY and RUN layers", len(manifest.Layers))
	}
	lastLayer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
	if got := readLayerFile(t, lastLayer, "executed"); got != platform.Architecture+"\n" {
		t.Fatalf("foreign RUN proof = %q, want %q", got, platform.Architecture+"\n")
	}
}

func TestForeignComponentPublishesInvokesAndReusesCache(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("foreign component\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	platform := foreignLinuxPlatform(t)
	planning := planner.Options{Mode: planner.Publish, Platform: platform.OS + "/" + platform.Architecture}
	component := parseWorkerDefinition(t, `
package as="pkg"
copy "payload" "/artifact"
extend
copy "/artifact" "/installed" from="pkg"
`)
	store := cacheTestStore(root)
	componentLayout := filepath.Join(root, "component")
	publication, err := PublishDefinitionSupervised(ctx, component, planning, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: componentLayout},
		CacheLocalDir: filepath.Join(root, "cache"), Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := readComponentMetadata(t, ctx, componentLayout, publication.Root)
	if metadata.Platform.OS != platform.OS || metadata.Platform.Architecture != platform.Architecture {
		t.Fatalf("component metadata platform = %s/%s, want %s/%s", metadata.Platform.OS, metadata.Platform.Architecture, platform.OS, platform.Architecture)
	}
	if len(metadata.Packages) != 1 {
		t.Fatalf("component packages = %d, want 1", len(metadata.Packages))
	}
	var packageImage v1.Image
	if err := json.Unmarshal(metadata.Packages[0].Config, &packageImage); err != nil {
		t.Fatal(err)
	}
	if packageImage.OS != platform.OS || packageImage.Architecture != platform.Architecture {
		t.Fatalf("component package platform = %s/%s, want %s/%s", packageImage.OS, packageImage.Architecture, platform.OS, platform.Architecture)
	}
	source, err := orasoci.NewWithContext(ctx, componentLayout)
	if err != nil {
		t.Fatal(err)
	}
	componentDir := filepath.Join(root, "components")
	if err := componentstore.Put(ctx, componentDir, source, publication.Root, "foreign-copy"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "payload")); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: componentDir})
	if err != nil {
		t.Fatal(err)
	}
	plan := planForPlatform(t, "from \"scratch\"\ncomponent \"local:foreign-copy\"\n", platform)
	var cold Result
	for attempt := range 2 {
		result, err := BuildPlan(ctx, plan, PlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Resolver: resolver,
			CacheLocalDir: filepath.Join(root, "cache"),
			Output:        Output{Path: filepath.Join(root, fmt.Sprintf("invoked-%d", attempt))},
		})
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			cold = result
			continue
		}
		if cold.CacheStats.Misses < 1 || cold.CacheStats.Stored < 1 || result.CacheStats.Hits < 1 {
			t.Fatalf("foreign component cache stats: cold=%+v warm=%+v", cold.CacheStats, result.CacheStats)
		}
		manifest, image := readPlanImage(t, result.Layout)
		if image.OS != platform.OS || image.Architecture != platform.Architecture {
			t.Fatalf("invoked component platform = %s/%s, want %s/%s", image.OS, image.Architecture, platform.OS, platform.Architecture)
		}
		if len(manifest.Layers) != 1 {
			t.Fatalf("invoked component layers = %d, want 1", len(manifest.Layers))
		}
		layer := filepath.Join(result.Layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
		if got := readLayerFile(t, layer, "installed"); got != "foreign component\n" {
			t.Fatalf("invoked foreign component payload = %q", got)
		}
	}
}

func TestBuildDefinitionUsesSelectedForeignExternalBase(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "base-proof"), []byte("foreign base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	platform := foreignLinuxPlatform(t)
	platformName := platform.OS + "/" + platform.Architecture
	store := cacheTestStore(root)
	baseLayout := filepath.Join(root, "base")
	basePlan := planForPlatform(t, "from \"scratch\"\ncopy \"base-proof\" \"/base-proof\"\nenv BASE=\"foreign\"\n", platform)
	base, err := BuildPlan(ctx, basePlan, PlanOptions{
		Store: store, ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: baseLayout},
	})
	if err != nil {
		t.Fatal(err)
	}
	baseRoot, err := oci.LayoutRoot(baseLayout)
	if err != nil {
		t.Fatal(err)
	}
	baseConfig, err := oci.ReadImageConfigLayout(ctx, baseLayout)
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
	imageStoreDir := filepath.Join(root, "images")
	const reference = "fixture.local/coopr/foreign-base:latest"
	importTestImageToCatalog(t, ctx, store, imageStoreDir, baseLayout, reference, baseRoot, baseRoot, baseConfig, platform, system)

	output := filepath.Join(root, "derived")
	result, err := BuildDefinitionSupervised(ctx, parseWorkerDefinition(t, "from \""+reference+"\"\nenv DERIVED=\"yes\"\n"), planner.Options{
		Mode: planner.Build, Platform: platformName,
	}, SupervisedPlanOptions{
		Store: store, ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: output},
		ImageStoreDir: imageStoreDir, Pull: false, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, result.Layout)
	if image.OS != platform.OS || image.Architecture != platform.Architecture {
		t.Fatalf("foreign-base output platform = %s/%s, want %s/%s", image.OS, image.Architecture, platform.OS, platform.Architecture)
	}
	if got := strings.Join(image.Config.Env, " "); !strings.Contains(got, "BASE=foreign") || !strings.Contains(got, "DERIVED=yes") {
		t.Fatalf("foreign-base output env = %#v", image.Config.Env)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("foreign-base output layers = %d, want inherited base layer", len(manifest.Layers))
	}
	layer := filepath.Join(result.Layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, layer, "base-proof"); got != "foreign base\n" {
		t.Fatalf("foreign-base inherited file = %q", got)
	}
	if base.ImageID == "" {
		t.Fatal("foreign base build returned no storage image")
	}
}

func buildScratchCopyForPlatform(t *testing.T, ctx context.Context, store StoreOptions, root, layout string, platform v1.Platform) {
	t.Helper()
	contextDir := filepath.Join(root, "context")
	if err := os.MkdirAll(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("cross-platform\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := planForPlatform(t, "from \"scratch\"\ncopy \"proof\" \"/proof\"\n", platform)
	if _, err := BuildPlan(ctx, plan, PlanOptions{
		Store: store, ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: layout, Reference: platform.Architecture},
	}); err != nil {
		t.Fatal(err)
	}
}

func planForPlatform(t *testing.T, source string, platform v1.Platform) *planner.Plan {
	t.Helper()
	def, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Create(def, planner.Options{
		Mode: planner.Build, Platform: platform.OS + "/" + platform.Architecture,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func foreignLinuxPlatform(t *testing.T) v1.Platform {
	t.Helper()
	switch runtime.GOARCH {
	case "amd64":
		return v1.Platform{OS: "linux", Architecture: "arm64"}
	case "arm64":
		return v1.Platform{OS: "linux", Architecture: "amd64"}
	default:
		t.Skipf("cross-platform integration fixture supports amd64 and arm64 hosts, not %s", runtime.GOARCH)
		return v1.Platform{}
	}
}

func requireBinfmtFixBinary(t *testing.T, architecture string) {
	t.Helper()
	name := map[string]string{"amd64": "qemu-x86_64", "arm64": "qemu-aarch64"}[architecture]
	override, supplied := os.LookupEnv("COOPR_TEST_BINFMT_HANDLER")
	if supplied {
		if override == "" || override == "." || override == ".." || strings.Contains(override, "/") {
			t.Fatalf("COOPR_TEST_BINFMT_HANDLER must name a binfmt registration: %q", override)
		}
		name = override
	}
	if name == "" {
		t.Skipf("no binfmt fixture name for %s", architecture)
	}
	data, err := os.ReadFile(filepath.Join("/proc/sys/fs/binfmt_misc", name))
	if err != nil {
		if supplied {
			t.Fatalf("foreign RUN requires registered %s binfmt handler: %v", name, err)
		}
		t.Skipf("foreign RUN requires registered %s binfmt handler: %v", architecture, err)
	}
	registration := string(data)
	if !binfmtFixBinaryEnabled(registration) {
		if supplied {
			t.Fatalf("foreign RUN requires an enabled %s binfmt handler with fix-binary flag; registration: %s", name, strings.TrimSpace(registration))
		}
		t.Skipf("foreign RUN requires an enabled %s binfmt handler with fix-binary flag; registration: %s", architecture, strings.TrimSpace(registration))
	}
}

func binfmtFixBinaryEnabled(registration string) bool {
	lines := strings.Split(registration, "\n")
	if lines[0] != "enabled" {
		return false
	}
	for _, line := range lines[1:] {
		if flags, found := strings.CutPrefix(line, "flags:"); found {
			return strings.Contains(strings.TrimSpace(flags), "F")
		}
	}
	return false
}

func buildForeignProofBinary(t *testing.T, contextDir, architecture string) string {
	t.Helper()
	source := filepath.Join(contextDir, "main.go")
	program := fmt.Sprintf("package main\nimport \"os\"\nfunc main() { if err := os.WriteFile(\"/executed\", []byte(%q), 0644); err != nil { panic(err) } }\n", architecture+"\n")
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(contextDir, "foreign-proof")
	command := exec.Command("go", "build", "-trimpath", "-o", output, source)
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+architecture)
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cross-compile foreign RUN fixture: %v\n%s", err, data)
	}
	return output
}

func assertELFArchitecture(t *testing.T, path, architecture string) {
	t.Helper()
	binary, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = binary.Close() }()
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[architecture]
	if binary.Machine != want {
		t.Fatalf("foreign fixture ELF machine = %s, want %s", binary.Machine, want)
	}
}
