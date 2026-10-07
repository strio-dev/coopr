package transfer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/definition"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

func TestPinnedVariantAliasSharesLayersAndSurvivesSourceRemoval(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live native alias coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	options := nativeTestStore(root)
	if err := os.WriteFile(filepath.Join(root, "proof"), []byte("shared native layer\n"), 0644); err != nil {
		t.Fatal(err)
	}
	build := func(source, output string) buildah.Result {
		t.Helper()
		parsed, err := definition.Parse(strings.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		result, err := buildah.BuildDefinitionSupervised(ctx, parsed, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, buildah.SupervisedPlanOptions{
			Store: options, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: buildah.Output{Path: filepath.Join(root, output)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	original := build("from \"scratch\"\ncopy \"proof\" \"/proof\"\n", "original")
	var alternate v1.Descriptor
	var aliasID string
	if err := buildah.WithStore(options, func(store storage.Store) error {
		if err := store.AddNames(original.ImageID, []string{"localhost/original:latest"}); err != nil {
			return err
		}
		data, err := store.ImageBigData(original.ImageID, storage.ImageDigestBigDataKey)
		if err != nil {
			return err
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return err
		}
		manifest.Annotations = map[string]string{"variant": "alternate"}
		data, err = json.Marshal(manifest)
		if err != nil {
			return err
		}
		alternate = oci.Descriptor(v1.MediaTypeImageManifest, data)
		if err := store.SetImageBigData(original.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+alternate.Digest.String(), data, func([]byte) (digest.Digest, error) { return alternate.Digest, nil }); err != nil {
			return err
		}
		layersBefore, err := store.Layers()
		if err != nil {
			return err
		}
		// Reuse a view created before exact compressed layer bytes were retained.
		retained, err := store.ImageBigData(original.ImageID, oci.StoredLayerBlobsKey)
		if err != nil {
			return err
		}
		if err := store.SetImageBigData(original.ImageID, oci.StoredLayerBlobsKey, []byte("{}"), nil); err != nil {
			return err
		}
		if _, err := oci.SelectedStoredImage(ctx, store, original.ImageID, alternate.Digest); err != nil {
			return err
		}
		if err := store.SetImageBigData(original.ImageID, oci.StoredLayerBlobsKey, retained, nil); err != nil {
			return err
		}
		if _, err := imagestore.FromStore(store).TagSelected(ctx, original.ImageID, alternate, "alias:latest"); err != nil {
			return err
		}
		source, err := store.Image(original.ImageID)
		if err != nil {
			return err
		}
		alias, err := store.Image("localhost/alias:latest")
		if err != nil {
			return err
		}
		aliasID = alias.ID
		if alias.ID == source.ID || alias.TopLayer == "" || alias.TopLayer != source.TopLayer {
			t.Fatalf("alias does not share source layers: source=%+v alias=%+v", source, alias)
		}
		layersAfter, err := store.Layers()
		if err != nil {
			return err
		}
		if len(layersAfter) != len(layersBefore) {
			t.Fatalf("retag added layers: before=%d after=%d", len(layersBefore), len(layersAfter))
		}
		_, err = store.DeleteImage(original.ImageID, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	consumed := build("from \"alias:latest\"\n", "consumed")
	if consumed.ImageID != aliasID || consumed.ManifestDigest != alternate.Digest.String() {
		t.Fatalf("FROM lost native alias identity: %+v, want %s/%s", consumed, aliasID, alternate.Digest)
	}
	consumedID := build("from \"sha256:"+aliasID+"\"\n", "consumed-id")
	if consumedID.ImageID != aliasID || consumedID.ManifestDigest != alternate.Digest.String() {
		t.Fatalf("FROM lost IID identity: %+v, want %s/%s", consumedID, aliasID, alternate.Digest)
	}
	build("from \"alias:latest\"\ncopy \"proof\" \"/second-proof\"\n", "transformed")
}
