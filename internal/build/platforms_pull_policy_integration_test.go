package build

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
	"go.podman.io/common/libimage"
	libconfig "go.podman.io/common/pkg/config"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestPinnedAlwaysPlatformDiscoveryMatchesNativeFrontend(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	ctx := context.Background()
	var requests atomic.Int64
	var unavailable atomic.Bool
	handler := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if unavailable.Load() {
			http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	name := strings.TrimPrefix(server.URL, "http://") + "/audit/all"
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), PullPolicy: "always"})
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	layout, err := orasoci.NewWithContext(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var variants []oci.ImageVariant
	var firstConfig []byte
	for _, arch := range []string{"amd64", "arm64"} {
		platform := v1.Platform{OS: "linux", Architecture: arch}
		raw, _ := json.Marshal(v1.Image{Platform: platform, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{}}})
		config := oci.Descriptor(v1.MediaTypeImageConfig, raw)
		if err := layout.Push(ctx, config, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		if firstConfig == nil {
			firstConfig = raw
		}
		mraw, _ := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{}})
		m := oci.Descriptor(v1.MediaTypeImageManifest, mraw)
		if err := layout.Push(ctx, m, bytes.NewReader(mraw)); err != nil {
			t.Fatal(err)
		}
		variants = append(variants, oci.ImageVariant{Manifest: m, Platform: platform})
	}
	root, data, err := oci.ImageIndexDescriptor(variants, "oci")
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Push(ctx, root, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := layout.Tag(ctx, root, root.Digest.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.PublishLayout(ctx, name+":latest", path, root); err != nil {
		t.Fatal(err)
	}
	reference := name + "@" + root.Digest.String()
	opts := nativeBuildTestStore(t.TempDir())
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{ArchitectureChoice: "amd64", OSChoice: "linux", DockerInsecureSkipTLSVerify: types.OptionalBoolTrue, SignaturePolicyPath: policy}
	err = buildah.WithStore(opts, func(store storage.Store) error {
		native, err := libimage.RuntimeFromStore(store, &libimage.RuntimeOptions{SystemContext: system})
		if err != nil {
			return err
		}
		images, err := native.Pull(ctx, reference, libconfig.PullPolicyAlways, &libimage.PullOptions{CopyOptions: libimage.CopyOptions{OS: "linux", Architecture: "amd64"}})
		if err != nil {
			return err
		}
		return oci.RecordStoredOrigin(ctx, store, reference, oci.StoredSelection{Root: root, Manifest: variants[0].Manifest, ImageID: images[0].ID(), ConfigData: firstConfig})
	})
	if err != nil {
		t.Fatal(err)
	}
	requests.Store(0)
	available, err := availableImagePlatforms(ctx, resolver, opts, reference)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Coopr explicit-always pinned discovery=%+v", available)
	if requests.Load() == 0 {
		t.Error("explicit always did not contact the registry")
	}

	requests.Store(0)
	if err := buildah.WithStore(opts, func(store storage.Store) error {
		native, err := libimage.RuntimeFromStore(store, &libimage.RuntimeOptions{SystemContext: system})
		if err != nil {
			return err
		}
		_, err = native.Pull(ctx, reference, libconfig.PullPolicyNewer, &libimage.PullOptions{CopyOptions: libimage.CopyOptions{OS: "linux", Architecture: "amd64"}})
		return err
	}); err != nil {
		t.Fatalf("native pinned-newer control: %v", err)
	}
	if requests.Load() == 0 {
		t.Fatal("native pinned newer did not contact registry")
	}
	for _, tc := range []struct {
		policy  string
		offline bool
		count   int
		remote  bool
		reject  bool
	}{
		{"missing", false, 1, false, false}, {"never", false, 1, false, false}, {"newer", false, 2, true, false},
		{"missing", true, 1, false, false}, {"never", true, 1, false, false}, {"newer", true, 1, true, false}, {"always", true, 0, true, true},
	} {
		t.Run(tc.policy+fmt.Sprint("-offline-", tc.offline), func(t *testing.T) {
			unavailable.Store(tc.offline)
			defer unavailable.Store(false)
			requests.Store(0)
			policyResolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), PullPolicy: tc.policy, RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			got, err := availableImagePlatforms(ctx, policyResolver, opts, reference)
			if (err != nil) != tc.reject || len(got) != tc.count {
				t.Fatalf("policy=%s offline=%t platforms=%+v err=%v", tc.policy, tc.offline, got, err)
			}
			if (requests.Load() > 0) != tc.remote {
				t.Fatalf("registry requests=%d expected-contact=%t", requests.Load(), tc.remote)
			}
		})
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, []byte("FROM "+reference+"\nENV proof=all\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err = buildah.WithStore(opts, func(store storage.Store) error {
		_, _, err := imagebuildah.BuildDockerfiles(ctx, store, define.BuildOptions{ContextDirectory: dir, AllPlatforms: true, PullPolicy: define.PullAlways, Isolation: define.IsolationChroot, CommonBuildOpts: &define.CommonBuildOptions{}, SystemContext: system, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
		if err != nil {
			return err
		}
		images, err := store.Images()
		if err != nil {
			return err
		}
		var architectures []string
		for _, image := range images {
			raw, err := store.ImageBigData(image.ID, "manifest")
			if err != nil {
				continue
			}
			var m v1.Manifest
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			raw, err = store.ImageBigData(image.ID, m.Config.Digest.String())
			if err != nil {
				continue
			}
			var cfg v1.Image
			if json.Unmarshal(raw, &cfg) == nil && slices.Contains(cfg.Config.Env, "proof=all") {
				architectures = append(architectures, cfg.Architecture)
			}
		}
		t.Logf("actual native all-platform build outputs=%v", architectures)
		slices.Sort(architectures)
		if len(available) != 2 || !slices.Equal(architectures, []string{"amd64", "arm64"}) {
			t.Fatalf("platform discovery diverges Coopr=%+v native=%v", available, architectures)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
