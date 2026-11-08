package buildah

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildCopyAndAddParentsPreservePivotSuffix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah parents build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.MkdirAll(filepath.Join(contextDir, "prefix", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "prefix", "nested", "proof"), []byte("parents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, testPlan(t, `from "scratch" {
 copy "prefix/./nested/proof" "/copied/" parents="true"
 add "prefix/./nested/proof" "/added/" parents="true"
}`), PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: layout, Reference: "coopr-parents"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 2 {
		t.Fatalf("parents build layers = %d, want COPY and ADD layers", len(manifest.Layers))
	}
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()), "copied/nested/proof"); got != "parents\n" {
		t.Fatalf("COPY --parents contents = %q", got)
	}
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded()), "added/nested/proof"); got != "parents\n" {
		t.Fatalf("ADD --parents contents = %q", got)
	}
}

func TestBuildLocalCopyPolicyMatchesContainerfileFrontend(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah COPY policy build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	for _, directory := range []string{"first/nested", "second/nested"} {
		if err := os.MkdirAll(filepath.Join(contextDir, directory), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"drop.tmp", "keep.txt"} {
			if err := os.WriteFile(filepath.Join(contextDir, directory, name), []byte(name+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, mode := range map[string]os.FileMode{"copy-mode": 0o4755, "add-mode": 0o2755} {
		path := filepath.Join(contextDir, name)
		if err := os.WriteFile(path, []byte(name+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, testPlan(t, `from "scratch" {
 copy "first" "second" "/filtered/" { exclude "nested/*.tmp" }
 copy "copy-mode" "/copy-mode"
 add "add-mode" "/add-mode"
}`), PlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: layout, Reference: "coopr-copy-policy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 3 {
		t.Fatalf("policy layers = %d, want three", len(manifest.Layers))
	}
	blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	for _, name := range []string{"filtered/first/nested/drop.tmp", "filtered/second/nested/drop.tmp"} {
		if layerContainsPath(t, blob, name) {
			t.Fatalf("source-relative COPY exclude retained %q", name)
		}
	}
	for index, name := range []string{"copy-mode", "add-mode"} {
		blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[index+1].Digest.Encoded())
		if got := readLayerHeader(t, blob, name).Mode & 0o7777; got != 0o755 {
			t.Fatalf("%s mode = %#o, want 0755", name, got)
		}
	}
}
