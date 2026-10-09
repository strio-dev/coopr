package buildah

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/planner"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
	"go.podman.io/image/v5/types"
)

func TestBuildDefinitionCompatVolumesPreservesRunBoundaryButAllowsCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live compatibility-volume coverage in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	if err := os.WriteFile(filepath.Join(root, "copied"), []byte("copied"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`from %q
run "/bin/busybox mkdir -p /vol; printf before >/vol/value" network="none"
volume "/vol"
run "printf during >/vol/value" network="none"
copy "copied" "/vol/copied"
run "printf '%%s:%%s' \"$(/bin/busybox cat /vol/value)\" \"$(/bin/busybox cat /vol/copied)\" >/proof" network="none"
`, base.reference)
	def, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		compat   bool
		noLayers bool
		want     string
	}{
		{name: "normal", want: "during:copied"},
		{name: "compat", compat: true, want: "before:copied"},
		{name: "compat-no-layers", compat: true, noLayers: true, want: "before:copied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout := filepath.Join(root, "layout-"+test.name)
			_, err := BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH}, SupervisedPlanOptions{
				Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
				Lifecycle: LifecycleControls{CompatVolumes: test.compat, NoLayers: test.noLayers},
				Output:    Output{Path: layout},
				Stdout:    io.Discard, Stderr: os.Stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			manifest, _ := readPlanImage(t, layout)
			last := filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded())
			if got := readLayerFile(t, last, "proof"); got != test.want {
				t.Fatalf("proof = %q, want %q", got, test.want)
			}
			nativeFile := filepath.Join(root, "Containerfile-"+test.name)
			nativeSource := fmt.Sprintf(`FROM %s
RUN --network=none /bin/busybox mkdir -p /vol; printf before >/vol/value
VOLUME /vol
RUN --network=none printf during >/vol/value
COPY copied /vol/copied
RUN --network=none printf '%%s:%%s' "$(/bin/busybox cat /vol/value)" "$(/bin/busybox cat /vol/copied)" >/proof
`, base.reference)
			if err := os.WriteFile(nativeFile, []byte(nativeSource), 0600); err != nil {
				t.Fatal(err)
			}
			lease, err := acquireStore(store)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lease.Close(); err != nil {
					t.Errorf("close native store: %v", err)
				}
			}()
			nativeExport := filepath.Join(root, "upstream-"+test.name)
			compat := types.OptionalBoolFalse
			if test.compat {
				compat = types.OptionalBoolTrue
			}
			_, _, err = imagebuildah.BuildDockerfiles(ctx, lease.store, define.BuildOptions{ContextDirectory: root, Layers: !test.noLayers, CompatVolumes: compat, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, Compression: define.Uncompressed, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true, BuildOutputs: []string{"type=local,dest=" + nativeExport}}, nativeFile)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(nativeExport, "proof"))
			if err != nil || string(got) != test.want {
				t.Fatalf("pinned Buildah proof=%q error=%v want=%q", got, err, test.want)
			}

		})
	}
}

func TestFinalVolumeCreatesDirectoryInFilesystemExport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native integration in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	export := filepath.Join(root, "rootfs")
	_, err := BuildPlanSupervised(ctx, testPlan(t, `from "scratch"
volume "/data"
`), SupervisedPlanOptions{Store: StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}, ContextDir: root, Output: Output{Path: filepath.Join(root, "layout"), Filesystem: FilesystemOutput{Type: "local", Path: export}}, Stdout: io.Discard, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(export, "data"))
	if err != nil || !info.IsDir() {
		t.Fatalf("final volume directory missing: %v", err)
	}
}

func TestCompatVolumesRestoreConfinesReplacedParentSymlink(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	outside := filepath.Join(root, "controlled-host-target")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`from %q
run "/bin/busybox mkdir -p /parent/data; /bin/busybox chmod 0710 /parent/data; printf original >/parent/data/value" network="none"
volume "/parent/data"
run %q network="none"
`, base.reference, "/bin/busybox rm -rf /parent; /bin/busybox ln -s "+outside+" /parent")
	export := filepath.Join(root, "export")
	_, err := BuildDefinitionSupervised(ctx, parseWorkerDefinition(t, source), planner.Options{Mode: planner.Build}, SupervisedPlanOptions{
		Store: store, Isolation: "rootless", Runtime: "crun",
		Lifecycle: LifecycleControls{CompatVolumes: true, NoLayers: true},
		Output:    Output{Path: filepath.Join(root, "layout"), Filesystem: FilesystemOutput{Type: "local", Path: export}},
		Stdout:    io.Discard, Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "data")); !os.IsNotExist(err) {
		t.Fatalf("volume restoration escaped into controlled host directory: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(outside, "sentinel")); err != nil || string(data) != "unchanged" {
		t.Fatalf("host sentinel changed: %q %v", data, err)
	}
	restored := filepath.Join(export, strings.TrimPrefix(outside, "/"), "data")
	if data, err := os.ReadFile(filepath.Join(restored, "value")); err != nil || string(data) != "original" {
		t.Fatalf("confined volume contents=%q error=%v", data, err)
	}
	if info, err := os.Stat(restored); err != nil || info.Mode().Perm() != 0710 {
		t.Fatalf("confined volume metadata=%v error=%v", info, err)
	}
}
