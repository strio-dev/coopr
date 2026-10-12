//go:build dockerintegration

package acceptance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	binary := dockerImportCLI(t, ctx)
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
	native := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	foreign := v1.Platform{OS: "linux", Architecture: "arm64"}
	if native.Architecture == foreign.Architecture {
		foreign.Architecture = "amd64"
	}
	for _, test := range []struct {
		name     string
		platform v1.Platform
		multi    bool
	}{
		{name: "native-single", platform: native},
		{name: "foreign-single", platform: foreign},
		{name: "mixed-index", platform: native, multi: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := fmt.Sprintf("localhost/coopr-import-%d-%d:latest", os.Getpid(), time.Now().UnixNano())
			var owned []string
			t.Cleanup(func() { removeDockerEngineImages(t, owned) })
			producer := build.Options{File: file, Platform: test.platform.OS + "/" + test.platform.Architecture, Tag: name, BuildStore: dockerTestStore(filepath.Join(work, "producer")), Jobs: 1}
			if test.multi {
				producer.Platforms = []string{"linux/amd64", "linux/arm64"}
			}
			if _, err := build.Run(ctx, producer); err != nil {
				t.Fatal(err)
			}
			_, err := transfer.Copy(ctx, oci.Image, name, transfer.Destination{Transport: "docker", Name: name}, transfer.Options{BuildStore: producer.BuildStore})
			if test.multi && !modern {
				if err == nil || !strings.Contains(err.Error(), "classic image store") {
					t.Fatalf("classic Docker must reject a complete index: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			owned = append(owned, name)
			storeDir := filepath.Join(work, "import-"+test.name)
			consumer := filepath.Join(work, "consumer.coopr")
			platforms := []v1.Platform{test.platform}
			if test.multi {
				platforms = []v1.Platform{native, foreign}
			}
			for _, platform := range platforms {
				t.Run(platform.Architecture, func(t *testing.T) {
					store := dockerTestStore(storeDir)
					args := []string{"--root", store.GraphRoot, "--runroot", store.RunRoot, "--storage-driver", store.GraphDriverName}
					for _, option := range store.GraphDriverOptions {
						args = append(args, "--storage-opt", option)
					}
					args = append(args, "pull", "--quiet", "--policy=always", "--platform", platform.OS+"/"+platform.Architecture, "docker-daemon:"+name)
					command := exec.CommandContext(ctx, binary, args...)
					var diagnostics bytes.Buffer
					command.Stderr = &diagnostics
					output, err := command.Output()
					if test.multi && platform.Architecture != native.Architecture {
						// The upstream daemon transport consumes Docker's compatibility
						// image; --platform validates it rather than selecting an index child.
						var exit *exec.ExitError
						if !errors.As(err, &exit) || exit.ExitCode() != 125 || !strings.Contains(diagnostics.String(), "differs from requested "+platform.OS+"/"+platform.Architecture+"/") {
							t.Fatalf("expected daemon index platform mismatch: %v: %s", err, diagnostics.String())
						}
						if len(bytes.TrimSpace(output)) != 0 {
							t.Fatalf("rejected daemon index platform printed an image ID: %q", output)
						}
						return
					}
					if err != nil {
						t.Fatalf("pull Docker image through Coopr CLI: %v: %s", err, diagnostics.String())
					}
					ids := strings.Fields(string(output))
					if len(ids) != 1 {
						t.Fatalf("pull Docker image returned %q, want one image ID", output)
					}
					id := ids[0]
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
					archive := filepath.Join(work, fmt.Sprintf("imported-%s-%s.oci.tar", test.name, platform.Architecture))
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

// Build once for all import cases; the CLI owns native storage namespace setup.
func dockerImportCLI(t *testing.T, ctx context.Context) string {
	t.Helper()
	if path, supplied := os.LookupEnv("COOPR_TEST_CLI"); supplied {
		if err := testutil.ValidateExecutable(path); err != nil {
			t.Fatalf("COOPR_TEST_CLI: %v", err)
		}
		return path
	}
	path := filepath.Join(t.TempDir(), "coopr")
	command := exec.CommandContext(ctx, "go", "build", "-o", path, "./cmd/coopr")
	command.Dir = filepath.Join("..", "..")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Coopr CLI for Docker import: %v: %s", err, output)
	}
	return path
}
