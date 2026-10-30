package transfer

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/define"
	imagebuildah "go.podman.io/buildah/imagebuildah"
	"go.podman.io/common/libimage"
	libconfig "go.podman.io/common/pkg/config"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestWholeIndexOptionalVariantExportMatchesNative(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}

	for _, tc := range []struct {
		name               string
		config, descriptor v1.Platform
	}{
		{"omitted-variant", v1.Platform{OS: "linux", Architecture: "amd64"}, v1.Platform{OS: "linux", Architecture: "amd64", Variant: "v3"}},
		{"different-real-architecture", v1.Platform{OS: "linux", Architecture: "arm64"}, v1.Platform{OS: "linux", Architecture: "amd64"}},
		{"omitted-platform-fields", v1.Platform{}, v1.Platform{OS: "linux", Architecture: "amd64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			opts := nativeTestStore(t.TempDir())
			base := tc.config
			selected := nativeEmptyImageFixture(t, opts, base, "audit-optional-variant")
			descriptorPlatform := tc.descriptor
			child := selected.Manifest
			child.Platform = &descriptorPlatform
			raw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{child}})
			root := oci.Descriptor(v1.MediaTypeImageIndex, raw)
			if err := nativeTestIndex(ctx, opts, "variant-export:latest", root, raw, map[string]oci.StoredSelection{"linux/amd64/v3": selected}); err != nil {
				t.Fatal(err)
			}
			policy := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := buildah.WithStore(opts, func(store storage.Store) error {
				native, err := libimage.RuntimeFromStore(store, &libimage.RuntimeOptions{SystemContext: &types.SystemContext{SignaturePolicyPath: policy}})
				if err != nil {
					return err
				}
				list, err := native.LookupManifestList("variant-export:latest")
				if err != nil {
					return err
				}
				_, err = list.Push(ctx, "oci-archive:"+filepath.Join(t.TempDir(), "native.tar"), &libimage.ManifestListPushOptions{ImageListSelection: imagecopy.CopyAllImages})
				return err
			}); err != nil {
				t.Fatalf("actual native all-instance archive copy: %v", err)
			}
			t.Log("actual native all-instance OCI archive export accepted")
			archive := filepath.Join(t.TempDir(), "coopr.tar")
			_, err := Copy(ctx, oci.Image, "variant-export:latest", Destination{Transport: "oci-archive", Name: archive}, Options{BuildStore: opts, SignaturePolicyPath: policy})
			t.Logf("actual Coopr all-instance export: %v", err)
			if err != nil {
				t.Fatalf("native-compatible whole-index export failed: %v", err)
			}
			got, err := oci.ArchiveRoot(archive)
			if err != nil || got.Digest != root.Digest {
				t.Fatalf("whole-index identity changed: %+v %v", got, err)
			}

		})
	}
}

func TestStoredIndexFromAdmissionMatchesNativeFrontend(t *testing.T) {
	ctx := context.Background()
	opts := nativeTestStore(t.TempDir())
	base := v1.Platform{OS: "linux", Architecture: "amd64"}
	selected := nativeEmptyImageFixture(t, opts, base, "audit-from")
	child := selected.Manifest
	child.Platform = &base
	raw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{child}})
	root := oci.Descriptor(v1.MediaTypeImageIndex, raw)
	if err := nativeTestIndex(ctx, opts, "variant-from:latest", root, raw, map[string]oci.StoredSelection{"linux/amd64": selected}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, []byte("FROM variant-from:latest\nENV proof=ok\n"), 0600); err != nil {
		t.Fatal(err)
	}
	request := base
	request.Variant = "v3"
	system := &types.SystemContext{OSChoice: request.OS, ArchitectureChoice: request.Architecture, VariantChoice: request.Variant, SignaturePolicyPath: policy}
	err := buildah.WithStore(opts, func(store storage.Store) error {
		native, err := libimage.RuntimeFromStore(store, &libimage.RuntimeOptions{SystemContext: system})
		if err != nil {
			return err
		}
		_, pullErr := native.Pull(ctx, "variant-from:latest", libconfig.PullPolicyNever, &libimage.PullOptions{CopyOptions: libimage.CopyOptions{OS: request.OS, Architecture: request.Architecture, Variant: request.Variant}})
		t.Logf("native PullNever=%v", pullErr)
		_, _, buildErr := imagebuildah.BuildDockerfiles(ctx, store, define.BuildOptions{ContextDirectory: dir, PullPolicy: define.PullNever, Isolation: define.IsolationChroot, CommonBuildOpts: &define.CommonBuildOptions{}, SystemContext: system, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true, Platforms: []struct{ OS, Arch, Variant string }{{"linux", "amd64", "v3"}}}, file)
		t.Logf("native BuildDockerfiles=%v", buildErr)
		resolver, err := oci.NewResolver(oci.Options{PullPolicy: "never", SignaturePolicyPath: policy})
		if err != nil {
			return err
		}
		_, cooprErr := buildah.ResolveImageSource(ctx, resolver, "variant-from:latest", request, store, system)
		t.Logf("Coopr ResolveImageSource=%v", cooprErr)
		if pullErr == nil || buildErr == nil || cooprErr == nil {
			t.Fatalf("cached platform admission mismatch: native pull=%v build=%v Coopr=%v", pullErr, buildErr, cooprErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
