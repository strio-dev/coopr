package buildah

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"coopr/internal/oci"
	"github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
	"go.podman.io/common/libimage"
	libconfig "go.podman.io/common/pkg/config"
	"go.podman.io/storage"
)

func TestNativeImageIDFromMatchesBuildah(t *testing.T) {
	f := newLocalComponentFixture(t)
	base, _ := f.build(t, "base", `from "scratch" { env marker="native-id"; }`)
	lease, err := acquireStore(f.options.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()

	t.Run("explicit-id-retains-native-platform-admission", func(t *testing.T) {
		native, err := libimage.RuntimeFromStore(lease.store, nil)
		if err != nil {
			t.Fatal(err)
		}
		foreign := v1.Platform{OS: "linux", Architecture: "arm64"}
		if runtime.GOARCH == "arm64" {
			foreign.Architecture = "amd64"
		}
		if _, err := native.Pull(f.ctx, base.ImageID, libconfig.PullPolicyNever, &libimage.PullOptions{CopyOptions: libimage.CopyOptions{OS: foreign.OS, Architecture: foreign.Architecture}}); err != nil {
			t.Fatalf("native explicit-ID control: %v", err)
		}
		resolver, err := oci.NewResolver(oci.Options{PullPolicy: "never"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := ResolveImageSource(f.ctx, resolver, base.ImageID, foreign, lease.store, f.options.SystemContext)
		if err != nil || got.ImageID != base.ImageID {
			t.Fatalf("explicit ID rejected after native admission: %+v %v", got, err)
		}
	})
	for _, reference := range []string{base.ImageID, "sha256:" + base.ImageID} {
		t.Run(reference[:8], func(t *testing.T) {
			file := filepath.Join(f.contextDir, "Containerfile")
			if err := os.WriteFile(file, []byte("FROM "+reference+"\nENV inherited=yes\n"), 0600); err != nil {
				t.Fatal(err)
			}
			_, _, err := imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{ContextDirectory: f.contextDir, Layers: true, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
			if err != nil {
				t.Fatalf("upstream FROM %s: %v", reference, err)
			}
			for _, policy := range []string{"missing", "never", "newer", "always"} {
				resolver, err := oci.NewResolver(oci.Options{PullPolicy: policy})
				if err != nil {
					t.Fatal(err)
				}
				selected, err := ResolveImageSource(f.ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, lease.store, f.options.SystemContext)
				if policy == "always" {
					if err == nil || !strings.Contains(err.Error(), "pull policy is always") {
						t.Fatalf("pull always: %v", err)
					}
					continue
				}
				if err != nil || selected.ImageID != base.ImageID || selected.Remote {
					t.Fatalf("%s by %s = %+v, %v", policy, reference, selected, err)
				}
			}
			_, layout := f.build(t, "output-"+strings.ReplaceAll(reference[:8], ":", "-"), fmt.Sprintf(`from %q { env inherited="yes"; }`, reference))
			_, image := readPlanImage(t, layout)
			if !slices.Contains(image.Config.Env, "marker=native-id") || !slices.Contains(image.Config.Env, "inherited=yes") {
				t.Fatalf("config: %+v", image.Config)
			}
		})
	}
	// The manifest digest is a distinct existing local identity, rather than
	// the config-derived storage ID accepted by libimage.
	if _, err := lease.store.Image(strings.TrimPrefix(base.ManifestDigest, "sha256:")); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("manifest digest unexpectedly resolves as storage ID: %v", err)
	}
	for _, policy := range []string{"missing", "never", "newer", "always"} {
		resolver, err := oci.NewResolver(oci.Options{PullPolicy: policy})
		if err != nil {
			t.Fatal(err)
		}
		selected, err := ResolveImageSource(f.ctx, resolver, base.ManifestDigest, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, lease.store, f.options.SystemContext)
		if policy == "always" {
			if err == nil || !strings.Contains(err.Error(), "pull policy is always") {
				t.Fatalf("manifest digest pull always: %v", err)
			}
			continue
		}
		if err != nil || selected.ImageID != base.ImageID || selected.Selected.Digest.String() != base.ManifestDigest || selected.Remote {
			t.Fatalf("manifest digest %s = %+v, %v", policy, selected, err)
		}
	}
	_, layout := f.build(t, "manifest-output", fmt.Sprintf(`from %q { env inherited="manifest"; }`, base.ManifestDigest))
	_, image := readPlanImage(t, layout)
	if !slices.Contains(image.Config.Env, "marker=native-id") || !slices.Contains(image.Config.Env, "inherited=manifest") {
		t.Fatalf("manifest-digest config: %+v", image.Config)
	}
	missing := strings.Repeat("f", 64)
	for _, reference := range []string{missing, "sha256:" + missing} {
		for _, policy := range []string{"missing", "never", "newer"} {
			resolver, err := oci.NewResolver(oci.Options{PullPolicy: policy})
			if err != nil {
				t.Fatal(err)
			}
			_, err = ResolveImageSource(f.ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, lease.store, f.options.SystemContext)
			if !errors.Is(err, storage.ErrImageUnknown) || strings.Contains(err.Error(), "resolve base image") {
				t.Fatalf("missing local %s/%s: %v", reference, policy, err)
			}
		}
	}
}
