package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/moby/buildkit/solver"
)

func TestCopyPreservesOpaqueParent(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	for _, name := range []string{"parent", "child"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	store := cacheTestStore(filepath.Join(root, "store"))
	plan := testPlan(t, `from "scratch" {
    layer { copy "parent" "/parent"; env grouped="yes"; }
    copy "child" "/child"
    label final="yes"
}
`)
	for _, attempt := range []string{"cold", "warm", "changed", "forced"} {
		if attempt == "changed" {
			if err := os.WriteFile(filepath.Join(root, "child"), []byte("changed"), 0o640); err != nil {
				t.Fatal(err)
			}
		}
		layout := filepath.Join(root, attempt)
		if _, err := BuildPlan(ctx, plan, PlanOptions{Store: store, ContextDir: root, Isolation: "rootless", NoCache: attempt == "forced", Output: Output{Path: layout}}); err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != 2 {
			t.Fatalf("%s layers = %d, want 2", attempt, len(manifest.Layers))
		}
		first := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
		last := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
		if readLayerFile(t, first, "parent") != "parent" || layerContainsPath(t, first, "child") {
			t.Fatalf("%s COPY contaminated its parent layer", attempt)
		}
		want := "child"
		if attempt == "changed" || attempt == "forced" {
			want = "changed"
		}
		if got := readLayerFile(t, last, "child"); got != want {
			t.Fatalf("%s child = %q, want %q", attempt, got, want)
		}
		physical := 0
		for _, h := range image.History {
			if !h.EmptyLayer {
				physical++
			}
		}
		if physical != 2 || !slices.Contains(image.Config.Env, "grouped=yes") || image.Config.Labels["final"] != "yes" {
			t.Fatalf("%s history/config = %+v / %+v", attempt, image.History, image.Config)
		}
		// Opaque parents retain their established forced-cache semantics. Check
		// the exact advertised parent, rather than requiring a warm cache hit.
		assertCopyParentLayerCount(t, store, 1)
	}
}

func assertCopyParentLayerCount(t *testing.T, options StoreOptions, want int) {
	t.Helper()
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	containers, err := lease.store.Containers()
	if err != nil || len(containers) != 0 {
		t.Fatalf("working containers = %v: %v", containers, err)
	}
	dir := filepath.Join(lease.store.GraphRoot(), "coopr-graph-cache-v1")
	keys, err := newGraphCacheKeyStorage(t.Context(), filepath.Join(dir, "keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	results := &instructionResultStorage{ctx: t.Context(), store: lease.store, dir: filepath.Join(dir, "results")}
	seen := 0
	if err := keys.Walk(func(id string) error {
		return keys.WalkResults(id, func(result solver.CacheResult) error {
			r, err := results.read(result.ID)
			if err != nil {
				return err
			}
			if r.Boundary == "" && r.Entry.ImageID != "" {
				seen++
				if r.ParentLayers != want {
					return fmt.Errorf("COPY parent layers = %d, want %d", r.ParentLayers, want)
				}
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("COPY result was not recorded")
	}
}

func TestCopyResolvesNamedOwnerAndDestinationFromOpaqueParent(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	var content bytes.Buffer
	w := tar.NewWriter(&content)
	for _, entry := range []struct{ name, data, target string }{
		{"etc/passwd", "builder:x:20:30::/:/bin/false\n", ""},
		{"etc/group", "build:x:30:\n", ""},
		{"real/parent", "untouched", ""},
		{"destination", "", "/real"},
	} {
		header := &tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.data)), ModTime: time.Unix(1700000000, 0)}
		if entry.target != "" {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = entry.target
			header.Mode = 0o777
		}
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "parent.tar"), content.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "child"), []byte("selected"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := cacheTestStore(filepath.Join(root, "store"))
	plan := testPlan(t, `from "scratch" {
    layer { add "parent.tar" "/"; }
    copy "child" "/destination/copied" chown="builder:build" chmod="0640"
}
`)
	for _, name := range []string{"cold", "warm"} {
		layout := filepath.Join(root, name)
		if _, err := BuildPlan(ctx, plan, PlanOptions{Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}}); err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		if len(manifest.Layers) != 2 {
			t.Fatalf("%s layers = %d, want 2", name, len(manifest.Layers))
		}
		first := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
		last := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
		if layerContainsPath(t, first, "real/copied") || readLayerFile(t, first, "real/parent") != "untouched" {
			t.Fatal("COPY changed its immutable parent")
		}
		if readLayerFile(t, last, "real/copied") != "selected" {
			t.Fatal("COPY did not follow the parent destination symlink")
		}
		header := readLayerHeader(t, last, "real/copied")
		if header.Uid != 20 || header.Gid != 30 || header.Mode&0o777 != 0o640 {
			t.Fatalf("copied metadata = %+v", header)
		}
		assertCopyParentLayerCount(t, store, 1)
	}
}
