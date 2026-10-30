package oci

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func baleComponentMetadata(t *testing.T, architecture, value string, dependencies ...string) (ComponentMetadata, map[string]string) {
	t.Helper()
	data := packageTar(t, value)
	meta, paths := publicationMetadata(t, data, true)
	meta.Platform.Architecture = architecture
	meta.Packages[0].Config = packageImageConfig(t, meta.Platform, data)
	var source strings.Builder
	fmt.Fprintf(&source, "package as=\"pkg\" {}\nextend as=\"base\" {\ncopy \"/src\" \"/dst\" from=\"pkg\"\nenv VERSION=%q\n", value)
	for _, reference := range dependencies {
		fmt.Fprintf(&source, "component %q\n", reference)
	}
	source.WriteString("}\n")
	def, err := definition.Parse(strings.NewReader(source.String()))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: "linux/" + architecture})
	if err != nil {
		t.Fatal(err)
	}
	meta.Component = *plan.Component
	return meta, paths
}

func baleHTTPClient(t *testing.T, hosts ...string) *bale.Client {
	t.Helper()
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: hosts})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type baleCaptureRoundTripper func(*http.Request) (*http.Response, error)

func (transport baleCaptureRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestComponentDependencyPublicationKeepsTagSelectorsAndContent(t *testing.T) {
	for _, mode := range []string{"direct", "layout", "index"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			_, _, resolver, name := testRepository(t, registryserver.New())
			var dependencyRequests atomic.Int64
			var publishing atomic.Bool
			publishing.Store(true)
			dependencyRegistry := registryserver.New()
			_, _, _, dependencyName := testRepository(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dependencyRequests.Add(1)
				if publishing.Load() {
					t.Logf("unresolved dependency request during publication: %s %s%s from %s", r.Method, r.Host, r.URL.String(), r.RemoteAddr)
				}
				dependencyRegistry.ServeHTTP(w, r)
			}))
			selector := dependencyName + ":stable"
			wantedReferences := []string{selector}
			meta, paths := baleComponentMetadata(t, "amd64", "parent", selector, selector)
			layout := t.TempDir()
			expected, err := WriteComponentLayout(ctx, layout, meta, paths)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "index" {
				armSelector := dependencyName + ":arm64"
				wantedReferences = append(wantedReferences, armSelector)
				arm, armPaths := baleComponentMetadata(t, "arm64", "parent", selector, armSelector)
				armLayout := t.TempDir()
				armRoot, err := WriteComponentLayout(ctx, armLayout, arm, armPaths)
				if err != nil {
					t.Fatal(err)
				}
				output := filepath.Join(t.TempDir(), "index")
				expected, _, err = AssembleComponentIndex(ctx, output, []IndexVariant{{Layout: layout, Manifest: expected, Platform: meta.Platform}, {Layout: armLayout, Manifest: armRoot, Platform: arm.Platform}})
				if err != nil {
					t.Fatal(err)
				}
				layout = output
			}
			var published v1.Descriptor
			if mode == "direct" {
				published, err = resolver.PublishComponent(ctx, name+":parent", meta, paths)
			} else {
				_, err = resolver.PublishLayout(ctx, name+":parent", layout, expected)
				published = expected
			}
			if err != nil {
				t.Fatal(err)
			}
			if !content.Equal(published, expected) {
				t.Fatalf("attachment publication changed component root: got %+v, want %+v", published, expected)
			}
			if dependencyRequests.Load() != 0 {
				t.Fatal("publishing a declaration contacted its unresolved dependency registry")
			}
			publishing.Store(false)
			ref, err := ParseReference(name + ":parent")
			if err != nil {
				t.Fatal(err)
			}
			client := baleHTTPClient(t, ref.Registry)
			declarations, err := client.InspectDependencies(ctx, name+"@"+published.Digest.String())
			if err != nil || len(declarations) != 1 {
				t.Fatalf("dependency declarations = %+v, %v; want one", declarations, err)
			}
			declaration := declarations[0]
			if !content.Equal(declaration.Subject, published) {
				t.Fatalf("declaration lost immutable subject: %+v", declaration)
			}
			var references []string
			for _, dependency := range declaration.Declaration.Dependencies {
				references = append(references, dependency.Ref)
			}
			slices.Sort(references)
			slices.Sort(wantedReferences)
			if !reflect.DeepEqual(references, wantedReferences) {
				t.Fatalf("declaration references = %v, want deduplicated platform union %v", references, wantedReferences)
			}
			remote, err := client.Resolve(ctx, name+"@"+published.Digest.String())
			if err != nil {
				t.Fatal(err)
			}
			local, err := orasoci.NewWithContext(ctx, layout)
			if err != nil {
				t.Fatal(err)
			}
			assertBaleComponentContent(t, ctx, local, remote.Store, expected, map[digest.Digest]bool{})
			if dependencyRequests.Load() != 0 {
				t.Fatal("inspecting the declaration or unchanged component content contacted its dependency registry")
			}
		})
	}
}

