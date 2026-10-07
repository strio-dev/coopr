package oci

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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
