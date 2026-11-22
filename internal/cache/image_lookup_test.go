package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

type countedCacheTarget struct {
	oras.Target
	mu      sync.Mutex
	fetches map[digest.Digest]int
	corrupt digest.Digest
}

func (t *countedCacheTarget) Fetch(ctx context.Context, desc v1.Descriptor) (io.ReadCloser, error) {
	t.mu.Lock()
	t.fetches[desc.Digest]++
	t.mu.Unlock()
	if desc.Digest == t.corrupt {
		return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), int(desc.Size)))), nil
	}
	return t.Target.Fetch(ctx, desc)
}
func imageLookupFixture(t *testing.T, created time.Time) (*OCIStore, ImageKey, []v1.Descriptor, *countedCacheTarget) {
	t.Helper()
	ctx := context.Background()
	sourcePath := t.TempDir()
	source, err := orasoci.New(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	layerData := []byte("exact layer")
	layer := oci.Descriptor(v1.MediaTypeImageLayer, layerData)
	layer.Annotations = map[string]string{"io.github.containers.zstd-chunked.manifest-checksum": digest.FromString("chunk table").String()}
	configData, _ := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}}})
	config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
	manifestData, _ := json.Marshal(oci.VersionedManifest(config, []v1.Descriptor{layer}, ""))
	manifest := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	for _, item := range []struct {
		desc v1.Descriptor
		data []byte
	}{{layer, layerData}, {config, configData}, {manifest, manifestData}} {
		if err := source.Push(ctx, item.desc, bytes.NewReader(item.data)); err != nil {
			t.Fatal(err)
		}
	}
	target, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedCacheTarget{Target: target, fetches: map[digest.Digest]int{}}
	store := &OCIStore{target: counted, stagingDir: t.TempDir()}
	key := ImageKey{Instruction: digest.FromString("instruction"), Parent: digest.FromString("parent"), Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Executor: "test", Format: "oci"}
	if _, err := store.PutImage(ctx, key, ImageRecord{Key: key, CreatedAt: created, Image: manifest}, sourcePath); err != nil {
		t.Fatal(err)
	}
	return store, key, []v1.Descriptor{manifest, config, layer}, counted
}
func TestImageLookupFetchesPayloadOnce(t *testing.T) {
	store, key, descriptors, counted := imageLookupFixture(t, time.Now())
	_, path, err := store.LookupImage(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	for _, desc := range descriptors[1:] {
		if counted.fetches[desc.Digest] != 1 {
			t.Errorf("payload %s fetched %d times; want once", desc.Digest, counted.fetches[desc.Digest])
		}
	}
}
func TestImageLookupRejectsCorruptPayload(t *testing.T) {
	for _, index := range []int{1, 2} {
		t.Run([]string{"config", "layer"}[index-1], func(t *testing.T) {
			store, key, descriptors, counted := imageLookupFixture(t, time.Now())
			counted.corrupt = descriptors[index].Digest
			_, path, err := store.LookupImage(context.Background(), key)
			if err == nil || path != "" {
				t.Fatalf("corrupt payload accepted: path=%s err=%v", path, err)
			}
			entries, err := os.ReadDir(store.stagingDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed lookup leaked staged data: %v %v", entries, err)
			}
		})
	}
}

func TestImageExpiryStillVerifiesCacheMetadata(t *testing.T) {
	store, key, _, counted := imageLookupFixture(t, time.Now().Add(-2*time.Hour))
	ttl := time.Hour
	store.ttl = &ttl
	keyDigest, _ := key.Digest()
	root, err := counted.Resolve(context.Background(), instructionImageTag(keyDigest))
	if err != nil {
		t.Fatal(err)
	}
	data, err := fetchVerified(context.Background(), counted, root, maxImageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	data, err = fetchVerified(context.Background(), counted, index.Manifests[0], maxImageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var artifact v1.Manifest
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	counted.corrupt = artifact.Config.Digest
	_, path, err := store.LookupImage(context.Background(), key)
	if err == nil || errors.Is(err, ErrMiss) || path != "" {
		t.Fatalf("expired corrupt metadata became miss: path=%s err=%v", path, err)
	}
}

func TestImageLookupRejectsInvalidEnvelopeEdges(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*v1.Index, *v1.Manifest)
	}{
		{"missing image", func(index *v1.Index, _ *v1.Manifest) { index.Manifests = index.Manifests[:1] }},
		{"different image", func(index *v1.Index, artifact *v1.Manifest) { index.Manifests[1] = artifact.Config }},
		{"unexpected base", func(index *v1.Index, _ *v1.Manifest) { index.Manifests = append(index.Manifests, index.Manifests[1]) }},
		{"image in metadata layers", func(index *v1.Index, artifact *v1.Manifest) { artifact.Layers = []v1.Descriptor{index.Manifests[1]} }},
		{"null metadata layers", func(_ *v1.Index, artifact *v1.Manifest) { artifact.Layers = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, key, descriptors, counted := imageLookupFixture(t, time.Now())
			keyDigest, _ := key.Digest()
			root, err := counted.Resolve(ctx, instructionImageTag(keyDigest))
			if err != nil {
				t.Fatal(err)
			}
			data, err := fetchVerified(ctx, counted, root, maxImageRecordBytes)
			if err != nil {
				t.Fatal(err)
			}
			var index v1.Index
			if err := json.Unmarshal(data, &index); err != nil {
				t.Fatal(err)
			}
			data, err = fetchVerified(ctx, counted, index.Manifests[0], maxImageRecordBytes)
			if err != nil {
				t.Fatal(err)
			}
			var artifact v1.Manifest
			if err := json.Unmarshal(data, &artifact); err != nil {
				t.Fatal(err)
			}
			test.change(&index, &artifact)
			index.Manifests[0] = pushGraphTestJSON(t, counted, v1.MediaTypeImageManifest, artifact)
			root = pushGraphTestJSON(t, counted, v1.MediaTypeImageIndex, index)
			if err := counted.Tag(ctx, root, instructionImageTag(keyDigest)); err != nil {
				t.Fatal(err)
			}
			counted.fetches = map[digest.Digest]int{}
			_, path, err := store.LookupImage(ctx, key)
			if err == nil || errors.Is(err, ErrMiss) || path != "" {
				t.Fatalf("invalid cache edges accepted: path=%s err=%v", path, err)
			}
			for _, descriptor := range descriptors[1:] {
				if counted.fetches[descriptor.Digest] != 0 {
					t.Errorf("invalid envelope hydrated image payload %s", descriptor.Digest)
				}
			}
			entries, err := os.ReadDir(store.stagingDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid envelope staged data: %v %v", entries, err)
			}
		})
	}
}
