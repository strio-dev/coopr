package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	cachetypes "github.com/moby/buildkit/cache/remotecache/v1/types"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func graphFixture(t *testing.T) (GraphRecord, string) {
	t.Helper()
	dir := t.TempDir()
	target, err := orasoci.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	record := GraphRecord{Version: GraphRecordVersion, Root: digest.FromString("logical graph"), CreatedAt: time.Now().UTC(), Metadata: json.RawMessage(`{"native_links":["first","second"]}`)}
	for _, name := range []string{"first", "second"} {
		layerData := []byte(name)
		layer := oci.Descriptor(v1.MediaTypeImageLayer, layerData)
		configData, err := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}}})
		if err != nil {
			t.Fatal(err)
		}
		config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
		manifestData, err := json.Marshal(oci.VersionedManifest(config, []v1.Descriptor{layer}, ""))
		if err != nil {
			t.Fatal(err)
		}
		image := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
		for _, item := range []struct {
			d v1.Descriptor
			b []byte
		}{{layer, layerData}, {config, configData}, {image, manifestData}} {
			if err := target.Push(context.Background(), item.d, bytes.NewReader(item.b)); err != nil {
				t.Fatal(err)
			}
		}
		if err := target.Tag(context.Background(), image, name); err != nil {
			t.Fatal(err)
		}
		result := GraphResult{ID: name, Image: image, Metadata: json.RawMessage(`{"root_mode":493}`)}
		record.Results = append(record.Results, result)
		desc, err := result.Descriptor()
		if err != nil {
			t.Fatal(err)
		}
		record.Config.Layers = append(record.Config.Layers, cachetypes.CacheLayer{Blob: desc.Digest, ParentIndex: -1})
	}
	record.Results[1].BaseImage = &record.Results[0].Image
	updated, err := record.Results[1].Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	record.Config.Layers[1].Blob = updated.Digest
	record.Config.Records = []cachetypes.CacheRecord{
		{Digest: digest.FromString("source")},
		{Digest: digest.FromString("operation"), Inputs: [][]cachetypes.CacheInput{{{LinkIndex: 0, Selector: "selected"}}}, Results: []cachetypes.CacheResult{{LayerIndex: 0, CreatedAt: record.CreatedAt.Add(-time.Hour)}, {LayerIndex: 1, CreatedAt: record.CreatedAt}}},
	}
	return record, dir
}

func TestGraphCacheRoundTripRetainsVariantsAndDependencies(t *testing.T) {
	ctx := context.Background()
	record, source := graphFixture(t)
	cacheRoot, staging := t.TempDir(), t.TempDir()
	store, err := NewLocalStore(ctx, cacheRoot, staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutGraph(ctx, record.Root, record, source); err != nil {
		t.Fatal(err)
	}
	// A new store object exercises on-disk graph recovery, not an in-memory cache.
	store, err = NewLocalStore(ctx, cacheRoot, staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, layout, err := store.LookupGraph(ctx, record.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(layout) }()
	if !reflect.DeepEqual(*got, record) {
		t.Fatalf("round trip differs: got %+v want %+v", got, record)
	}
	images, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range got.Results {
		if err := verifyImageGraph(ctx, images, result.Image); err != nil {
			t.Fatal(err)
		}
	}
	if len(got.Config.Records[1].Results) != 2 || got.Config.Records[1].Inputs[0][0].LinkIndex != 0 {
		t.Fatal("lost historical result or dependency")
	}
	if _, _, err := store.LookupGraph(ctx, digest.FromString("other")); !errors.Is(err, ErrMiss) {
		t.Fatalf("unknown graph: %v", err)
	}
}

func TestGraphCacheRejectsMalformedGraphs(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*GraphRecord)
	}{
		{"missing sidecar", func(r *GraphRecord) { r.Config.Layers[0].Blob = digest.FromString("absent") }},
		{"record cycle", func(r *GraphRecord) { r.Config.Records[0].Inputs = [][]cachetypes.CacheInput{{{LinkIndex: 1}}} }},
		{"layer cycle", func(r *GraphRecord) { r.Config.Layers[0].ParentIndex = 0 }},
		{"invalid index", func(r *GraphRecord) { r.Config.Records[1].Results[0].LayerIndex = 9 }},
		{"invalid parent", func(r *GraphRecord) { r.Config.Layers[0].ParentIndex = -2 }},
		{"invalid digest", func(r *GraphRecord) { r.Config.Records[1].Digest = "invalid" }},
		{"duplicate identity", func(r *GraphRecord) { r.Results[1].ID = r.Results[0].ID }},
		{"unreferenced result", func(r *GraphRecord) { r.Config.Records[1].Results = r.Config.Records[1].Results[:1] }},
		{"empty input", func(r *GraphRecord) { r.Config.Records[1].Inputs[0] = nil }},
		{"external result image", func(r *GraphRecord) { r.Results[0].Image.URLs = []string{"https://example.invalid"} }},
		{"malformed metadata", func(r *GraphRecord) { r.Metadata = json.RawMessage(`{`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, _ := graphFixture(t)
			test.change(&r)
			if err := r.Validate(r.Root); err == nil {
				t.Fatal("accepted malformed graph")
			}
		})
	}
}

func TestGraphCachePreservesChainedResults(t *testing.T) {
	r, _ := graphFixture(t)
	r.Config.Records[1].Results = nil
	r.Config.Records[1].ChainedResults = []cachetypes.ChainedResult{{LayerIndexes: []int{0, 1}, CreatedAt: r.CreatedAt}}
	if err := r.Validate(r.Root); err != nil {
		t.Fatal(err)
	}
}

func TestGraphCacheMissingOrCorruptImageCannotPublish(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "corrupt"}[corrupt], func(t *testing.T) {
			r, source := graphFixture(t)
			path := filepath.Join(source, "blobs", "sha256", r.Results[1].Image.Digest.Encoded())
			if corrupt {
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(r.Results[1].Image.Size)), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			store, err := NewLocalStore(context.Background(), t.TempDir(), t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.PutGraph(context.Background(), r.Root, r, source); err == nil {
				t.Fatal("published incomplete graph")
			}
			if _, _, err := store.LookupGraph(context.Background(), r.Root); !errors.Is(err, ErrMiss) {
				t.Fatalf("failed publication became visible: %v", err)
			}
		})
	}
}

