package build

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

func TestAllPlatformsDiscoversLocalBasesAndIntersectsPlatforms(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	ctx := context.Background()
	workspace := dockerEngineWorkspace(t)
	storeDir := filepath.Join(workspace, "images")
	base := filepath.Join(workspace, "base.coopr")
	consumer := filepath.Join(workspace, "consumer.coopr")
	if err := os.WriteFile(base, []byte("from \"scratch\"\nlabel purpose=\"discovery\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, Options{File: base, BuildStore: nativeBuildTestStore(storeDir), Tag: "base", Platforms: []string{"linux/amd64", "linux/arm64"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(consumer, []byte("arg \"base\"\nfrom \"$base\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := Options{File: consumer, BuildStore: nativeBuildTestStore(storeDir), Tag: "discovered", AllPlatforms: true, PullPolicy: "never", Args: map[string]string{"base": "base"}}
	if _, err := Run(ctx, opts); err != nil {
		t.Fatal(err)
	}
	name, err := localstore.NormalizeImageTag("discovered")
	if err != nil {
		t.Fatal(err)
	}
	_, _, selections, found, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), name)
	if err != nil || !found || len(selections) != 2 {
		t.Fatalf("discovered index found=%v platforms=%v err=%v", found, selections, err)
	}
	if _, err := Run(ctx, Options{File: base, BuildStore: nativeBuildTestStore(storeDir), Tag: "single", Platform: "linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(consumer, []byte("from \"base\" as=\"multi\"\nfrom \"single\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts.Tag = "intersection"
	if _, err := Run(ctx, opts); err != nil {
		t.Fatal(err)
	}
	name, err = localstore.NormalizeImageTag("intersection")
	if err != nil {
		t.Fatal(err)
	}
	_, platform, found, err := testStoredSoleImage(ctx, nativeBuildTestStore(storeDir), name)
	if err != nil || !found || platform.Architecture != "amd64" {
		t.Fatalf("common platform=%+v found=%v err=%v", platform, found, err)
	}
	if _, err := Run(ctx, Options{File: base, BuildStore: nativeBuildTestStore(storeDir), AllPlatforms: true}); err == nil || !strings.Contains(err.Error(), "non-scratch") {
		t.Fatalf("scratch-only discovery error=%v", err)
	}
}

func TestManifestAppendsConcurrentPlatformsAndReplacesExistingPlatform(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	ctx := context.Background()
	workspace := dockerEngineWorkspace(t)
	storeDir := filepath.Join(workspace, "images")
	definition := filepath.Join(workspace, "image.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\"\narg \"revision\"\nlabel revision=\"$revision\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Run(ctx, Options{File: definition, BuildStore: nativeBuildTestStore(storeDir), Manifest: map[string]string{"linux/amd64": "combined", "linux/arm64": "localhost/combined:latest"}[platform], Platform: platform, Args: map[string]string{"revision": "first"}})
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	name, err := localstore.NormalizeImageTag("combined")
	if err != nil {
		t.Fatal(err)
	}
	_, _, previous, found, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), name)
	if err != nil || !found || len(previous) != 2 {
		t.Fatalf("concurrent manifest found=%v platforms=%v err=%v", found, previous, err)
	}
	iid := filepath.Join(workspace, "iid")
	rebuilt, err := Run(ctx, Options{File: definition, BuildStore: nativeBuildTestStore(storeDir), Manifest: "combined", IIDFile: iid, Platform: "linux/amd64", Args: map[string]string{"revision": "replacement"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := buildah.WithStore(nativeBuildTestStore(storeDir), func(store storage.Store) error {
		record, err := store.Image("localhost/" + name)
		if err != nil {
			return err
		}
		if rebuilt.ImageID != record.ID {
			t.Fatalf("append returned child/root digest instead of native list ID: %+v native=%s", rebuilt, record.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(iid); err != nil || string(data) != "sha256:"+rebuilt.ImageID {
		t.Fatalf("appended iid=%s err=%v", data, err)
	}
	_, _, replaced, found, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), name)
	if err != nil || !found || len(replaced) != 2 {
		t.Fatalf("replacement manifest found=%v platforms=%v err=%v", found, replaced, err)
	}
	if previous["linux/amd64"].Manifest.Digest == replaced["linux/amd64"].Manifest.Digest || previous["linux/arm64"].Manifest.Digest != replaced["linux/arm64"].Manifest.Digest {
		t.Fatal("replacement changed the wrong manifest instances")
	}
	consumer := filepath.Join(workspace, "consumer.coopr")
	if err := os.WriteFile(consumer, []byte("from \"combined\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, Options{File: consumer, BuildStore: nativeBuildTestStore(storeDir), AllPlatforms: true, PullPolicy: "never", Tag: "consumed"}); err != nil {
		t.Fatalf("consume appended manifest offline: %v", err)
	}
}

func TestDuplicatePlatformIndexDiscoveryAndManifestAppend(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	ctx := context.Background()
	workspace := dockerEngineWorkspace(t)
	store := nativeBuildTestStore(filepath.Join(workspace, "images"))
	definition := filepath.Join(workspace, "base.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\"\narg \"revision\"\nlabel revision=\"$revision\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	variants := []oci.ImageVariant{}
	ids := map[digest.Digest]string{}
	for _, revision := range []string{"first", "second"} {
		result, err := Run(ctx, Options{File: definition, BuildStore: store, Tag: revision, Platform: "linux/amd64", Args: map[string]string{"revision": revision}})
		if err != nil {
			t.Fatal(err)
		}
		if err := buildah.WithStore(store, func(backend storage.Store) error {
			selected, err := oci.ResolveStoredImage(ctx, backend, result.ImageID, v1.Platform{OS: "linux", Architecture: "amd64"})
			if err != nil {
				return err
			}
			variants = append(variants, oci.ImageVariant{Manifest: selected.Selected, Platform: selected.Platform})
			ids[selected.Selected.Digest] = selected.StorageImageID
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	variants[1].Manifest.Annotations = map[string]string{"example.com/member": "retained"}
	variants[1].Platform.OSVersion = "1"
	variants[1].Platform.OSFeatures = []string{"feature"}

	root, data, err := oci.RetainedImageIndexDescriptor(variants, "oci")
	if err != nil {
		t.Fatal(err)
	}
	var annotated v1.Index
	if err := json.Unmarshal(data, &annotated); err != nil {
		t.Fatal(err)
	}
	annotated.Annotations = map[string]string{"example.com/retain": "yes"}
	data, err = json.Marshal(annotated)
	if err != nil {
		t.Fatal(err)
	}
	root = oci.Descriptor(v1.MediaTypeImageIndex, data)
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		_, err := imagestore.FromStore(backend).WriteStoredIndex(ctx, root, data, ids, "duplicate")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	consumer := filepath.Join(workspace, "consumer.coopr")
	if err := os.WriteFile(consumer, []byte("from \"duplicate\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, Options{File: consumer, BuildStore: store, Tag: "consumed", AllPlatforms: true, PullPolicy: "never"}); err != nil {
		t.Fatalf("discover duplicate platforms: %v", err)
	}
	if _, err := Run(ctx, Options{File: definition, BuildStore: store, Manifest: "duplicate", Platform: "linux/arm64", Args: map[string]string{"revision": "foreign"}}); err != nil {
		t.Fatalf("append unrelated platform: %v", err)
	}
	_, afterData, after, found, err := testStoredImageIndex(ctx, store, "localhost/duplicate:latest")
	if err != nil || !found || len(after) != 3 {
		t.Fatalf("append selections=%v found=%v error=%v", after, found, err)
	}
	var retained v1.Index
	if err := json.Unmarshal(afterData, &retained); err != nil || retained.Annotations["example.com/retain"] != "yes" {
		t.Fatalf("append lost index annotations: %s error=%v", afterData, err)
	}

	annotatedMember := false
	for _, child := range retained.Manifests {
		if child.Annotations["example.com/member"] == "retained" {
			annotatedMember = true
			if child.Platform == nil || child.Platform.OSVersion != "1" || len(child.Platform.OSFeatures) != 1 || child.Platform.OSFeatures[0] != "feature" {
				t.Fatalf("append lost member platform metadata: %+v", child)
			}
		}
	}
	if !annotatedMember {
		t.Fatalf("append lost member annotation: %s", afterData)
	}

	for original := range ids {
		present := false
		for _, selection := range after {
			present = present || selection.Manifest.Digest == original
		}
		if !present {
			t.Fatalf("append discarded original member %s", original)
		}
	}
}
