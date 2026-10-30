package buildah

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/oci"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	libconfig "go.podman.io/common/pkg/config"
	"go.podman.io/image/v5/types"
)

func TestResolveImageSourcePinnedAlwaysUsesNativePolicy(t *testing.T) {
	testPinnedRefreshPolicy(t, "always")
}

func TestResolveImageSourcePinnedNewerUsesNativePolicy(t *testing.T) {
	testPinnedRefreshPolicy(t, "newer")
}

func testPinnedRefreshPolicy(t *testing.T, policy string) {
	t.Helper()
	f := newLocalComponentFixture(t)
	base, _ := f.build(t, "base", `from "scratch" { env proof="cached"; }`)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	name := strings.TrimPrefix(server.URL, "http://") + "/proof"
	lease, err := acquireStore(f.options.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := lease.store.AddNames(base.ImageID, []string{name + ":latest"}); err != nil {
		t.Fatal(err)
	}
	ref := name + "@" + base.ManifestDigest
	system := &types.SystemContext{SignaturePolicyPath: writeComponentTestPolicy(t, f.root), DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}
	native, err := libimage.RuntimeFromStore(lease.store, &libimage.RuntimeOptions{SystemContext: system})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := native.Pull(ctx, ref, libconfig.PullPolicyNever, nil); err != nil {
		t.Fatalf("native cached control: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("never contacted registry")
	}
	resolver, err := oci.NewResolver(oci.Options{PullPolicy: policy, TLSVerify: new(false), SignaturePolicyPath: system.SignaturePolicyPath, RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	got, cooprErr := ResolveImageSource(ctx, resolver, ref, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, lease.store, system)
	cooprRequests := requests.Swap(0)
	nativePolicy, err := libconfig.ParsePullPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	nativeImages, nativeErr := native.Pull(ctx, ref, nativePolicy, nil)
	nativeRequests := requests.Load()
	t.Logf("ref=%s Coopr image=%s err=%v requests=%d native error=%v requests=%d", ref, got.ImageID, cooprErr, cooprRequests, nativeErr, nativeRequests)
	wantError := policy == "always"
	if (cooprErr != nil) != wantError || cooprRequests == 0 || (nativeErr != nil) != wantError || nativeRequests == 0 {
		t.Fatalf("pull-%s bypass: Coopr=%v/%d native=%v/%d", policy, cooprErr, cooprRequests, nativeErr, nativeRequests)
	}
	if !wantError && (got.ImageID != base.ImageID || len(nativeImages) != 1 || nativeImages[0].ID() != base.ImageID) {
		t.Fatalf("newer fallback changed cached image: Coopr=%+v native=%+v", got, nativeImages)
	}
}
