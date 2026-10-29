package componentstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/localstore"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

func deliveryGraph(t *testing.T, ctx context.Context, store *orasoci.Store, root v1.Descriptor, repository, marker string) *Binding {
	t.Helper()
	configData := []byte(`{}`)
	config := descriptor(v1.MediaTypeEmptyJSON, configData)
	if err := store.Push(ctx, config, bytes.NewReader(configData)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		t.Fatal(err)
	}
	payload := []byte(marker)
	layer := descriptor("application/vnd.test.delivery", payload)
	if err := store.Push(ctx, layer, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/vnd.test.delivery", Subject: &root, Config: config, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	attachment := descriptor(v1.MediaTypeImageManifest, data)
	if err := store.Push(ctx, attachment, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	return &Binding{Repository: repository, Attachments: []v1.Descriptor{attachment}}
}

func TestComponentBindingPreservesNameAndQualifiedDigest(t *testing.T) {
	ctx := context.Background()
	source, root, _ := testGraph(t, ctx, "same")
	first := deliveryGraph(t, ctx, source, root, "first.example/team/component", "first")
	second := deliveryGraph(t, ctx, source, root, "second.example/team/component", "second")
	dir := filepath.Join(t.TempDir(), "store")
	if err := PutWithBinding(ctx, dir, source, root, "first", first); err != nil {
		t.Fatal(err)
	}
	if err := PutWithBinding(ctx, dir, source, root, "second", second); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ reference, repository string }{{"first", first.Repository}, {"second", second.Repository}, {first.Repository + "@" + root.Digest.String(), first.Repository}, {second.Repository + "@" + root.Digest.String(), second.Repository}} {
		_, got, binding, err := Resolve(ctx, dir, item.reference)
		if err != nil || got.Digest != root.Digest || binding == nil || binding.Repository != item.repository {
			t.Fatalf("Resolve(%q): root=%s binding=%+v err=%v", item.reference, got.Digest, binding, err)
		}
	}
	_, _, aliasBinding, err := Resolve(ctx, dir, "first@"+root.Digest.String())
	if err != nil || aliasBinding == nil || aliasBinding.Repository != first.Repository {
		t.Fatalf("local alias-qualified digest lost its context: %+v %v", aliasBinding, err)
	}
	if _, _, _, err := Resolve(ctx, dir, root.Digest.String()); err == nil || !strings.Contains(err.Error(), "multiple delivery contexts") {
		t.Fatalf("bare digest ambiguity: %v", err)
	}
	newSource, newRoot, _ := testGraph(t, ctx, "new")
	if err := Put(ctx, dir, newSource, newRoot, "first"); err != nil {
		t.Fatal(err)
	}
	_, got, binding, err := Resolve(ctx, dir, first.Repository+"@"+root.Digest.String())
	if err != nil || got.Digest != root.Digest || binding == nil || binding.Repository != first.Repository {
		t.Fatalf("qualified old digest lost after alias moves: %+v %+v %v", got, binding, err)
	}
	if _, _, _, err := Resolve(ctx, dir, "missing.example/team/component@"+root.Digest.String()); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("foreign qualified digest selected a context: %v", err)
	}
}

