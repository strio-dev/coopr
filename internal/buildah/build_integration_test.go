package buildah

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
		// Rootless reexec needs containers/storage's cgo constructor. Without
		// it, the child keeps its original UID and repeats the bootstrap.
		if !testCGOEnabled {
			fmt.Fprintln(os.Stderr, "live Buildah tests require CGO_ENABLED=1 for rootless namespace bootstrap; use -short for tests without native execution")
			os.Exit(2)
		}
		if InitRootlessReexec() {
			return
		}
	}
	os.Exit(m.Run())
}

func TestBuildPlanScratchOCIReferenceAndConfiguration(t *testing.T) {
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
	result, err := BuildPlan(ctx, testPlan(t, `from "scratch" {
 copy "proof" "/proof"
 env COOPR_BUILDER="buildah"
}`), PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: contextDir, Isolation: "rootless",
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
	if got := index.Manifests[0].Annotations[v1.AnnotationRefName]; got != "coopr-scratch" {
		t.Fatalf("OCI reference = %q", got)
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
	if !slices.Contains(image.Config.Env, "COOPR_BUILDER=buildah") {
		t.Fatalf("OCI image env = %#v", image.Config.Env)
	}
}
