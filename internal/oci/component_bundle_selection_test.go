package oci

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/coopr/internal/componentstore"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// The native Nix harness supplies its actual child archive. Create two original
// Bale captures of that same root, with distinct selected declaration contexts.
// Registries and producer stores disappear when this preparation test returns.
func TestPrepareNixDeliveryArchives(t *testing.T) {
	input := os.Getenv("COOPR_NIX_DELIVERY_FIXTURE_INPUT")
	if input == "" {
		t.Skip("requires a real child archive from the Nix acceptance harness")
	}
	output := os.Getenv("COOPR_NIX_DELIVERY_FIXTURE_OUTPUT")
	if !filepath.IsAbs(output) {
		t.Fatal("requires an explicit new fixture output directory")
	}
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	_, _, publisher, repository := testRepository(t, registryserver.New())
	root, err := publisher.LoadComponent(t.Context(), input, "seed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishLayout(t.Context(), repository+":child", publisher.ComponentStoreDir(), root); err != nil {
		t.Fatal(err)
	}
	reference, err := ParseReference(repository + ":child")
	if err != nil {
		t.Fatal(err)
	}
	client := baleHTTPClient(t, reference.Registry)
	var fixtures []map[string]string
	for _, name := range []string{"a", "b"} {
		declaration, err := bale.PrepareDependencyDeclaration(t.Context(), bale.Root{Repository: repository, Descriptor: root}, bale.DependencyDeclaration{Context: name})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Attach(t.Context(), declaration); err != nil {
			t.Fatal(err)
		}
		capture := filepath.Join(output, name+".bale")
		policy := bale.DependencyPolicy{Follow: "auto", Select: &bale.DependencySelector{Context: name}}
		if _, err := client.CreateLayout(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: repository + "@" + root.Digest.String(), Dependencies: policy}}}, capture, bale.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		plan, err := client.PlanPush(t.Context(), capture, repository+"/delivery-"+name+":bundle", bale.PushOptions{})
		if err != nil {
			t.Fatal(err)
		}
		delivered, err := client.Push(t.Context(), plan)
		if err != nil {
			t.Fatal(err)
		}
		placement, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: repository, Digest: root.Digest})
		if !ok {
			t.Fatal("selected child has no delivery placement")
		}
		tag := "bound-" + name
		if _, err := publisher.PullComponent(t.Context(), placement.Destination.Repository+"@"+root.Digest.String(), tag); err != nil {
			t.Fatal(err)
		}
		store, selected, binding, err := componentstore.Resolve(t.Context(), publisher.ComponentStoreDir(), tag)
		if err != nil || binding == nil || selected.Digest != root.Digest {
			t.Fatalf("selected original archive: %+v, %v", binding, err)
		}
		archive := filepath.Join(output, name+".oci.tar")
		if err := componentstore.WriteArchive(t.Context(), store, selected, archive); err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, map[string]string{"context": name, "archive": archive, "bundle": capture, "repository": binding.Repository, "digest": root.Digest.String()})
	}
	data, err := json.MarshalIndent(fixtures, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "fixtures.json"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestComponentBundleSelectedArchivePreservesCaptureAndPolicy(t *testing.T) {
	for _, scenario := range []struct {
		name, context string
		shared        bool
		indexed       bool
	}{{name: "default"}, {name: "selected-context", context: "selected"}, {name: "shared-repository", shared: true}, {name: "indexed-child", indexed: true}} {
		t.Run(scenario.name, func(t *testing.T) {
			context := scenario.context
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			fixture := captureBaleComponents(t)
			childRepository := fixture.name + "/child"
			if scenario.indexed {
				childRepository = fixture.name + "/indexed"
				var variants []IndexVariant
				for _, architecture := range []string{"amd64", "arm64"} {
					metadata, paths := publicationMetadata(t, nil, false)
					metadata.Platform.Architecture = architecture
					metadata.Component.Platform = "linux/" + architecture
					layout := filepath.Join(t.TempDir(), architecture)
					manifest, err := WriteComponentLayout(t.Context(), layout, metadata, paths)
					if err != nil {
						t.Fatal(err)
					}
					variants = append(variants, IndexVariant{Layout: layout, Manifest: manifest, Platform: metadata.Platform})
				}
				layout := filepath.Join(t.TempDir(), "index")
				root, _, err := AssembleComponentIndex(t.Context(), layout, variants)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.resolver.PublishLayout(t.Context(), childRepository+":stable", layout, root); err != nil {
					t.Fatal(err)
				}
				fixture.child = root
			}
			input := filepath.Join(t.TempDir(), "complete")
			policy := bale.DependencyPolicy{Follow: "auto"}
			if scenario.indexed {
				if _, err := fixture.client.CreateLayout(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: childRepository + ":stable"}}}, input, bale.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			} else if context == "" {
				if _, err := fixture.client.Pull(t.Context(), fixture.bundle, input, bale.PullLocked); err != nil {
					t.Fatal(err)
				}
			} else {
				request, err := bale.PrepareDependencyDeclaration(t.Context(), bale.Root{Repository: childRepository, Descriptor: fixture.child}, bale.DependencyDeclaration{Context: context, Dependencies: []bale.DependencyReference{{Ref: fixture.name + "/leaf:stable"}}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.client.Attach(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				policy.Select = &bale.DependencySelector{Context: context}
				if _, err := fixture.client.CreateLayout(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: childRepository + ":stable", Dependencies: policy}}}, input, bale.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			wrong := filepath.Join(t.TempDir(), "different-capture")
			if _, err := fixture.client.CreateLayout(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: fixture.name + "/leaf:stable"}}}, wrong, bale.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			server, _, publisher, destination := testRepository(t, registryserver.New())
			destinationRef, err := ParseReference(destination + ":original")
			if err != nil {
				t.Fatal(err)
			}
			client := baleHTTPClient(t, destinationRef.Registry)
			plan, err := client.PlanPush(t.Context(), input, destination+":original", bale.PushOptions{})
			if err != nil {
				t.Fatal(err)
			}
			delivered, err := client.Push(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}
			location, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: childRepository, Digest: fixture.child.Digest})
			if !ok {
				t.Fatal("original child was not delivered")
			}
			if _, err := publisher.PullComponent(t.Context(), location.Destination.Repository+"@"+fixture.child.Digest.String(), "child"); err != nil {
				t.Fatal(err)
			}
			store, root, binding, err := componentstore.Resolve(t.Context(), publisher.ComponentStoreDir(), "child")
			if err != nil || binding == nil {
				t.Fatalf("original binding: %+v, %v", binding, err)
			}
			archive := filepath.Join(t.TempDir(), "child.tar")
			if err := componentstore.WriteArchive(t.Context(), store, root, archive); err != nil {
				t.Fatal(err)
			}
			producer, err := NewResolver(Options{ComponentStoreDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := producer.LoadComponent(t.Context(), archive, "selected"); err != nil {
				t.Fatal(err)
			}
			leafAlias := "coopr.invalid/selected/child@" + fixture.leaf.Digest.String()
			if scenario.shared {
				placement, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: fixture.name + "/leaf", Digest: fixture.leaf.Digest})
				if !ok {
					t.Fatal("missing co-located leaf")
				}
				if _, err := publisher.PullComponent(t.Context(), placement.Destination.Repository+"@"+fixture.leaf.Digest.String(), "leaf"); err != nil {
					t.Fatal(err)
				}
				leafStore, leafRoot, _, err := componentstore.Resolve(t.Context(), publisher.ComponentStoreDir(), "leaf")
				if err != nil {
					t.Fatal(err)
				}
				leafArchive := filepath.Join(t.TempDir(), "leaf.tar")
				if err := componentstore.WriteArchive(t.Context(), leafStore, leafRoot, leafArchive); err != nil {
					t.Fatal(err)
				}
				if _, err := producer.LoadComponent(t.Context(), leafArchive, "selected-leaf"); err != nil {
					t.Fatal(err)
				}
			}
			fixture.closeSource()
			server.Close()

			childRef := "coopr.invalid/selected/child@" + fixture.child.Digest.String()
			dependencies := []string{childRef}
			if scenario.shared {
				dependencies = append(dependencies, leafAlias)
			}
			metadata, paths := baleComponentMetadata(t, "amd64", "new-parent", dependencies...)
			layout := filepath.Join(t.TempDir(), "parent")
			parentRoot, err := WriteComponentLayout(t.Context(), layout, metadata, paths)
			if err != nil {
				t.Fatal(err)
			}
			source, err := orasoci.NewFromFS(t.Context(), os.DirFS(layout))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := producer.storeComponent(t.Context(), source, parentRoot, "parent"); err != nil {
				t.Fatal(err)
			}
			options := ComponentBundleOptions{Reference: "parent", Origin: "packages.example/parent:stable", Components: map[string]string{childRef: "selected"}}
			if scenario.shared {
				options.Components[leafAlias] = "selected-leaf"
			}
			for _, inputs := range [][]string{nil, {wrong}} {
				options.Output, options.Inputs = filepath.Join(t.TempDir(), "reject"), inputs
				if _, err := producer.BundleComponent(t.Context(), options); err == nil || !strings.Contains(err.Error(), "no complete input bundle matches") {
					t.Fatalf("accepted wrong/missing original capture: %v", err)
				}
				if _, err := os.Lstat(options.Output); !os.IsNotExist(err) {
					t.Fatalf("rejected capture exposed output: %v", err)
				}
			}
			options.Output, options.Inputs = filepath.Join(t.TempDir(), "complete-parent"), []string{input}
			created, err := producer.BundleComponent(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			verified, err := client.InspectLayout(t.Context(), options.Output)
			if err != nil || verified.MetadataOnly || !content.Equal(verified.Descriptor, created.Descriptor) {
				t.Fatalf("complete output: %+v, %v", verified, err)
			}
			outputStore, err := orasoci.NewFromFS(t.Context(), os.DirFS(options.Output))
			if err != nil {
				t.Fatal(err)
			}
			for _, attachment := range binding.Attachments {
				before, err := content.FetchAll(t.Context(), store, attachment)
				if err != nil {
					t.Fatal(err)
				}
				after, err := content.FetchAll(t.Context(), outputStore, attachment)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("original attachment changed: %v", err)
				}
			}
			for _, descriptor := range []v1.Descriptor{plan.Descriptor, fixture.parent} {
				present, err := outputStore.Exists(t.Context(), descriptor)
				if err != nil || present {
					t.Fatalf("unselected original capture/ancestor %s was included: present=%t, %v", descriptor.Digest, present, err)
				}
			}

			_, _, consumer, consumerDestination := testRepository(t, registryserver.New())
			if consumer.ComponentStoreDir() == publisher.ComponentStoreDir() || consumer.ComponentStoreDir() == producer.ComponentStoreDir() {
				t.Fatal("consumer did not get an independent component store")
			}
			entries, err := os.ReadDir(consumer.ComponentStoreDir())
			if err == nil && len(entries) != 0 {
				t.Fatal("consumer store was not empty")
			}
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			consumerRef, err := ParseReference(consumerDestination + ":new")
			if err != nil {
				t.Fatal(err)
			}
			consumerClient := baleHTTPClient(t, consumerRef.Registry)
			newPlan, err := consumerClient.PlanPush(t.Context(), options.Output, consumerDestination+":new", bale.PushOptions{})
			if err != nil {
				t.Fatal(err)
			}
			newDelivery, err := consumerClient.Push(t.Context(), newPlan)
			if err != nil {
				t.Fatal(err)
			}
			parentPlacement, ok := newDelivery.Relocations.Lookup(bale.ArtifactID{Repository: "packages.example/parent", Digest: parentRoot.Digest})
			if !ok {
				t.Fatal("new parent has no placement")
			}
			parent, err := consumer.ResolveComponent(t.Context(), parentPlacement.Destination.Repository+"@"+parentRoot.Digest.String(), v1.Platform{OS: "linux", Architecture: "amd64"}, nil, nil)
			if err != nil || parent.Delivery == nil {
				t.Fatalf("new parent: %+v, %v", parent, err)
			}
			mappedChild, err := parent.Delivery.MapExternalInput("component", childRef)
			if err != nil || mappedChild == childRef {
				t.Fatalf("new child placement: %s, %v", mappedChild, err)
			}
			childScope, err := parent.Delivery.DependencyScope(childRef)
			if err != nil || childScope == nil || !reflect.DeepEqual(childScope.Policy, policy) {
				t.Fatalf("selected policy lost: %+v, %v", childScope, err)
			}
			child, err := consumer.ResolveComponent(t.Context(), mappedChild, parent.Platform, nil, childScope)
			if err != nil || child.Root.Digest != fixture.child.Digest || child.Delivery == nil {
				t.Fatalf("new child: %+v, %v", child, err)
			}
			if scenario.indexed {
				for _, architecture := range []string{"amd64", "arm64"} {
					selected, err := consumer.ResolveComponent(t.Context(), mappedChild, v1.Platform{OS: "linux", Architecture: architecture}, nil, childScope)
					if err != nil || selected.Root.Digest != fixture.child.Digest || selected.Selected.Digest == fixture.child.Digest || selected.Platform.Architecture != architecture || selected.Component.Component.Platform != "linux/"+architecture {
						t.Fatalf("bound index/member selection changed: %+v, %v", selected, err)
					}
				}
				return
			}
			if child.Delivery.Resolution.Declaration == nil || child.Delivery.Resolution.Declaration.Context != context {
				t.Fatalf("selected declaration changed: %+v", child.Delivery.Resolution)
			}
			leafRef := fixture.name + "/leaf:stable"
			mappedLeaf, err := child.Delivery.MapExternalInput("component", leafRef)
			if err != nil || mappedLeaf == leafRef {
				t.Fatalf("original nested dependency was not relocated: %s, %v", mappedLeaf, err)
			}
			leafScope, err := child.Delivery.DependencyScope(leafRef)
			if err != nil {
				t.Fatal(err)
			}
			leaf, err := consumer.ResolveComponent(t.Context(), mappedLeaf, child.Platform, nil, leafScope)
			if err != nil || leaf.Root.Digest != fixture.leaf.Digest {
				t.Fatalf("original locked leaf changed: %+v, %v", leaf, err)
			}
			if scenario.shared {
				mapped, err := parent.Delivery.MapExternalInput("component", leafAlias)
				if err != nil || mapped == leafAlias {
					t.Fatalf("second immutable child did not share alias repository: %s, %v", mapped, err)
				}
				scope, err := parent.Delivery.DependencyScope(leafAlias)
				if err != nil {
					t.Fatal(err)
				}
				selected, err := consumer.ResolveComponent(t.Context(), mapped, parent.Platform, nil, scope)
				if err != nil || selected.Root.Digest != fixture.leaf.Digest {
					t.Fatalf("co-located immutable child: %+v, %v", selected, err)
				}
			}
		})
	}
}

