package buildah

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/strio-dev/coopr/internal/cache"
	"github.com/strio-dev/coopr/internal/oci"
)

func TestInstructionGraphMissingRootMetadataRebuilds(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "cache recovery\n")
	const source = `from "scratch" { copy "payload" "/payload"; }`
	cold, _ := f.build(t, "cold", source)
	if cold.CacheStats.Stored != 1 || cold.CacheStats.Errors != 0 {
		t.Fatalf("cold cache export: %+v", cold.CacheStats)
	}
	roots := instructionGraphArtifactResults(t, f.options.CacheLocalDir)
	if len(roots) != 1 {
		t.Fatalf("expected one cached operation, got %d", len(roots))
	}
	store, err := cache.NewLocalStore(f.ctx, f.options.CacheLocalDir, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for root := range roots {
		record, layout, err := store.LookupGraph(f.ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.RemoveAll(layout) }()
		metadata, err := record.NativeMetadata(root)
		if err != nil {
			t.Fatal(err)
		}
		// Leave the image closure and identities valid, but omit the root
		// attributes required to restore the operation's filesystem.
		for i := range record.Results {
			result := &record.Results[i]
			var value instructionSolverResult
			if err := json.Unmarshal(result.Metadata, &value); err != nil {
				t.Fatal(err)
			}
			if value.Entry.RootMetadata == nil {
				t.Fatal("cold export did not capture root metadata")
			}
			value.Entry.RootMetadata = nil
			oldID := result.ID
			result.ID = value.ID()
			result.Metadata, err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			for n := range metadata.Nodes {
				for r := range metadata.Nodes[n].Results {
					if metadata.Nodes[n].Results[r].ID == oldID {
						metadata.Nodes[n].Results[r].ID = result.ID
					}
				}
			}
		}
		record.Metadata, err = json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutGraph(f.ctx, root, *record, layout); err != nil {
			t.Fatal(err)
		}
	}
	f.options.Store = cacheTestStore(filepath.Join(f.root, "rebuild-store"))
	rebuilt, layout := f.build(t, "rebuilt", source)
	if rebuilt.CacheStats.Errors == 0 || rebuilt.CacheStats.Misses != 1 || rebuilt.CacheStats.Hits != 0 || rebuilt.CacheStats.Stored != 1 {
		t.Fatalf("incomplete cache entry did not rebuild: %+v", rebuilt.CacheStats)
	}
	if got := localComponentLastFile(t, layout, "payload"); got != "cache recovery\n" {
		t.Fatalf("rebuilt payload = %q", got)
	}
	f.options.Store = cacheTestStore(filepath.Join(f.root, "warm-store"))
	warm, layout := f.build(t, "warm", source)
	if warm.CacheStats.Hits != 1 || warm.CacheStats.Misses != 0 || warm.CacheStats.Errors != 0 {
		t.Fatalf("rebuilt cache entry did not restore cleanly: %+v", warm.CacheStats)
	}
	if got := localComponentLastFile(t, layout, "payload"); got != "cache recovery\n" {
		t.Fatalf("restored payload = %q", got)
	}
}

func TestInstructionCacheImportRetainsExactCompressedManifest(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "compressed cache source\n")
	options := f.options
	options.Output = Output{Path: filepath.Join(f.root, "compressed-source"), CompressionFormat: "gzip"}
	cold, err := BuildPlan(f.ctx, testPlan(t, `from "scratch" { copy "payload" "/payload"; }`), options)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := oci.LayoutRoot(cold.Layout)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(cacheTestStore(filepath.Join(f.root, "fresh")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	id, err := importInstructionCacheImage(f.ctx, lease.store, options.SystemContext, cold.Layout, selected)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := retainedLayerIDs(lease.store, id)
	if err != nil || len(retained) != 1 {
		t.Fatalf("exact compressed layers not retained: %v, %v", retained, err)
	}
	if err := os.RemoveAll(cold.Layout); err != nil {
		t.Fatal(err)
	}
	exported, err := exportStoredImageVariantRaw(f.ctx, lease.store, id, Output{Path: filepath.Join(f.root, "recovered")}, options.SystemContext, &selected.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if exported.ManifestDigest != selected.Digest.String() {
		t.Fatalf("cache import changed manifest: %s != %s", exported.ManifestDigest, selected.Digest)
	}
	if got := localComponentLastFile(t, exported.Layout, "payload"); got != "compressed cache source\n" {
		t.Fatalf("restored payload = %q", got)
	}
}

func TestInstructionGraphCachesFromNativeCompressedSource(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "base source\n")
	f.write(t, "tail", "new instruction\n")
	options := f.options
	options.Output = Output{Path: filepath.Join(f.root, "base-source"), CompressionFormat: "gzip"}
	base, err := BuildPlan(f.ctx, testPlan(t, `from "scratch" { copy "payload" "/payload"; }`), options)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := oci.LayoutRoot(base.Layout)
	if err != nil {
		t.Fatal(err)
	}
	store := cacheTestStore(filepath.Join(f.root, "native"))
	lease, err := acquireStore(store)
	if err != nil {
		t.Fatal(err)
	}
	id, importErr := ImportSelectedImage(f.ctx, lease.store, options.SystemContext, base.Layout, selected)
	closeErr := lease.Close()
	if importErr != nil || closeErr != nil {
		t.Fatalf("native source import: %v; %v", importErr, closeErr)
	}
	options.Store = store
	options.CacheLocalDir = filepath.Join(f.root, "graph-cache")
	options.Output = Output{Path: filepath.Join(f.root, "consumer")}
	plan := testPlan(t, "from \""+id+"\" { copy \"tail\" \"/tail\"; }")
	result, err := BuildPlan(f.ctx, plan, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.CacheStats.Stored != 2 || result.CacheStats.Errors != 0 {
		t.Fatalf("native base cache export: %+v", result.CacheStats)
	}
	if got := localComponentLastFile(t, result.Layout, "tail"); got != "new instruction\n" {
		t.Fatalf("consumer payload = %q", got)
	}
}
