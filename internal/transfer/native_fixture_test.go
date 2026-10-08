package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

func nativeTestStore(root string) buildah.StoreOptions {
	return buildah.StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"}
}

func nativeEmptyImageFixture(t *testing.T, options buildah.StoreOptions, platform v1.Platform, marker string) oci.StoredSelection {
	t.Helper()
	config, err := json.Marshal(v1.Image{Platform: platform, RootFS: v1.RootFS{Type: "layers"}, Config: v1.ImageConfig{Env: []string{"MARKER=" + marker}}})
	if err != nil {
		t.Fatal(err)
	}
	configDesc := oci.Descriptor(v1.MediaTypeImageConfig, config)
	data, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: configDesc})
	if err != nil {
		t.Fatal(err)
	}
	root := oci.Descriptor(v1.MediaTypeImageManifest, data)
	err = buildah.WithStore(options, func(store storage.Store) error {
		_, err := store.CreateImage(configDesc.Digest.Encoded(), nil, "", "", &storage.ImageOptions{BigData: []storage.ImageBigDataOption{
			{Key: configDesc.Digest.String(), Data: config, Digest: configDesc.Digest},
			{Key: storage.ImageDigestBigDataKey, Data: data, Digest: root.Digest},
			{Key: storage.ImageDigestManifestBigDataNamePrefix + "-" + root.Digest.String(), Data: data, Digest: root.Digest},
		}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return oci.StoredSelection{Root: root, Manifest: root, ImageID: configDesc.Digest.Encoded(), ConfigData: config}
}

func nameNativeTestImage(ctx context.Context, options buildah.StoreOptions, name string, selected oci.StoredSelection) error {
	return buildah.WithStore(options, func(store storage.Store) error {
		_, err := imagestore.FromStore(store).TagSelected(ctx, selected.ImageID, selected.Manifest, name)
		return err
	})
}

func nativeTestIndex(ctx context.Context, options buildah.StoreOptions, name string, root v1.Descriptor, data []byte, selections map[string]oci.StoredSelection) error {
	ids := map[digest.Digest]string{}
	for _, selected := range selections {
		ids[selected.Manifest.Digest] = selected.ImageID
	}
	return buildah.WithStore(options, func(store storage.Store) error {
		_, err := imagestore.FromStore(store).WriteStoredIndex(ctx, root, data, ids, name)
		return err
	})
}

func TestNativeRetagChangesDefaultSignaturesWithSelectedManifest(t *testing.T) {
	ctx := context.Background()
	options := nativeTestStore(t.TempDir())
	selected := nativeEmptyImageFixture(t, options, v1.Platform{OS: "linux", Architecture: "amd64"}, "signatures")
	err := buildah.WithStore(options, func(store storage.Store) error {
		raw, err := store.ImageBigData(selected.ImageID, storage.ImageDigestBigDataKey)
		if err != nil {
			return err
		}
		var value v1.Manifest
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		value.Annotations = map[string]string{"variant": "alternate"}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		alternate := oci.Descriptor(v1.MediaTypeImageManifest, data)
		if err := store.SetImageBigData(selected.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+alternate.Digest.String(), data, func([]byte) (digest.Digest, error) { return alternate.Digest, nil }); err != nil {
			return err
		}
		previousSignature := []byte("previous signature")
		alternateSignature := []byte("alternate signature of a different length")
		metadata, err := json.Marshal(map[string]any{"signature-sizes": []int{len(previousSignature)}, "signatures-sizes": map[digest.Digest][]int{alternate.Digest: {len(alternateSignature)}}, "custom": "preserve"})
		if err != nil {
			return err
		}
		if err := store.SetMetadata(selected.ImageID, string(metadata)); err != nil {
			return err
		}
		if err := store.SetImageBigData(selected.ImageID, "signatures", previousSignature, nil); err != nil {
			return err
		}
		if err := store.SetImageBigData(selected.ImageID, "signature-"+alternate.Digest.Encoded(), alternateSignature, nil); err != nil {
			return err
		}
		if _, err := imagestore.FromStore(store).TagSelected(ctx, selected.ImageID, alternate, "alternate:latest"); err != nil {
			return err
		}
		defaultSignature, err := store.ImageBigData(selected.ImageID, "signatures")
		if err != nil {
			return err
		}
		if string(defaultSignature) != string(alternateSignature) {
			t.Fatalf("default signature remained paired with previous manifest: %q", defaultSignature)
		}
		state, err := oci.CaptureStoredSignatures(store, selected.ImageID)
		if err != nil {
			return err
		}
		old, err := oci.StoredSignatureBlobs(state, selected.Manifest.Digest, false)
		if err != nil {
			return err
		}
		if len(old) != 1 || string(old[0]) != string(previousSignature) {
			t.Fatalf("previous manifest signature was lost: %q", old)
		}
		retained, err := store.Metadata(selected.ImageID)
		if err != nil {
			return err
		}
		var parsed map[string]json.RawMessage
		if err := json.Unmarshal([]byte(retained), &parsed); err != nil {
			return err
		}
		if string(parsed["custom"]) != `"preserve"` {
			t.Fatalf("unknown metadata lost: %s", retained)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativeTagCopyPreservesRecordAndPodmanImageName(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") != "1" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live native copy workers")
	}
	ctx := context.Background()
	work := t.TempDir()
	conf := filepath.Join(work, "storage.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf("[storage]\ndriver = \"vfs\"\ngraphroot = %q\nrunroot = %q\n", filepath.Join(work, "graph"), filepath.Join(work, "run"))), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_STORAGE_CONF", conf)
	options, err := buildah.DefaultStoreOptions()
	if err != nil {
		t.Fatal(err)
	}
	selected := nativeEmptyImageFixture(t, options, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "shared-image")
	if err := nameNativeTestImage(ctx, options, "podman:latest", selected); err != nil {
		t.Fatal(err)
	}
	if err := buildah.WithStore(options, func(store storage.Store) error { return store.SetMetadata(selected.ImageID, `{"custom":"preserve"}`) }); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		source      string
		destination Destination
		want        string
	}{
		{"podman:latest", Destination{Transport: "local", Name: "alias:latest"}, "alias:latest"},
		{"alias:latest", Destination{Transport: "local", Name: "final:latest"}, "final:latest"},
	} {
		result, err := Copy(ctx, oci.Image, test.source, test.destination, Options{BuildStore: options})
		if err != nil || result != test.want {
			t.Fatalf("native copy %s = %q, %v", test.source, result, err)
		}
	}
	if err := buildah.WithStore(options, func(store storage.Store) error {
		metadata, err := store.Metadata(selected.ImageID)
		if err != nil {
			return err
		}
		if metadata != `{"custom":"preserve"}` {
			t.Fatalf("native copy replaced unrelated metadata: %s", metadata)
		}
		images, err := store.Images()
		if err != nil {
			return err
		}
		if len(images) != 1 || images[0].ID != selected.ImageID {
			t.Fatalf("copy created another native image: %+v", images)
		}
		for _, name := range []string{"localhost/podman:latest", "localhost/alias:latest", "localhost/final:latest"} {
			image, err := store.Image(name)
			if err != nil {
				return err
			}
			if image.ID != selected.ImageID {
				t.Fatalf("%s points to %s", name, image.ID)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveBoundaryHandlesMissingNativeRoots(t *testing.T) {
	work := t.TempDir()
	physical := filepath.Join(work, "physical")
	if err := os.Mkdir(physical, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(work, "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(alias, "missing", "native")
	if err := rejectStoreArchivePath(root, filepath.Join(work, "outside", "image.tar")); err != nil {
		t.Fatal(err)
	}
	if err := rejectStoreArchivePath(root, filepath.Join(physical, "missing", "native", "image.tar")); err == nil {
		t.Fatal("archive inside missing native root accepted")
	}
	if _, err := os.Stat(filepath.Join(physical, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("boundary validation created directories: %v", err)
	}
}

func TestSinglePlatformRetagClearsOnlyDestinationOrigin(t *testing.T) {
	for _, variant := range []string{"same", "alternate"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			options := nativeTestStore(t.TempDir())
			platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
			selected := nativeEmptyImageFixture(t, options, platform, "origin-retag")
			child := selected.Manifest
			child.Platform = &platform
			missing := oci.Descriptor(v1.MediaTypeImageManifest, []byte("missing foreign child"))
			missing.Platform = &v1.Platform{OS: "linux", Architecture: "ppc64le"}
			data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{child, missing}})
			if err != nil {
				t.Fatal(err)
			}
			root := oci.Descriptor(v1.MediaTypeImageIndex, data)
			selected.Root = root
			for _, name := range []string{"source:latest", "destination:latest"} {
				if err := nameNativeTestImage(ctx, options, name, selected); err != nil {
					t.Fatal(err)
				}
				if err := buildah.WithStore(options, func(store storage.Store) error {
					if err := store.SetImageBigData(selected.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+root.Digest.String(), data, func([]byte) (digest.Digest, error) { return root.Digest, nil }); err != nil {
						return err
					}
					return oci.RecordStoredOrigin(ctx, store, name, selected)
				}); err != nil {
					t.Fatal(err)
				}
			}
			target := selected.Manifest
			if err := buildah.WithStore(options, func(store storage.Store) error {
				if variant == "alternate" {
					data, err := store.ImageBigData(selected.ImageID, storage.ImageDigestBigDataKey)
					if err != nil {
						return err
					}
					var manifest v1.Manifest
					if err := json.Unmarshal(data, &manifest); err != nil {
						return err
					}
					manifest.Annotations = map[string]string{"variant": variant}
					data, err = json.Marshal(manifest)
					if err != nil {
						return err
					}
					target = oci.Descriptor(v1.MediaTypeImageManifest, data)
					if err := store.SetImageBigData(selected.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+target.Digest.String(), data, func([]byte) (digest.Digest, error) { return target.Digest, nil }); err != nil {
						return err
					}
				}
				_, err := imagestore.FromStore(store).TagSelected(ctx, selected.ImageID, target, "destination:latest")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := buildah.WithStore(options, func(store storage.Store) error {
				for name, want := range map[string]digest.Digest{"source:latest": root.Digest, "destination:latest": target.Digest} {
					actual, _, _, err := oci.StoredImageSelections(ctx, store, name)
					if err != nil {
						return err
					}
					if actual.Digest != want {
						t.Fatalf("%s resolves to %s, want %s", name, actual.Digest, want)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
