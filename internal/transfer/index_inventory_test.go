package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

func TestStoredIndexNativeCopyPreservesSourceInventory(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	for _, existing := range []bool{false, true} {
		for _, reject := range []bool{false, true} {
			name := map[bool]string{false: "unmaterialized", true: "existing"}[existing] + "/" + map[bool]string{false: "success", true: "reject"}[reject]
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				store := nativeTestStore(t.TempDir())
				root, raw, selections := signedIndexFixture(t, store, "inventory")
				if !existing {
					var index v1.Index
					if err := json.Unmarshal(raw, &index); err != nil {
						t.Fatal(err)
					}
					index.Annotations["snapshot"] = "unmaterialized"
					var err error
					raw, err = json.Marshal(index)
					if err != nil {
						t.Fatal(err)
					}
					root = oci.Descriptor(v1.MediaTypeImageIndex, raw)
				}

				before := storedImageInventory(t, store)
				opts := Options{BuildStore: store}
				if reject {
					policy := filepath.Join(t.TempDir(), "policy.json")
					if err := os.WriteFile(policy, []byte(`{"default":[{"type":"reject"}]}`), 0600); err != nil {
						t.Fatal(err)
					}
					opts.SignaturePolicyPath = policy
				}
				destination := Destination{Transport: "dir", Name: filepath.Join(t.TempDir(), "output")}
				_, err := copyStoredIndex(ctx, store.GraphRoot, root, raw, selections, destination, opts)
				if reject && (err == nil || !strings.Contains(err.Error(), "rejected by policy")) {
					t.Fatalf("copy rejection = %v, want signature-policy rejection", err)
				}
				if !reject && err != nil {
					t.Fatal(err)
				}
				if after := storedImageInventory(t, store); !reflect.DeepEqual(before, after) {
					t.Fatalf("copy changed source image inventory: before=%+v after=%+v", before, after)
				}
			})
		}
	}
}

// storedImageInventory includes all source records so an unnamed staging image
// or a change to an existing source cannot hide behind a tag-only comparison.
func storedImageInventory(t *testing.T, store buildah.StoreOptions) map[string]storage.Image {
	t.Helper()
	result := map[string]storage.Image{}
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		images, err := backend.Images()
		if err != nil {
			return err
		}
		for _, image := range images {
			result[image.ID] = image
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHistoricalStoredIndexCopyPreservesSourceIDsAndNames(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	for _, failDestination := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "destination-error"}[failDestination], func(t *testing.T) {
			ctx := context.Background()
			store := nativeTestStore(t.TempDir())
			root, raw, selections := signedIndexFixture(t, store, "historical-member")
			selected := selections["linux/amd64"]
			if err := buildah.WithStore(store, func(backend storage.Store) error {
				data, err := backend.ImageBigData(selected.ImageID, storage.ImageDigestBigDataKey)
				if err != nil {
					return err
				}
				var image v1.Manifest
				if err := json.Unmarshal(data, &image); err != nil {
					return err
				}
				image.Annotations = map[string]string{"default": "newer"}
				data, err = json.Marshal(image)
				if err != nil {
					return err
				}
				alternate := oci.Descriptor(v1.MediaTypeImageManifest, data)
				if err := backend.SetImageBigData(selected.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+alternate.Digest.String(), data, func([]byte) (digest.Digest, error) { return alternate.Digest, nil }); err != nil {
					return err
				}
				_, err = imagestore.FromStore(backend).TagSelected(ctx, selected.ImageID, alternate, "alternate:latest")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			before := storedImageInventory(t, store)
			destination := Destination{Transport: "dir", Name: filepath.Join(t.TempDir(), "output")}
			if failDestination {
				if err := os.WriteFile(destination.Name, []byte("occupied destination"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var progress bytes.Buffer
			_, err := copyStoredIndex(ctx, store.GraphRoot, root, raw, selections, destination, Options{BuildStore: store, ProgressWriter: &progress})
			if failDestination {
				if err == nil || !strings.Contains(err.Error(), "not a directory") {
					t.Fatalf("destination failure = %v", err)
				}
				if !strings.Contains(progress.String(), "Writing manifest") {
					t.Fatalf("destination failed before member staging: %s", &progress)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				copied, err := os.ReadFile(filepath.Join(destination.Name, selected.Manifest.Digest.Encoded()+".manifest.json"))
				if err != nil {
					t.Fatal(err)
				}
				if digest.FromBytes(copied) != selected.Manifest.Digest {
					t.Fatal("copy followed the newer default instead of the selected historical member")
				}
			}
			after := storedImageInventory(t, store)
			if len(before) != len(after) {
				t.Fatalf("source image count changed: %d -> %d", len(before), len(after))
			}
			for id, image := range before {
				current, ok := after[id]
				if !ok || !reflect.DeepEqual(image.Names, current.Names) {
					t.Fatalf("source image ID/names changed for %s: %v -> %v", id, image.Names, current.Names)
				}
			}
			// A deterministic one-leaf BigData entry is existing native export
			// behavior. Its presence proves the historical member was staged.
			if len(after[selected.ImageID].BigDataNames) <= len(before[selected.ImageID].BigDataNames) {
				t.Fatal("historical member staging did not run")
			}
		})
	}
}
