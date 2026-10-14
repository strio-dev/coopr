package buildah

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
)

func TestNegativeCacheTTLMatchesNativeBuildah(t *testing.T) {
	f := newLocalComponentFixture(t)
	negative := -time.Second
	file := filepath.Join(f.contextDir, "Containerfile")
	if err := os.WriteFile(file, []byte("FROM scratch\nENV proof=negative-ttl\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(f.options.Store)
	if err != nil {
		t.Fatal(err)
	}
	_, _, nativeErr := imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{
		ContextDirectory: f.contextDir, Layers: true, CacheTTL: negative,
		PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun",
		CommonBuildOpts: &define.CommonBuildOptions{}, SystemContext: f.options.SystemContext,
		Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true,
	}, file)
	closeErr := lease.Close()
	if nativeErr != nil || closeErr != nil {
		t.Fatalf("native negative TTL: %v; close: %v", nativeErr, closeErr)
	}
	t.Log("pinned native BuildDockerfiles accepted negative cache TTL")
	f.options.CacheTTL = &negative
	f.write(t, "payload", "negative TTL still writes snapshots")
	for _, attempt := range []string{"cold", "warm"} {
		result, _ := f.build(t, attempt, `from "scratch" { copy "payload" "/payload"; }`)
		if result.CacheStats.Hits != 0 || result.CacheStats.Stored == 0 {
			t.Fatalf("%s negative TTL stats: %+v", attempt, result.CacheStats)
		}
	}
}
