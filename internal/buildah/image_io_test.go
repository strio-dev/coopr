package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"coopr/internal/localstore"
	"coopr/internal/oci"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestLoadImagesWithNonRunnableIndexChildren(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx := context.Background()
	layout := filepath.Join(t.TempDir(), "layout")
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	push := func(mediaType string, data []byte) v1.Descriptor {
		t.Helper()
		desc := oci.Descriptor(mediaType, data)
		if err := source.Push(ctx, desc, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		return desc
	}
	marshal := func(value any) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	runnable, _ := namedContextLayoutImage(t, ctx, source, platform)
	runnable.Platform = &platform
	runnable.Annotations = map[string]string{"org.example.child": "preserved"}
	// BuildKit attestations carry in-toto JSON rather than runnable rootfs tar layers.
	statement := push("application/vnd.in-toto+json", []byte(`{"_type":"https://in-toto.io/Statement/v0.1","subject":[],"predicateType":"https://slsa.dev/provenance/v0.2","predicate":{}}`))
	attestationConfig := push(v1.MediaTypeImageConfig, marshal(v1.Image{
		Platform: v1.Platform{OS: "unknown", Architecture: "unknown"},
		RootFS:   v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{statement.Digest}},
	}))
	unknown := push(v1.MediaTypeImageManifest, marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest,
		Config: attestationConfig, Layers: []v1.Descriptor{statement},
	}))
	unknown.Platform = &v1.Platform{OS: "unknown", Architecture: "unknown"}
	unknown.Annotations = map[string]string{"vnd.docker.reference.type": "attestation-manifest", "vnd.docker.reference.digest": runnable.Digest.String()}
	absent := unknown
	absent.Platform = nil
	index := v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex,
		Manifests:   []v1.Descriptor{absent, runnable, unknown},
		Annotations: map[string]string{"org.example.index": "preserved"},
	}
	root := push(v1.MediaTypeImageIndex, marshal(index))
	if err := source.Tag(ctx, root, "mixed"); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "mixed.tar")
	if err := localstore.WriteArchive(ctx, source, root, archive); err != nil {
		t.Fatal(err)
	}
	// Anchor only the root index, as OCI archive export does. ORAS otherwise
	// also exposes the untagged child manifests in the layout's index.json.
	if err := os.WriteFile(filepath.Join(layout, v1.ImageIndexFile), marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{root},
	}), 0o644); err != nil {
		t.Fatal(err)
	}
	index.Manifests = []v1.Descriptor{runnable}
	wantData := marshal(index)
	wantRoot := oci.Descriptor(v1.MediaTypeImageIndex, wantData)
	for _, input := range []struct{ name, path string }{{"layout", layout}, {"archive", archive}} {
		t.Run(input.name, func(t *testing.T) {
			options := cacheTestStore(t.TempDir())
			policy := writeComponentTestPolicy(t, t.TempDir())
			names, err := LoadImages(ctx, options, input.path, oci.Options{SignaturePolicyPath: policy}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if len(names) != 1 {
				t.Fatalf("loaded names = %q", names)
			}
			if err := WithStore(options, func(backend storage.Store) error {
				loadedRoot, data, selections, err := oci.StoredImageSelections(ctx, backend, names[0])
				if err != nil {
					return err
				}
				if loadedRoot.Digest != wantRoot.Digest || !bytes.Equal(data, wantData) || len(selections) != 1 {
					t.Fatalf("loaded root=%s data=%s selections=%d", loadedRoot.Digest, data, len(selections))
				}
				var loadedIndex v1.Index
				if err := json.Unmarshal(data, &loadedIndex); err != nil {
					return err
				}
				if !reflect.DeepEqual(loadedIndex.Manifests, []v1.Descriptor{runnable}) {
					t.Fatalf("loaded descriptors=%+v want %+v", loadedIndex.Manifests, runnable)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
