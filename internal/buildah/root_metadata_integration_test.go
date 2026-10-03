package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	storagearchive "go.podman.io/storage/pkg/archive"
)

func TestBuildPlanAcceptsRootMetadataChanges(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	writeRootMetadataArchive(t, root)
	layout := filepath.Join(root, "layout")
	if _, err := BuildPlan(ctx, testPlan(t, "from \"scratch\"\nadd \"root.tar\" \"/\"\n"), PlanOptions{
		Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	assertRootMetadataChildControl(t, layout, manifest)
}

func TestBuildPlanAcceptsRootMetadataChangesAcrossCheckpointAndWarmCache(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	writeRootMetadataArchive(t, root)
	if err := os.WriteFile(filepath.Join(root, "checkpoint-control"), []byte("checkpoint control\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := cacheTestStore(root)
	plan := testPlan(t, "from \"scratch\"\nadd \"root.tar\" \"/\"\ncopy \"checkpoint-control\" \"/checkpoint-control\"\n")
	var coldLayers []digest.Digest
	for attempt := range 2 {
		layout := filepath.Join(root, fmt.Sprintf("layout-%d", attempt))
		if _, err := BuildPlan(ctx, plan, PlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		}); err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		assertRootMetadataChildControl(t, layout, manifest)
		last := manifest.Layers[len(manifest.Layers)-1]
		if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "checkpoint-control"); got != "checkpoint control\n" {
			t.Fatalf("build %d checkpoint control = %q", attempt+1, got)
		}
		layers := make([]digest.Digest, len(manifest.Layers))
		for index, layer := range manifest.Layers {
			layers[index] = layer.Digest
		}
		if attempt == 0 {
			coldLayers = layers
		} else if !digestSlicesEqual(layers, coldLayers) {
			t.Fatalf("warm layers = %v, want cold layers %v", layers, coldLayers)
		}
	}
	if count := instructionCacheRecordCount(t, store); count != 2 {
		t.Fatalf("root metadata build cache records = %d, want 2", count)
	}
}

func TestBuildPlanComponentAcceptsRootMetadataChanges(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	resolver := localConfigComponentResolver(t, ctx, root, `extend
run "/bin/busybox echo child control >/child-control; /bin/busybox chmod 0640 /child-control; /bin/busybox chown 3:4 /child-control; /bin/busybox chmod 0711 /; /bin/busybox chown 1:2 /" network="none"
`, false)
	plan := testPlan(t, "from \""+base.reference+"\"\ncomponent \"local:config\"\n")
	policy := writeComponentTestPolicy(t, root)
	cacheDir := filepath.Join(root, "cache")
	var coldLayers []digest.Digest
	for attempt, name := range []string{"cold", "warm"} {
		layout := filepath.Join(root, name)
		options := componentTestOptions(root, layout, resolver, policy)
		options.Store = store
		options.Runtime = "crun"
		options.CacheLocalDir = cacheDir
		result, err := BuildPlan(ctx, plan, options)
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 && (result.CacheStats.Misses < 1 || result.CacheStats.Stored < 1 || result.CacheStats.Errors != 0) {
			t.Fatalf("cold component cache stats = %+v", result.CacheStats)
		}
		if attempt == 1 && (result.CacheStats.Hits != 1 || result.CacheStats.Misses != 0 || result.CacheStats.Errors != 0) {
			t.Fatalf("warm component cache stats = %+v", result.CacheStats)
		}
		manifest, _ := readPlanImage(t, layout)
		assertRootMetadataChildControl(t, layout, manifest)
		layers := make([]digest.Digest, len(manifest.Layers))
		for index, layer := range manifest.Layers {
			layers[index] = layer.Digest
		}
		if attempt == 0 {
			coldLayers = layers
		} else if !digestSlicesEqual(layers, coldLayers) {
			t.Fatalf("warm component layers = %v, want cold layers %v", layers, coldLayers)
		}
	}
}

func writeRootMetadataArchive(t *testing.T, root string) {
	t.Helper()
	var contents bytes.Buffer
	writer := tar.NewWriter(&contents)
	if err := writer.WriteHeader(&tar.Header{
		Name: ".", Typeflag: tar.TypeDir, Mode: 0o711, Uid: 1, Gid: 2,
		PAXRecords: map[string]string{"SCHILY.xattr.user.coopr": "root-metadata"},
	}); err != nil {
		t.Fatal(err)
	}
	control := []byte("child control\n")
	if err := writer.WriteHeader(&tar.Header{Name: "child-control", Mode: 0o640, Uid: 3, Gid: 4, Size: int64(len(control))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(control); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "root.tar"), contents.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertRootMetadataChildControl(t *testing.T, layout string, manifest v1.Manifest) {
	t.Helper()
	var childHeader *tar.Header
	var childContents string
	for _, layer := range manifest.Layers {
		path := filepath.Join(layout, "blobs", "sha256", layer.Digest.Encoded())
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := storagearchive.DecompressStream(file)
		if err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		reader := tar.NewReader(stream)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = stream.Close()
				_ = file.Close()
				t.Fatal(err)
			}
			name := strings.TrimPrefix(filepath.ToSlash(header.Name), "./")
			switch name {
			case "child-control":
				copy := *header
				childHeader = &copy
				contents, err := io.ReadAll(reader)
				if err != nil {
					_ = stream.Close()
					_ = file.Close()
					t.Fatal(err)
				}
				childContents = string(contents)
			}
		}
		if err := stream.Close(); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if childHeader == nil {
		t.Fatal("image layers contain no child control")
	}
	if childContents != "child control\n" {
		t.Fatalf("child control contents = %q", childContents)
	}
	if got := childHeader.Mode & 0o7777; got != 0o640 || childHeader.Uid != 3 || childHeader.Gid != 4 {
		t.Fatalf("child control metadata = mode %#o uid %d gid %d, want 0640/3/4", got, childHeader.Uid, childHeader.Gid)
	}
	if _, present := childHeader.PAXRecords["SCHILY.xattr.user.coopr"]; present {
		t.Fatalf("root xattr leaked to child control: %v", childHeader.PAXRecords)
	}
}
