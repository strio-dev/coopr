package build

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/transfer"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

func TestSharedStoreMultiPlatformLocalIndex(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") != "1" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for offline shared-store index coverage")
	}
	t.Setenv("COOPR_TEST_CONTAINER_STORAGE", "1")
	loadTestBackend(t)
	for _, format := range []string{"oci", "docker"} {
		for _, tagged := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/tagged=%t", format, tagged), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				work := t.TempDir()
				storeOptions := buildah.StoreOptions{
					RunRoot: filepath.Join(work, "runtime"), GraphRoot: filepath.Join(work, "native-graph"),
					ImageStore: filepath.Join(work, "native-images"), GraphDriverName: "vfs",
					GraphDriverOptions: []string{"vfs.ignore_chown_errors=true"}, TransientStore: true,
				}
				t.Cleanup(func() { cleanupSharedIndexStore(t, storeOptions) })
				definition := filepath.Join(work, "image.coopr")
				writeDefinition := func(label string) {
					t.Helper()
					if err := os.WriteFile(definition, []byte(fmt.Sprintf("from \"scratch\"\nlabel shared_index=%q\n", label)), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				writeDefinition("original")
				const tag = "localhost/shared-index:latest"
				opts := Options{File: definition, Context: work, BuildStore: storeOptions, Platforms: []string{"linux/amd64", "linux/arm64"}, Format: format}
				if tagged {
					opts.Tag = tag
				}
				built, err := Run(ctx, opts)
				if err != nil {
					t.Fatalf("build shared-store multi-platform image: %v", err)
				}
				copyOptions := transfer.Options{BuildStore: storeOptions}
				if !tagged {
					if got, err := transfer.Copy(ctx, oci.Image, built, transfer.Destination{Transport: "local", Name: tag}, copyOptions); err != nil || got != tag {
						t.Fatalf("tag untagged index by digest: got=%q err=%v", got, err)
					}
				}
				root, data, selections, found, err := testStoredImageIndex(ctx, storeOptions, tag)
				if err != nil || !found || len(selections) != 2 {
					t.Fatalf("catalog index: root=%s found=%t selections=%d err=%v", root.Digest, found, len(selections), err)
				}
				wantType := v1.MediaTypeImageIndex
				if format == "docker" {
					wantType = "application/vnd.docker.distribution.manifest.list.v2+json"
				}
				if root.MediaType != wantType {
					t.Fatalf("index media type=%s want=%s", root.MediaType, wantType)
				}
				original := inspectSharedIndex(t, ctx, storeOptions, tag, root, data, selections)
				before, err := testNativeNames(storeOptions)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := transfer.Copy(ctx, oci.Image, root.Digest.String(), transfer.Destination{Transport: "local", Name: "/"}, copyOptions); err == nil || !strings.Contains(err.Error(), "tag stored multi-platform image") {
					t.Fatalf("expected native tagging failure for invalid destination, got %v", err)
				}
				after, err := testNativeNames(storeOptions)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("failed native tag changed names: before=%+v after=%+v err=%v", before, after, err)
				}
				if _, _, _, found, err := testStoredImageIndex(ctx, storeOptions, "/"); err == nil || found {
					t.Fatalf("invalid native reference was accepted: found=%t err=%v", found, err)
				}
				const alias = "localhost/shared-index-alias:latest"
				for _, source := range []string{root.Digest.String(), tag, root.Digest.String()} {
					if got, err := transfer.Copy(ctx, oci.Image, source, transfer.Destination{Transport: "local", Name: alias}, copyOptions); err != nil || got != alias {
						t.Fatalf("copy %s to alias: got=%q err=%v", source, got, err)
					}
					if got := inspectSharedIndex(t, ctx, storeOptions, alias, root, data, selections); !reflect.DeepEqual(got, original) {
						t.Fatalf("retag changed native identity/children: got=%+v want=%+v", got, original)
					}
				}
				// A fresh catalog forces consumption through the shared native store.
				child := filepath.Join(work, "child.coopr")
				if err := os.WriteFile(child, []byte("from \"localhost/shared-index-alias:latest\"\nlabel child=\"yes\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				for _, arch := range []string{"amd64", "arm64"} {
					archive := filepath.Join(work, "child-"+arch+".tar")
					if _, err := Run(ctx, Options{File: child, Context: work, BuildStore: storeOptions, Platform: "linux/" + arch, Format: format, Tag: "oci-archive:" + archive}); err != nil {
						t.Fatalf("consume local index for %s: %v", arch, err)
					}
					_, image := readExampleImage(t, ctx, archive)
					if image.OS != "linux" || image.Architecture != arch || image.Config.Labels["shared_index"] != "original" || image.Config.Labels["child"] != "yes" {
						t.Fatalf("child %s lost platform or inherited configuration: %+v", arch, image)
					}
				}
				if !tagged {
					return
				}
				writeDefinition("updated")
				if _, err := Run(ctx, opts); err != nil {
					t.Fatalf("rebuild tagged index: %v", err)
				}
				updatedRoot, updatedData, updatedSelections, found, err := testStoredImageIndex(ctx, storeOptions, tag)
				if err != nil || !found || updatedRoot.Digest == root.Digest {
					t.Fatalf("rebuild did not update index: root=%s found=%t err=%v", updatedRoot.Digest, found, err)
				}
				updated := inspectSharedIndex(t, ctx, storeOptions, tag, updatedRoot, updatedData, updatedSelections)
				if updated.ID == original.ID {
					t.Fatal("changed index reused the old native list identity")
				}
				if got := inspectSharedIndex(t, ctx, storeOptions, original.ID, root, data, selections); !reflect.DeepEqual(got, original) {
					t.Fatalf("pinned old digest changed: got=%+v want=%+v", got, original)
				}
				const pinnedAlias = "localhost/shared-index-pinned:latest"
				if _, err := transfer.Copy(ctx, oci.Image, root.Digest.String(), transfer.Destination{Transport: "local", Name: pinnedAlias}, copyOptions); err != nil {
					t.Fatalf("copy pinned old digest after rebuild: %v", err)
				}
				if got := inspectSharedIndex(t, ctx, storeOptions, pinnedAlias, root, data, selections); !reflect.DeepEqual(got, original) {
					t.Fatalf("pinned copy changed native identity: got=%+v want=%+v", got, original)
				}
			})
		}
	}
}

