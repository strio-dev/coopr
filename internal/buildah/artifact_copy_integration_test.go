package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/planner"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	"go.podman.io/storage"
)

func TestBuildDefinitionCopiesArtifactArchiveAndAddsItsContents(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	reference := "registry.example/artifact/archive:stable"
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	payload := "artifact archive payload\n"
	if err := writer.WriteHeader(&tar.Header{Name: "nested/payload", Mode: 0o600, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	seedArtifactCopyInput(t, ctx, store, reference, map[string]string{"payload.tar": archive.String()})
	definition := parseWorkerDefinition(t, fmt.Sprintf(`from "scratch" {
    copy "payload.tar" "/raw/payload.tar" from=%q chmod=0o640 chown="12:34"
    add "payload.tar" "/unpacked/" from=%q
}
`, reference, reference))
	layout := filepath.Join(root, "result")
	buildArtifactCopyDefinition(t, ctx, root, store, definition, layout)
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 2 {
		t.Fatalf("COPY/ADD artifact output has %d layers, want 2", len(manifest.Layers))
	}
	rawLayer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, rawLayer, "raw/payload.tar"); got != archive.String() {
		t.Fatal("COPY unpacked or changed the artifact's archive blob")
	}
	header := readLayerHeader(t, rawLayer, "raw/payload.tar")
	if header.Mode&0o7777 != 0o640 || header.Uid != 12 || header.Gid != 34 {
		t.Fatalf("artifact COPY mode/owner = %#o/%d:%d, want 0640/12:34", header.Mode, header.Uid, header.Gid)
	}
	unpackedLayer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
	if got := readLayerFile(t, unpackedLayer, "unpacked/nested/payload"); got != payload {
		t.Fatalf("ADD artifact archive payload = %q, want %q", got, payload)
	}
}

func TestBuildDefinitionArtifactCopyPreservesOptions(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	reference := "registry.example/artifact/options:stable"
	seedArtifactCopyInput(t, ctx, store, reference, map[string]string{
		"hello.txt": "artifact file\n",
		"skip.tmp":  "excluded\n",
	})
	definition := parseWorkerDefinition(t, fmt.Sprintf(`from "scratch" {
    copy "/./hello.txt" "/parents/" from=%q parents=#true
    copy "*" "/filtered/" from=%q { exclude "*.tmp" }
    copy "hello.txt" "/linked" from=%q link=#true
}
`, reference, reference, reference))
	layout := filepath.Join(root, "result")
	buildArtifactCopyDefinition(t, ctx, root, store, definition, layout)
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 3 {
		t.Fatalf("artifact COPY output has %d layers, want 3", len(manifest.Layers))
	}
	for index, name := range []string{"parents/hello.txt", "filtered/hello.txt", "linked"} {
		layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[index].Digest.Encoded())
		if got := readLayerFile(t, layer, name); got != "artifact file\n" {
			t.Fatalf("artifact COPY %s = %q", name, got)
		}
	}
	filteredLayer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
	if layerContainsPath(t, filteredLayer, "filtered/skip.tmp") {
		t.Fatal("artifact COPY ignored the source exclusion")
	}
}

func TestBuildDefinitionCachesArtifactCopyAndInvalidatesChangedBlob(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	reference := "registry.example/artifact/cache:stable"
	definition := parseWorkerDefinition(t, fmt.Sprintf(`from "scratch" {
    copy "message" "/message" from=%q
}
`, reference))
	seedArtifactCopyInput(t, ctx, store, reference, map[string]string{"message": "first\n"})
	build := func(name, want string) digest.Digest {
		t.Helper()
		layout := filepath.Join(root, name)
		buildArtifactCopyDefinition(t, ctx, root, store, definition, layout)
		manifest, _ := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 {
			t.Fatalf("%s artifact COPY layers = %d, want 1", name, len(manifest.Layers))
		}
		layer := manifest.Layers[0]
		if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", layer.Digest.Encoded()), "message"); got != want {
			t.Fatalf("%s artifact COPY payload = %q, want %q", name, got, want)
		}
		return layer.Digest
	}
	cold := build("cold", "first\n")
	if records := instructionCacheRecordCount(t, store); records != 1 {
		t.Fatalf("cold artifact COPY cache records = %d, want 1", records)
	}
	if warm := build("warm", "first\n"); warm != cold {
		t.Fatalf("warm artifact COPY layer = %s, want %s", warm, cold)
	}
	if records := instructionCacheRecordCount(t, store); records != 1 {
		t.Fatalf("warm artifact COPY cache records = %d, want 1", records)
	}
	seedArtifactCopyInput(t, ctx, store, reference, map[string]string{"message": "second\n"})
	if changed := build("changed", "second\n"); changed == cold {
		t.Fatalf("changed artifact COPY reused layer %s", changed)
	}
	if records := instructionCacheRecordCount(t, store); records != 2 {
		t.Fatalf("changed artifact COPY cache records = %d, want 2", records)
	}
}

