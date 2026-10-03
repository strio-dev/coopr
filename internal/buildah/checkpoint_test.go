package buildah

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	ocilayout "go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestCheckpointValidatesInputs(t *testing.T) {
	ctx := context.Background()
	// This deliberately tests Checkpoint's nil-context guard.
	if _, _, err := Checkpoint(nil, nil, nil, upstream.BuilderOptions{}); err == nil || !strings.Contains(err.Error(), "context") { //nolint:staticcheck
		t.Fatalf("nil context error = %v", err)
	}
	if _, _, err := Checkpoint(ctx, nil, nil, upstream.BuilderOptions{}); err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("nil store error = %v", err)
	}
}

func TestCheckpointCreatesOneLayerPerFilesystemOperation(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah checkpoint build")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"first": "one\n", "second": "two\n"} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	store, err := storage.GetStore(storage.StoreOptions{
		GraphDriverName: "vfs",
		GraphRoot:       filepath.Join(root, "graph"),
		RunRoot:         filepath.Join(root, "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(true); err != nil {
			t.Errorf("shutdown checkpoint store: %v", err)
		}
	})
	network, err := newNetworkInterface(store)
	if err != nil {
		t.Fatal(err)
	}
	builderOptions := upstream.BuilderOptions{
		FromImage:        "scratch",
		PullPolicy:       define.PullNever,
		Isolation:        define.IsolationChroot,
		Format:           define.OCIv1ImageManifest,
		NetworkInterface: network,
		SystemContext: &types.SystemContext{
			BigFilesTemporaryDir: root,
		},
	}
	builder, err := upstream.NewBuilder(ctx, store, builderOptions)
	if err != nil {
		t.Fatal(err)
	}
	deleteBuilder := true
	defer func() {
		if deleteBuilder {
			if err := builder.Delete(); err != nil {
				t.Errorf("delete checkpoint builder: %v", err)
			}
		}
	}()

	builder.SetEnv("COOPR_CHECKPOINT", "preserved")
	if err := builder.Add("/first", false, upstream.AddAndCopyOptions{ContextDir: contextDir}, "first"); err != nil {
		t.Fatal(err)
	}
	builder, firstImageID, err := Checkpoint(ctx, store, builder, builderOptions)
	if err != nil {
		t.Fatal(err)
	}
	if firstImageID == "" {
		t.Fatal("checkpoint returned an empty image ID")
	}
	if err := builder.Add("/second", false, upstream.AddAndCopyOptions{ContextDir: contextDir}, "second"); err != nil {
		t.Fatal(err)
	}

	layout := filepath.Join(root, "layout")
	destination, err := ocilayout.NewReference(layout, "checkpoint-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := builder.Commit(ctx, destination, upstream.CommitOptions{
		PreferredManifestType: define.OCIv1ImageManifest,
		SystemContext:         builderOptions.SystemContext,
	}); err != nil {
		t.Fatal(err)
	}
	if err := builder.Delete(); err != nil {
		t.Fatal(err)
	}
	deleteBuilder = false

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
	if len(manifest.Layers) != 2 {
		t.Fatalf("OCI layer count = %d, want 2", len(manifest.Layers))
	}
	configData, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", manifest.Config.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var image v1.Image
	if err := json.Unmarshal(configData, &image); err != nil {
		t.Fatal(err)
	}
	if len(image.RootFS.DiffIDs) != 2 {
		t.Fatalf("OCI diff ID count = %d, want 2", len(image.RootFS.DiffIDs))
	}
	if len(image.Config.Env) != 1 || image.Config.Env[0] != "COOPR_CHECKPOINT=preserved" {
		t.Fatalf("OCI image env = %#v", image.Config.Env)
	}
}