func TestComponentBindingArchiveAndPrune(t *testing.T) {
	ctx := context.Background()
	source, root, _ := testGraph(t, ctx, "archivebinding")
	binding := deliveryGraph(t, ctx, source, root, "delivery.example/team/component", "delivery")
	dir := filepath.Join(t.TempDir(), "store")
	if err := PutWithBinding(ctx, dir, source, root, "demo", binding); err != nil {
		t.Fatal(err)
	}
	entries, err := localstore.List(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Descriptor.Digest == binding.Attachments[0].Digest {
			t.Fatal("attachment leaked into component list")
		}
	}
	if _, err := localstore.Prune(ctx, dir, false); err != nil {
		t.Fatal(err)
	}
	store, bound, gotBinding, err := Resolve(ctx, dir, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if gotBinding == nil || gotBinding.Repository != binding.Repository {
		t.Fatalf("binding after prune: %+v", gotBinding)
	}
	if _, err := content.FetchAll(ctx, store, binding.Attachments[0]); err != nil {
		t.Fatalf("prune lost retained attachment: %v", err)
	}
	path := filepath.Join(t.TempDir(), "component.tar")
	if err := WriteArchive(ctx, store, bound, path); err != nil {
		t.Fatal(err)
	}
	roots, err := ArchiveRoots(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 {
		t.Fatalf("archive has %d roots, want component+attachment", len(roots))
	}
	for _, descriptor := range roots {
		if descriptor.Annotations[bindingAnnotation] != "" || descriptor.Annotations[bindingHistoryAnnotation] != "" || descriptor.Annotations[localstore.AuxiliaryRootOwner] != "" {
			t.Fatalf("private metadata exported: %+v", descriptor.Annotations)
		}
	}
	archived, err := orasoci.NewFromTar(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	archivedRoot, archivedBinding, err := ArchiveRoot(ctx, archived, roots)
	if err != nil {
		t.Fatal(err)
	}
	if archivedRoot.Digest != root.Digest || bindingKey(archivedBinding) != bindingKey(binding) {
		t.Fatalf("archive context changed: %+v %+v", archivedRoot, archivedBinding)
	}
	loadedDir := filepath.Join(t.TempDir(), "loaded")
	if err := PutWithBinding(ctx, loadedDir, archived, archivedRoot, "loaded", archivedBinding); err != nil {
		t.Fatal(err)
	}
	_, _, loaded, err := Resolve(ctx, loadedDir, "loaded")
	if err != nil || bindingKey(loaded) != bindingKey(binding) {
		t.Fatalf("restored binding %+v %v", loaded, err)
	}
	if _, err := localstore.RemoveTag(ctx, dir, "demo:latest"); err != nil {
		t.Fatal(err)
	}
	if _, err := localstore.Prune(ctx, dir, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Resolve(ctx, dir, "demo"); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("removed component still resolves: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", binding.Attachments[0].Digest.Algorithm().String(), binding.Attachments[0].Digest.Encoded())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan delivery attachment remains after prune: %v", err)
	}
}

type missingGraph struct {
	content.ReadOnlyStorage
	missing digest.Digest
}

func (s missingGraph) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	if descriptor.Digest == s.missing {
		return nil, errors.New("injected missing attachment")
	}
	return s.ReadOnlyStorage.Fetch(ctx, descriptor)
}

func TestComponentBindingFailureDoesNotMoveName(t *testing.T) {
	ctx := context.Background()
	source, root, _ := testGraph(t, ctx, "first")
	dir := filepath.Join(t.TempDir(), "store")
	if err := Put(ctx, dir, source, root, "stable"); err != nil {
		t.Fatal(err)
	}
	secondSource, secondRoot, _ := testGraph(t, ctx, "second")
	binding := deliveryGraph(t, ctx, secondSource, secondRoot, "delivery.example/team/component", "failed")
	broken := missingGraph{ReadOnlyStorage: secondSource, missing: binding.Attachments[0].Digest}
	if err := PutWithBinding(ctx, dir, broken, secondRoot, "stable", binding); err == nil {
		t.Fatal("missing attachment accepted")
	}
	_, got, gotBinding, err := Resolve(ctx, dir, "stable")
	if err != nil || got.Digest != root.Digest || gotBinding != nil {
		t.Fatalf("failed write moved name/context: %+v %+v %v", got, gotBinding, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := PutWithBinding(cancelled, dir, secondSource, secondRoot, "stable", binding); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write: %v", err)
	}
	_, got, _, err = Resolve(ctx, dir, "stable")
	if err != nil || got.Digest != root.Digest {
		t.Fatalf("cancelled write moved name: %+v %v", got, err)
	}
}

func TestComponentBindingMultiPlatformGraphSurvivesPrune(t *testing.T) {
	ctx := context.Background()
	source, child, _ := testGraph(t, ctx, "platform")
	child.Platform = &v1.Platform{OS: "linux", Architecture: "amd64"}
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{child}})
	if err != nil {
		t.Fatal(err)
	}
	root := descriptor(v1.MediaTypeImageIndex, data)
	if err := source.Push(ctx, root, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	binding := deliveryGraph(t, ctx, source, root, "delivery.example/team/index", "indexdelivery")
	dir := filepath.Join(t.TempDir(), "store")
	if err := PutWithBinding(ctx, dir, source, root, "multi", binding); err != nil {
		t.Fatal(err)
	}
	if _, err := localstore.Prune(ctx, dir, false); err != nil {
		t.Fatal(err)
	}
	store, got, _, err := Resolve(ctx, dir, "multi")
	if err != nil || got.Digest != root.Digest {
		t.Fatalf("index resolution after prune: %+v %v", got, err)
	}
	if _, err := content.FetchAll(ctx, store, child); err != nil {
		t.Fatalf("prune removed indexed component platform: %v", err)
	}
	if _, err := content.FetchAll(ctx, store, binding.Attachments[0]); err != nil {
		t.Fatalf("prune removed index delivery record: %v", err)
	}
}

func TestComponentResolveDoesNotInitializeAbsentStore(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "absent")
	if _, _, _, err := Resolve(ctx, dir, "missing"); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("absent store: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lookup initialized absent store: %v", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Resolve(ctx, dir, "missing"); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("empty store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "resident"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Resolve(ctx, dir, "missing"); err == nil || errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("corrupt store reported as cache miss: %v", err)
	}
}

func TestQualifiedDigestPrefersDeliveryOverUnboundHistory(t *testing.T) {
	ctx := context.Background()
	source, root, _ := testGraph(t, ctx, "unbound-and-delivered")
	dir := filepath.Join(t.TempDir(), "store")
	if err := Put(ctx, dir, source, root, "built"); err != nil {
		t.Fatal(err)
	}
	qualified := "delivery.example/team/component@" + root.Digest.String()
	if _, _, binding, err := Resolve(ctx, dir, qualified); err != nil || binding != nil {
		t.Fatalf("unbound CAS lookup changed: binding=%+v err=%v", binding, err)
	}
	first := deliveryGraph(t, ctx, source, root, "delivery.example/team/component", "first-delivery")
	if err := PutWithBinding(ctx, dir, source, root, "delivered", first); err != nil {
		t.Fatal(err)
	}
	_, selected, binding, err := Resolve(ctx, dir, qualified)
	if err != nil || selected.Digest != root.Digest || bindingKey(binding) != bindingKey(first) {
		t.Fatalf("qualified digest did not select exact delivery: binding=%+v err=%v", binding, err)
	}
	if _, _, _, err := Resolve(ctx, dir, root.Digest.String()); err == nil || !strings.Contains(err.Error(), "multiple delivery contexts") {
		t.Fatalf("bare digest should retain unbound/delivered ambiguity: %v", err)
	}
	if _, _, _, err := Resolve(ctx, dir, "foreign.example/team/component@"+root.Digest.String()); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("unbound history satisfied unknown foreign repository: %v", err)
	}
	second := deliveryGraph(t, ctx, source, root, "delivery.example/team/component", "second-delivery")
	if err := PutWithBinding(ctx, dir, source, root, "another-delivery", second); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Resolve(ctx, dir, qualified); err == nil || !strings.Contains(err.Error(), "multiple delivery contexts") {
		t.Fatalf("qualified digest concealed distinct deliveries in same repository: %v", err)
	}
	if err := PutWithBinding(ctx, dir, source, root, first.Repository+":latest", first); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Resolve(ctx, dir, qualified); err == nil || !strings.Contains(err.Error(), "multiple delivery contexts") {
		t.Fatalf("delivery repository silently selected implicit latest: %v", err)
	}
	if _, _, selected, err := Resolve(ctx, dir, first.Repository+":latest@"+root.Digest.String()); err != nil || bindingKey(selected) != bindingKey(first) {
		t.Fatalf("explicit tagged name did not select its delivery context: %+v %v", selected, err)
	}
}

func TestResolveWithBindingSelectorSelectsFrozenContext(t *testing.T) {
	ctx := context.Background()
	source, root, _ := testGraph(t, ctx, "filtered-deliveries")
	first := deliveryGraph(t, ctx, source, root, "delivery.example/team/component", "first-capture")
	second := deliveryGraph(t, ctx, source, root, "delivery.example/team/component", "second-capture")
	dir := filepath.Join(t.TempDir(), "store")
	if err := Put(ctx, dir, source, root, "unbound"); err != nil {
		t.Fatal(err)
	}
	if err := PutWithBinding(ctx, dir, source, root, "first", first); err != nil {
		t.Fatal(err)
	}
	if err := PutWithBinding(ctx, dir, source, root, "second", second); err != nil {
		t.Fatal(err)
	}
	qualified := first.Repository + "@" + root.Digest.String()
	if _, _, _, err := Resolve(ctx, dir, qualified); err == nil || !strings.Contains(err.Error(), "multiple delivery contexts") {
		t.Fatalf("unscoped delivery should be ambiguous: %v", err)
	}
	selectFirst := func(descriptor v1.Descriptor, binding *Binding) (string, error) {
		if descriptor.Digest != root.Digest {
			t.Fatalf("filter received wrong root: %s", descriptor.Digest)
		}
		if bindingKey(binding) == bindingKey(first) {
			return "first-context", nil
		}
		return "", nil
	}
	_, selected, binding, err := ResolveWithBindingSelector(ctx, dir, qualified, selectFirst)
	if err != nil || selected.Digest != root.Digest || bindingKey(binding) != bindingKey(first) {
		t.Fatalf("frozen selection failed: %+v %+v %v", selected, binding, err)
	}
	if _, _, _, err := ResolveWithBindingSelector(ctx, dir, "second", selectFirst); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("named lookup ignored filter: %v", err)
	}
	if _, _, _, err := ResolveWithBindingSelector(ctx, dir, "second@"+root.Digest.String(), selectFirst); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("qualified alias lookup ignored filter: %v", err)
	}
	if _, _, _, err := ResolveWithBindingSelector(ctx, dir, qualified, func(_ v1.Descriptor, binding *Binding) (string, error) {
		if binding == nil {
			return "unbound", nil
		}
		return "", nil
	}); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("filter exposed unbound fallback despite delivery history: %v", err)
	}
	sentinel := errors.New("invalid retained delivery")
	if _, _, _, err := ResolveWithBindingSelector(ctx, dir, qualified, func(v1.Descriptor, *Binding) (string, error) { return "", sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("filter error lost identity: %v", err)
	}
}