func TestEquivalentCapturedResolutionIgnoresOnlyPlacementAndOrder(t *testing.T) {
	original := bale.DestinationResolution{Capture: digest.FromString("capture"), Policy: bale.DependencyPolicy{Follow: "auto"}, Outcome: "selected", Declaration: &bale.DestinationDeclaration{Digest: digest.FromString("declaration"), Context: "selected"}, Dependencies: []bale.DestinationDependency{{Ref: "example.com/one:stable", Policy: bale.DependencyPolicy{Follow: "auto"}, Source: "example.com/one@" + digest.FromString("one").String(), Destination: "old.example/one@" + digest.FromString("one").String()}, {Ref: "example.com/two:stable", Policy: bale.DependencyPolicy{Follow: "auto"}, Source: "example.com/two@" + digest.FromString("two").String(), Destination: "old.example/two@" + digest.FromString("two").String()}}}
	placement := cloneDeliveryValue(original)
	placement.Dependencies[0], placement.Dependencies[1] = placement.Dependencies[1], placement.Dependencies[0]
	placement.Dependencies[0].Destination = "new.example/placement"
	if !equivalentCapturedResolution(original, placement) {
		t.Fatal("new placement/order invalidated the original capture")
	}
	for name, mutate := range map[string]func(*bale.DestinationResolution){
		"capture":             func(r *bale.DestinationResolution) { r.Capture = digest.FromString("other") },
		"policy":              func(r *bale.DestinationResolution) { r.Policy.Follow = "none" },
		"outcome":             func(r *bale.DestinationResolution) { r.Outcome = "absent" },
		"declaration digest":  func(r *bale.DestinationResolution) { r.Declaration.Digest = digest.FromString("other") },
		"declaration context": func(r *bale.DestinationResolution) { r.Declaration.Context = "other" },
		"dependency ref":      func(r *bale.DestinationResolution) { r.Dependencies[0].Ref = "example.com/other:stable" },
		"dependency policy":   func(r *bale.DestinationResolution) { r.Dependencies[0].Policy.Follow = "none" },
		"dependency source": func(r *bale.DestinationResolution) {
			r.Dependencies[0].Source = "example.com/other@" + digest.FromString("other").String()
		},
		"dependency count": func(r *bale.DestinationResolution) { r.Dependencies = r.Dependencies[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			changed := cloneDeliveryValue(original)
			mutate(&changed)
			if equivalentCapturedResolution(original, changed) {
				t.Fatal("different capture semantics were accepted")
			}
		})
	}
}
