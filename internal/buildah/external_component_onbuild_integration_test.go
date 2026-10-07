package buildah

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"coopr/internal/componentstore"
	"coopr/internal/oci"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestComponentInvocationPlansExternalOnBuildAgainstPublishedPackageOffline(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless external component ONBUILD invocation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture"), []byte("published package payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{
		RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
	}
	componentDir := filepath.Join(root, "components")

	// The image is invocation-only. Publishing must therefore succeed before
	// the reference exists in either a registry or the native image store.
	component := parseWorkerDefinition(t, `
arg "runtime_base" "registry.invalid/coopr/missing:latest"
package as="assets"
copy "fixture" "/artifact"
from "${runtime_base}" as="prepared"
extend
copy "/inherited" "/selected" from="prepared"
`)
	componentLayout := filepath.Join(root, "component")
	publication, err := PublishDefinitionSupervised(ctx, component, planner.Options{
		Mode: planner.Publish, Platform: "linux/" + runtime.GOARCH,
	}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: componentLayout},
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := readComponentMetadata(t, ctx, componentLayout, publication.Root)
	if len(metadata.Packages) != 1 || metadata.Packages[0].Stage != "assets" {
		t.Fatalf("published packages = %+v, want assets", metadata.Packages)
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	if selection, found, err := nativeFixtureSelection(ctx, store, "registry.invalid/coopr/missing:latest", platform); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatalf("publication selected invocation-only image: %+v", selection)
	}
	componentSource, err := orasoci.NewWithContext(ctx, componentLayout)
	if err != nil {
		t.Fatal(err)
	}
	if err := componentstore.Put(ctx, componentDir, componentSource, publication.Root, "external-onbuild"); err != nil {
		t.Fatal(err)
	}

	// Seed exactly one immutable image selection for the invocation. Its
	// inherited COPY reaches the named package stage only after base selection.
	baseLayout := filepath.Join(root, "base")
	base, err := BuildPlan(ctx, testPlan(t, `
from "scratch"
onbuild { arg "source" "assets" }
onbuild { copy "/artifact" "/inherited" from="$source" }
`), PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: baseLayout, Format: outputFormatDocker},
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	baseConfig, err := oci.ReadImageConfigLayout(ctx, baseLayout)
	if err != nil {
		t.Fatal(err)
	}
	baseManifest, err := oci.LayoutRoot(baseLayout)
	if err != nil {
		t.Fatal(err)
	}
	const baseReference = "registry.example/coopr/invocation-base:latest"
	if err := nameNativeFixture(ctx, store, baseReference, oci.StoredSelection{
		Root: baseManifest, Manifest: baseManifest, ImageID: base.ImageID, ConfigData: baseConfig,
	}); err != nil {
		t.Fatal(err)
	}
	selection, found, err := nativeFixtureSelection(ctx, store, baseReference, platform)
	if err != nil {
		t.Fatal(err)
	}
	if !found || selection.ImageID != base.ImageID || selection.Manifest.Digest != baseManifest.Digest {
		t.Fatalf("local invocation image selection = %+v, found %t", selection, found)
	}

	if err := os.Remove(filepath.Join(root, "fixture")); err != nil {
		t.Fatal(err)
	}
	invokedLayout := filepath.Join(root, "invoked")
	_, err = BuildDefinitionSupervised(ctx, parseWorkerDefinition(t, `
from "scratch"
component "local:external-onbuild" runtime_base="registry.example/coopr/invocation-base:latest"
`), planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: invokedLayout},
		ComponentStoreDir: componentDir, Pull: false,
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, invokedLayout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("invoked component layers = %d, want one", len(manifest.Layers))
	}
	blob := filepath.Join(invokedLayout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, blob, "selected"); got != "published package payload\n" {
		t.Fatalf("selected inherited package content = %q", got)
	}
}
