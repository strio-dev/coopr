package cache

import (
	"bytes"
	"encoding/json"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestGraphVerificationReadsSharedLayerOnce(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared layer", true: "conflicting size"}[conflict], func(t *testing.T) {
			ctx := t.Context()
			record, path := graphFixture(t)
			source, err := orasoci.New(path)
			if err != nil {
				t.Fatal(err)
			}
			first := record.Results[0].Image
			descriptors, err := imageGraphDescriptors(ctx, source, first)
			if err != nil {
				t.Fatal(err)
			}
			layer := descriptors[1]
			configBytes, err := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Config: v1.ImageConfig{Labels: map[string]string{"variant": "same layers"}}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}}})
			if err != nil {
				t.Fatal(err)
			}
			config := oci.Descriptor(v1.MediaTypeImageConfig, configBytes)
			if conflict {
				layer.Size++
			}
			manifestBytes, err := json.Marshal(oci.VersionedManifest(config, []v1.Descriptor{layer}, ""))
			if err != nil {
				t.Fatal(err)
			}
			second := oci.Descriptor(v1.MediaTypeImageManifest, manifestBytes)
			for _, blob := range []struct {
				desc v1.Descriptor
				data []byte
			}{{config, configBytes}, {second, manifestBytes}} {
				if err := source.Push(ctx, blob.desc, bytes.NewReader(blob.data)); err != nil {
					t.Fatal(err)
				}
			}
			counter := &countedCacheTarget{Target: source, fetches: map[digest.Digest]int{}}
			err = verifyGraphImages(ctx, counter, []v1.Descriptor{first, second})
			if (err != nil) != conflict {
				t.Fatalf("verification error=%v, conflict=%v", err, conflict)
			}
			if count := counter.fetches[layer.Digest]; count != 1 {
				t.Fatalf("shared layer fetched %d times, want one", count)
			}
		})
	}
}
