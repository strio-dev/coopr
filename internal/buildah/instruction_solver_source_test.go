package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"github.com/strio-dev/coopr/internal/oci"
	orasoci "oras.land/oras-go/v2/content/oci"
)

type instructionSourceStore struct {
	solverTestStore
	manifests map[string][]byte
}

func (s instructionSourceStore) ImageBigData(id, key string) ([]byte, error) {
	value, ok := s.manifests[id]
	if !ok {
		return nil, errors.New("unknown test image")
	}
	return value, nil
}

func instructionSourceFixture(t *testing.T) (*cache.OCIStore, string, cache.GraphRecord, map[string][]byte) {
	t.Helper()
	ctx := t.Context()
	path := t.TempDir()
	layout, err := orasoci.New(path)
	if err != nil {
		t.Fatal(err)
	}
	metadata, results := instructionGraphFixture(t)
	manifests := map[string][]byte{}
	for i := range results {
		configData, err := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Config: v1.ImageConfig{Labels: map[string]string{"variant": results[i].ID}}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{}}})
		if err != nil {
			t.Fatal(err)
		}
		config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
		imageData, err := json.Marshal(oci.VersionedManifest(config, nil, ""))
		if err != nil {
			t.Fatal(err)
		}
		image := oci.Descriptor(v1.MediaTypeImageManifest, imageData)
		for _, blob := range []struct {
			descriptor v1.Descriptor
			data       []byte
		}{{config, configData}, {image, imageData}} {
			if err := layout.Push(ctx, blob.descriptor, bytes.NewReader(blob.data)); err != nil {
				t.Fatal(err)
			}
		}
		value := instructionSolverResult{Entry: instructionCacheEntry{ManifestDigest: image.Digest, RootMetadata: &PackageRootMetadata{Mode: 0o755}}, CreatedAt: metadata.Nodes[1].Results[i].CreatedAt}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		results[i] = cache.GraphResult{ID: value.ID(), Image: image, Metadata: raw}
		metadata.Nodes[1].Results[i].ID = value.ID()
		manifests[image.Digest.String()] = imageData
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	record := cache.GraphRecord{Version: cache.GraphRecordVersion, Root: digest.FromString("source relay root"), CreatedAt: time.Now().UTC(), Results: results, Metadata: raw}
	cachePath := t.TempDir()
	store, err := cache.NewLocalStore(ctx, cachePath, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutGraph(ctx, record.Root, record, path); err != nil {
		t.Fatal(err)
	}
	return store, cachePath, record, manifests
}

func instructionSourceEngine(t *testing.T, store *cache.OCIStore, binding digest.Digest, manifests map[string][]byte) *instructionSolver {
	t.Helper()
	native := instructionSourceStore{solverTestStore: solverTestStore{root: t.TempDir()}, manifests: manifests}
	engine, err := newInstructionSolver(t.Context(), native, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &instructionGraphPortable{engine: engine, executor: &graphExecutor{store: native}, cache: &portableInstructionCache{stagingDir: t.TempDir(), readStores: []instructionCacheStore{store}, readBindingIDs: []digest.Digest{binding}}, roots: map[digest.Digest]instructionGraphRoot{}, read: map[digest.Digest]bool{}}
	engine.portable = p
	t.Cleanup(func() {
		if err := engine.close(); err != nil {
			t.Error(err)
		}
	})
	return engine
}

func TestInstructionSelectedGraphSourceFetchAndCompleteExport(t *testing.T) {
	store, _, record, manifests := instructionSourceFixture(t)
	binding := instructionReadBindingID(CacheSpec{Transport: "registry", Reference: "localhost/test"})
	engine := instructionSourceEngine(t, store, binding, manifests)
	if err := engine.preparePortable(t.Context(), engine.source(record.Root, instructionSolverResult{}, false)); err != nil {
		t.Fatal(err)
	}
	if len(engine.portable.sources) != 1 {
		t.Fatal("source metadata not retained")
	}
	var imports atomic.Int32
	for _, result := range record.Results {
		pending := engine.results.lazy.get(result.ID)
		if pending == nil {
			t.Fatal("missing retained alternative")
		}
		pending.importImage = func(ctx context.Context) (string, error) {
			imports.Add(1)
			if _, _, err := pending.ensureLayout(ctx); err != nil {
				return "", err
			}
			return result.Image.Digest.String(), nil
		}
		if _, err := engine.results.read(result.ID); err == nil {
			t.Fatal("metadata admission imported a native result")
		}
	}
	selected := record.Results[1]
	if _, err := engine.results.Load(t.Context(), solver.CacheResult{ID: selected.ID}); err != nil {
		t.Fatal(err)
	}
	if imports.Load() != 1 {
		t.Fatal("selection did not import exactly one native result")
	}
	// Export all retained alternatives directly from verified source layouts.
	// The selected result is already native, so falling back to its native
	// transport instead of retaining its OCI bytes fails on this fake store.
	export, err := newInstructionGraphExport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(export.dir) }()
	for _, result := range record.Results {
		got, err := export.result(t.Context(), engine.portable, result.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, result) {
			t.Fatal("export changed retained alternative", got, result)
		}
		if exists, err := export.layout.Exists(t.Context(), result.Image); err != nil || !exists {
			t.Fatal("complete export omitted result", err)
		}
	}
	if imports.Load() != 1 {
		t.Fatal("full export unnecessarily imported native historical images")
	}
}

