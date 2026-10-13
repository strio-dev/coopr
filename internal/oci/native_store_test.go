package oci

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

func TestStoredNamedDigestMissMatchesImageUnknown(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{GraphRoot: filepath.Join(dir, "graph"), RunRoot: filepath.Join(dir, "run"), GraphDriverName: "vfs"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.Shutdown(false); store.Free() })
	name := "registry.example/coopr/base@" + digest.FromString("missing manifest").String()
	_, err = ResolveStoredImage(context.Background(), store, name, v1.Platform{OS: "linux", Architecture: "amd64"})
	if !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("missing named digest = %v, want storage.ErrImageUnknown", err)
	}
}

func TestStoredIndexDigestUsesVerifiedDefaultWhenDigestKeyIsAbsent(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{GraphRoot: filepath.Join(dir, "graph"), RunRoot: filepath.Join(dir, "run"), GraphDriverName: "vfs"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.Shutdown(false); store.Free() })
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex})
	if err != nil {
		t.Fatal(err)
	}
	root := Descriptor(v1.MediaTypeImageIndex, raw)
	_, err = store.CreateImage(root.Digest.Encoded(), []string{"localhost/native-list:latest"}, "", "", &storage.ImageOptions{BigData: []storage.ImageBigDataOption{{Key: storage.ImageDigestBigDataKey, Data: raw, Digest: root.Digest}}})
	if err != nil {
		t.Fatal(err)
	}
	got, data, indexed, err := StoredImageIndex(context.Background(), store, root.Digest.String())
	if err != nil || !indexed || got.Digest != root.Digest || string(data) != string(raw) {
		t.Fatalf("native index exact digest = %+v indexed=%v err=%v", got, indexed, err)
	}
	mismatch := digest.FromString("other index")
	if _, _, _, err := StoredImageIndex(context.Background(), store, "native-list@"+mismatch.String()); err == nil {
		t.Fatal("accepted mismatched default for missing digest key")
	}
}

func TestStoredLookupRejectsForeignTransportWithoutOpeningStore(t *testing.T) {
	root := t.TempDir()
	backend, err := storage.GetStore(storage.StoreOptions{GraphRoot: filepath.Join(root, "selected-graph"), RunRoot: filepath.Join(root, "selected-run"), GraphDriverName: "vfs"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = backend.Shutdown(false); backend.Free() })
	foreignRoot := filepath.Join(root, "foreign-graph")
	foreignRun := filepath.Join(root, "foreign-run")
	selector := "containers-storage:[vfs@" + foreignRoot + "+" + foreignRun + "]localhost/foreign:latest"
	if _, _, err := findStoredImageAndName(backend, selector); err == nil || !strings.Contains(err.Error(), `unsupported transport "containers-storage"`) {
		t.Fatalf("foreign lookup: %v", err)
	}
	for _, path := range []string{foreignRoot, foreignRun} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("foreign store was opened/created at %s: %v", path, err)
		}
	}
	if _, _, err := findStoredImageAndName(backend, "localhost/missing:latest"); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("ordinary native lookup: %v", err)
	}
}

func TestStoredLookupDisambiguatesLiteralTransportLikeTag(t *testing.T) {
	root := t.TempDir()
	backend, err := storage.GetStore(storage.StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = backend.Shutdown(false); backend.Free() })
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	descriptor := Descriptor(v1.MediaTypeImageIndex, raw)
	created, err := backend.CreateImage(descriptor.Digest.Encoded(), []string{"localhost/docker-daemon:dev", "containers-storage:5000/name:tag"}, "", "", &storage.ImageOptions{BigData: []storage.ImageBigDataOption{{Key: storage.ImageDigestBigDataKey, Data: raw, Digest: descriptor.Digest}}})
	if err != nil {
		t.Fatal(err)
	}
	found, matched, err := findStoredImageAndName(backend, "docker-daemon:dev")
	if err != nil || found.ID != created.ID || matched != "localhost/docker-daemon:dev" {
		t.Fatalf("ordinary local tag lookup: image=%v name=%q err=%v", found, matched, err)
	}
	if managementName, err := StoredImageName(backend, "docker-daemon:dev"); err != nil || managementName != matched {
		t.Fatalf("management selector name=%q err=%v", managementName, err)
	}
	qualified, qualifiedName, err := findStoredImageAndName(backend, "containers-storage:5000/name:tag")
	if err != nil || qualified.ID != created.ID || qualifiedName != "containers-storage:5000/name:tag" {
		t.Fatalf("qualified transport-like registry: image=%v name=%q err=%v", qualified, qualifiedName, err)
	}
	for _, selector := range []string{"containers-storage:5000/name@" + descriptor.Digest.String(), "containers-storage:5000/name:tag@" + descriptor.Digest.String()} {
		image, matched, err := findStoredImageAndName(backend, selector)
		if err != nil || image.ID != created.ID || matched != "containers-storage:5000/name@"+descriptor.Digest.String() {
			t.Fatalf("named digest %q: image=%v name=%q err=%v", selector, image, matched, err)
		}
	}
	if _, _, err := findStoredImageAndName(backend, "containers-storage:5000/name@"+digest.FromString("missing").String()); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("missing named digest: %v", err)
	}
	if _, _, err := findStoredImageAndName(backend, "containers-storage:5000/missing:tag"); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("qualified missing name: %v", err)
	}
	if err := backend.RemoveNames(found.ID, []string{matched}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := findStoredImageAndName(backend, "docker-daemon:dev"); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("removed literal tag: %v", err)
	}
	for _, name := range []string{"docker-daemon:repo:tag", "oci:/not/an/image"} {
		if _, _, err := findStoredImageAndName(backend, name); err == nil || !strings.Contains(err.Error(), "unsupported transport") {
			t.Fatalf("transport %q: %v", name, err)
		}
	}
}