func TestPrunePreservesAuxiliaryReferencedByAnotherLiveGraph(t *testing.T) {
	ctx := context.Background()
	source, root, _ := testGraph(t, ctx, "shared-attachment")
	binding := deliveryGraph(t, ctx, source, root, "delivery.example/team/component", "shared")
	dir := filepath.Join(t.TempDir(), "store")
	if err := PutWithBinding(ctx, dir, source, root, "owner", binding); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: binding.Attachments})
	if err != nil {
		t.Fatal(err)
	}
	parent := descriptor(v1.MediaTypeImageIndex, data)
	if err := source.Push(ctx, parent, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := Put(ctx, dir, source, parent, "live-parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := localstore.RemoveTag(ctx, dir, "owner:latest"); err != nil {
		t.Fatal(err)
	}
	if _, err := localstore.Prune(ctx, dir, false); err != nil {
		t.Fatal(err)
	}
	store, _, _, err := Resolve(ctx, dir, "live-parent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := content.FetchAll(ctx, store, binding.Attachments[0]); err != nil {
		t.Fatalf("prune deleted reachable auxiliary manifest: %v", err)
	}
}

func TestBindingSelectorDeduplicatesOverlappingSnapshots(t *testing.T) {
	ctx := context.Background()
	source, root, _ := testGraph(t, ctx, "overlapping-bindings")
	first := deliveryGraph(t, ctx, source, root, "delivery.example/team/component", "original-record")
	second := deliveryGraph(t, ctx, source, root, "delivery.example/team/component", "another-record")
	overlapping := &Binding{Repository: first.Repository, Attachments: append(append([]v1.Descriptor{}, first.Attachments...), second.Attachments...)}
	dir := filepath.Join(t.TempDir(), "store")
	if err := PutWithBinding(ctx, dir, source, root, "original", first); err != nil {
		t.Fatal(err)
	}
	if err := PutWithBinding(ctx, dir, source, root, "updated", overlapping); err != nil {
		t.Fatal(err)
	}
	qualified := first.Repository + "@" + root.Digest.String()
	if _, _, _, err := Resolve(ctx, dir, qualified); err == nil || !strings.Contains(err.Error(), "multiple delivery contexts") {
		t.Fatalf("unscoped snapshots should remain ambiguous: %v", err)
	}
	selectOriginal := func(_ v1.Descriptor, binding *Binding) (string, error) {
		if binding == nil {
			return "", nil
		}
		for _, attachment := range binding.Attachments {
			if attachment.Digest == first.Attachments[0].Digest {
				return first.Attachments[0].Digest.String(), nil
			}
		}
		return "", nil
	}
	_, selected, binding, err := ResolveWithBindingSelector(ctx, dir, qualified, selectOriginal)
	if err != nil || selected.Digest != root.Digest || binding == nil {
		t.Fatalf("same selected context in overlapping snapshots remained ambiguous: %+v %+v %v", selected, binding, err)
	}
	if _, _, _, err := ResolveWithBindingSelector(ctx, dir, qualified, func(_ v1.Descriptor, binding *Binding) (string, error) { return bindingKey(binding), nil }); err == nil || !strings.Contains(err.Error(), "multiple delivery contexts") {
		t.Fatalf("distinct selector keys did not retain ambiguity: %v", err)
	}
}

