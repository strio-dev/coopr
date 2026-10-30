package oci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/componentstore"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
)

func TestComponentDeliverySelectsExactCaptureAndPolicy(t *testing.T) {
	root := Descriptor(v1.MediaTypeImageManifest, []byte("root"))
	repository := "delivery.example/parent"
	attachment := Descriptor(v1.MediaTypeImageManifest, []byte("attachment"))
	policy := bale.DependencyPolicy{Follow: "auto"}
	record := bale.InspectedDestination{Local: true, Subject: root, Descriptor: attachment, Record: bale.DestinationRecord{
		Bundle: "delivery.example/bundle@" + digest.FromString("bundle").String(), Destination: repository + "@" + root.Digest.String(),
		Resolutions: []bale.DestinationResolution{{Capture: digest.FromString("capture"), Policy: policy, Outcome: "selected"}},
	}}
	selected, err := selectComponentDelivery([]bale.InspectedDestination{record}, repository, root, nil)
	if err != nil || selected == nil {
		t.Fatalf("select delivery: %+v, %v", selected, err)
	}
	other := cloneDeliveryValue(record)
	other.Descriptor = Descriptor(v1.MediaTypeImageManifest, []byte("other attachment"))
	other.Record.Resolutions[0].Capture = digest.FromString("other capture")
	if _, err := selectComponentDelivery([]bale.InspectedDestination{record, other}, repository, root, nil); err == nil {
		t.Fatal("ambiguous captures were silently selected")
	}
	if got, err := selectComponentDelivery([]bale.InspectedDestination{record, other}, repository, root, &selected.Scope); err != nil || got.Identity() != selected.Identity() {
		t.Fatalf("explicit capture: %+v, %v", got, err)
	}
	missing := selected.Scope
	missing.Policy = bale.DependencyPolicy{Follow: "none"}
	if _, err := selectComponentDelivery([]bale.InspectedDestination{record}, repository, root, &missing); err == nil {
		t.Fatal("missing dependency policy fell back to another resolution")
	}
	foreign := cloneDeliveryValue(record)
	foreign.Local = false
	if got, err := selectComponentDelivery([]bale.InspectedDestination{foreign}, repository, root, nil); err != nil || got != nil {
		t.Fatalf("historical foreign record selected: %+v, %v", got, err)
	}
}

func TestComponentDeliveryMappingsPreserveDependencyPolicy(t *testing.T) {
	target := "delivery.example/child@" + digest.FromString("child").String()
	delivery := &ComponentDelivery{Scope: DeliveryScope{Bundle: "delivery.example/bundle@" + digest.FromString("bundle").String(), Capture: digest.FromString("capture")},
		Resolution: bale.DestinationResolution{Dependencies: []bale.DestinationDependency{
			{Ref: "docker.io/company/child:one", Source: "docker.io/company/child@" + digest.FromString("child").String(), Destination: target, Policy: bale.DependencyPolicy{Follow: "auto"}},
			{Ref: "docker.io/company/child:two", Source: "docker.io/company/child@" + digest.FromString("child").String(), Destination: target, Policy: bale.DependencyPolicy{Follow: "none"}},
		}},
	}
	for _, value := range []string{"company/child:one", "docker://docker.io/company/child:one"} {
		kind := "component"
		if strings.HasPrefix(value, "docker://") {
			kind = "image"
		}
		if got, err := delivery.MapExternalInput(kind, value); err != nil || got != target {
			t.Fatalf("map %s = %s, %v", value, got, err)
		}
	}
	for _, value := range []string{"./company/child:one", "../child.coopr", "oci:/tmp/child", digest.FromString("local").String(), "other.example/child:one"} {
		if got, err := delivery.MapExternalInput("component", value); err != nil || got != value {
			t.Fatalf("changed unrelated input %s = %s, %v", value, got, err)
		}
	}
	for _, name := range []string{"one", "two"} {
		scope, err := delivery.DependencyScope("company/child:" + name)
		if err != nil || scope == nil || scope.Bundle != delivery.Scope.Bundle || scope.Capture != delivery.Scope.Capture {
			t.Fatalf("nested scope: %+v, %v", scope, err)
		}
		want := "auto"
		if name == "two" {
			want = "none"
		}
		if scope.Policy.Follow != want {
			t.Fatalf("policy for %s = %s", name, scope.Policy.Follow)
		}
	}
	if _, err := delivery.DependencyScope(target); err == nil {
		t.Fatal("ambiguous destination alias silently chose a policy")
	}
	copy := delivery.Clone()
	copy.Resolution.Dependencies[0].Policy.Follow = "none"
	if delivery.Resolution.Dependencies[0].Policy.Follow != "auto" {
		t.Fatal("clone shared policy data")
	}
}

