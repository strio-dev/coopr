package build

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestManifestAppendPreservesNativeHandleAndHistoricalDigest(t *testing.T) {
	ctx := context.Background()
	options := nativeBuildTestStore(t.TempDir())
	err := buildah.WithStore(options, func(store storage.Store) error {
		makeSelection := func(arch string) oci.StoredSelection {
			config, err := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: arch}, RootFS: v1.RootFS{Type: "layers"}})
			if err != nil {
				t.Fatal(err)
			}
			configDesc := oci.Descriptor(v1.MediaTypeImageConfig, config)
			data, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: configDesc})
			if err != nil {
				t.Fatal(err)
			}
			manifest := oci.Descriptor(v1.MediaTypeImageManifest, data)
			id := configDesc.Digest.Encoded()
			_, err = store.CreateImage(id, nil, "", "", &storage.ImageOptions{BigData: []storage.ImageBigDataOption{
				{Key: configDesc.Digest.String(), Data: config, Digest: configDesc.Digest},
				{Key: storage.ImageDigestBigDataKey, Data: data, Digest: manifest.Digest},
				{Key: storage.ImageDigestManifestBigDataNamePrefix + "-" + manifest.Digest.String(), Data: data, Digest: manifest.Digest},
			}})
			if err != nil {
				t.Fatal(err)
			}
			return oci.StoredSelection{Root: manifest, Manifest: manifest, ImageID: id, ConfigData: config}
		}
		amd, arm, ppc := makeSelection("amd64"), makeSelection("arm64"), makeSelection("ppc64le")
		oldRoot, _, _, _, err := appendManifest(ctx, "combined", "oci", options, map[string]oci.StoredSelection{"linux/amd64": amd})
		if err != nil {
			return err
		}
		runtime, err := libimage.RuntimeFromStore(store, &libimage.RuntimeOptions{SystemContext: &types.SystemContext{BigFilesTemporaryDir: os.TempDir()}})
		if err != nil {
			return err
		}
		// This handle models an external native client that resolved the list
		// before waiting for Coopr's append operation to release its lock.
		handle, err := runtime.LookupManifestList("localhost/combined:latest")
		if err != nil {
			return err
		}
		originalID := handle.ID()
		if _, err := imagestore.FromStore(store).TagExisting("combined", "combined-alias"); err != nil {
			return err
		}
		if _, _, _, _, err := appendManifest(ctx, "combined", "oci", options, map[string]oci.StoredSelection{"linux/arm64": arm}); err != nil {
			return err
		}
		external, err := imagestorage.Transport.NewStoreReference(store, nil, ppc.ImageID)
		if err != nil {
			return err
		}
		if _, err := handle.Add(ctx, transports.ImageName(external), nil); err != nil {
			return err
		}
		for _, name := range []string{"combined", "combined-alias"} {
			matched, err := oci.StoredImageName(store, name)
			if err != nil {
				return err
			}
			image, err := store.Image(matched)
			if err != nil {
				return err
			}
			if image.ID != originalID {
				t.Fatalf("native append moved %s (%s) from %s to %s", name, matched, originalID, image.ID)
			}
			_, _, selections, err := oci.StoredImageSelections(ctx, store, name)
			if err != nil {
				return err
			}
			if len(selections) != 3 {
				t.Fatalf("%s lost native or Coopr update: %+v", name, selections)
			}
		}
		root, data, selected, err := oci.StoredImageSelections(ctx, store, oldRoot.Digest.String())
		if err != nil {
			return err
		}
		if root.Digest != oldRoot.Digest || len(selected) != 1 || len(data) == 0 {
			t.Fatalf("old index snapshot changed: %s, %+v", root.Digest, selected)
		}
		ids := map[digest.Digest]string{amd.Manifest.Digest: amd.ImageID}
		if _, err := imagestore.FromStore(store).WriteStoredIndex(ctx, oldRoot, data, ids, "historical-copy"); err != nil {
			return err
		}
		copied, _, selections, err := oci.StoredImageSelections(ctx, store, "historical-copy")
		if err != nil {
			return err
		}
		if copied.Digest != oldRoot.Digest || len(selections) != 1 {
			t.Fatalf("historical copy changed: %s, %+v", copied.Digest, selections)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
