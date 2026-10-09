package buildah

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestMain(m *testing.M) {
	if InitReexec() {
		return
	}
	flag.Parse()
	if !testing.Short() && flag.Lookup("test.list").Value.String() == "" {
		if InitRootlessReexec() {
			return
		}
	}
	os.Exit(m.Run())
}

func TestBuildRunsAndCommitsOCIImage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah build in short mode")
	}
	base := os.Getenv("COOPR_TEST_BUILDAH_BASE")
	graphRoot := os.Getenv("COOPR_TEST_BUILDAH_GRAPHROOT")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: graphRoot}
	if base == "" || graphRoot == "" {
		store.GraphRoot = filepath.Join(root, "graph")
		store.GraphDriverName = "vfs"
		fixture := newLiveBusyBoxStorage(t, ctx, root, store)
		selection, found, err := nativeFixtureSelection(ctx, store, fixture.reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
		if err != nil || !found {
			t.Fatalf("preloaded fixture lookup found=%t: %v", found, err)
		}
		base = selection.ImageID
	}
	layout := filepath.Join(root, "layout")
	result, err := Build(ctx, Request{
		Store: store,
		Base:  base, Isolation: "rootless",
		Operations: []Operation{
			Run{Command: []string{"/bin/sh", "-c", "printf ready >/proof"}},
			Env{Name: "COOPR_BUILDER", Value: "buildah"},
		},
		Output: Output{Path: layout, Reference: "coopr-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ManifestDigest == "" {
		t.Fatal("commit returned no manifest digest")
	}
	data, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("OCI layout index: %v, %+v", err, index.Manifests)
	}
	if got := index.Manifests[0].Annotations[v1.AnnotationRefName]; got != "coopr-test" {
		t.Fatalf("OCI reference = %q", got)
	}
}

func TestBuildScratchCopiesAndCommitsOCIImage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("direct-buildah\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "layout")
	result, err := Build(ctx, Request{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		Base: "scratch", ContextDir: contextDir, Isolation: "rootless",
		Operations: []Operation{
			Copy{Sources: []string{"proof"}, Destination: "/proof"},
			Env{Name: "COOPR_BUILDER", Value: "buildah"},
		},
		Output: Output{Path: layout, Reference: "coopr-scratch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ManifestDigest == "" {
		t.Fatal("commit returned no manifest digest")
	}
	indexData, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("OCI layout index: %v, %+v", err, index.Manifests)
	}
	manifestData, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", index.Manifests[0].Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("scratch COPY layer count = %d, want 1", len(manifest.Layers))
	}
	configData, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", manifest.Config.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var image v1.Image
	if err := json.Unmarshal(configData, &image); err != nil {
		t.Fatal(err)
	}
	if len(image.Config.Env) != 1 || image.Config.Env[0] != "COOPR_BUILDER=buildah" {
		t.Fatalf("OCI image env = %#v", image.Config.Env)
	}
}
