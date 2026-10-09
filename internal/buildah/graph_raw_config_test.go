package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"coopr/internal/oci"

	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestBuildPlanKeepsImportedConfigThroughCheckpointsAndStageFork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah graph build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	baseConfig, err := json.Marshal(map[string]any{
		"architecture": platform.Architecture, "os": platform.OS,
		"rootfs":  map[string]any{"type": "layers", "diff_ids": []string{}},
		"history": []any{},
		"config": map[string]any{
			"Env": []string{"BASE=yes"}, "Cmd": []string{"old-command"},
			"Entrypoint": []string{"/old-entry"}, "Shell": []string{"/custom-shell", "-c"},
			"vendorNested": map[string]any{"keep": true},
		},
		"vendorTop": map[string]any{"keep": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(root, "source")
	source, err := orasoci.NewWithContext(ctx, sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	configDescriptor := descriptor(v1.MediaTypeImageConfig, baseConfig)
	if err := source.Push(ctx, configDescriptor, bytes.NewReader(baseConfig)); err != nil {
		t.Fatal(err)
	}
	manifestData, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: configDescriptor})
	if err != nil {
		t.Fatal(err)
	}
	selected := descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := source.Push(ctx, selected, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	const reference = "registry.example/team/extended-base:latest"
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	resolver, err := oci.NewResolver(oci.Options{})
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
	importTestImageToNative(t, ctx, store, sourceDir, reference, selected, system)
	plan := testPlan(t, `
from "registry.example/team/extended-base:latest" as="source"
copy "first" "/first"
copy "second" "/second"
from "source"
env FINAL="yes"
entrypoint { exec "/new-entry" }
`)
	layout := filepath.Join(root, "layout")
	result, err := BuildPlan(ctx, plan, PlanOptions{
		Store:      store,
		ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: layout}, Resolver: resolver,
		SystemContext: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) != 2 || len(image.RootFS.DiffIDs) != 2 {
		t.Fatalf("layers=%d diff IDs=%d; want two", len(manifest.Layers), len(image.RootFS.DiffIDs))
	}
	if !reflect.DeepEqual(image.Config.Env, []string{"BASE=yes", "FINAL=yes"}) || !reflect.DeepEqual(image.Config.Entrypoint, []string{"/new-entry"}) || len(image.Config.Cmd) != 0 {
		t.Fatalf("final runtime config = %+v", image.Config)
	}
	configData, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", manifest.Config.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageID != manifest.Config.Digest.Encoded() {
		t.Fatalf("result image ID %q != published config digest %s", result.ImageID, manifest.Config.Digest)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(configData, &raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw["vendorTop"], []byte(`"keep":true`)) {
		t.Fatalf("top-level extension lost: %s", raw["vendorTop"])
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw["config"], &nested); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(nested["vendorNested"], []byte(`"keep":true`)) || !bytes.Contains(nested["Shell"], []byte(`"/custom-shell"`)) {
		t.Fatalf("nested extensions lost: %s", raw["config"])
	}
}
