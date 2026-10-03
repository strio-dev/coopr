package buildah

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"coopr/internal/imagecatalog"
	"coopr/internal/oci"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
)

func TestSupervisedRawBuildPlansExternalOnBuildBeforeExecution(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah ONBUILD planning test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	for name, contents := range map[string]string{"artifact": "from producer\n", "payload": "from inherited ARG\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	parentLayout := filepath.Join(root, "parent")
	parent, err := BuildPlan(ctx, testPlan(t, `
from "scratch"
onbuild { arg "filename" "payload" }
onbuild { copy "/artifact" "/inherited" from="producer" }
`), PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless",
		Output:        Output{Path: parentLayout, Format: outputFormatDocker},
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := oci.ReadImageConfigLayout(ctx, parentLayout)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := oci.LayoutRoot(parentLayout)
	if err != nil {
		t.Fatal(err)
	}
	const reference = "registry.example/coopr/onbuild-arg:latest"
	imageStoreDir := filepath.Join(root, "images")
	if err := imagecatalog.Commit(ctx, imageStoreDir, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, imagecatalog.Selection{
		Root: selected, Manifest: selected, ImageID: parent.ImageID, ConfigData: config,
	}); err != nil {
		t.Fatal(err)
	}

	child := parseWorkerDefinition(t, `
from "registry.example/coopr/onbuild-arg:latest" as="final"
copy "$filename" "/selected"
from "scratch" as="producer"
copy "artifact" "/artifact"
`)
	childLayout := filepath.Join(root, "child")
	_, err = BuildDefinitionSupervised(ctx, child, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH, Target: "final"}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: childLayout, Format: outputFormatDocker},
		ImageStoreDir: imageStoreDir, SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, childLayout)
	if len(manifest.Layers) < 2 {
		t.Fatalf("child layers = %d, want inherited and authored COPY layers", len(manifest.Layers))
	}
	for index, want := range []struct{ path, contents string }{
		{"inherited", "from producer\n"},
		{"selected", "from inherited ARG\n"},
	} {
		blob := filepath.Join(childLayout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-2+index].Digest.Encoded())
		if got := readLayerFile(t, blob, want.path); got != want.contents {
			t.Fatalf("layer %d %s = %q, want %q", index, want.path, got, want.contents)
		}
	}
	childConfig, err := oci.ReadImageConfigLayout(ctx, childLayout)
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Config struct {
			OnBuild []string `json:"OnBuild"`
		} `json:"config"`
	}
	if err := json.Unmarshal(childConfig, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Config.OnBuild) != 0 {
		t.Fatalf("inherited triggers leaked into child image: %v", metadata.Config.OnBuild)
	}
}

func TestSupervisedRawBuildPlansLocalOnBuildBeforeExecution(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah ONBUILD planning test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	for name, contents := range map[string]string{"artifact": "from producer\n", "payload": "from inherited ARG\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	child := parseWorkerDefinition(t, `
from "scratch" as="producer"
copy "artifact" "/artifact"
from "scratch" as="base"
onbuild { arg "filename" "payload" }
onbuild { copy "/artifact" "/inherited" from="producer" }
from "base"
copy "$filename" "/selected"
`)
	layout := filepath.Join(root, "child")
	_, err := BuildDefinitionSupervised(ctx, child, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout, Format: outputFormatDocker},
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) < 2 {
		t.Fatalf("child layers = %d, want inherited and authored COPY layers", len(manifest.Layers))
	}
	for index, want := range []struct{ path, contents string }{
		{"inherited", "from producer\n"},
		{"selected", "from inherited ARG\n"},
	} {
		blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-2+index].Digest.Encoded())
		if got := readLayerFile(t, blob, want.path); got != want.contents {
			t.Fatalf("layer %d %s = %q, want %q", index, want.path, got, want.contents)
		}
	}
	config, err := oci.ReadImageConfigLayout(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Config struct {
			OnBuild []string `json:"OnBuild"`
		} `json:"config"`
	}
	if err := json.Unmarshal(config, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Config.OnBuild) != 0 {
		t.Fatalf("local inherited triggers leaked into child image: %v", metadata.Config.OnBuild)
	}
}
