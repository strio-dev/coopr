package build

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"coopr/internal/buildah"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestDockerAnnotationControlsAreIgnored(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Docker annotation controls")
	}
	root := dockerEngineWorkspace(t)
	t.Setenv("XDG_RUNTIME_DIR", root)
	definition := filepath.Join(root, "image.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\"\nlabel marker=\"kept\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"docker", "oci"} {
		for attempt := range 2 {
			archive := filepath.Join(root, format+".tar")
			_, err := Run(context.Background(), Options{
				File: definition, Format: format,
				Tag:        "oci-archive:" + archive,
				BuildStore: buildah.StoreOptions{GraphRoot: filepath.Join(root, "store", "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"},
				ImageControls: buildah.ImageControls{
					Annotations:      []string{"org.example.kept=value", "org.example.removed=value"},
					UnsetAnnotations: []string{"org.example.removed"}, DropInheritedAnnotations: true,
				},
			})
			if err != nil {
				t.Fatalf("%s attempt %d: %v", format, attempt, err)
			}
			manifest, image := readExampleImage(t, context.Background(), archive)
			if image.Config.Labels["marker"] != "kept" {
				t.Fatalf("%s attempt %d lost image configuration", format, attempt)
			}
			if format == "docker" {
				if len(manifest.Annotations) != 0 || manifest.Config.MediaType == v1.MediaTypeImageConfig {
					t.Fatalf("Docker manifest retained OCI metadata: %+v", manifest)
				}
			} else if manifest.Annotations["org.example.kept"] != "value" || manifest.Annotations["org.example.removed"] != "" {
				t.Fatalf("OCI annotations = %+v", manifest.Annotations)
			}
		}
	}
}

func TestMultiPlatformTarFilesystemOutputUsesLastPlatform(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live rootfs exports")
	}
	root := dockerEngineWorkspace(t)
	definition := filepath.Join(root, "image.coopr")
	for name, contents := range map[string]string{
		"image.coopr":  "from \"scratch\"\narg \"TARGETARCH\"\ncopy \"marker-$TARGETARCH\" \"/marker\"\n",
		"marker-amd64": "amd64\n", "marker-arm64": "arm64\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(root, "rootfs.tar")
	_, err := Run(context.Background(), Options{
		File: definition, Tag: "tar-output:latest",
		Platforms: []string{"linux/amd64", "linux/arm64"}, Jobs: 0,
		Output:     buildah.FilesystemOutput{Type: "tar", Path: output},
		BuildStore: buildah.StoreOptions{GraphRoot: filepath.Join(root, "store", "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("missing marker in export: %v", err)
		}
		if header.Name == "marker" || header.Name == "./marker" {
			data, err := io.ReadAll(reader)
			if err != nil || string(data) != "arm64\n" {
				t.Fatalf("last platform output=%q err=%v", data, err)
			}
			break
		}
	}
}

func TestMultiPlatformLocalFilesystemExports(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live rootfs exports")
	}
	root := dockerEngineWorkspace(t)
	definition := filepath.Join(root, "image.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\"\ncopy \"marker\" \"/marker\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte("multi export\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "output")
	store := filepath.Join(root, "store")
	for range 2 {
		if _, err := Run(context.Background(), Options{File: definition, BuildStore: nativeBuildTestStore(store), Tag: "exported:latest", Platforms: []string{"linux/amd64", "linux/arm64"}, Output: buildah.FilesystemOutput{Type: "local", Path: output}, Squash: true}); err != nil {
			t.Fatal(err)
		}
		for _, platform := range []string{"linux_amd64", "linux_arm64"} {
			data, err := os.ReadFile(filepath.Join(output, platform, "marker"))
			if err != nil || string(data) != "multi export\n" {
				t.Fatalf("%s output=%q %v", platform, data, err)
			}
		}
		_, _, selections, found, err := testStoredImageIndex(context.Background(), nativeBuildTestStore(store), "exported:latest")
		if err != nil || !found || len(selections) != 2 {
			t.Fatalf("retained index found=%t selections=%d err=%v", found, len(selections), err)
		}
	}
}