func TestComponentDependencyPublicationOmitsEmptyDeclarations(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		t.Run(mode, func(t *testing.T) {
			_, _, resolver, name := testRepository(t, registryserver.New())
			meta, paths := baleComponentMetadata(t, "amd64", "no-additional-dependencies")
			var root v1.Descriptor
			var err error
			if mode == "direct" {
				root, err = resolver.PublishComponent(t.Context(), name+":empty", meta, paths)
			} else {
				layout := t.TempDir()
				root, err = WriteComponentLayout(t.Context(), layout, meta, paths)
				if err == nil {
					_, err = resolver.PublishLayout(t.Context(), name+":empty", layout, root)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			ref, err := ParseReference(name + ":empty")
			if err != nil {
				t.Fatal(err)
			}
			declarations, err := baleHTTPClient(t, ref.Registry).InspectDependencies(t.Context(), name+"@"+root.Digest.String())
			if err != nil || len(declarations) != 0 {
				t.Fatalf("component without additional dependencies gained a declaration: %+v, %v", declarations, err)
			}
		})
	}
}

func TestComponentDependencyPublicationExcludesBakedAndUnselectedProducers(t *testing.T) {
	_, _, resolver, name := testRepository(t, registryserver.New())
	meta, paths := baleComponentMetadata(t, "amd64", "selected")
	runtimeReference := name + "/runtime:stable"
	source := fmt.Sprintf(`package as="pkg" {
    component "unavailable.invalid/components/package-producer:stable"
}
extend as="selected" {
    copy "/src" "/dst" from="pkg"
    component %q
}
extend as="unselected" {
    component "unavailable.invalid/components/unselected:stable"
}
`, runtimeReference)
	def, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	publication, err := planner.Create(def, planner.Options{Mode: planner.Publish, Target: "selected", Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	meta.Component = *publication.Component
	root, err := resolver.PublishComponent(t.Context(), name+":selected", meta, paths)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := ParseReference(name + ":selected")
	if err != nil {
		t.Fatal(err)
	}
	declarations, err := baleHTTPClient(t, ref.Registry).InspectDependencies(t.Context(), name+"@"+root.Digest.String())
	if err != nil || len(declarations) != 1 || len(declarations[0].Declaration.Dependencies) != 1 || declarations[0].Declaration.Dependencies[0].Ref != runtimeReference {
		t.Fatalf("declaration must contain only the retained invocation dependency: %+v, %v", declarations, err)
	}
}

func assertBaleComponentContent(t *testing.T, ctx context.Context, source, destination content.ReadOnlyStorage, descriptor v1.Descriptor, visited map[digest.Digest]bool) {
	t.Helper()
	if visited[descriptor.Digest] {
		return
	}
	visited[descriptor.Digest] = true
	want, err := content.FetchAll(ctx, source, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	got, err := content.FetchAll(ctx, destination, descriptor)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("component content changed for %s: %v", descriptor.Digest, err)
	}
	children, err := content.Successors(ctx, source, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		assertBaleComponentContent(t, ctx, source, destination, child, visited)
	}
}

type baleComponentCapture struct {
	client              *bale.Client
	resolver            *Resolver
	name, bundle        string
	parent, child, leaf v1.Descriptor
	declarations        map[string]v1.Descriptor
	created             bale.CreateResult
	closeSource         func()
}

func captureBaleComponents(t *testing.T) baleComponentCapture {
	t.Helper()
	server, _, resolver, name := testRepository(t, registryserver.New())
	ref, err := ParseReference(name + ":fixture")
	if err != nil {
		t.Fatal(err)
	}
	client := baleHTTPClient(t, ref.Registry)
	publish := func(repository, value string, dependencies ...string) v1.Descriptor {
		meta, paths := baleComponentMetadata(t, "amd64", value, dependencies...)
		root, err := resolver.PublishComponent(t.Context(), repository+":stable", meta, paths)
		if err != nil {
			t.Fatal(err)
		}
		return root
	}
	leaf := publish(name+"/leaf", "shared-leaf")
	child := publish(name+"/child", "child-v1", name+"/leaf:stable")
	parent := publish(name+"/parent", "parent", name+"/child:stable", name+"/child:stable", name+"/leaf:stable")
	declarations := map[string]v1.Descriptor{}
	for repository, root := range map[string]v1.Descriptor{name + "/parent": parent, name + "/child": child} {
		attached, err := client.InspectDependencies(t.Context(), repository+"@"+root.Digest.String())
		if err != nil || len(attached) != 1 {
			t.Fatalf("component publication must expose one declaration before capture: %s: %+v, %v", repository, attached, err)
		}
		declarations[repository] = attached[0].Descriptor
	}
	bundle := name + "/bundles:snapshot"
	created, err := client.Create(t.Context(), bale.Recipe{Include: []bale.Selection{{Ref: name + "/parent:stable"}}}, bundle, bale.CreateOptions{})
	if err != nil || !created.Published {
		t.Fatalf("capture parent-only recipe: %+v, %v", created, err)
	}
	return baleComponentCapture{client: client, resolver: resolver, name: name, bundle: bundle, parent: parent, child: child, leaf: leaf, declarations: declarations, created: created, closeSource: server.Close}
}

func capturedComponentDependency(t *testing.T, report bale.InspectResult, repository, selector string) digest.Digest {
	t.Helper()
	for _, resolution := range report.Metadata.Dependencies {
		if resolution.Repository != repository || resolution.Outcome != "selected" {
			continue
		}
		for _, dependency := range resolution.Dependencies {
			if dependency.Ref == selector {
				return dependency.Descriptor.Digest
			}
		}
	}
	t.Fatalf("capture has no selected dependency %q for %q", selector, repository)
	return ""
}

func TestBaleComponentCaptureRecursesAndDeduplicates(t *testing.T) {
	fixture := captureBaleComponents(t)
	if len(fixture.created.Metadata.Roots) != 1 || fixture.created.Metadata.Roots[0].Descriptor.Digest != fixture.parent.Digest {
		t.Fatalf("recipe roots = %+v; want only the selected parent", fixture.created.Metadata.Roots)
	}
	for repository, root := range map[string]v1.Descriptor{fixture.name + "/parent": fixture.parent, fixture.name + "/child": fixture.child, fixture.name + "/leaf": fixture.leaf} {
		count := 0
		for _, origin := range fixture.created.Metadata.Origins {
			if origin.Repository == repository && origin.Descriptor.Digest == root.Digest {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("captured %s root %d times, want one", repository, count)
		}
	}
	selected := map[string][]string{}
	for _, resolution := range fixture.created.Metadata.Dependencies {
		if resolution.Outcome == "selected" {
			for _, dependency := range resolution.Dependencies {
				selected[resolution.Repository] = append(selected[resolution.Repository], dependency.Ref)
			}
		}
	}
	for repository, want := range map[string][]string{fixture.name + "/parent": {fixture.name + "/child:stable", fixture.name + "/leaf:stable"}, fixture.name + "/child": {fixture.name + "/leaf:stable"}} {
		got := selected[repository]
		slices.Sort(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("direct dependencies of %s = %v, want %v", repository, got, want)
		}
	}
}

func TestBaleComponentCaptureLockedAndLatest(t *testing.T) {
	fixture := captureBaleComponents(t)
	meta, paths := baleComponentMetadata(t, "amd64", "child-v2", fixture.name+"/leaf:stable")
	replacement, err := fixture.resolver.PublishComponent(t.Context(), fixture.name+"/child:stable", meta, paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []bale.PullMode{bale.PullLocked, bale.PullLatest} {
		t.Run(string(mode), func(t *testing.T) {
			layout := filepath.Join(t.TempDir(), "capture")
			pulled, err := fixture.client.Pull(t.Context(), fixture.bundle, layout, mode)
			if err != nil {
				t.Fatal(err)
			}
			report, err := fixture.client.InspectLayout(t.Context(), layout)
			if err != nil {
				t.Fatal(err)
			}
			want := fixture.child.Digest
			if mode == bale.PullLatest {
				want = replacement.Digest
				if pulled.Descriptor.Digest == fixture.created.Descriptor.Digest {
					t.Fatal("latest pull did not refresh the changed dependency")
				}
			} else if pulled.Descriptor.Digest != fixture.created.Descriptor.Digest {
				t.Fatal("locked pull changed the captured bundle")
			}
			if got := capturedComponentDependency(t, report, fixture.name+"/parent", fixture.name+"/child:stable"); got != want {
				t.Fatalf("%s child digest = %s, want %s", mode, got, want)
			}
		})
	}
}

func TestBaleComponentCaptureTransfersDeclarationsAndDestinations(t *testing.T) {
	fixture := captureBaleComponents(t)
	layout := filepath.Join(t.TempDir(), "locked")
	if _, err := fixture.client.Pull(t.Context(), fixture.bundle, layout, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	_, _, _, destinationName := testRepository(t, registryserver.New())
	destinationRef, err := ParseReference(destinationName + ":complete")
	if err != nil {
		t.Fatal(err)
	}
	var unexpectedRequests atomic.Int64
	client, err := bale.NewClient(bale.ClientOptions{
		PlainHTTPRegistries: []string{destinationRef.Registry},
		HTTPClient: &http.Client{Transport: baleCaptureRoundTripper(func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != destinationRef.Registry {
				unexpectedRequests.Add(1)
				return nil, fmt.Errorf("delivery attempted non-destination request to %s", request.URL.Host)
			}
			return http.DefaultTransport.RoundTrip(request)
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	placements := map[string]string{fixture.name + "/parent": "parent", fixture.name + "/child": "child", fixture.name + "/leaf": "leaf"}
	plan, err := client.PlanPush(t.Context(), layout, destinationName+":complete", bale.PushOptions{RepositoryMap: placements})
	if err != nil {
		t.Fatal(err)
	}
	fixture.closeSource()
	delivered, err := client.Push(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, attachment := range delivered.Destinations {
		if !attachment.Published || !attachment.Discoverable {
			t.Fatalf("destination record was not confirmed: %+v", attachment)
		}
	}
	store, err := orasoci.NewWithContext(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	for repository, root := range map[string]v1.Descriptor{fixture.name + "/parent": fixture.parent, fixture.name + "/child": fixture.child, fixture.name + "/leaf": fixture.leaf} {
		location, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: repository, Digest: root.Digest})
		if !ok || location.Destination.Digest != root.Digest {
			t.Fatalf("missing unchanged relocation of %s: %+v", repository, delivered.Relocations)
		}
		pinned := location.Destination.Repository + "@" + root.Digest.String()
		remote, err := client.Resolve(t.Context(), pinned)
		if err != nil {
			t.Fatal(err)
		}
		assertBaleComponentContent(t, t.Context(), store, remote.Store, root, map[digest.Digest]bool{})
		records, err := client.InspectDestinations(t.Context(), pinned)
		if err != nil || len(records) != 1 || !records[0].Local || records[0].Record.Source != repository+"@"+root.Digest.String() || records[0].Record.Destination != pinned {
			t.Fatalf("destination mapping for %s = %+v, %v", repository, records, err)
		}
		if records[0].Record.Bundle != destinationName+"@"+fixture.created.Descriptor.Digest.String() {
			t.Fatalf("destination record names the wrong bundle: %+v", records[0].Record)
		}
		if repository == fixture.name+"/parent" {
			mapped := map[string]string{}
			for _, resolution := range records[0].Record.Resolutions {
				if resolution.Outcome != "selected" || resolution.Capture != fixture.created.Descriptor.Digest || resolution.Declaration == nil {
					continue
				}
				for _, dependency := range resolution.Dependencies {
					mapped[dependency.Ref] = dependency.Destination
					_, deliveredDigest, pinned := strings.Cut(dependency.Destination, "@")
					if !pinned || dependency.Source != strings.TrimSuffix(dependency.Ref, ":stable")+"@"+deliveredDigest {
						t.Fatalf("direct dependency source/destination identity changed: %+v", dependency)
					}
				}
			}
			if len(mapped) != 2 {
				t.Fatalf("parent destination record has %d direct dependencies, want two", len(mapped))
			}
			for ref, wanted := range map[string]v1.Descriptor{fixture.name + "/child:stable": fixture.child, fixture.name + "/leaf:stable": fixture.leaf} {
				location, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: strings.TrimSuffix(ref, ":stable"), Digest: wanted.Digest})
				if !ok || mapped[ref] != location.Destination.Repository+"@"+wanted.Digest.String() {
					t.Fatalf("direct dependency placement for %s = %q, relocation %+v", ref, mapped[ref], location)
				}
			}
		}
		declarations, err := client.InspectDependencies(t.Context(), pinned)
		if err != nil {
			t.Fatal(err)
		}
		if repository != fixture.name+"/leaf" && len(declarations) != 1 {
			t.Fatalf("transferred dependency declaration for %s = %+v", repository, declarations)
		}
		if original, ok := fixture.declarations[repository]; ok && declarations[0].Descriptor.Digest != original.Digest {
			t.Fatalf("transfer changed dependency attachment for %s", repository)
		}
	}
	if unexpectedRequests.Load() != 0 {
		t.Fatal("delivery or destination inspection attempted source/provenance access")
	}
}

func TestImagePublicationDoesNotAddComponentDependencies(t *testing.T) {
	_, _, resolver, name := testRepository(t, registryserver.New())
	archive, expected, _ := imageArchive(t, false)
	root, err := resolver.PublishImageArchive(t.Context(), name+":image", archive)
	if err != nil || !content.Equal(root, expected) {
		t.Fatalf("publish image = %+v, %v", root, err)
	}
	ref, err := ParseReference(name + ":image")
	if err != nil {
		t.Fatal(err)
	}
	declarations, err := baleHTTPClient(t, ref.Registry).InspectDependencies(t.Context(), name+"@"+root.Digest.String())
	if err != nil || len(declarations) != 0 {
		t.Fatalf("image gained component dependency declarations: %+v, %v", declarations, err)
	}
}
