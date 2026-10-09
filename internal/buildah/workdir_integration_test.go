package buildah

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestBuildCreatesChainedRelativeWorkDir(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("workdir\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "layout")
	_, err := Build(ctx, Request{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		Base: "scratch", ContextDir: contextDir, Isolation: "rootless",
		Operations: []Operation{
			WorkDir("/opt"), WorkDir("application"), WorkDir("nested/.."),
			Copy{Sources: []string{"proof"}, Destination: "/opt/application/proof"},
		},
		Output: Output{Path: layout, Reference: "workdir-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	indexData, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("OCI index: %v, %+v", err, index.Manifests)
	}
	manifestData, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", index.Manifests[0].Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	configData, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", manifest.Config.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var image v1.Image
	if err := json.Unmarshal(configData, &image); err != nil {
		t.Fatal(err)
	}
	if image.Config.WorkingDir != "/opt/application" {
		t.Fatalf("OCI working directory = %q, want /opt/application", image.Config.WorkingDir)
	}
}
