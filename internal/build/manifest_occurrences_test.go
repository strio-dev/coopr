package build

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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

func TestManifestAppendPreservesRepeatedResidentDescriptors(t *testing.T) {
	ctx := context.Background()
	options := nativeBuildTestStore(t.TempDir())
	err := buildah.WithStore(options, func(store storage.Store) error {
		makeSelection := func(arch string) oci.StoredSelection {
			config, _ := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: arch}, RootFS: v1.RootFS{Type: "layers"}, History: []v1.History{{CreatedBy: "retained history", EmptyLayer: true}}})
			cfg := oci.Descriptor(v1.MediaTypeImageConfig, config)
			data, _ := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg})
			desc := oci.Descriptor(v1.MediaTypeImageManifest, data)
			id := cfg.Digest.Encoded()
			_, err := store.CreateImage(id, nil, "", "", &storage.ImageOptions{BigData: []storage.ImageBigDataOption{{Key: cfg.Digest.String(), Data: config, Digest: cfg.Digest}, {Key: storage.ImageDigestBigDataKey, Data: data, Digest: desc.Digest}, {Key: storage.ImageDigestManifestBigDataNamePrefix + "-" + desc.Digest.String(), Data: data, Digest: desc.Digest}}})
			if err != nil {
				t.Fatal(err)
			}
			return oci.StoredSelection{Root: desc, Manifest: desc, ImageID: id, ConfigData: config}
		}
		amd, arm := makeSelection("amd64"), makeSelection("arm64")
		members := []v1.Descriptor{}
		for i := 0; i < 3; i++ {
			d := amd.Manifest
			d.Platform = &v1.Platform{OS: "linux", Architecture: "amd64"}
			d.Annotations = map[string]string{"occurrence": fmt.Sprint(i)}
			members = append(members, d)
		}
		raw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: members, Annotations: map[string]string{"root": "retained"}, Subject: &amd.Manifest, ArtifactType: "application/example"})
		root := oci.Descriptor(v1.MediaTypeImageIndex, raw)
		if _, err := imagestore.FromStore(store).WriteStoredIndex(ctx, root, raw, map[digest.Digest]string{amd.Manifest.Digest: amd.ImageID}, "repeated"); err != nil {
			return err
		}
		_, stored, existing, err := oci.StoredImageSelections(ctx, store, "repeated")
		if err != nil {
			return err
		}
		var index v1.Index
		if err := json.Unmarshal(stored, &index); err != nil {
			return err
		}
		for _, d := range index.Manifests {
			if _, err := oci.ResolveStoredImage(ctx, store, d.Digest.String(), *d.Platform); err != nil {
				return err
			}
		}
		t.Logf("resident descriptors=%d, selection map entries=%d; every descriptor resolves", len(index.Manifests), len(existing))
		_, appended, _, _, appendErr := appendManifest(ctx, "repeated", "oci", options, map[string]oci.StoredSelection{"linux/arm64": arm})
		if appendErr != nil {
			return appendErr
		}
		var actual v1.Index
		if err := json.Unmarshal(appended, &actual); err != nil {
			return err
		}
		if len(actual.Manifests) != 4 || !reflect.DeepEqual(actual.Manifests[:3], members) || actual.Manifests[3].Digest != arm.Manifest.Digest || !reflect.DeepEqual(actual.Annotations, index.Annotations) || !reflect.DeepEqual(actual.Subject, index.Subject) || actual.ArtifactType != index.ArtifactType {
			t.Fatalf("append lost descriptor occurrences or root metadata: %s", appended)
		}
		_, historical, _, err := oci.StoredImageSelections(ctx, store, root.Digest.String())
		if err != nil {
			return err
		}
		if !bytes.Equal(historical, raw) {
			t.Fatal("append changed historical root")
		}
		for _, selection := range []oci.StoredSelection{amd, arm} {
			data, err := store.ImageBigData(selection.ImageID, digest.FromBytes(selection.ConfigData).String())
			if err != nil {
				return err
			}
			if !bytes.Equal(data, selection.ConfigData) {
				t.Fatal("append changed raw config")
			}
		}
		if _, err := imagestore.FromStore(store).WriteStoredIndex(ctx, root, raw, map[digest.Digest]string{amd.Manifest.Digest: amd.ImageID}, "native-control"); err != nil {
			return err
		}
		runtime, err := libimage.RuntimeFromStore(store, &libimage.RuntimeOptions{SystemContext: &types.SystemContext{}})
		if err != nil {
			return err
		}
		handle, err := runtime.LookupManifestList("localhost/native-control:latest")
		if err != nil {
			return err
		}
		armRef, err := imagestorage.Transport.NewStoreReference(store, nil, arm.ImageID)
		if err != nil {
			return err
		}
		_, nativeErr := handle.Add(ctx, transports.ImageName(armRef), nil)
		if nativeErr != nil {
			return fmt.Errorf("native Add control failed: %w", nativeErr)
		}
		data, err := store.ImageBigData(handle.ID(), storage.ImageDigestBigDataKey)
		if err != nil {
			return err
		}
		var updated v1.Index
		if err := json.Unmarshal(data, &updated); err != nil {
			return err
		}
		if len(updated.Manifests) != 4 || !reflect.DeepEqual(updated.Manifests[:3], members) {
			t.Fatalf("native control lost occurrences: %s", data)
		}
		t.Logf("Coopr and public native Add each retain all three occurrences and append one descriptor")
		return appendErr
	})
	if err != nil {
		t.Fatalf("append complete resident list: %v", err)
	}
}
