package buildah

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"coopr/internal/oci"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestOutputFormatValidation(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{"", outputFormatOCI},
		{outputFormatOCI, outputFormatOCI},
		{outputFormatDocker, outputFormatDocker},
	} {
		got, err := normalizedOutputFormat(test.input)
		if err != nil {
			t.Fatalf("normalizedOutputFormat(%q): %v", test.input, err)
		}
		if got != test.want {
			t.Fatalf("normalizedOutputFormat(%q) = %q, want %q", test.input, got, test.want)
		}
	}
	if _, err := normalizedOutputFormat("containerd"); err == nil {
		t.Fatal("unsupported output format was accepted")
	}
}

func TestComponentCacheSemanticsSeparateOutputFormats(t *testing.T) {
	implicit, err := componentCacheExecutorSemantics("buildah", "modules", "rootless", "vfs", nil, "crun", "")
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := componentCacheExecutorSemantics("buildah", "modules", "rootless", "vfs", nil, "crun", outputFormatOCI)
	if err != nil {
		t.Fatal(err)
	}
	docker, err := componentCacheExecutorSemantics("buildah", "modules", "rootless", "vfs", nil, "crun", outputFormatDocker)
	if err != nil {
		t.Fatal(err)
	}
	if implicit != explicit {
		t.Fatalf("implicit OCI semantics %q differ from explicit OCI semantics %q", implicit, explicit)
	}
	if docker == explicit {
		t.Fatal("Docker and OCI component cache semantics are identical")
	}
}

func TestComponentCacheSemanticsIncludeGraphDriverOptions(t *testing.T) {
	defaultOptions, err := componentCacheExecutorSemantics("buildah", "modules", "rootless", "overlay", nil, "crun", outputFormatOCI)
	if err != nil {
		t.Fatal(err)
	}
	forcedMask, err := componentCacheExecutorSemantics("buildah", "modules", "rootless", "overlay", []string{"overlay.force_mask=0700"}, "crun", outputFormatOCI)
	if err != nil {
		t.Fatal(err)
	}
	if defaultOptions == forcedMask {
		t.Fatal("graph driver options did not invalidate component cache semantics")
	}
}

func TestBuildPlanSupervisedPreservesOutputManifestFormat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah format test in short mode")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "proof"), []byte("format\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{
		RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
	}
	for _, test := range []struct {
		name, format, manifest, config, layer, definition string
		disableCompression                                bool
	}{
		{name: "default OCI", manifest: v1.MediaTypeImageManifest, config: v1.MediaTypeImageConfig, definition: "from \"scratch\"\ncopy \"proof\" \"/proof\"\nenv FORMAT=\"proof\"\n"},
		{name: "Docker schema 2", format: outputFormatDocker, manifest: dockerManifestMediaType, config: dockerImageConfigMediaType, definition: "from \"scratch\"\ncopy \"proof\" \"/proof\"\nenv FORMAT=\"proof\"\n"},
		{name: "Docker uncompressed", format: outputFormatDocker, manifest: dockerManifestMediaType, config: dockerImageConfigMediaType, layer: "application/vnd.docker.image.rootfs.diff.tar", definition: "from \"scratch\"\ncopy \"proof\" \"/proof\"\n", disableCompression: true},
		{name: "Docker config only", format: outputFormatDocker, manifest: dockerManifestMediaType, config: dockerImageConfigMediaType, definition: "from \"scratch\"\nenv FORMAT=\"proof\"\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout := filepath.Join(root, test.name)
			result, err := BuildPlanSupervised(context.Background(), testPlan(t, test.definition), SupervisedPlanOptions{
				Store: store, ContextDir: root, Isolation: "rootless",
				Output: Output{Path: layout, Reference: "format-proof", Format: test.format, DisableCompression: test.disableCompression},
				Stdout: io.Discard, Stderr: io.Discard,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertLayoutMediaTypes(t, layout, test.manifest, test.config)
			if test.layer != "" {
				manifest, _ := readPlanImage(t, layout)
				if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != test.layer {
					t.Fatalf("layer descriptors = %+v, want one %s layer", manifest.Layers, test.layer)
				}
			}
			if result.ManifestDigest == "" || result.ImageID == "" {
				t.Fatalf("incomplete build result: %#v", result)
			}

			copied := filepath.Join(root, test.name+"-copy")
			copyResult, err := ExportStoredImageSupervised(context.Background(), store, result.ImageID, copied)
			if err != nil {
				t.Fatal(err)
			}
			assertLayoutMediaTypes(t, copied, test.manifest, test.config)
			if copyResult.ManifestDigest != result.ManifestDigest {
				t.Fatalf("copied manifest digest %s, want original %s", copyResult.ManifestDigest, result.ManifestDigest)
			}
		})
	}
}

