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
	"coopr/internal/imagecatalog"
	"coopr/internal/oci"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestSupervisedRawPublicationPlansExternalOnBuildBeforePackageProduction(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah publication")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	for name, contents := range map[string]string{"artifact": "hidden producer\n", "payload": "inherited argument\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	parentLayout := filepath.Join(root, "parent")
	parent, err := BuildPlan(ctx, testPlan(t, `
from "scratch"
onbuild { arg "filename" "payload" }
onbuild { copy "/artifact" "/inherited" from="hidden" }
`), PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless",
		Output:        Output{Path: parentLayout, Format: outputFormatDocker},
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := oci.ReadImageConfigLayout(ctx, parentLayout)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := oci.LayoutRoot(parentLayout)
	if err != nil {
		t.Fatal(err)
	}
	const reference = "registry.example/coopr/package-onbuild:latest"
	imageStoreDir := filepath.Join(root, "images")
	if err := imagecatalog.Commit(ctx, imageStoreDir, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, imagecatalog.Selection{
		Root: selected, Manifest: selected, ImageID: parent.ImageID, ConfigData: config,
	}); err != nil {
		t.Fatal(err)
	}

	component := parseWorkerDefinition(t, `
from "scratch" as="hidden"
copy "artifact" "/artifact"
from "registry.example/coopr/package-onbuild:latest" as="producer"
copy "$filename" "/selected"
package as="bundle"
copy "/selected" "/selected" from="producer"
copy "/inherited" "/inherited" from="producer"
extend
copy "/selected" "/selected" from="bundle"
copy "/inherited" "/inherited" from="bundle"
`)
	layout := filepath.Join(root, "component")
	result, err := PublishDefinitionSupervised(ctx, component, planner.Options{Mode: planner.Publish, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		ImageStoreDir: imageStoreDir, SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := readComponentMetadata(t, ctx, layout, result.Root)
	if len(metadata.Packages) != 1 || metadata.Packages[0].Stage != "bundle" {
		t.Fatalf("published packages = %+v", metadata.Packages)
	}
	componentSource, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	componentDir := filepath.Join(root, "components")
	if err := componentstore.Put(ctx, componentDir, componentSource, result.Root, "bundle-onbuild"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"artifact", "payload"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: componentDir})
	if err != nil {
		t.Fatal(err)
	}
	invokedLayout := filepath.Join(root, "invoked")
	_, err = BuildPlan(ctx, testPlan(t, `
from "scratch"
component "local:bundle-onbuild"
`), PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: invokedLayout}, Resolver: resolver,
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, invokedLayout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("invoked component layers = %d, want one", len(manifest.Layers))
	}
	blob := filepath.Join(invokedLayout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	for name, want := range map[string]string{"selected": "inherited argument\n", "inherited": "hidden producer\n"} {
		if got := readLayerFile(t, blob, name); got != want {
			t.Fatalf("invoked %s = %q, want %q", name, got, want)
		}
	}
}

func TestPublishedPackageOnBuildArgumentPlansInvocationFromPackage(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless package ONBUILD invocation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture"), []byte("from package snapshot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	component := parseWorkerDefinition(t, `
package as="payload"
copy "fixture" "/fixture"
onbuild { arg "name" "fixture" }
from "payload" as="prepared"
copy "/$name" "/selected" from="payload"
extend
copy "/selected" "/selected" from="prepared"
`)
	layout := filepath.Join(root, "package-component")
	result, err := PublishDefinitionSupervised(ctx, component, planner.Options{Mode: planner.Publish, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	componentDir := filepath.Join(root, "components")
	if err := componentstore.Put(ctx, componentDir, source, result.Root, "package-onbuild"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "fixture")); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: componentDir})
	if err != nil {
		t.Fatal(err)
	}
	invokedLayout := filepath.Join(root, "package-invoked")
	_, err = BuildPlan(ctx, testPlan(t, `
from "scratch"
component "local:package-onbuild"
`), PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: invokedLayout}, Resolver: resolver,
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, invokedLayout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("invoked layers = %d, want one component layer", len(manifest.Layers))
	}
	blob := filepath.Join(invokedLayout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, blob, "selected"); got != "from package snapshot\n" {
		t.Fatalf("selected package file = %q", got)
	}
}

func TestPublishedPackageOnBuildRetainsHiddenPackageStage(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless package ONBUILD stage reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture"), []byte("hidden package stage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	component := parseWorkerDefinition(t, `
package as="assets"
copy "fixture" "/asset"
package as="payload"
onbuild { arg "source" "assets" }
onbuild { copy "/asset" "/selected" from="$source" }
from "payload" as="prepared"
extend
copy "/selected" "/selected" from="prepared"
`)
	layout := filepath.Join(root, "component")
	result, err := PublishDefinitionSupervised(ctx, component, planner.Options{Mode: planner.Publish, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := readComponentMetadata(t, ctx, layout, result.Root)
	if len(metadata.Packages) != 2 {
		t.Fatalf("published packages = %+v, want both payload and hidden assets", metadata.Packages)
	}
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	componentDir := filepath.Join(root, "components")
	if err := componentstore.Put(ctx, componentDir, source, result.Root, "hidden-package-onbuild"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "fixture")); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: componentDir})
	if err != nil {
		t.Fatal(err)
	}
	invokedLayout := filepath.Join(root, "invoked")
	_, err = BuildPlan(ctx, testPlan(t, `
from "scratch"
component "local:hidden-package-onbuild"
`), PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: invokedLayout}, Resolver: resolver,
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, invokedLayout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("invoked layers = %d, want one component layer", len(manifest.Layers))
	}
	blob := filepath.Join(invokedLayout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, blob, "selected"); got != "hidden package stage\n" {
		t.Fatalf("selected hidden package content = %q", got)
	}
}
