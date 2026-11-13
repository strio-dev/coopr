package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

type instructionSnapshotFetchCounter struct {
	oras.Target
	mu     sync.Mutex
	counts map[digest.Digest]int
}

func (source *instructionSnapshotFetchCounter) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	source.mu.Lock()
	source.counts[descriptor.Digest]++
	source.mu.Unlock()
	return source.Target.Fetch(ctx, descriptor)
}

func TestInstructionGraphSnapshotExportsExactSelectedResult(t *testing.T) {
	for _, damage := range []string{"missing", "corrupt"} {
		t.Run(damage, func(t *testing.T) {
			engine, _, sourceRecord, source, manifests := instructionGraphPublicationFixture(t)
			key := digest.FromString("selected snapshot root")
			// Select the older result explicitly. Newer timestamps must not replace
			// an exact solved output or cause an unused alternative to be retrieved.
			selected := sourceRecord.Results[0]
			addInstructionGraphPublicationRoot(t, engine, key, sourceRecord.Results...)
			unused := instructionGraphPublicationPayload(t, source, sourceRecord.Results[1], manifests)
			if damage == "missing" {
				if err := os.Remove(unused); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Chmod(unused, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(unused, []byte("invalid unused alternative"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			export, err := newInstructionGraphExport(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.RemoveAll(export.dir) }()
			record, layout, err := engine.portable.snapshot(t.Context(), key, engine.portable.roots[key], export)
			if err != nil {
				t.Fatal("unused alternative blocked selected export", err)
			}
			defer func() { _ = os.RemoveAll(filepath.Dir(layout)) }()
			if len(record.Results) != 1 || record.Results[0].ID != selected.ID {
				t.Fatalf("selected snapshot changed result: got %+v, want %s", record.Results, selected.ID)
			}
			retained := 0
			if err := engine.keys.WalkResults(solver.NewCacheKey(key, "", 0).ID, func(solver.CacheResult) error {
				retained++
				return nil
			}); err != nil || retained != len(sourceRecord.Results) {
				t.Fatalf("selected export discarded local associations: %d, %v", retained, err)
			}
			target, err := cache.NewLocalStore(t.Context(), t.TempDir(), t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := target.PutGraph(t.Context(), key, record, layout); err != nil {
				t.Fatal("selected image closure is incomplete", err)
			}
			restored, restoredLayout, err := target.LookupGraph(t.Context(), key)
			if err != nil || len(restored.Results) != 1 || restored.Results[0].ID != selected.ID {
				t.Fatalf("selected result did not survive complete copy: %+v, %v", restored, err)
			}
			defer func() { _ = os.RemoveAll(restoredLayout) }()
		})
	}
}

func TestInstructionGraphSnapshotRequiresSelectedAssociation(t *testing.T) {
	engine, _, record, _, _ := instructionGraphPublicationFixture(t)
	key := digest.FromString("associated snapshot root")
	addInstructionGraphPublicationRoot(t, engine, key, record.Results[0])
	selected := engine.portable.roots[key]
	selected.result = record.Results[1].ID
	export, err := newInstructionGraphExport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(export.dir) }()
	if _, layout, err := engine.portable.snapshot(t.Context(), key, selected, export); err == nil || layout != "" {
		t.Fatalf("unassociated selected result was replaced or published: %q, %v", layout, err)
	}
}

func TestInstructionGraphSnapshotAncestorSelectionUsesMetadata(t *testing.T) {
	engine, _, record, source, manifests := instructionGraphPublicationFixture(t)
	key := digest.FromString("ancestor alternatives")
	addInstructionGraphPublicationRoot(t, engine, key, record.Results...)
	// The fallback is a metadata choice, not a search that fetches every image.
	if err := os.Remove(instructionGraphPublicationPayload(t, source, record.Results[0], manifests)); err != nil {
		t.Fatal(err)
	}
	newest, err := engine.portable.snapshotResult(solver.NewCacheKey(key, "", 0).ID, "")
	if err != nil || newest == nil || newest.ID != record.Results[1].ID {
		t.Fatalf("ancestor selection differs from newest associated result: %+v, %v", newest, err)
	}
}

func TestInstructionGraphSnapshotDoesNotAttachResultToOtherMatchingKeys(t *testing.T) {
	engine, _, record, _, _ := instructionGraphPublicationFixture(t)
	root := addInstructionGraphSnapshotChain(t, engine, record.Results)
	selected := engine.portable.roots[root]
	const other = "other-matching-operation"
	if err := engine.keys.AddLink("snapshot-node-0", solver.CacheInfoLink{Digest: selected.vertex.key}, other); err != nil {
		t.Fatal(err)
	}
	if err := engine.results.lazy.add(nil, []instructionGraphNode{{ID: other, Results: []solver.CacheResult{{ID: record.Results[0].ID, CreatedAt: time.Unix(1, 0)}}}}); err != nil {
		t.Fatal(err)
	}
	export, err := newInstructionGraphExport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(export.dir) }()
	got, layout, err := engine.portable.snapshot(t.Context(), root, selected, export)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(layout)) }()
	var metadata instructionGraphMetadata
	if err := json.Unmarshal(got.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Roots) != 2 {
		t.Fatalf("fixture lacks multiple matching keys: %+v", metadata.Roots)
	}
	for _, node := range metadata.Nodes {
		if node.ID == other && len(node.Results) != 0 {
			t.Fatalf("result attached to an unrelated matching key: %+v", node)
		}
	}
}

func TestInstructionGraphSnapshotPreservesSharedDAGInputSlots(t *testing.T) {
	engine, _, record, _, _ := instructionGraphPublicationFixture(t)
	root := addInstructionGraphSnapshotChain(t, engine, record.Results)
	selected := engine.portable.roots[root]
	parent := selected.vertex.parent
	var value instructionSolverResult
	if err := json.Unmarshal(record.Results[0].Metadata, &value); err != nil {
		t.Fatal(err)
	}
	branch := &instructionVertex{parent: parent.parent, key: digest.FromString("snapshot branch"), completed: &value}
	sourceID := solver.NewCacheKey(parent.parent.key, "", 0).ID
	if err := engine.keys.AddLink(sourceID, solver.CacheInfoLink{Digest: branch.key}, "snapshot-branch"); err != nil {
		t.Fatal(err)
	}
	if err := engine.results.lazy.add(nil, []instructionGraphNode{{ID: "snapshot-branch", Results: []solver.CacheResult{{ID: value.ID(), CreatedAt: value.CreatedAt}}}}); err != nil {
		t.Fatal(err)
	}
	selected.vertex.dependencies = []*instructionVertex{branch, parent}
	for slot, input := range []string{"snapshot-node-0", "snapshot-branch", "snapshot-node-0"} {
		if err := engine.keys.AddLink(input, solver.CacheInfoLink{Input: solver.Index(slot), Digest: selected.vertex.key}, "snapshot-node-1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.keys.AddLink("snapshot-node-0", solver.CacheInfoLink{Digest: selected.vertex.key}, "primary-only"); err != nil {
		t.Fatal(err)
	}
	root = mustInstructionPortableDigest(t, selected.vertex)
	export, err := newInstructionGraphExport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(export.dir) }()
	got, layout, err := engine.portable.snapshot(t.Context(), root, selected, export)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := got.NativeMetadata(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.Nodes) != 4 || len(metadata.Roots) != 1 || metadata.Roots[0] != "snapshot-node-1" || len(got.Results) != 2 {
		t.Fatalf("shared DAG duplicated nodes/results or admitted a partial match: %+v, %d results", metadata, len(got.Results))
	}
	for _, node := range metadata.Nodes {
		if node.ID == "snapshot-node-1" {
			if len(node.Inputs) != 3 {
				t.Fatalf("lost ordered input links: %+v", node.Inputs)
			}
			for slot, source := range []string{"snapshot-node-0", "snapshot-branch", "snapshot-node-0"} {
				if !slices.Contains(node.Inputs, instructionGraphInput{Source: source, Link: solver.CacheInfoLink{Input: solver.Index(slot), Digest: selected.vertex.key}}) {
					t.Fatalf("input slot %d lost source %s: %+v", slot, source, node.Inputs)
				}
			}
		}
	}
	target, err := cache.NewLocalStore(t.Context(), t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.PutGraph(t.Context(), root, got, layout); err != nil {
		t.Fatal("DAG export lost its complete selected closure", err)
	}
}

func TestInstructionGraphSelectedPublicationConcurrentReplacement(t *testing.T) {
	first, target, record, _, _ := instructionGraphPublicationFixture(t)
	second, _, _, _, _ := instructionGraphPublicationFixture(t)
	key := digest.FromString("same-root selected publication")
	addInstructionGraphPublicationRoot(t, first, key, record.Results...)
	addInstructionGraphPublicationRoot(t, second, key, record.Results[1], record.Results[0])
	// Exercise the actual locked local cache rather than a test call recorder.
	first.portable.cache.writeStores = []instructionCacheStore{target.OCIStore}
	second.portable.cache.writeStores = []instructionCacheStore{target.OCIStore}
	if err := first.publishPortable(t.Context()); err != nil {
		t.Fatal(err)
	}
	errors := make(chan error, 2)
	var writers sync.WaitGroup
	for _, engine := range []*instructionSolver{first, second} {
		writers.Go(func() { errors <- engine.publishPortable(t.Context()) })
	}
	for range 4 {
		got, layout, err := target.LookupGraph(t.Context(), key)
		if err != nil {
			t.Error("concurrent cache reader observed an incomplete graph", err)
			continue
		}
		if len(got.Results) != 1 || got.Results[0].ID != record.Results[0].ID && got.Results[0].ID != record.Results[1].ID {
			t.Errorf("concurrent replacement merged or changed selected output: %+v", got.Results)
		}
		if err := os.RemoveAll(layout); err != nil {
			t.Error(err)
		}
	}
	writers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestInstructionGraphPublicationReusesInvocationLayout(t *testing.T) {
	engine, target, record, _, _ := instructionGraphPublicationFixture(t)
	root := addInstructionGraphSnapshotChain(t, engine, record.Results)
	ancestor := engine.portable.roots[root].vertex.parent
	prefix := mustInstructionPortableDigest(t, ancestor)
	engine.portable.roots[prefix] = instructionGraphRoot{vertex: ancestor, result: record.Results[0].ID}
	if err := engine.publishPortable(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(target.layouts) != 2 || target.layouts[0] != target.layouts[1] || !strings.HasPrefix(filepath.Base(filepath.Dir(target.layouts[0])), "graph-export-") {
		t.Fatalf("prefix publication did not share its invocation-owned export layout: %v", target.layouts)
	}
	if _, err := os.Stat(target.layouts[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publication retained its temporary export layout: %v", err)
	}
	if engine.portable.cache.stats.Stored != 2 {
		t.Fatalf("publication did not store both prefixes: %+v", engine.portable.cache.stats)
	}
	for _, selected := range []digest.Digest{prefix, root} {
		got, layout, err := target.LookupGraph(t.Context(), selected)
		if err != nil {
			t.Fatal("published prefix lacks its complete image closure", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(layout) })
		want := 1
		if selected == root {
			want = len(record.Results)
		}
		if len(got.Results) != want {
			t.Fatalf("shared layout leaked unrelated results into prefix %s: %+v", selected, got.Results)
		}
		for index, result := range got.Results {
			if result.ID != record.Results[index].ID {
				t.Fatalf("prefix %s changed its selected result: got %s, want %s", selected, result.ID, record.Results[index].ID)
			}
		}
	}
}

func TestInstructionGraphSnapshotBatchesSharedPayloads(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "selected ancestors", true: "corrupt selected ancestor"}[corrupt], func(t *testing.T) {
			engine := instructionSourceEngine(t, nil, "", nil)
			exports, err := newInstructionGraphExport(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(exports.dir); err != nil {
					t.Error(err)
				}
			})
			layerData := []byte("shared immutable snapshot payload")
			layer := oci.Descriptor(v1.MediaTypeImageLayer, layerData)
			if err := exports.layout.Push(t.Context(), layer, bytes.NewReader(layerData)); err != nil {
				t.Fatal(err)
			}
			var results []cache.GraphResult
			configs := map[string]v1.Descriptor{}
			for _, name := range []string{"first", "second"} {
				configData, err := json.Marshal(v1.Image{
					Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
					Config:   v1.ImageConfig{Labels: map[string]string{"variant": name}},
					RootFS:   v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}},
				})
				if err != nil {
					t.Fatal(err)
				}
				config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
				manifestData, err := json.Marshal(oci.VersionedManifest(config, []v1.Descriptor{layer}, ""))
				if err != nil {
					t.Fatal(err)
				}
				image := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
				for _, blob := range []struct {
					descriptor v1.Descriptor
					data       []byte
				}{{config, configData}, {image, manifestData}} {
					if err := exports.layout.Push(t.Context(), blob.descriptor, bytes.NewReader(blob.data)); err != nil {
						t.Fatal(err)
					}
				}
				value := instructionSolverResult{Entry: instructionCacheEntry{ManifestDigest: image.Digest}, CreatedAt: time.Unix(123, 0).UTC()}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				result := cache.GraphResult{ID: value.ID(), Image: image, Metadata: raw}
				results = append(results, result)
				configs[result.ID] = config
				exports.results[result.ID] = result
				if err := engine.results.lazy.add([]*instructionPortableResult{{value: value}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			key := addInstructionGraphSnapshotChain(t, engine, results)
			if corrupt {
				// Damage a selected ancestor; exporting a healthy root must not
				// turn the selected graph into an incomplete successful artifact.
				path := filepath.Join(exports.dir, "layout", "blobs", "sha256", configs[results[0].ID].Digest.Encoded())
				// ORAS makes blobs read-only; this fixture owns the temporary
				// layout and deliberately corrupts only its selected blob.
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("invalid retained config"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			counter := &instructionSnapshotFetchCounter{Target: exports.layout, counts: map[digest.Digest]int{}}
			exports.layout = counter
			record, layout, err := engine.portable.snapshot(t.Context(), key, engine.portable.roots[key], exports)
			if err != nil {
				t.Fatal(err)
			}
			if layout != filepath.Join(exports.dir, "layout") {
				t.Fatalf("snapshot created a separate payload layout: %s", layout)
			}
			if len(record.Results) != len(results) {
				t.Fatalf("snapshot lost selected ancestors: %+v", record.Results)
			}
			// The invocation already owns these exported bytes. Snapshotting only
			// selects metadata; the destination verifies the complete closure.
			if reads := counter.counts[layer.Digest]; reads != 0 {
				t.Fatalf("snapshot reread shared payload %d times, want zero", reads)
			}
			target, err := cache.NewLocalStore(t.Context(), t.TempDir(), t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := target.PutGraph(t.Context(), key, record, layout); corrupt {
				if err == nil {
					t.Fatal("corrupt selected ancestor was published")
				}
				if _, _, err := target.LookupGraph(t.Context(), key); !errors.Is(err, cache.ErrMiss) {
					t.Fatalf("failed publication exposed a graph root: %v", err)
				}
				return
			} else if err != nil {
				t.Fatal("complete selected image closure was rejected", err)
			}
			destination, err := orasoci.New(layout)
			if err != nil {
				t.Fatal(err)
			}
			verification, err := orasoci.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var images []v1.Descriptor
			for _, result := range results {
				images = append(images, result.Image)
			}
			if err := cache.CopyGraphImages(t.Context(), destination, verification, images); err != nil {
				t.Fatalf("snapshot omitted complete image closures: %v", err)
			}
		})
	}
}

func addInstructionGraphSnapshotChain(t *testing.T, engine *instructionSolver, results []cache.GraphResult) digest.Digest {
	t.Helper()
	parent := engine.source(digest.FromString("snapshot source"), instructionSolverResult{}, false)
	parentID := solver.NewCacheKey(parent.key, "", 0).ID
	for index, result := range results {
		var value instructionSolverResult
		if err := json.Unmarshal(result.Metadata, &value); err != nil {
			t.Fatal(err)
		}
		vertex := &instructionVertex{parent: parent, key: digest.FromString(fmt.Sprintf("snapshot operation %d", index)), completed: &value}
		id := fmt.Sprintf("snapshot-node-%d", index)
		if err := engine.keys.AddLink(parentID, solver.CacheInfoLink{Digest: vertex.key}, id); err != nil {
			t.Fatal(err)
		}
		if err := engine.results.lazy.add(nil, []instructionGraphNode{{ID: id, Results: []solver.CacheResult{{ID: result.ID, CreatedAt: value.CreatedAt}}}}); err != nil {
			t.Fatal(err)
		}
		parent, parentID = vertex, id
	}
	root := mustInstructionPortableDigest(t, parent)
	engine.portable.roots[root] = instructionGraphRoot{vertex: parent, result: results[len(results)-1].ID}
	return root
}
