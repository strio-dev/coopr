//go:build dockerintegration

package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"coopr/internal/build"
	"coopr/internal/oci"
	"coopr/internal/testutil"
	"coopr/internal/transfer"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestExplicitDockerImportThenOfflineFrom(t *testing.T) {
	if testing.Short() {
		t.Skip("live Docker imports")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	modern := dockerEngineSupportsIndexes(t, ctx)
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
			name := fmt.Sprintf("localhost/coopr-import-%d-%d:latest", os.Getpid(), time.Now().UnixNano())
			var owned []string
			t.Cleanup(func() { removeDockerEngineImages(t, owned) })
			producer := build.Options{File: file, Platform: "linux/" + runtime.GOARCH, Tag: "docker:" + name, BuildStore: dockerTestStore(filepath.Join(work, "producer")), Jobs: 1}
			if multi {
				producer.Platforms = []string{"linux/amd64", "linux/arm64"}
			}
			_, err := build.Run(ctx, producer)
			if multi && !modern {
				if err == nil || !strings.Contains(err.Error(), "classic image store") {
					t.Fatalf("classic Docker must reject a complete index: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			owned = append(owned, name)
			storeDir := filepath.Join(work, fmt.Sprintf("import-%t", multi))
			destination, err := transfer.ParseDestination("imported:latest", oci.Image)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transfer.Copy(ctx, oci.Image, "docker:"+name, destination, transfer.Options{BuildStore: dockerTestStore(storeDir)}); err != nil {
				t.Fatal(err)
			}
			importedPlatforms := inspectDockerImportedPlatforms(t, ctx, dockerTestStore(storeDir), "imported:latest", multi)
			if multi && len(importedPlatforms) != 2 {
				t.Fatalf("imported Docker index incomplete: platforms=%v", importedPlatforms)
			}
			if !slices.Contains(importedPlatforms, "linux/"+runtime.GOARCH) {
				t.Fatalf("imported engine image missing host platform: %v", importedPlatforms)
			}
			consumer := filepath.Join(work, "consumer.coopr")
			if err := os.WriteFile(consumer, []byte("from \"imported:latest\"\nlabel offline=\"yes\"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			platforms := []v1.Platform{{OS: "linux", Architecture: runtime.GOARCH}}
			if multi {
				platforms = []v1.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}
			}
			for _, platform := range platforms {
				t.Run(platform.Architecture, func(t *testing.T) {
					archive := filepath.Join(work, fmt.Sprintf("imported-%t-%s.oci.tar", multi, platform.Architecture))
					if _, err := build.Run(ctx, build.Options{File: consumer, BuildStore: dockerTestStore(storeDir), Tag: "oci-archive:" + archive, Platform: platform.OS + "/" + platform.Architecture, PullPolicy: "never", Jobs: 1}); err != nil {
						t.Fatal(err)
					}
					_, config := testutil.ReadImage(t, ctx, archive)
					if config.OS != platform.OS || config.Architecture != platform.Architecture {
						t.Fatalf("offline Docker import selected %s/%s, want %s/%s", config.OS, config.Architecture, platform.OS, platform.Architecture)
					}
					if config.Config.Labels["engine_import"] != "yes" || config.Config.Labels["offline"] != "yes" || !dockerArchiveHasPath(t, ctx, archive, "marker") {
						t.Fatal("offline Docker import lost content/configuration")
					}
				})
			}
		})
	}
}
