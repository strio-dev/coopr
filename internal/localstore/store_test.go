package localstore

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coopr/internal/storeactivity"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestOCILayoutRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "layout")

	source, root, blobs := testImageGraph(t, ctx)
	if err := Put(ctx, dir, source, root, "example:latest"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{root.Digest.String(), "example:latest"} {
		got, err := store.Resolve(ctx, ref)
		if err != nil {
			t.Fatalf("resolve %q: %v", ref, err)
		}
		if got.Digest != root.Digest || got.MediaType != root.MediaType || got.Size != root.Size {
			t.Fatalf("resolved %q to %+v, want %+v", ref, got, root)
		}
	}
	for dgst, want := range blobs {
		got, err := content.FetchAll(ctx, store, v1.Descriptor{Digest: dgst, Size: int64(len(want))})
		if err != nil {
			t.Fatalf("fetch %s: %v", dgst, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("blob %s changed", dgst)
		}
	}
}

func TestRemoveTagAndPruneDigestOnlyGraph(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "components")
	source, root, _ := testImageGraph(t, ctx)
	if err := Put(ctx, dir, source, root, "example"); err != nil {
		t.Fatal(err)
	}
	if removed, err := Prune(ctx, dir, false); err != nil || removed != 0 {
		t.Fatalf("prune named graph = %d, %v", removed, err)
	}
	if removed, err := RemoveTag(ctx, dir, "example"); err != nil || !removed {
		t.Fatalf("remove tag = %t, %v", removed, err)
	}
	if descriptor, found, err := Inspect(ctx, dir, root.Digest.String()); err != nil || !found || descriptor.Digest != root.Digest {
		t.Fatalf("digest after untag = %+v, %t, %v", descriptor, found, err)
	}
	if removed, err := Prune(ctx, dir, true); err != nil || removed != 1 {
		t.Fatalf("dry-run prune = %d, %v", removed, err)
	}
	if _, found, err := Inspect(ctx, dir, root.Digest.String()); err != nil || !found {
		t.Fatalf("dry-run removed graph: found=%t err=%v", found, err)
	}
	if removed, err := Prune(ctx, dir, false); err != nil || removed != 1 {
		t.Fatalf("prune = %d, %v", removed, err)
	}
	if _, found, err := Inspect(ctx, dir, root.Digest.String()); err != nil || found {
		t.Fatalf("pruned graph still resolves: found=%t err=%v", found, err)
	}
}

func TestActiveStoreLeaseBlocksPruneAndPreservesNewName(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "components")
	source, root, _ := testImageGraph(t, ctx)
	if err := Put(ctx, dir, source, root); err != nil {
		t.Fatal(err)
	}
	activity, err := storeactivity.AcquireShared(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	pruned := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		count, pruneErr := Prune(ctx, dir, false)
		pruned <- struct {
			count int
			err   error
		}{count: count, err: pruneErr}
	}()
	select {
	case result := <-pruned:
		t.Fatalf("prune completed during active mutation: %+v", result)
	case <-time.After(75 * time.Millisecond):
	}
	if err := TagExisting(ctx, dir, root, "example:latest"); err != nil {
		t.Fatal(err)
	}
	if err := activity.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-pruned
	if result.err != nil || result.count != 0 {
		t.Fatalf("prune after named publication = %d, %v", result.count, result.err)
	}
	if descriptor, found, err := Inspect(ctx, dir, "example:latest"); err != nil || !found || descriptor.Digest != root.Digest {
		t.Fatalf("published root after prune = %+v, %t, %v", descriptor, found, err)
	}
}