func TestComponentDeliverySourceOffAndOfflineArchive(t *testing.T) {
	fixture := captureBaleComponents(t)
	layout := filepath.Join(t.TempDir(), "bundle")
	if _, err := fixture.client.Pull(t.Context(), fixture.bundle, layout, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	otherBundle := fixture.name + "/bundles:other-capture"
	if _, err := fixture.client.Create(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: fixture.name + "/child:stable"}}}, otherBundle, bale.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	otherLayout := filepath.Join(t.TempDir(), "other-bundle")
	if _, err := fixture.client.Pull(t.Context(), otherBundle, otherLayout, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	server, _, resolver, destination := testRepository(t, registryserver.New())
	ref, err := ParseReference(destination + ":complete")
	if err != nil {
		t.Fatal(err)
	}
	client := baleHTTPClient(t, ref.Registry)
	plan, err := client.PlanPush(t.Context(), layout, destination+":complete", bale.PushOptions{RepositoryMap: map[string]string{fixture.name + "/parent": "parent", fixture.name + "/child": "child", fixture.name + "/leaf": "leaf"}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.closeSource()
	delivered, err := client.Push(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	var parent *Resolved
	for _, item := range []struct {
		name       string
		descriptor v1.Descriptor
	}{{"parent", fixture.parent}, {"child", fixture.child}, {"leaf", fixture.leaf}} {
		location, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: fixture.name + "/" + item.name, Digest: item.descriptor.Digest})
		if !ok {
			t.Fatalf("no delivery for %s", item.name)
		}
		pinned := location.Destination.Repository + "@" + item.descriptor.Digest.String()
		resolved, err := resolver.Resolve(t.Context(), pinned, v1.Platform{OS: "linux", Architecture: "amd64"}, Component)
		if err != nil || resolved.Delivery == nil {
			t.Fatalf("resolve delivered %s: %+v, %v", item.name, resolved, err)
		}
		if !content.Equal(resolved.Root, item.descriptor) {
			t.Fatal("delivery changed component root")
		}
		if item.name == "parent" {
			parent = resolved
		}
		if _, err := resolver.PullComponent(t.Context(), pinned, item.name); err != nil {
			t.Fatal(err)
		}
	}
	childOriginal := fixture.name + "/child:stable"
	childDestination, err := parent.Delivery.MapExternalInput("component", childOriginal)
	if err != nil || childDestination == childOriginal {
		t.Fatalf("dependency was not relocated: %q, %v", childDestination, err)
	}
	scope, err := parent.Delivery.DependencyScope(childOriginal)
	if err != nil {
		t.Fatal(err)
	}
	child, err := resolver.ResolveComponent(t.Context(), childDestination, v1.Platform{OS: "linux", Architecture: "amd64"}, nil, scope)
	if err != nil || child.Delivery == nil || child.Delivery.Scope.Bundle != parent.Delivery.Scope.Bundle || child.Delivery.Scope.Capture != parent.Delivery.Scope.Capture {
		t.Fatalf("nested delivery: %+v, %v", child, err)
	}
	frozenChild := child.ComponentSelection()
	otherPlan, err := client.PlanPush(t.Context(), otherLayout, destination+":other", bale.PushOptions{RepositoryMap: map[string]string{fixture.name + "/child": "child", fixture.name + "/leaf": "leaf"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Push(t.Context(), otherPlan); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.PullComponent(t.Context(), childDestination, "child-new-capture"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(t.Context(), childDestination, child.Platform, Component); err == nil {
		t.Fatal("repository-qualified digest implicitly selected a bundle capture")
	}
	unchangedChild, err := resolver.ResolveComponent(t.Context(), childDestination, child.Platform, &frozenChild, scope)
	if err != nil || unchangedChild.Delivery.Identity() != child.Delivery.Identity() {
		t.Fatalf("worker refreshed frozen delivery after new records appeared: %+v, %v", unchangedChild, err)
	}
	selectedChild, err := resolver.ResolveComponent(t.Context(), childDestination, child.Platform, nil, scope)
	if err != nil || selectedChild.Delivery.Identity() != child.Delivery.Identity() {
		t.Fatalf("parent scope did not select its stored child capture: %+v, %v", selectedChild, err)
	}
	_, root, _, err := componentstore.Resolve(t.Context(), resolver.ComponentStoreDir(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	store, err := componentstore.Open(t.Context(), resolver.ComponentStoreDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "component.tar")
	if err := componentstore.WriteArchive(t.Context(), store, root, archive); err != nil {
		t.Fatal(err)
	}
	server.Close()
	local, err := resolver.Resolve(t.Context(), "parent", v1.Platform{OS: "linux", Architecture: "amd64"}, Component)
	if err != nil || local.Delivery.Identity() != parent.Delivery.Identity() {
		t.Fatalf("offline context: %+v, %v", local, err)
	}
	loaded, err := testResolver(t, Options{ComponentStoreDir: filepath.Join(t.TempDir(), "components")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.LoadComponent(t.Context(), archive, "restored"); err != nil {
		t.Fatal(err)
	}
	// An archive's repository label cannot turn a historical/nonlocal record
	// into a usable local delivery or replace an existing good name.
	binding, err := componentstore.BindingFromDescriptor(root)
	if err != nil || binding == nil {
		t.Fatalf("missing retained binding: %+v, %v", binding, err)
	}
	forgedBinding := *binding
	forgedBinding.Repository = "other.example/component"
	forgedDir := t.TempDir()
	if err := componentstore.PutWithBinding(t.Context(), forgedDir, store, root, "forged", &forgedBinding); err != nil {
		t.Fatal(err)
	}
	forgedSource, forgedRoot, _, err := componentstore.Resolve(t.Context(), forgedDir, "forged")
	if err != nil {
		t.Fatal(err)
	}
	forgedArchive := filepath.Join(t.TempDir(), "forged.tar")
	if err := componentstore.WriteArchive(t.Context(), forgedSource, forgedRoot, forgedArchive); err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.LoadComponent(t.Context(), forgedArchive, "restored"); err == nil {
		t.Fatal("archive repository did not match its original destination record")
	}
	restored, err := loaded.Resolve(t.Context(), "restored", v1.Platform{OS: "linux", Architecture: "amd64"}, Component)
	if err != nil || restored.Delivery.Identity() != parent.Delivery.Identity() {
		t.Fatalf("restored context: %+v, %v", restored, err)
	}
	frozen := restored.ComponentSelection()
	data, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	var worker ComponentSelection
	if err := json.Unmarshal(data, &worker); err != nil {
		t.Fatal(err)
	}
	if got, err := loaded.ResolveComponent(t.Context(), "restored", restored.Platform, &worker, nil); err != nil || got.Delivery.Identity() != restored.Delivery.Identity() {
		t.Fatalf("frozen offline selection: %+v, %v", got, err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(childDestination, ref.Registry+"/") {
		t.Fatalf("dependency still points to source: %s", childDestination)
	}
}

func TestRegistryInputDistinguishesComponentNamesAndImageTransports(t *testing.T) {
	for _, name := range []string{"docker:stable", "oci:stable", "containers-storage:stable"} {
		got, ok := registryInput("component", name)
		if !ok || got != "docker.io/library/"+name {
			t.Fatalf("component %s normalized to %q, %t", name, got, ok)
		}
	}
	if _, ok := registryInput("image", "oci:/some/layout"); ok {
		t.Fatal("filesystem image transport became a registry dependency")
	}
	if got, ok := registryInput("image", "docker://docker.io/library/ubuntu:latest"); !ok || got != "docker.io/library/ubuntu:latest" {
		t.Fatalf("image registry transport: %s, %t", got, ok)
	}
}

func TestComponentDeliveryRetainsIndexRoot(t *testing.T) {
	fixture := captureBaleComponents(t)
	var variants []IndexVariant
	for _, architecture := range []string{"amd64", "arm64"} {
		metadata, paths := baleComponentMetadata(t, architecture, "indexed", fixture.name+"/child:stable")
		layout := t.TempDir()
		root, err := WriteComponentLayout(t.Context(), layout, metadata, paths)
		if err != nil {
			t.Fatal(err)
		}
		variants = append(variants, IndexVariant{Layout: layout, Manifest: root, Platform: metadata.Platform})
	}
	indexLayout := filepath.Join(t.TempDir(), "index")
	index, _, err := AssembleComponentIndex(t.Context(), indexLayout, variants)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.resolver.PublishLayout(t.Context(), fixture.name+"/indexed:stable", indexLayout, index); err != nil {
		t.Fatal(err)
	}
	bundle := fixture.name + "/bundles:indexed"
	if _, err := fixture.client.Create(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: fixture.name + "/indexed:stable"}}}, bundle, bale.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	localBundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := fixture.client.Pull(t.Context(), bundle, localBundle, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	server, _, resolver, destination := testRepository(t, registryserver.New())
	ref, err := ParseReference(destination + ":index")
	if err != nil {
		t.Fatal(err)
	}
	client := baleHTTPClient(t, ref.Registry)
	plan, err := client.PlanPush(t.Context(), localBundle, destination+":index", bale.PushOptions{RepositoryMap: map[string]string{fixture.name + "/indexed": "indexed", fixture.name + "/child": "child", fixture.name + "/leaf": "leaf"}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.closeSource()
	delivered, err := client.Push(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	location, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: fixture.name + "/indexed", Digest: index.Digest})
	if !ok {
		t.Fatal("missing index delivery")
	}
	pinned := location.Destination.Repository + "@" + index.Digest.String()
	if _, err := resolver.PullComponent(t.Context(), pinned, "indexed"); err != nil {
		t.Fatal(err)
	}
	server.Close()
	for _, architecture := range []string{"amd64", "arm64"} {
		platform := v1.Platform{OS: "linux", Architecture: architecture}
		resolved, err := resolver.Resolve(t.Context(), "indexed", platform, Component)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Root.Digest != index.Digest || resolved.Selected.Digest == index.Digest || resolved.Delivery == nil || resolved.Delivery.Subject.Digest != index.Digest {
			t.Fatalf("index identity lost: root=%s selected=%s delivery=%+v", resolved.Root.Digest, resolved.Selected.Digest, resolved.Delivery)
		}
		frozen := resolved.ComponentSelection()
		worker, err := resolver.ResolveComponent(t.Context(), pinned, platform, &frozen, nil)
		if err != nil || worker.Root.Digest != index.Digest || worker.Selected.Digest != resolved.Selected.Digest {
			t.Fatalf("worker index selection: %+v, %v", worker, err)
		}
	}
}