func TestImmutableLookupKeepsOnlyExplicitlySelectedArchiveName(t *testing.T) {
	ctx := context.Background()
	source, root, _ := testGraph(t, ctx, "archive-name")
	binding := deliveryGraph(t, ctx, source, root, "delivery.example/team/component", "archive-name")
	dir := filepath.Join(t.TempDir(), "store")
	for _, tag := range []string{"selected:v1", "other:v1"} {
		if err := PutWithBinding(ctx, dir, source, root, tag, binding); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the unrelated name last to expose accidental dependence on index order.
	index, err := readIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ordered []v1.Descriptor
	for _, entry := range index.Manifests {
		if entry.Annotations[v1.AnnotationRefName] != "other:v1" {
			ordered = append(ordered, entry)
		}
	}
	for _, entry := range index.Manifests {
		if entry.Annotations[v1.AnnotationRefName] == "other:v1" {
			ordered = append(ordered, entry)
		}
	}
	index.Manifests = ordered
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, v1.ImageIndexFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	store, selected, _, err := Resolve(ctx, dir, "selected:v1@"+root.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	if name := selected.Annotations[v1.AnnotationRefName]; name != "selected:v1" {
		t.Fatalf("explicit name replaced by unrelated index entry: %q", name)
	}
	path := filepath.Join(t.TempDir(), "selected.tar")
	if err := WriteArchive(ctx, store, selected, path); err != nil {
		t.Fatal(err)
	}
	roots, err := ArchiveRoots(path)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := orasoci.NewFromTar(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	archivedRoot, archivedBinding, err := ArchiveRoot(ctx, archive, roots)
	if err != nil {
		t.Fatal(err)
	}
	defaultName := archivedRoot.Annotations[v1.AnnotationRefName]
	if defaultName != "selected:v1" {
		t.Fatalf("archive default load name changed: %q", defaultName)
	}
	loaded := filepath.Join(t.TempDir(), "loaded")
	if err := PutWithBinding(ctx, loaded, archive, archivedRoot, defaultName, archivedBinding); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Resolve(ctx, loaded, "selected:v1"); err != nil {
		t.Fatalf("archive default name did not survive load: %v", err)
	}
	if _, _, _, err := Resolve(ctx, loaded, "other:v1"); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("archive inherited unrelated name: %v", err)
	}
	for _, reference := range []string{binding.Repository + "@" + root.Digest.String(), root.Digest.String()} {
		_, immutable, _, err := Resolve(ctx, dir, reference)
		if err != nil {
			t.Fatal(err)
		}
		if name := immutable.Annotations[v1.AnnotationRefName]; name != "" {
			t.Fatalf("immutable reference %q inherited arbitrary name %q", reference, name)
		}
	}
}
