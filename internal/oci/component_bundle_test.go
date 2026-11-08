package oci

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/coopr/internal/componentstore"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

func TestComponentBundleOfflineCaptureAndDelivery(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fixture := captureBaleComponents(t)
	input := filepath.Join(t.TempDir(), "input")
	if _, err := fixture.client.Pull(t.Context(), fixture.bundle, input, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	nestedInput := filepath.Join(t.TempDir(), "nested-input")
	if _, err := fixture.client.CreateLayout(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: fixture.bundle}}}, nestedInput, bale.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.resolver.PullComponent(t.Context(), fixture.name+"/parent:stable", "local-parent"); err != nil {
		t.Fatal(err)
	}
	fixture.closeSource()
	origin := "packages.example/nix/environment:stable"
	output := filepath.Join(t.TempDir(), "output")
	created, err := fixture.resolver.BundleComponent(t.Context(), ComponentBundleOptions{Reference: "local-parent", Origin: origin, Output: output, Inputs: []string{input}})
	if err != nil {
		t.Fatal(err)
	}
	if created.Published || created.MetadataOnly || created.LayoutPath != output || len(created.Metadata.Roots) != 1 || created.Metadata.Roots[0].Descriptor.Digest != fixture.parent.Digest {
		t.Fatalf("unexpected local capture: %+v", created)
	}
	nested, err := fixture.resolver.BundleComponent(t.Context(), ComponentBundleOptions{Reference: "local-parent", Origin: origin, Output: filepath.Join(t.TempDir(), "from-nested"), Inputs: []string{nestedInput}})
	if err != nil || !content.Equal(nested.Descriptor, created.Descriptor) {
		t.Fatalf("nested native capture changed locked dependencies: %+v, %v", nested, err)
	}
	verified, err := fixture.client.InspectLayout(t.Context(), output)
	if err != nil || verified.MetadataOnly || !content.Equal(verified.Descriptor, created.Descriptor) {
		t.Fatalf("verify complete capture: %+v, %v", verified, err)
	}
	for _, item := range []struct {
		repository string
		descriptor v1.Descriptor
	}{
		{"packages.example/nix/environment", fixture.parent},
		{fixture.name + "/child", fixture.child},
		{fixture.name + "/leaf", fixture.leaf},
	} {
		found := false
		for _, origin := range verified.Metadata.Origins {
			found = found || origin.Repository == item.repository && content.Equal(origin.Descriptor, item.descriptor)
		}
		if !found {
			t.Fatalf("missing captured dependency %s@%s", item.repository, item.descriptor.Digest)
		}
	}
	_, _, _, destination := testRepository(t, registryserver.New())
	receiver, err := NewResolver(Options{TLSVerify: new(false), ComponentStoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := ParseReference(destination + ":delivered")
	if err != nil {
		t.Fatal(err)
	}
	client := baleHTTPClient(t, ref.Registry)
	plan, err := client.PlanPush(t.Context(), output, destination+":delivered", bale.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := client.Push(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	parentLocation, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: "packages.example/nix/environment", Digest: fixture.parent.Digest})
	if !ok {
		t.Fatal("parent has no relocated destination")
	}
	parent, err := receiver.Resolve(t.Context(), parentLocation.Destination.Repository+"@"+fixture.parent.Digest.String(), v1.Platform{OS: "linux", Architecture: "amd64"}, Component)
	if err != nil || parent.Delivery == nil {
		t.Fatalf("resolve parent: %+v, %v", parent, err)
	}
	childReference := fixture.name + "/child:stable"
	childDestination, err := parent.Delivery.MapExternalInput("component", childReference)
	if err != nil || childDestination == childReference {
		t.Fatalf("child mapping: %s, %v", childDestination, err)
	}
	scope, err := parent.Delivery.DependencyScope(childReference)
	if err != nil {
		t.Fatal(err)
	}
	child, err := receiver.ResolveComponent(t.Context(), childDestination, parent.Platform, nil, scope)
	if err != nil || child.Delivery == nil || child.Root.Digest != fixture.child.Digest {
		t.Fatalf("resolve child in new parent capture: %+v, %v", child, err)
	}
	leafReference := fixture.name + "/leaf:stable"
	leafDestination, err := child.Delivery.MapExternalInput("component", leafReference)
	if err != nil || leafDestination == leafReference {
		t.Fatalf("leaf mapping: %s, %v", leafDestination, err)
	}
	leafScope, err := child.Delivery.DependencyScope(leafReference)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := receiver.ResolveComponent(t.Context(), leafDestination, child.Platform, nil, leafScope)
	if err != nil || leaf.Root.Digest != fixture.leaf.Digest {
		t.Fatalf("resolve leaf: %+v, %v", leaf, err)
	}
	if _, err := receiver.PullComponent(t.Context(), parentLocation.Destination.Repository+"@"+fixture.parent.Digest.String(), "delivered"); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.BundleComponent(t.Context(), ComponentBundleOptions{Reference: "delivered", Origin: "packages.example/rebound:stable", Output: filepath.Join(t.TempDir(), "rebound"), Inputs: []string{output}}); err == nil || !strings.Contains(err.Error(), "already has a delivery binding") {
		t.Fatalf("bound parent silently acquired a new delivery: %v", err)
	}
}

func TestComponentBundleRejectsMissingInputsAndPreservesOutput(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fixture := captureBaleComponents(t)
	if _, err := fixture.resolver.PullComponent(t.Context(), fixture.name+"/parent:stable", "parent"); err != nil {
		t.Fatal(err)
	}
	fixture.closeSource()
	output := filepath.Join(t.TempDir(), "output")
	if _, err := fixture.resolver.BundleComponent(t.Context(), ComponentBundleOptions{Reference: "parent", Origin: "packages.example/parent:stable", Output: output, Inputs: nil}); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("missing dependency must fail without registry fallback: %v", err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed capture exposed output: %v", err)
	}
	if err := os.WriteFile(output, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.resolver.BundleComponent(t.Context(), ComponentBundleOptions{Reference: "parent", Origin: "packages.example/parent:stable", Output: output, Inputs: nil}); err == nil {
		t.Fatal("existing output accepted")
	}
	if data, err := os.ReadFile(output); err != nil || string(data) != "keep" {
		t.Fatalf("existing output changed: %q, %v", data, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := fixture.resolver.BundleComponent(ctx, ComponentBundleOptions{Reference: "parent", Origin: "packages.example/parent:stable", Output: filepath.Join(t.TempDir(), "cancel"), Inputs: nil}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestComponentBundleRejectsConflictingCapturedSelectors(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fixture := captureBaleComponents(t)
	first := filepath.Join(t.TempDir(), "first")
	if _, err := fixture.client.Pull(t.Context(), fixture.bundle, first, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	meta, paths := baleComponentMetadata(t, "amd64", "different-leaf")
	if _, err := fixture.resolver.PublishComponent(t.Context(), fixture.name+"/leaf:stable", meta, paths); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(t.TempDir(), "second")
	if _, err := fixture.client.CreateLayout(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: fixture.name + "/leaf:stable"}}}, second, bale.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.resolver.PullComponent(t.Context(), fixture.name+"/parent:stable", "parent"); err != nil {
		t.Fatal(err)
	}
	fixture.closeSource()
	if _, err := fixture.resolver.BundleComponent(t.Context(), ComponentBundleOptions{Reference: "parent", Origin: "packages.example/parent:stable", Output: filepath.Join(t.TempDir(), "output"), Inputs: []string{first, second}}); err == nil || !strings.Contains(err.Error(), "conflicting results") {
		t.Fatalf("conflicting captures implicitly selected an alternative: %v", err)
	}
}

func TestComponentBundleRejectsUncapturedDiscovery(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fixture := captureBaleComponents(t)
	input := filepath.Join(t.TempDir(), "disabled-discovery")
	selection := bale.Selection{Ref: fixture.name + "/child:stable", Dependencies: bale.DependencyPolicy{Follow: "none"}, Referrers: bale.ReferrerPolicy{Include: "none"}}
	if _, err := fixture.client.CreateLayout(t.Context(), bale.Recipe{Include: []bale.Selection{selection, {Ref: fixture.name + "/leaf:stable"}}}, input, bale.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.resolver.PullComponent(t.Context(), fixture.name+"/parent:stable", "parent"); err != nil {
		t.Fatal(err)
	}
	fixture.closeSource()
	output := filepath.Join(t.TempDir(), "output")
	if _, err := fixture.resolver.BundleComponent(t.Context(), ComponentBundleOptions{Reference: "parent", Origin: "packages.example/parent:stable", Output: output, Inputs: []string{input}}); err == nil || !strings.Contains(err.Error(), "not present in complete input bundles") {
		t.Fatalf("disabled discovery was silently converted to an absent declaration: %v", err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed preflight exposed a bundle: %v", err)
	}
}

func TestComponentBundlePreservesIndex(t *testing.T) {
	resolver, err := NewResolver(Options{ComponentStoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var variants []IndexVariant
	for _, architecture := range []string{"amd64", "arm64"} {
		metadata, paths := publicationMetadata(t, nil, false)
		metadata.Platform.Architecture = architecture
		metadata.Component.Platform = "linux/" + architecture
		layout := filepath.Join(t.TempDir(), architecture)
		root, err := WriteComponentLayout(t.Context(), layout, metadata, paths)
		if err != nil {
			t.Fatal(err)
		}
		variants = append(variants, IndexVariant{Layout: layout, Manifest: root, Platform: metadata.Platform})
	}
	layout := filepath.Join(t.TempDir(), "index")
	root, _, err := AssembleComponentIndex(t.Context(), layout, variants)
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.NewFromFS(t.Context(), os.DirFS(layout))
	if err != nil {
		t.Fatal(err)
	}
	if err := componentstore.Put(t.Context(), resolver.ComponentStoreDir(), source, root, "multi"); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "bundle")
	created, err := resolver.BundleComponent(t.Context(), ComponentBundleOptions{Reference: "multi", Origin: "packages.example/multi:stable", Output: output, Inputs: nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Metadata.Roots) != 1 || !content.Equal(created.Metadata.Roots[0].Descriptor, root) {
		t.Fatalf("indexed root changed: %+v", created.Metadata.Roots)
	}
	for _, variant := range variants {
		found := false
		for _, origin := range created.Metadata.Origins {
			found = found || content.Equal(origin.Descriptor, variant.Manifest)
		}
		if !found {
			t.Fatalf("missing indexed platform %s", variant.Platform.Architecture)
		}
	}
}

func TestComponentBundleRejectsIncompleteAndCorruptInputs(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fixture := captureBaleComponents(t)
	complete := filepath.Join(t.TempDir(), "complete")
	if _, err := fixture.client.Pull(t.Context(), fixture.bundle, complete, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	metadataOnly := filepath.Join(t.TempDir(), "metadata-only")
	if _, err := fixture.client.CreateLocal(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: fixture.name + "/parent:stable"}}}, metadataOnly, bale.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.resolver.PullComponent(t.Context(), fixture.name+"/parent:stable", "parent"); err != nil {
		t.Fatal(err)
	}
	fixture.closeSource()
	manifest := filepath.Join(complete, "blobs", fixture.child.Digest.Algorithm().String(), fixture.child.Digest.Encoded())
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 1
	if err := os.Chmod(manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{metadataOnly, complete} {
		output := filepath.Join(t.TempDir(), "output")
		if _, err := fixture.resolver.BundleComponent(t.Context(), ComponentBundleOptions{Reference: "parent", Origin: "packages.example/parent:stable", Output: output, Inputs: []string{input}}); err == nil {
			t.Fatalf("accepted incomplete/corrupt capture %s", input)
		}
		if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed input validation exposed output: %v", err)
		}
	}
}
