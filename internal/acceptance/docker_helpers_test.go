//go:build dockerintegration

package acceptance

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/testutil"
	"coopr/internal/transfer"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage/pkg/archive"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func dockerEngineWorkspace(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	hostEnvironment := os.Environ()
	t.Cleanup(func() {
		command := exec.Command("podman", "unshare", "rm", "-rf", "--", work)
		command.Env = hostEnvironment
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("remove Docker test-owned storage: %v: %s", err, output)
		}
	})
	return work
}

func dockerTestStore(root string) buildah.StoreOptions {
	return buildah.StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs", GraphDriverOptions: []string{"vfs.ignore_chown_errors=true"}}
}

func loadDockerTestDefinition(t *testing.T, ctx context.Context, work string) string {
	t.Helper()
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(work, "runner"), "../build/testdata/storage-runner/main.go")
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build static Docker runner: %v: %s", err, output)
	}
	definition := filepath.Join(work, "app.coopr")
	data := "from \"scratch\"\ncopy \"runner\" \"/runner\"\ncopy \"marker\" \"/marker\"\nlabel coopr_test=\"load\"\ncmd { exec \"/runner\" }\n"
	if err := os.WriteFile(definition, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return definition
}

// Export through the supervised transfer API so inspection stays outside the
// build worker's rootless namespace and preserves the complete native index.
func exportDockerTestImage(t *testing.T, ctx context.Context, store buildah.StoreOptions, name string) v1.Descriptor {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.oci.tar")
	if _, err := transfer.Copy(ctx, oci.Image, name, transfer.Destination{Transport: "oci-archive", Name: path}, transfer.Options{BuildStore: store}); err != nil {
		t.Fatalf("export stored Docker fixture: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	var index v1.Index
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(header.Name, "./") == "index.json" {
			if err := json.NewDecoder(reader).Decode(&index); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("exported image roots: %+v", index.Manifests)
	}
	return index.Manifests[0]
}

func dockerArchiveHasPath(t *testing.T, ctx context.Context, path, name string) bool {
	t.Helper()
	manifest, _ := testutil.ReadImage(t, ctx, path)
	layout, err := orasoci.NewFromTar(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, layer := range manifest.Layers {
		stream, err := layout.Fetch(ctx, layer)
		if err != nil {
			t.Fatal(err)
		}
		decompressed, err := archive.DecompressStream(stream)
		if err != nil {
			_ = stream.Close()
			t.Fatal(err)
		}
		found := false
		reader := tar.NewReader(decompressed)
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			found = found || strings.TrimPrefix(header.Name, "./") == name
		}
		if err := decompressed.Close(); err != nil {
			t.Fatal(err)
		}
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
		if found {
			return true
		}
	}
	return false
}

// Inspect metadata through host Podman rather than imposing a raw-blob export
// requirement on images imported from Docker's storage.
func inspectDockerImportedPlatforms(t *testing.T, ctx context.Context, store buildah.StoreOptions, name string, multi bool) []string {
	t.Helper()
	args := []string{"--root", store.GraphRoot, "--runroot", store.RunRoot, "--storage-driver=vfs", "--storage-opt=vfs.ignore_chown_errors=true"}
	if multi {
		args = append(args, "manifest", "inspect", name)
	} else {
		args = append(args, "image", "inspect", name)
	}
	command := exec.CommandContext(ctx, "podman", args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	data, err := command.Output()
	if err != nil {
		t.Fatalf("inspect imported Docker metadata: %v: %s", err, stderr.String())
	}
	if multi {
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil {
			t.Fatal(err)
		}
		var platforms []string
		for _, descriptor := range index.Manifests {
			if descriptor.Platform == nil {
				t.Fatal("imported Docker index manifest lacks a platform")
			}
			platforms = append(platforms, descriptor.Platform.OS+"/"+descriptor.Platform.Architecture)
		}
		return platforms
	}
	var images []struct {
		OS           string
		Architecture string
	}
	if err := json.Unmarshal(data, &images); err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 {
		t.Fatalf("imported Docker images: %+v", images)
	}
	return []string{images[0].OS + "/" + images[0].Architecture}
}
