//go:build dockerintegration

package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/build"
	"coopr/internal/buildah"
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
	if err := os.WriteFile(file, []byte(`from "scratch" {
    copy "marker" "/marker"
    label engine_import="yes"
}
`), 0600); err != nil {
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
			producer := build.Options{File: file, Platform: "linux/" + runtime.GOARCH, Tag: name, BuildStore: dockerTestStore(filepath.Join(work, "producer")), Jobs: 1}
			if multi {
				producer.Platforms = []string{"linux/amd64", "linux/arm64"}
			}
			if _, err := build.Run(ctx, producer); err != nil {
				t.Fatal(err)
			}
			_, err := transfer.Copy(ctx, oci.Image, name, transfer.Destination{Transport: "docker", Name: name}, transfer.Options{BuildStore: producer.BuildStore})
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
			consumer := filepath.Join(work, "consumer.coopr")
			platforms := []v1.Platform{{OS: "linux", Architecture: runtime.GOARCH}}
			if multi {
				platforms = []v1.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}
			}
			for _, platform := range platforms {
				t.Run(platform.Architecture, func(t *testing.T) {
					id, err := buildah.PullImage(ctx, dockerTestStore(storeDir), oci.Options{PullPolicy: "always"}, "docker-daemon:"+name, platform)
					if err != nil {
						t.Fatal(err)
					}
					base := "imported:" + platform.Architecture
					destination, err := transfer.ParseTag(base, oci.Image)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := transfer.Copy(ctx, oci.Image, id, destination, transfer.Options{BuildStore: dockerTestStore(storeDir), Platform: platform, PlatformExplicit: true}); err != nil {
						t.Fatal(err)
					}
					if got := inspectDockerImportedPlatforms(t, ctx, dockerTestStore(storeDir), base); len(got) != 1 || got[0] != platform.OS+"/"+platform.Architecture {
						t.Fatalf("pulled Engine platform = %v, want %s/%s", got, platform.OS, platform.Architecture)
					}
					if err := os.WriteFile(consumer, []byte(fmt.Sprintf(`from %q {
    label offline="yes"
}
`, base)), 0600); err != nil {
						t.Fatal(err)
					}
					archive := filepath.Join(work, fmt.Sprintf("imported-%t-%s.oci.tar", multi, platform.Architecture))
					if _, err := build.Run(ctx, build.Options{File: consumer, BuildStore: dockerTestStore(storeDir), Tag: "coopr-imported-child:" + platform.Architecture, Platform: platform.OS + "/" + platform.Architecture, PullPolicy: "never", Jobs: 1}); err != nil {
						t.Fatal(err)
					}
					if _, err := transfer.Copy(ctx, oci.Image, "coopr-imported-child:"+platform.Architecture, transfer.Destination{Transport: "oci-archive", Name: archive}, transfer.Options{BuildStore: dockerTestStore(storeDir), Platform: platform, PlatformExplicit: true}); err != nil {
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
