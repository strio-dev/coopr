package buildah

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

func TestBuildPlanContextCopyAndAddCache(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	fixed := time.Unix(1_700_000_000, 0)
	write := func(name, contents string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, fixed, fixed); err != nil {
			t.Fatal(err)
		}
	}
	write("message", "hello\nworld\n")
	write("other", "one")
	store := cacheTestStore(root)
	build := func(name, contents string) (int, []digest.Digest) {
		t.Helper()
		plan := testPlan(t, "from \"scratch\"\ncopy \"message\" \"/message\" chmod=\"0755\"\nadd \"other\" \"/other\"\n")
		layout := filepath.Join(root, name)
		if _, err := BuildPlan(ctx, plan, PlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout, Reference: name},
		}); err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		if len(manifest.Layers) != 2 {
			t.Fatalf("%s layers = %d, want 2", name, len(manifest.Layers))
		}
		first := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
		if got := readLayerFile(t, first, "message"); got != "hello\nworld\n" {
			t.Fatalf("%s COPY = %q", name, got)
		}
		second := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
		if got := readLayerFile(t, second, "other"); got != contents {
			t.Fatalf("%s ADD = %q, want %q", name, got, contents)
		}
		return instructionCacheRecordCount(t, store), []digest.Digest{manifest.Layers[0].Digest, manifest.Layers[1].Digest}
	}
	coldRecords, coldLayers := build("cold", "one")
	if coldRecords != 2 {
		t.Fatalf("cold records = %d, want 2", coldRecords)
	}
	warmRecords, warmLayers := build("warm", "one")
	if warmRecords != coldRecords || !digestSlicesEqual(warmLayers, coldLayers) {
		t.Fatalf("warm records/layers = %d/%v, want %d/%v", warmRecords, warmLayers, coldRecords, coldLayers)
	}
	write("other", "two")
	changedRecords, changedLayers := build("changed", "two")
	if changedRecords != coldRecords+1 || changedLayers[1] == coldLayers[1] {
		t.Fatalf("changed ADD records/layers = %d/%v, want a new second layer", changedRecords, changedLayers)
	}
}