func TestInstructionGraphSourceWorkerHandoffSurvivesChildAndMetadataRemoval(t *testing.T) {
	store, cachePath, record, manifests := instructionSourceFixture(t)
	binding := instructionReadBindingID(CacheSpec{Transport: "oci-layout", Reference: cachePath})
	child := instructionSourceEngine(t, store, binding, manifests)
	if err := child.preparePortable(t.Context(), child.source(record.Root, instructionSolverResult{}, false)); err != nil {
		t.Fatal(err)
	}
	relays, err := child.relayInstructionSources([]digest.Digest{binding})
	if err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(t.TempDir(), "result.json")
	summary, err := writeInstructionGraphSources(resultPath, relays)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeWorkerJSON(resultPath, stageWorkerResponse{CleanupComplete: true, GraphSources: summary}); err != nil {
		t.Fatal(err)
	}
	response, err := readStageWorkerResponse(resultPath)
	if proofErr := requireWorkerCleanup(response.CleanupComplete, err); proofErr != nil {
		t.Fatal(proofErr)
	}
	response.graphSources, err = readInstructionGraphSources(resultPath, response.GraphSources)
	if err != nil {
		t.Fatal(err)
	}
	// Metadata must be owned in memory before supervisor cleanup removes files.
	if err := os.Remove(filepath.Join(filepath.Dir(resultPath), instructionGraphSourcesName)); err != nil {
		t.Fatal(err)
	}
	if err := child.closePortable(); err != nil {
		t.Fatal(err)
	}
	layout, err := orasoci.New(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	layout.AutoGC = false // Source metadata disappears independently of payloads.
	snapshot := relays[0].Metadata
	var manifest v1.Manifest
	if err := json.Unmarshal(snapshot.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	descriptors := []v1.Descriptor{snapshot.Artifact, manifest.Config, oci.Descriptor(v1.MediaTypeImageManifest, snapshot.Manifest)}
	for _, descriptor := range descriptors {
		if err := layout.Delete(t.Context(), descriptor); err != nil {
			t.Fatal(err)
		}
	}
	parent := instructionSourceEngine(t, store, binding, manifests)
	if err := parent.acceptInstructionSources(t.Context(), response.graphSources); err != nil {
		t.Fatal(err)
	}
	export, err := newInstructionGraphExport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(export.dir) }()
	for _, result := range record.Results {
		if _, err := parent.results.read(result.ID); err == nil {
			t.Fatal("worker handoff imported historical image")
		}
		if _, err := export.result(t.Context(), parent.portable, result.ID); err != nil {
			t.Fatal("parent lost retained alternative after child cleanup", err)
		}
	}
	if len(export.results) != len(record.Results) {
		t.Fatal("incomplete historical export")
	}
}

func TestInstructionGraphSourceCompleteExportFailsWhenRetainedPayloadDisappears(t *testing.T) {
	store, cachePath, record, manifests := instructionSourceFixture(t)
	binding := instructionReadBindingID(CacheSpec{Transport: "oci-layout", Reference: cachePath})
	engine := instructionSourceEngine(t, store, binding, manifests)
	if err := engine.preparePortable(t.Context(), engine.source(record.Root, instructionSolverResult{}, false)); err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.New(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	source.AutoGC = false
	if err := source.Delete(t.Context(), record.Results[1].Image); err != nil {
		t.Fatal(err)
	}
	export, err := newInstructionGraphExport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(export.dir) }()
	if _, err := export.result(t.Context(), engine.portable, record.Results[0].ID); err != nil {
		t.Fatal("available selected input was lost", err)
	}
	if _, err := export.result(t.Context(), engine.portable, record.Results[1].ID); err == nil {
		t.Fatal("complete export silently omitted unavailable retained image")
	}
	if _, exists := export.results[record.Results[1].ID]; exists {
		t.Fatal("failed export claimed complete retained result")
	}
	for _, result := range record.Results {
		if _, err := engine.results.read(result.ID); err == nil {
			t.Fatal("export failure imported a native historical image")
		}
	}
}