func TestGraphCacheCancellationAndTTL(t *testing.T) {
	r, source := graphFixture(t)
	ttl := time.Hour
	store, err := NewLocalStore(context.Background(), t.TempDir(), t.TempDir(), &ttl)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.PutGraph(canceled, r.Root, r, source); !errors.Is(err, context.Canceled) {
		t.Fatalf("put: %v", err)
	}
	if _, _, err := store.LookupGraph(canceled, r.Root); !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup: %v", err)
	}
	r.CreatedAt = time.Now().Add(-2 * time.Hour)
	if _, err := store.PutGraph(context.Background(), r.Root, r, source); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LookupGraph(context.Background(), r.Root); !errors.Is(err, ErrMiss) {
		t.Fatalf("expired lookup: %v", err)
	}
}

func TestGraphCacheRegistryRestoresIndependentLayout(t *testing.T) {
	ctx := context.Background()
	r, source := graphFixture(t)
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	repository := strings.TrimPrefix(server.URL, "http://") + "/coopr/graph-cache"
	writer, err := NewRegistryStore(resolver, repository, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.PutGraph(ctx, r.Root, r, source); err != nil {
		t.Fatal(err)
	}
	reader, err := NewRegistryStore(resolver, repository, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, layout, err := reader.LookupGraph(ctx, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(layout) }()
	if !reflect.DeepEqual(*got, r) {
		t.Fatal("registry lost complete graph metadata")
	}
	local, err := NewLocalStore(ctx, t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.PutGraph(ctx, r.Root, *got, layout); err != nil {
		t.Fatal(err)
	}
	if _, restored, err := local.LookupGraph(ctx, r.Root); err != nil {
		t.Fatal(err)
	} else {
		_ = os.RemoveAll(restored)
	}
}

func TestGraphCacheCorruptStoredResultIsRejected(t *testing.T) {
	for _, sidecar := range []bool{false, true} {
		t.Run(map[bool]string{false: "image", true: "sidecar"}[sidecar], func(t *testing.T) {
			r, source := graphFixture(t)
			cacheRoot := t.TempDir()
			store, err := NewLocalStore(context.Background(), cacheRoot, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.PutGraph(context.Background(), r.Root, r, source); err != nil {
				t.Fatal(err)
			}
			desc := r.Results[1].Image
			if sidecar {
				desc, err = r.Results[1].Descriptor()
				if err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(cacheRoot, "blobs", "sha256", desc.Digest.Encoded())
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(desc.Size)), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.LookupGraph(context.Background(), r.Root); err == nil {
				t.Fatal("accepted corrupt stored result")
			}
		})
	}
}

func TestGraphResultIdentityIncludesRestorationMetadata(t *testing.T) {
	r, source := graphFixture(t)
	r.Results[1].Image = r.Results[0].Image
	r.Results[1].BaseImage = nil
	r.Results[1].Metadata = json.RawMessage(`{"root_mode":448}`)
	desc, err := r.Results[1].Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	r.Config.Layers[1].Blob = desc.Digest
	if desc.Digest == r.Config.Layers[0].Blob {
		t.Fatal("restoration metadata not part of result identity")
	}
	store, err := NewLocalStore(context.Background(), t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutGraph(context.Background(), r.Root, r, source); err != nil {
		t.Fatal(err)
	}
	got, layout, err := store.LookupGraph(context.Background(), r.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(layout) }()
	if len(got.Results) != 2 || !bytes.Equal(got.Results[1].Metadata, r.Results[1].Metadata) {
		t.Fatal("lost alternate metadata for identical image")
	}
}

func TestGraphCacheRejectsDeepDependencies(t *testing.T) {
	r, _ := graphFixture(t)
	r.Config.Records = nil
	for i := 0; i <= maxGraphDepth; i++ {
		record := cachetypes.CacheRecord{Digest: digest.FromString("operation " + strings.Repeat("x", i))}
		if i > 0 {
			record.Inputs = [][]cachetypes.CacheInput{{{LinkIndex: i - 1}}}
		}
		r.Config.Records = append(r.Config.Records, record)
	}
	r.Config.Records[maxGraphDepth].Results = []cachetypes.CacheResult{{LayerIndex: 0, CreatedAt: r.CreatedAt}, {LayerIndex: 1, CreatedAt: r.CreatedAt}}
	if err := r.Validate(r.Root); err == nil {
		t.Fatal("accepted overly deep graph")
	}
}

type graphTagFailure struct {
	oras.Target
	err error
}

func (t graphTagFailure) Tag(context.Context, v1.Descriptor, string) error { return t.err }

func TestGraphCachePublicationFailureIsReturned(t *testing.T) {
	r, source := graphFixture(t)
	target, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("declared cache publication failed")
	store := &OCIStore{target: graphTagFailure{Target: target, err: failure}, stagingDir: t.TempDir()}
	if _, err := store.PutGraph(context.Background(), r.Root, r, source); !errors.Is(err, failure) {
		t.Fatalf("publication error: %v", err)
	}
	if _, _, err := store.LookupGraph(context.Background(), r.Root); !errors.Is(err, ErrMiss) {
		t.Fatalf("failed tag visible: %v", err)
	}
}