func TestBuildDefinitionCachesIndexedArtifactCopyByPlatform(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	repository := artifactIndexTestRepository(t, server.URL)
	reference := repository.Reference.String() + ":latest"
	artifactIndexTestPublish(t, repository, "first")
	definition := parseWorkerDefinition(t, fmt.Sprintf(`from "scratch" {
    copy "payload" "/payload" from=%q
}
`, reference))
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(name, architecture, pullPolicy, want string) digest.Digest {
		t.Helper()
		layout := filepath.Join(root, name)
		if _, err := BuildDefinitionSupervised(ctx, definition, planner.Options{
			Mode: planner.Build, Platform: "linux/" + architecture,
		}, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
			PullPolicy: pullPolicy, TLSVerify: new(false), SignaturePolicyPath: policy,
		}); err != nil {
			t.Fatalf("%s indexed artifact build: %v", name, err)
		}
		manifest, config := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 || config.Architecture != architecture {
			t.Fatalf("%s output layers=%d architecture=%s, want one layer for %s", name, len(manifest.Layers), config.Architecture, architecture)
		}
		layer := manifest.Layers[0]
		if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", layer.Digest.Encoded()), "payload"); got != want {
			t.Fatalf("%s indexed artifact payload = %q, want %q", name, got, want)
		}
		return layer.Digest
	}
	layers := make(map[string]digest.Digest)
	for _, architecture := range []string{"amd64", "arm64"} {
		layers[architecture] = build(architecture+"-cold", architecture, "always", "first-"+architecture)
		if warm := build(architecture+"-warm", architecture, "never", "first-"+architecture); warm != layers[architecture] {
			t.Fatalf("warm %s artifact COPY layer = %s, want %s", architecture, warm, layers[architecture])
		}
	}
	if records := instructionCacheRecordCount(t, store); records != 2 {
		t.Fatalf("platform artifact COPY cache records = %d, want 2", records)
	}
	artifactIndexTestPublish(t, repository, "second")
	for _, architecture := range []string{"amd64", "arm64"} {
		changed := build(architecture+"-changed", architecture, "always", "second-"+architecture)
		if changed == layers[architecture] {
			t.Fatalf("changed %s artifact COPY reused layer %s", architecture, changed)
		}
		layers[architecture] = changed
	}
	if records := instructionCacheRecordCount(t, store); records != 4 {
		t.Fatalf("changed platform artifact COPY cache records = %d, want 4", records)
	}
	server.Close()
	for _, architecture := range []string{"amd64", "arm64"} {
		if offline := build(architecture+"-offline", architecture, "never", "second-"+architecture); offline != layers[architecture] {
			t.Fatalf("offline %s artifact COPY layer = %s, want %s", architecture, offline, layers[architecture])
		}
	}
}

func seedArtifactCopyInput(t *testing.T, ctx context.Context, options StoreOptions, reference string, files map[string]string) {
	t.Helper()
	if err := WithStore(options, func(store storage.Store) error {
		artifactSourceTestAdd(t, ctx, store, reference, files)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func buildArtifactCopyDefinition(t *testing.T, ctx context.Context, root string, store StoreOptions, definition *definition.Definition, layout string) {
	t.Helper()
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDefinitionSupervised(ctx, definition, planner.Options{
		Mode: planner.Build, Platform: "linux/" + runtime.GOARCH,
	}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		PullPolicy: "never", SignaturePolicyPath: policy,
	}); err != nil {
		t.Fatal(err)
	}
}