func TestDockerGzipMetadataTailPreservesFormatColdAndWarm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Docker metadata-tail coverage in short mode")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "proof"), []byte("format\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	plan := testPlan(t, "from \"scratch\"\ncopy \"proof\" \"/proof\"\nenv FORMAT=\"proof\"\n")
	var firstDiffID string
	for _, attempt := range []string{"cold", "warm"} {
		layout := filepath.Join(root, attempt)
		_, err := BuildPlanSupervised(context.Background(), plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless",
			Output: Output{Path: layout, Format: outputFormatDocker, DisableCompression: false},
			Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		assertLayoutMediaTypes(t, layout, dockerManifestMediaType, dockerImageConfigMediaType)
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 || len(image.RootFS.DiffIDs) != 1 {
			t.Fatalf("%s Docker metadata tail layers=%d diffIDs=%d, want one", attempt, len(manifest.Layers), len(image.RootFS.DiffIDs))
		}
		if manifest.Layers[0].MediaType != "application/vnd.docker.image.rootfs.diff.tar.gzip" {
			t.Fatalf("%s Docker layer media type = %q", attempt, manifest.Layers[0].MediaType)
		}
		if attempt == "cold" {
			firstDiffID = image.RootFS.DiffIDs[0].String()
		} else if image.RootFS.DiffIDs[0].String() != firstDiffID {
			t.Fatalf("warm Docker metadata tail changed diffID from %s to %s", firstDiffID, image.RootFS.DiffIDs[0])
		}
	}
}

func TestDockerOutputInstructionCacheIsFormatScoped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah format cache test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{
		RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
	}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \n' >/proof" network="none"
`, base.reference))
	build := func(name, format string) string {
		t.Helper()
		layout := filepath.Join(root, name)
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output:              Output{Path: layout, Format: format},
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		return readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
	}
	ociProof := build("oci", outputFormatOCI)
	firstDockerProof := build("docker-1", outputFormatDocker)
	secondDockerProof := build("docker-2", outputFormatDocker)
	if firstDockerProof == ociProof {
		t.Fatal("Docker build reused an OCI instruction-cache entry")
	}
	if secondDockerProof != firstDockerProof {
		t.Fatalf("Docker instruction cache miss: first %q, second %q", firstDockerProof, secondDockerProof)
	}
}

func TestDockerOutputInvokesAndCachesComponent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Docker component cache test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	resolver, _, _ := localComponentResolver(t, ctx, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \"scratch\"\ncomponent \"local:tool\" channel=\"stable\"\n")
	cacheDir := filepath.Join(root, "cache")
	for attempt, name := range []string{"cold", "warm"} {
		options := componentTestOptions(root, filepath.Join(root, name), resolver, policy)
		options.Output.Format = outputFormatDocker
		options.CacheLocalDir = cacheDir
		result, err := BuildPlan(ctx, plan, options)
		if err != nil {
			t.Fatal(err)
		}
		assertLayoutMediaTypes(t, options.Output.Path, dockerManifestMediaType, dockerImageConfigMediaType)
		if attempt == 0 && (result.CacheStats.Misses < 1 || result.CacheStats.Stored < 1) {
			t.Fatalf("cold Docker component cache stats = %+v", result.CacheStats)
		}
		if attempt == 1 && result.CacheStats.Hits != 1 {
			t.Fatalf("warm Docker component cache stats = %+v", result.CacheStats)
		}
	}
}

func assertLayoutMediaTypes(t *testing.T, layout, wantManifest, wantConfig string) {
	t.Helper()
	root, err := oci.LayoutRoot(layout)
	if err != nil {
		t.Fatal(err)
	}
	if root.MediaType != wantManifest {
		t.Fatalf("manifest media type = %q, want %q", root.MediaType, wantManifest)
	}
	data, err := readVerifiedLayoutBlob(layout, root)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Config.MediaType != wantConfig {
		t.Fatalf("config media type = %q, want %q", manifest.Config.MediaType, wantConfig)
	}
}
