package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/oci"
	"coopr/internal/transfer"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Opt-in because this writes images into the Docker daemon selected by
// DOCKER_HOST. Every name is unique to this test and removed afterward.
func TestBuildAndCopyToDockerEngine(t *testing.T) {
	if testing.Short() || os.Getenv("COOPR_TEST_DOCKER") != "1" || os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_DOCKER=1 and COOPR_TEST_BUILDAH=1 for a live Docker Engine transfer")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("Docker CLI is required for independent runtime verification: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	work := dockerEngineWorkspace(t)
	file := loadTestDefinition(t, ctx, work)
	storeDir := filepath.Join(work, "images")
	marker := fmt.Sprintf("docker-%d-%d", os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(work, "marker"), []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"oci", "docker"} {
		t.Run(format, func(t *testing.T) {
			name := "localhost/coopr-docker-" + marker + "-" + format + ":latest"
			copiedName := name + "-copy"
			var created []string
			t.Cleanup(func() { removeDockerEngineImages(t, created) })
			options := Options{
				File: file, Platform: "linux/" + runtime.GOARCH,
				Tag: "docker:" + name, Format: format, BuildStore: nativeBuildTestStore(storeDir),
			}
			got, err := Run(ctx, options)
			if err != nil || got != "docker:"+name {
				t.Fatalf("build directly into Docker: result=%q error=%v", got, err)
			}
			created = append(created, name)
			localName := "coopr-docker-source-" + marker + "-" + format + ":latest"
			options.Tag = localName
			if _, err := Run(ctx, options); err != nil {
				t.Fatalf("build local image for later Docker copy: %v", err)
			}
			destination, err := transfer.ParseDestination("docker:"+copiedName, oci.Image)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transfer.Copy(ctx, oci.Image, localName, destination, transfer.Options{BuildStore: nativeBuildTestStore(storeDir)}); err != nil {
				t.Fatalf("copy stored image into Docker: %v", err)
			}
			created = append(created, copiedName)
			for _, image := range []string{name, copiedName} {
				output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network=none", image).CombinedOutput()
				if err != nil || strings.TrimSpace(string(output)) != "loaded:"+marker {
					t.Fatalf("run transferred image %s: %v: %s", image, err, output)
				}
				data, err := exec.CommandContext(ctx, "docker", "image", "inspect", image).CombinedOutput()
				if err != nil {
					t.Fatalf("inspect transferred image: %v: %s", err, data)
				}
				var images []struct {
					Architecture string
					OS           string
					RootFS       struct{ Layers []string }
					Config       struct {
						Cmd    []string
						Labels map[string]string
					}
				}
				if err := json.Unmarshal(data, &images); err != nil {
					t.Fatal(err)
				}
				if len(images) != 1 || images[0].Architecture != runtime.GOARCH || images[0].OS != "linux" ||
					len(images[0].RootFS.Layers) != 2 || images[0].Config.Labels["coopr_test"] != "load" ||
					len(images[0].Config.Cmd) != 1 || images[0].Config.Cmd[0] != "/runner" {
					t.Fatalf("Docker transfer lost image layers/config: %+v", images)
				}
			}
		})
	}
}

func dockerEngineWorkspace(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	// Committed storage layers can retain read-only root-directory modes.
	// Restore owner permissions before testing.TempDir removes this owned graph.
	t.Cleanup(func() {
		if err := filepath.WalkDir(work, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return nil
		}); err != nil {
			t.Errorf("prepare test graph cleanup: %v", err)
		}
	})
	return work
}

func removeDockerEngineImages(t *testing.T, names []string) {
	t.Helper()
	if len(names) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", append([]string{"image", "rm"}, names...)...).CombinedOutput(); err != nil {
		t.Errorf("remove test-owned Docker images: %v: %s", err, output)
	}
}

