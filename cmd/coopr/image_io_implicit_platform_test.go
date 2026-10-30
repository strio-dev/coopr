package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/storage"
)

func TestImageIOLoadSaveImplicitPlatformIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("requires live native image load/save")
	}
	ctx := context.Background()
	native := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	foreign := v1.Platform{OS: "linux", Architecture: "arm64"}
	if native.Architecture == foreign.Architecture {
		foreign.Architecture = "amd64"
	}
	layoutA, rootA, _ := maintenanceImageLayout(t, foreign, "explicit")
	layoutB, rootB, _ := maintenanceImageLayout(t, native, "implicit")
	layout := filepath.Join(t.TempDir(), "mixed")
	root, data, err := oci.AssembleImageIndex(ctx, layout, []oci.ImageVariant{{Layout: layoutA, Manifest: rootA, Platform: foreign}, {Layout: layoutB, Manifest: rootB, Platform: native}}, "oci")
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	index.Manifests[1].Platform = nil
	root, data, err = oci.RewriteImageIndex(root, data, index.Manifests)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout, "blobs", root.Digest.Algorithm().String(), root.Digest.Encoded()), data, 0600); err != nil {
		t.Fatal(err)
	}
	const tag = "localhost/mixed-implicit:latest"
	tagged := root
	tagged.Annotations = map[string]string{v1.AnnotationRefName: tag}
	top, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{tagged}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout, "index.json"), top, 0600); err != nil {
		t.Fatal(err)
	}
	store := maintenanceStoreOptions(t.TempDir())
	call := func(options buildah.StoreOptions, args ...string) string {
		t.Helper()
		var out, errs bytes.Buffer
		if status := run(imageIOArgs(options, args...), &out, &errs); status != 0 {
			t.Fatalf("%v status=%d: %s", args, status, &errs)
		}
		return strings.TrimSpace(out.String())
	}
	loaded := strings.TrimPrefix(call(store, "load", "--input", layout), "Loaded image: ")
	call(store, "tag", loaded, tag)
	assertIndex := func(options buildah.StoreOptions) {
		t.Helper()
		err := buildah.WithStore(options, func(backend storage.Store) error {
			actual, encoded, selections, err := oci.StoredImageSelections(ctx, backend, tag)
			if err != nil {
				return err
			}
			if actual.Digest != root.Digest || !bytes.Equal(encoded, data) || len(selections) != 2 {
				t.Errorf("restored index=%s members=%d, want %s and 2", actual.Digest, len(selections), root.Digest)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	assertIndex(store)
	call(store, "inspect", tag)
	archive := filepath.Join(t.TempDir(), "mixed.tar")
	call(store, "save", "--format", "oci-archive", "--output", archive, tag)
	fresh := maintenanceStoreOptions(t.TempDir())
	call(fresh, "load", "--input", archive)
	assertIndex(fresh)
	call(fresh, "inspect", tag)
}
