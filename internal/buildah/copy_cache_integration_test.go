package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
)

func TestBuildPlanCachesGeneralLocalCopy(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.MkdirAll(filepath.Join(contextDir, "tree", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_700_000_000, 0)
	write := func(path, contents string) {
		t.Helper()
		path = filepath.Join(contextDir, filepath.FromSlash(path))
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, fixed, fixed); err != nil {
			t.Fatal(err)
		}
	}
	write("first", "first\n")
	write("second", "second\n")
	write("tree/nested/value", "one\n")
	write("tree/nested/skip.tmp", "excluded\n")

	store := cacheTestStore(root)
	plan := testPlan(t, `
from "scratch"
copy "first" "second" "/multi/"
copy "tree/./nested" "/tree/" chown="12:34" chmod="0640" parents="true" { exclude "*.tmp" }
`)
	build := func(name, want string) (int, []digest.Digest) {
		t.Helper()
		layout := filepath.Join(root, name)
		if _, err := BuildPlan(ctx, plan, PlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless",
			Output: Output{Path: layout, Reference: name},
		}); err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		if len(manifest.Layers) != 2 {
			t.Fatalf("%s layers = %d, want 2", name, len(manifest.Layers))
		}
		firstLayer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
		if got := readLayerFile(t, firstLayer, "multi/first"); got != "first\n" {
			t.Fatalf("%s multi-source first = %q", name, got)
		}
		if got := readLayerFile(t, firstLayer, "multi/second"); got != "second\n" {
			t.Fatalf("%s multi-source second = %q", name, got)
		}
		lastLayer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
		if got := readLayerFile(t, lastLayer, "tree/nested/value"); got != want {
			t.Fatalf("%s directory COPY value = %q, want %q", name, got, want)
		}
		return instructionCacheRecordCount(t, store), []digest.Digest{manifest.Layers[0].Digest, manifest.Layers[1].Digest}
	}

	coldRecords, coldLayers := build("cold", "one\n")
	if coldRecords != 2 {
		t.Fatalf("cold COPY records = %d, want 2", coldRecords)
	}
	warmRecords, warmLayers := build("warm", "one\n")
	if warmRecords != coldRecords || !digestSlicesEqual(warmLayers, coldLayers) {
		t.Fatalf("warm COPY records/layers = %d/%v, want %d/%v", warmRecords, warmLayers, coldRecords, coldLayers)
	}
	write("tree/nested/value", "two\n")
	changedRecords, changedLayers := build("changed", "two\n")
	if changedRecords != coldRecords+1 {
		t.Fatalf("changed directory COPY records = %d, want %d", changedRecords, coldRecords+1)
	}
	if changedLayers[1] == coldLayers[1] {
		t.Fatalf("changed directory COPY reused layer %s", changedLayers[1])
	}
}

func TestBuildPlanCachesLocalArchiveAdd(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(contextDir, "payload.tar")
	fixed := time.Unix(1_700_000_000, 0)
	writeArchive := func(contents string) {
		t.Helper()
		var data bytes.Buffer
		writer := tar.NewWriter(&data)
		if err := writer.WriteHeader(&tar.Header{Name: "payload", Mode: 0o600, Size: int64(len(contents)), ModTime: fixed}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archive, data.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(archive, fixed, fixed); err != nil {
			t.Fatal(err)
		}
	}
	writeArchive("one\n")
	store := cacheTestStore(root)
	plan := testPlan(t, "from \"scratch\"\nadd \"payload.tar\" \"/unpacked/\"\n")
	build := func(name, want string) (int, digest.Digest) {
		t.Helper()
		layout := filepath.Join(root, name)
		if _, err := BuildPlan(ctx, plan, PlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: layout},
		}); err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 {
			t.Fatalf("%s ADD layers = %d, want 1", name, len(manifest.Layers))
		}
		layer := manifest.Layers[0]
		if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", layer.Digest.Encoded()), "unpacked/payload"); got != want {
			t.Fatalf("%s ADD payload = %q, want %q", name, got, want)
		}
		return instructionCacheRecordCount(t, store), layer.Digest
	}
	coldRecords, coldLayer := build("cold", "one\n")
	warmRecords, warmLayer := build("warm", "one\n")
	if coldRecords != 1 || warmRecords != 1 || warmLayer != coldLayer {
		t.Fatalf("warm ADD records/layer = %d/%s, cold = %d/%s", warmRecords, warmLayer, coldRecords, coldLayer)
	}
	writeArchive("two\n")
	changedRecords, changedLayer := build("changed", "two\n")
	if changedRecords != 2 || changedLayer == coldLayer {
		t.Fatalf("changed ADD records/layer = %d/%s, cold layer %s", changedRecords, changedLayer, coldLayer)
	}
}

func TestBuildDefinitionCachesExternalImageCopy(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	definition := parseWorkerDefinition(t, "from \"scratch\"\ncopy \"/bin/busybox\" \"/tool\" from=\""+base.reference+"\"\n")
	build := func(name string) (int, digest.Digest) {
		t.Helper()
		layout := filepath.Join(root, name)
		if _, err := BuildDefinitionSupervised(ctx, definition, planner.Options{
			Mode: planner.Build, Platform: "linux/" + runtime.GOARCH,
		}, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
			Pull: false, SignaturePolicyPath: policy,
		}); err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 {
			t.Fatalf("%s external-image COPY layers = %d, want 1", name, len(manifest.Layers))
		}
		layer := manifest.Layers[0]
		if copied := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", layer.Digest.Encoded()), "tool"); copied == "" {
			t.Fatalf("%s external-image COPY produced an empty tool", name)
		}
		return instructionCacheRecordCount(t, store), layer.Digest
	}
	coldRecords, coldLayer := build("cold")
	warmRecords, warmLayer := build("warm")
	if coldRecords != 1 || warmRecords != 1 || warmLayer != coldLayer {
		t.Fatalf("warm external-image COPY records/layer = %d/%s, cold = %d/%s", warmRecords, warmLayer, coldRecords, coldLayer)
	}
}

func requireLiveInstructionCache(t *testing.T) {
	t.Helper()
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless instruction cache test")
	}
}

func cacheTestStore(root string) StoreOptions {
	return StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
}

func instructionCacheRecordCount(t *testing.T, options StoreOptions) int {
	t.Helper()
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close instruction cache store: %v", err)
		}
	}()
	images, err := lease.store.Images()
	if err != nil {
		t.Fatal(err)
	}
	containers, err := lease.store.Containers()
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 0 {
		t.Fatalf("instruction cache builders leaked: %+v", containers)
	}
	keys := map[digest.Digest]bool{}
	for _, image := range images {
		for _, name := range image.Names {
			if !strings.HasPrefix(name, instructionCacheNamePrefix) {
				continue
			}
			key, err := digest.Parse(strings.TrimPrefix(name, instructionCacheNamePrefix))
			if err != nil {
				t.Fatal(err)
			}
			data, err := lease.store.ImageBigData(image.ID, instructionCacheBigDataName(key))
			if err != nil {
				t.Fatal(err)
			}
			var record instructionCacheRecord
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if record.Schema != instructionCacheSchema || record.Key != key {
				t.Fatalf("invalid instruction cache record for %s", name)
			}
			keys[key] = true
		}
	}
	return len(keys)
}

func digestSlicesEqual(left, right []digest.Digest) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
