package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func pushGraphTestJSON(t *testing.T, target oras.Target, mediaType string, value any) v1.Descriptor {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := oci.Descriptor(mediaType, data)
	if err := pushGraphBlob(t.Context(), target, descriptor, data); err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func graphTestMetadata(t *testing.T, target oras.Target, artifact v1.Descriptor) (v1.Index, v1.Manifest) {
	t.Helper()
	data, err := fetchVerified(t.Context(), target, artifact, maxGraphRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil || len(index.Manifests) == 0 {
		t.Fatal("invalid graph test index", err)
	}
	data, err = fetchVerified(t.Context(), target, index.Manifests[0], maxGraphRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return index, manifest
}

func graphFixture(t *testing.T) (GraphRecord, string) {
	t.Helper()
	dir := t.TempDir()
	target, err := orasoci.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	record := GraphRecord{Version: GraphRecordVersion, Root: digest.FromString("logical graph"), CreatedAt: time.Now().UTC()}
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
	}
	record.Results[1].BaseImage = &record.Results[0].Image
	source := digest.FromString("source").String()
	metadata := GraphMetadata{Roots: []string{"operation-key"}, Nodes: []GraphNode{
		{ID: source},
		{ID: "operation-key", Inputs: []GraphInput{{Source: source, Link: solver.CacheInfoLink{Digest: digest.FromString("operation"), Selector: digest.FromString("selected")}}}, Results: []solver.CacheResult{{ID: "first", CreatedAt: record.CreatedAt.Add(-time.Hour)}, {ID: "second", CreatedAt: record.CreatedAt}}},
	}}
	record.Metadata, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
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
	artifact, err := store.PutGraph(ctx, record.Root, record, source)
	if err != nil {
		t.Fatal(err)
	}
	index, manifest := graphTestMetadata(t, store.target, artifact)
	if artifact.MediaType != v1.MediaTypeImageIndex || len(index.Manifests) != len(graphImages(record))+1 || manifest.Layers == nil || len(manifest.Layers) != 0 {
		t.Fatal("cache root must index the metadata manifest and every image alternative")
	}
	config, err := fetchVerified(ctx, store.target, manifest.Config, maxGraphRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var retained GraphRecord
	if err := json.Unmarshal(config, &retained); err != nil || !reflect.DeepEqual(retained, record) {
		t.Fatal("record config did not retain all result metadata", err)
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
	if _, _, err := store.LookupGraph(ctx, digest.FromString("other")); !errors.Is(err, ErrMiss) {
		t.Fatalf("unknown graph: %v", err)
	}
}

func TestGraphCacheRejectsMalformedGraphs(t *testing.T) {
	base, _ := graphFixture(t)
	for _, test := range []struct {
		name   string
		change func(*GraphMetadata, *GraphRecord)
	}{
		{"node limit", func(m *GraphMetadata, _ *GraphRecord) {
			for len(m.Nodes) <= maxGraphEntries {
				m.Nodes = append(m.Nodes, GraphNode{ID: digest.FromString(fmt.Sprint("source ", len(m.Nodes))).String()})
			}
		}},
		{"root limit", func(m *GraphMetadata, _ *GraphRecord) {
			for len(m.Roots) <= maxGraphEntries {
				m.Roots = append(m.Roots, m.Roots[0])
			}
		}},
		{"node result limit", func(m *GraphMetadata, _ *GraphRecord) {
			for len(m.Nodes[1].Results) <= maxGraphEntries {
				m.Nodes[1].Results = append(m.Nodes[1].Results, m.Nodes[1].Results[0])
			}
		}},
		{"input alias limit", func(m *GraphMetadata, _ *GraphRecord) {
			for len(m.Nodes[1].Inputs) <= maxGraphEntries {
				m.Nodes[1].Inputs = append(m.Nodes[1].Inputs, m.Nodes[1].Inputs[0])
			}
		}},
		{"total link limit", func(m *GraphMetadata, _ *GraphRecord) {
			for i := range 4 {
				node := GraphNode{ID: "alias-group-" + strings.Repeat("x", i), Inputs: make([]GraphInput, maxGraphEntries)}
				for j := range node.Inputs {
					node.Inputs[j] = m.Nodes[1].Inputs[0]
				}
				m.Nodes = append(m.Nodes, node)
			}
		}},
		{"missing result", func(m *GraphMetadata, _ *GraphRecord) { m.Nodes[1].Results[0].ID = "absent" }},
		{"record cycle", func(m *GraphMetadata, _ *GraphRecord) {
			m.Nodes[0].Inputs = []GraphInput{{Source: m.Nodes[1].ID, Link: solver.CacheInfoLink{Digest: digest.FromString("cycle")}}}
		}},
		{"duplicate node", func(m *GraphMetadata, _ *GraphRecord) { m.Nodes[1].ID = m.Nodes[0].ID }},
		{"missing source", func(m *GraphMetadata, _ *GraphRecord) { m.Nodes[1].Inputs[0].Source = "absent" }},
		{"invalid source digest", func(m *GraphMetadata, _ *GraphRecord) {
			m.Nodes[0].ID = "invalid"
			m.Nodes[1].Inputs[0].Source = "invalid"
		}},
		{"negative input", func(m *GraphMetadata, _ *GraphRecord) { m.Nodes[1].Inputs[0].Link.Input = -1 }},
		{"input ordinal hole", func(m *GraphMetadata, _ *GraphRecord) { m.Nodes[1].Inputs[0].Link.Input = 1 }},
		{"input ordinal limit", func(m *GraphMetadata, _ *GraphRecord) { m.Nodes[1].Inputs[0].Link.Input = 65 }},
		{"unsupported output", func(m *GraphMetadata, _ *GraphRecord) { m.Nodes[1].Inputs[0].Link.Output = 1 }},
		{"invalid operation digest", func(m *GraphMetadata, _ *GraphRecord) { m.Nodes[1].Inputs[0].Link.Digest = "invalid" }},
		{"invalid selector", func(m *GraphMetadata, _ *GraphRecord) { m.Nodes[1].Inputs[0].Link.Selector = "invalid" }},
		{"conflicting operations", func(m *GraphMetadata, _ *GraphRecord) {
			input := m.Nodes[1].Inputs[0]
			input.Link.Digest = digest.FromString("other")
			m.Nodes[1].Inputs = append(m.Nodes[1].Inputs, input)
		}},
		{"missing root", func(m *GraphMetadata, _ *GraphRecord) { m.Roots = []string{"absent"} }},
		{"no roots", func(m *GraphMetadata, _ *GraphRecord) { m.Roots = nil }},
		{"duplicate identity", func(_ *GraphMetadata, r *GraphRecord) { r.Results[1].ID = r.Results[0].ID }},
		{"unreferenced result", func(m *GraphMetadata, _ *GraphRecord) { m.Nodes[1].Results = m.Nodes[1].Results[:1] }},
		{"missing result time", func(m *GraphMetadata, _ *GraphRecord) { m.Nodes[1].Results[0].CreatedAt = time.Time{} }},
		{"external result image", func(_ *GraphMetadata, r *GraphRecord) { r.Results[0].Image.URLs = []string{"https://example.invalid"} }},
		{"result metadata", func(_ *GraphMetadata, r *GraphRecord) { r.Results[0].Metadata = json.RawMessage(`{`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := base
			r.Results = append([]GraphResult(nil), base.Results...)
			var metadata GraphMetadata
			if err := json.Unmarshal(r.Metadata, &metadata); err != nil {
				t.Fatal(err)
			}
			test.change(&metadata, &r)
			var err error
			r.Metadata, err = json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Validate(r.Root); err == nil {
				t.Fatal("accepted malformed graph")
			}
		})
	}
	for _, raw := range []string{"", `{`, `{"unexpected":true}`, `{"roots":[]} {}`, strings.Repeat(" ", maxGraphRecordBytes+1)} {
		r := base
		r.Metadata = json.RawMessage(raw)
		if err := r.Validate(r.Root); err == nil {
			t.Fatal("accepted malformed native graph metadata")
		}
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
	for _, metadata := range []bool{false, true} {
		t.Run(map[bool]string{false: "image", true: "record config"}[metadata], func(t *testing.T) {
			r, source := graphFixture(t)
			cacheRoot := t.TempDir()
			store, err := NewLocalStore(context.Background(), cacheRoot, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			artifact, err := store.PutGraph(context.Background(), r.Root, r, source)
			if err != nil {
				t.Fatal(err)
			}
			desc := r.Results[1].Image
			if metadata {
				_, manifest := graphTestMetadata(t, store.target, artifact)
				desc = manifest.Config
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
	store, err := NewLocalStore(context.Background(), t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.PutGraph(context.Background(), r.Root, r, source)
	if err != nil {
		t.Fatal(err)
	}
	_, before := graphTestMetadata(t, store.target, first)
	r.Results[1].Metadata = json.RawMessage(`{"root_mode":448}`)
	after, err := store.PutGraph(context.Background(), r.Root, r, source)
	if err != nil {
		t.Fatal(err)
	}
	_, manifest := graphTestMetadata(t, store.target, after)
	if manifest.Config.Digest == before.Config.Digest || after.Digest == first.Digest {
		t.Fatal("restoration metadata not bound by the record config and artifact")
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

func TestGraphCacheBoundsDependencyDepth(t *testing.T) {
	r, _ := graphFixture(t)
	for _, depth := range []int{maxGraphDepth, maxGraphDepth + 1} {
		metadata := GraphMetadata{}
		for i := 0; i < depth; i++ {
			node := GraphNode{ID: digest.FromString("operation " + strings.Repeat("x", i)).String()}
			if i > 0 {
				node.Inputs = []GraphInput{{Source: metadata.Nodes[i-1].ID, Link: solver.CacheInfoLink{Digest: digest.FromString("operation")}}}
			}
			metadata.Nodes = append(metadata.Nodes, node)
		}
		last := &metadata.Nodes[depth-1]
		last.Results = []solver.CacheResult{{ID: "first", CreatedAt: r.CreatedAt}, {ID: "second", CreatedAt: r.CreatedAt}}
		metadata.Roots = []string{last.ID}
		var err error
		r.Metadata, err = json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Validate(r.Root); (err == nil) != (depth <= maxGraphDepth) {
			t.Fatalf("depth %d: %v", depth, err)
		}
		t.Logf("native graph depth=%d encodedMetadataBytes=%d", depth, len(r.Metadata))
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
