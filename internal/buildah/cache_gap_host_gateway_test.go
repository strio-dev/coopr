package buildah

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
	imageoci "go.podman.io/image/v5/oci/layout"
)

func TestCacheGapHostGatewayCacheMatchesNative(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	root := t.TempDir()
	nativeStore := cacheTestStore(filepath.Join(root, "native"))
	base := newLiveBusyBoxStorage(t, ctx, root, nativeStore)
	policy := writeComponentTestPolicy(t, root)
	hosts := []string{"gateway.test:host-gateway"}
	command := `grep gateway.test /etc/hosts >/dev/null; od -An -N16 -tx1 /dev/urandom | tr -d ' \n' >/proof`
	file := filepath.Join(root, "Containerfile")
	if err := os.WriteFile(file, []byte(fmt.Sprintf("FROM %s\nRUN --network=none %s\n", base.reference, command)), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(nativeStore)
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for _, attempt := range []string{"cold", "warm"} {
		var progress bytes.Buffer
		id, _, err := imagebuildah.BuildDockerfiles(ctx, lease.store, define.BuildOptions{ContextDirectory: root, Output: "native-gateway-cache:test", OutputFormat: define.OCIv1ImageManifest, Layers: true, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{AddHost: hosts}, Compression: define.Uncompressed, Out: &progress, Err: &progress, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
		if err != nil {
			_ = lease.Close()
			t.Fatalf("native %s: %v\n%s", attempt, err, progress.String())
		}
		layout := filepath.Join(root, "native-"+attempt)
		destination, err := imageoci.NewReference(layout, "latest")
		if err != nil {
			_ = lease.Close()
			t.Fatal(err)
		}
		if _, _, err := upstream.Push(ctx, id, destination, upstream.PushOptions{Store: lease.store, SignaturePolicyPath: policy}); err != nil {
			_ = lease.Close()
			t.Fatal(err)
		}
		proof := localComponentLastFile(t, layout, "proof")
		if attempt == "cold" {
			first = proof
		} else if proof != first || !strings.Contains(progress.String(), "Using cache") {
			_ = lease.Close()
			t.Fatalf("native host-gateway cache did not hit: %s", progress.String())
		}
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	remote, _ := newLiveBusyBoxRegistry(t, ctx)
	resolver := localConfigComponentResolver(t, ctx, root, fmt.Sprintf("extend { run %q network=\"none\"; }", command), false)
	for _, scope := range []string{"instruction", "component"} {
		t.Run(scope, func(t *testing.T) {
			var first string
			for _, attempt := range []string{"cold", "warm"} {
				dir := filepath.Join(root, scope, attempt)
				body := fmt.Sprintf("run %q network=\"none\"", command)
				if scope == "component" {
					body = `component "config"`
				}
				result, err := BuildPlanSupervised(ctx, testPlan(t, fmt.Sprintf("from %q { %s; }", remote, body)), SupervisedPlanOptions{Store: cacheTestStore(dir), ContextDir: root, ComponentStoreDir: resolver.ComponentStoreDir(), CacheLocalDir: filepath.Join(root, scope, "cache"), Isolation: "rootless", Runtime: "crun", AddHosts: hosts, TLSVerify: new(false), SignaturePolicyPath: policy, Output: Output{Path: filepath.Join(dir, "layout")}, Stdout: io.Discard, Stderr: io.Discard})
				if err != nil {
					t.Fatal(err)
				}
				proof := localComponentLastFile(t, result.Layout, "proof")
				if attempt == "cold" {
					first = proof
				} else if proof != first || result.CacheStats.Hits != 1 || result.CacheStats.Misses != 0 {
					t.Fatalf("%s host-gateway cache=%+v", scope, result.CacheStats)
				}
			}
		})
	}
}