func TestNormalizeImageTag(t *testing.T) {
	tests := map[string]string{
		"base":                          "base:latest",
		"base:dev":                      "base:dev",
		"team/base":                     "team/base:latest",
		"localhost/base":                "localhost/base:latest",
		"registry.example/team/base":    "registry.example/team/base:latest",
		"registry.example/team/base:v1": "registry.example/team/base:v1",
	}
	for input, want := range tests {
		got, err := NormalizeImageTag(input)
		if err != nil || got != want {
			t.Errorf("NormalizeImageTag(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", " bad", "bad tag", "-bad", "docker://base", "base@sha256:deadbeef"} {
		got, err := NormalizeImageTag(input)
		if input == "" {
			if err != nil || got != "" {
				t.Errorf("NormalizeImageTag(empty) = %q, %v", got, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("NormalizeImageTag(%q) = %q, want error", input, got)
		}
	}
}

func TestTagExistingRejectsMissingDescendantWithoutPublishingNames(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	store, err := orasoci.NewWithContext(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	store.AutoSaveIndex = false
	missingConfig := testDescriptor(v1.MediaTypeImageConfig, []byte("missing config"))
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    missingConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := testDescriptor(v1.MediaTypeImageManifest, manifestData)
	if err := store.Push(ctx, root, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}

	err = TagExisting(ctx, dir, root, "broken:latest")
	if err == nil || !strings.Contains(err.Error(), missingConfig.Digest.String()) {
		t.Fatalf("TagExisting() error = %v, want missing descendant %s", err, missingConfig.Digest)
	}
	reopened, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := reopened.Resolve(ctx, "broken:latest"); err == nil {
		t.Fatalf("Resolve(broken:latest) = %+v, want unpublished tag", resolved)
	}
	indexData, err := os.ReadFile(filepath.Join(dir, v1.ImageIndexFile))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 0 {
		t.Fatalf("index manifests = %+v, want no published names", index.Manifests)
	}
}

func TestWriteArchiveToAnchorsSingleImageRoot(t *testing.T) {
	ctx := context.Background()
	source, root, _ := testImageGraph(t, ctx)
	var archive bytes.Buffer
	if err := WriteArchiveTo(ctx, source, root, &archive); err != nil {
		t.Fatal(err)
	}
	assertArchiveRoot(t, archive.Bytes(), root)
}

func TestWriteArchiveToAnchorsOnlyMultiPlatformIndex(t *testing.T) {
	ctx := context.Background()
	source, err := orasoci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	amd := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm := v1.Platform{OS: "linux", Architecture: "arm64"}
	amdManifest := catalogTestImage(t, ctx, source, amd, "amd")
	armManifest := catalogTestImage(t, ctx, source, arm, "arm")
	amdManifest.Platform, armManifest.Platform = &amd, &arm
	root := catalogTestIndex(t, ctx, source, amdManifest, armManifest)
	var archive bytes.Buffer
	if err := WriteArchiveTo(ctx, source, root, &archive); err != nil {
		t.Fatal(err)
	}
	assertArchiveRoot(t, archive.Bytes(), root)
}

func assertArchiveRoot(t *testing.T, data []byte, root v1.Descriptor) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if err != nil {
			if err == io.EOF {
				t.Fatal("archive does not contain index.json")
			}
			t.Fatal(err)
		}
		if header.Name != "index.json" {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil {
			t.Fatal(err)
		}
		if len(index.Manifests) != 1 || index.Manifests[0].Digest != root.Digest {
			t.Fatalf("archive roots = %+v, want only %s", index.Manifests, root.Digest)
		}
		break
	}
}

func TestReadLockKeepsWritersOutUntilCopyFinishes(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	source, root, _ := testImageGraph(t, ctx)
	if err := Put(ctx, dir, source, root); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- WithReadLock(ctx, dir, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := Put(deadline, dir, source, root, "later:dev"); err != context.DeadlineExceeded {
		close(release)
		t.Fatalf("writer was not blocked by read lock: %v", err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := Put(ctx, dir, source, root, "later:dev"); err != nil {
		t.Fatalf("writer did not resume after read lock: %v", err)
	}
}

func testImageGraph(t *testing.T, ctx context.Context) (*orasoci.Store, v1.Descriptor, map[digest.Digest][]byte) {
	t.Helper()
	store, err := orasoci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configData := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	config := testDescriptor(v1.MediaTypeImageConfig, configData)
	layerData := []byte("test image layer")
	layer := testDescriptor(v1.MediaTypeImageLayer, layerData)
	for _, blob := range []struct {
		desc v1.Descriptor
		data []byte
	}{{config, configData}, {layer, layerData}} {
		if err := store.Push(ctx, blob.desc, bytes.NewReader(blob.data)); err != nil {
			t.Fatal(err)
		}
	}
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    config,
		Layers:    []v1.Descriptor{layer},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := testDescriptor(v1.MediaTypeImageManifest, manifestData)
	if err := store.Push(ctx, root, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	return store, root, map[digest.Digest][]byte{
		config.Digest: configData,
		layer.Digest:  layerData,
		root.Digest:   manifestData,
	}
}

func testDescriptor(mediaType string, data []byte) v1.Descriptor {
	return v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
}