type sharedIndexSnapshot struct {
	ID       string
	ChildIDs map[string]string
}

func inspectSharedIndex(t *testing.T, ctx context.Context, opts buildah.StoreOptions, name string, root v1.Descriptor, data []byte, selections map[string]oci.StoredSelection) sharedIndexSnapshot {
	t.Helper()
	backend, err := storage.GetStore(buildah.NativeStoreOptions(opts))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := backend.Shutdown(true); err != nil {
			t.Errorf("shutdown native inspection: %v", err)
		}
	}()
	matched, err := oci.StoredImageName(backend, name)
	if err != nil {
		t.Fatalf("native index name %s: %v", name, err)
	}
	image, err := backend.Image(matched)
	if err != nil {
		t.Fatal(err)
	}
	gotRoot, gotData, found, err := oci.StoredImageIndex(ctx, backend, name)
	if err != nil || !found || gotRoot.Digest != root.Digest || gotRoot.MediaType != root.MediaType || gotRoot.Size != root.Size || !bytes.Equal(gotData, data) {
		t.Fatalf("native index differs for %s: root=%+v found=%t err=%v", name, gotRoot, found, err)
	}
	images, err := backend.Images()
	if err != nil {
		t.Fatal(err)
	}
	identities := 0
	for _, candidate := range images {
		manifest, err := backend.ImageBigData(candidate.ID, storage.ImageDigestManifestBigDataNamePrefix)
		if err == nil && bytes.Equal(manifest, data) {
			identities++
		}
	}
	if identities != 1 {
		t.Fatalf("native store has %d records for index %s, want one", identities, root.Digest)
	}
	var index v1.Index
	if err := json.Unmarshal(gotData, &index); err != nil || len(index.Manifests) != 2 {
		t.Fatalf("native index platform count: %+v err=%v", index, err)
	}
	snapshot := sharedIndexSnapshot{ID: image.ID, ChildIDs: map[string]string{}}
	for i, arch := range []string{"amd64", "arm64"} {
		platform := v1.Platform{OS: "linux", Architecture: arch}
		if p := index.Manifests[i].Platform; p == nil || p.OS != platform.OS || p.Architecture != arch {
			t.Fatalf("native index platform %d: %+v", i, p)
		}
		resolved, err := oci.ResolveStoredImage(ctx, backend, name, platform)
		selection := selections["linux/"+arch]
		if err != nil {
			t.Fatalf("resolve native %s instance through saved list references: %v", arch, err)
		}
		if resolved.Root.Digest != root.Digest || resolved.Selected.Digest != selection.Manifest.Digest || resolved.StorageImageID != selection.ImageID {
			t.Fatalf("native %s child differs: root=%s manifest=%s imageID=%s want=%+v", arch, resolved.Root.Digest, resolved.Selected.Digest, resolved.StorageImageID, selection)
		}
		snapshot.ChildIDs[arch] = resolved.StorageImageID
	}
	return snapshot
}

func cleanupSharedIndexStore(t *testing.T, opts buildah.StoreOptions) {
	t.Helper()
	backend, err := storage.GetStore(buildah.NativeStoreOptions(opts))
	if err != nil {
		t.Errorf("open isolated store for cleanup: %v", err)
		return
	}
	images, err := backend.Images()
	if err != nil {
		t.Errorf("list isolated store for cleanup: %v", err)
	}
	for _, image := range images {
		if _, err := backend.DeleteImage(image.ID, true); err != nil {
			t.Errorf("delete isolated image %s: %v", image.ID, err)
		}
	}
	if _, err := backend.Shutdown(true); err != nil {
		t.Errorf("shutdown isolated store: %v", err)
	}
}
