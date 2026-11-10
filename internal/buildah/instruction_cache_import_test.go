package buildah

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/strio-dev/coopr/internal/oci"
)

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
	if result.CacheStats.Stored != 1 || result.CacheStats.Errors != 0 {
		t.Fatalf("native base cache export: %+v", result.CacheStats)
	}
	if got := localComponentLastFile(t, result.Layout, "tail"); got != "new instruction\n" {
		t.Fatalf("consumer payload = %q", got)
	}
}