func TestBuildAndCopyMultiPlatformToDockerEngine(t *testing.T) {
	if testing.Short() || os.Getenv("COOPR_TEST_DOCKER") != "1" || os.Getenv("COOPR_TEST_DOCKER_MULTIPLATFORM") != "1" || os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_DOCKER=1, COOPR_TEST_DOCKER_MULTIPLATFORM=1, and COOPR_TEST_BUILDAH=1 for live Docker multi-platform transfer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	info, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .DriverStatus}}").CombinedOutput()
	if err != nil {
		t.Fatalf("inspect Docker image store: %v: %s", err, info)
	}
	var driverStatus [][2]string
	if err := json.Unmarshal(info, &driverStatus); err != nil {
		t.Fatal(err)
	}
	modern := false
	for _, entry := range driverStatus {
		modern = modern || entry == [2]string{"driver-type", "io.containerd.snapshotter.v1"}
	}
	work := dockerEngineWorkspace(t)
	source := filepath.Join(work, "proof.go")
	program := `package main
import ("fmt"; "os"; "runtime")
func main() {
  if _, err := os.Stat("/built"); os.IsNotExist(err) {
    if err := os.WriteFile("/built", []byte(runtime.GOARCH), 0644); err != nil { panic(err) }
  }
  data, err := os.ReadFile("/built"); if err != nil { panic(err) }
  if string(data) != runtime.GOARCH { panic("incorrect platform selected") }
  fmt.Print(string(data))
}
`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(work, "proof-"+arch), source)
		command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("compile %s proof: %v: %s", arch, err, output)
		}
	}
	file := filepath.Join(work, "image.coopr")
	if err := os.WriteFile(file, []byte(`from "scratch"
arg "TARGETARCH"
copy "proof-$TARGETARCH" "/proof" chmod="0755"
run network="none" { exec "/proof" }
cmd { exec "/proof" }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(work, "images")
	marker := fmt.Sprintf("multi-%d-%d", os.Getpid(), time.Now().UnixNano())
	for _, format := range []string{"oci", "docker"} {
		t.Run(format, func(t *testing.T) {
			name := "localhost/coopr-docker-" + marker + "-" + format + ":latest"
			var created []string
			t.Cleanup(func() { removeDockerEngineImages(t, created) })
			options := Options{File: file, BuildStore: nativeBuildTestStore(storeDir), Format: format,
				Platforms: []string{"linux/amd64", "linux/arm64"}, Tag: "docker:" + name}
			got, err := Run(ctx, options)
			if !modern {
				if err == nil || !strings.Contains(err.Error(), "classic image store") {
					t.Fatalf("classic Docker must reject a complete index: %q, %v", got, err)
				}
				return
			}
			if err != nil || got != "docker:"+name {
				t.Fatalf("build multi-platform image into Docker: %q, %v", got, err)
			}
			created = append(created, name)
			options.Tag = "coopr-docker-source-" + marker + "-" + format + ":latest"
			if _, err := Run(ctx, options); err != nil {
				t.Fatal(err)
			}
			root, _, _, found, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), options.Tag)
			if err != nil || !found {
				t.Fatalf("lookup local source index: found=%t error=%v", found, err)
			}
			copiedName := name + "-copy"
			if _, err := transfer.Copy(ctx, oci.Image, options.Tag, transfer.Destination{Transport: "docker", Name: copiedName}, transfer.Options{BuildStore: nativeBuildTestStore(storeDir)}); err != nil {
				t.Fatalf("copy complete local index into Docker: %v", err)
			}
			created = append(created, copiedName)
			data, err := exec.CommandContext(ctx, "docker", "image", "inspect", copiedName).CombinedOutput()
			if err != nil {
				t.Fatalf("inspect Docker index: %v: %s", err, data)
			}
			var images []struct{ Descriptor v1.Descriptor }
			if err := json.Unmarshal(data, &images); err != nil || len(images) != 1 || images[0].Descriptor.Digest != root.Digest {
				t.Fatalf("Docker index identity changed: %+v, %v, want %s", images, err, root.Digest)
			}
			for _, image := range created {
				for _, arch := range []string{"amd64", "arm64"} {
					output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network=none", "--platform", "linux/"+arch, image).CombinedOutput()
					if err != nil || strings.TrimSpace(string(output)) != arch {
						t.Fatalf("run Docker index child %s: %v: %s", arch, err, output)
					}
				}
			}
		})
	}
}
