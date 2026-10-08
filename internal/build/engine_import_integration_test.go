package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"coopr/internal/oci"
	"coopr/internal/transfer"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestExplicitDockerImportThenOfflineFrom(t *testing.T) {
	if os.Getenv("COOPR_TEST_DOCKER") != "1" || os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_DOCKER=1 and COOPR_TEST_BUILDAH=1 for live Docker imports")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	work := dockerEngineWorkspace(t)
	file := filepath.Join(work, "base.coopr")
	if err := os.WriteFile(file, []byte("from \"scratch\"\ncopy \"marker\" \"/marker\"\nlabel engine_import=\"yes\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "marker"), []byte("unique Docker import\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprint(multi), func(t *testing.T) {
			if multi && os.Getenv("COOPR_TEST_DOCKER_MULTIPLATFORM") != "1" {
				t.Skip("set COOPR_TEST_DOCKER_MULTIPLATFORM=1 for modern-store index import")
			}
			name := fmt.Sprintf("localhost/coopr-import-%d-%d:latest", os.Getpid(), time.Now().UnixNano())
			var owned []string
			t.Cleanup(func() { removeDockerEngineImages(t, owned) })
			producer := Options{File: file, Platform: "linux/" + runtime.GOARCH, Tag: "docker:" + name, BuildStore: nativeBuildTestStore(filepath.Join(work, "producer")), Jobs: 1}
			if multi {
				producer.Platforms = []string{"linux/amd64", "linux/arm64"}
			}
			if _, err := Run(ctx, producer); err != nil {
				t.Fatal(err)
			}
			owned = append(owned, name)
			storeDir := filepath.Join(work, fmt.Sprintf("import-%t", multi))
			destination, err := transfer.ParseDestination("imported:latest", oci.Image)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transfer.Copy(ctx, oci.Image, "docker:"+name, destination, transfer.Options{BuildStore: nativeBuildTestStore(storeDir)}); err != nil {
				t.Fatal(err)
			}
			if multi {
				_, _, selections, complete, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), "imported:latest")
				if err != nil || !complete || len(selections) != 2 {
					t.Fatalf("imported Docker index incomplete: complete=%t selections=%d err=%v", complete, len(selections), err)
				}
			}
			_, found, err := testStoredImageSelection(ctx, nativeBuildTestStore(storeDir), "imported:latest", v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
			if err != nil || !found {
				t.Fatalf("imported engine image missing: %t %v", found, err)
			}
			consumer := filepath.Join(work, "consumer.coopr")
			if err := os.WriteFile(consumer, []byte("from \"imported:latest\"\nlabel offline=\"yes\"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(work, fmt.Sprintf("imported-%t.oci.tar", multi))
			if _, err := Run(ctx, Options{File: consumer, BuildStore: nativeBuildTestStore(storeDir), Tag: "oci-archive:" + archive, Platform: "linux/" + runtime.GOARCH, PullPolicy: "never", Jobs: 1}); err != nil {
				t.Fatal(err)
			}
			config := archiveImageConfig(t, archive)
			if config.Config.Labels["engine_import"] != "yes" || config.Config.Labels["offline"] != "yes" || !containsName(archiveLayerNames(t, archive), "marker") {
				t.Fatal("offline Docker import lost content/configuration")
			}
		})
	}
}
