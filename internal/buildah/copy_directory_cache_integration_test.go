package buildah

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
)

func TestInstructionCacheKeepsCopyParentAfterUncachedExport(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, backend := range []string{"local", "registry"} {
		t.Run(backend, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			root, contextDir := copyDirectoryCacheContext(t)
			policy := writeComponentTestPolicy(t, root)
			cacheDir, repository := "", ""
			if backend == "local" {
				cacheDir = filepath.Join(root, "cache")
			} else {
				server := httptest.NewServer(registryserver.New(registryserver.Logger(log.New(io.Discard, "", 0))))
				t.Cleanup(server.Close)
				repository = strings.TrimPrefix(server.URL, "http://") + "/coopr/copy-parent"
			}
			build := func(name, command string, noCache, metadataProbe bool) (Result, string) {
				t.Helper()
				base := filepath.Join(root, name)
				layout := filepath.Join(base, "layout")
				metadata := ""
				if metadataProbe {
					metadata = `label cache_probe="copy-parent"`
				}
				plan := testPlan(t, fmt.Sprintf(`from "scratch" {
    copy "busybox" "/bin/busybox"
    run network="none" { exec "/bin/busybox" "sh" "-eu" "-c" %q; }
    %s
}`, command, metadata))
				var diagnostics bytes.Buffer
				result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
					Store: cacheTestStore(base), ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
					NoCache: noCache, CacheLocalDir: cacheDir, CacheRepository: repository,
					Output: Output{Path: layout}, SignaturePolicyPath: policy, TLSVerify: new(false),
					Stdout: io.Discard, Stderr: &diagnostics,
				})
				if err != nil {
					t.Fatalf("%v\n%s", err, diagnostics.String())
				}
				if result.CacheStats.Errors != 0 {
					t.Logf("%s: %s", name, diagnostics.String())
				}
				return result, layout
			}
			const command = "/bin/busybox stat -c %Y /bin > /parent-time; /bin/busybox od -An -N16 -tx1 /dev/urandom > /proof"
			cold, coldLayout := build("cold", command, false, false)
			if cold.CacheStats.Hits != 0 || cold.CacheStats.Misses != 2 || cold.CacheStats.Stored != 3 {
				t.Fatalf("cold cache: %+v", cold.CacheStats)
			}
			proof := localComponentLastFile(t, coldLayout, "proof")
			// Guarantee that an unnormalized COPY would create a different
			// directory mtime, rather than depending on machine speed.
			second := time.Now().Unix()
			for time.Now().Unix() == second {
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			uncached, uncachedLayout := build("other-branch", "/bin/busybox stat -c %Y /bin > /parent-time; printf 'other branch\\n' > /proof", true, false)
			if uncached.CacheStats.Hits != 0 || uncached.CacheStats.Misses != 0 || uncached.CacheStats.Stored != 3 {
				t.Fatalf("uncached export: %+v", uncached.CacheStats)
			}
			if got, original := localComponentLastFile(t, uncachedLayout, "parent-time"), localComponentLastFile(t, coldLayout, "parent-time"); got == original {
				t.Fatalf("forced native COPY did not produce a distinct parent timestamp: %q", got)
			}
			// Change only final metadata so the aggregate misses and both native
			// instruction hits still exercise the shared COPY parent.
			warm, warmLayout := build("warm", command, false, true)
			if warm.CacheStats.Hits != 2 || warm.CacheStats.Misses != 0 {
				t.Fatalf("uncached shared COPY replaced the warm RUN parent: %+v", warm.CacheStats)
			}
			_, warmImage := readPlanImage(t, warmLayout)
			if warmImage.Config.Labels["cache_probe"] != "copy-parent" {
				t.Fatal("warm instruction hits lost the final metadata probe")
			}
			if got := localComponentLastFile(t, warmLayout, "proof"); got != proof {
				t.Fatalf("warm RUN executed again: %q != %q", got, proof)
			}
			if got, want := localComponentLastFile(t, warmLayout, "parent-time"), localComponentLastFile(t, coldLayout, "parent-time"); got != want || got == "0\n" {
				t.Fatalf("warm graph changed native COPY parent timestamp: got %q, original %q", got, want)
			}
			// Refresh must still execute nondeterministic work and replace its
			// result. Keeping the first published result is not a repair.
			refreshed, refreshedLayout := build("refreshed", command, true, false)
			freshProof := localComponentLastFile(t, refreshedLayout, "proof")
			if freshProof == proof || refreshed.CacheStats.Hits != 0 || refreshed.CacheStats.Misses != 0 || refreshed.CacheStats.Stored != 3 {
				t.Fatalf("no-cache failed to refresh RUN: proof=%q stats=%+v", freshProof, refreshed.CacheStats)
			}
			after, afterLayout := build("after-refresh", command, false, false)
			if after.CacheStats.Hits != 1 || after.CacheStats.Misses != 0 || localComponentLastFile(t, afterLayout, "proof") != freshProof {
				t.Fatalf("warm cache did not retain refreshed result: %+v", after.CacheStats)
			}
		})
	}
}

func TestInstructionCacheTracksAuthoredFilesystemMetadata(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root, contextDir := copyDirectoryCacheContext(t)
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("payload\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	policy := writeComponentTestPolicy(t, root)
	for index, stamp := range []int64{100, 200, 200} {
		base := filepath.Join(root, fmt.Sprint(index))
		layout := filepath.Join(base, "layout")
		metadata := ""
		if index == 2 {
			metadata = `label cache_probe="authored-metadata"`
		}
		plan := testPlan(t, fmt.Sprintf(`from "scratch" {
    copy "busybox" "/bin/busybox"
    run network="none" { exec "/bin/busybox" "touch" "-m" "-d" "@%d" "/bin" "/authored"; }
    copy "payload" "/bin/payload"
    run network="none" { exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox stat -c %%Y /authored > /parent-time"; }
    %s
}`, stamp, metadata))
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: cacheTestStore(base), ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
			CacheLocalDir: filepath.Join(root, "cache"), Output: Output{Path: layout},
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := localComponentLastFile(t, layout, "parent-time"); got != fmt.Sprintf("%d\n", stamp) {
			t.Fatalf("graph lost authored filesystem metadata: %q, want %d", got, stamp)
		}
		if index == 1 && (result.CacheStats.Hits != 1 || result.CacheStats.Misses != 3) {
			t.Fatalf("authored filesystem change did not invalidate downstream instructions: %+v", result.CacheStats)
		}
		if index == 2 && (result.CacheStats.Hits != 4 || result.CacheStats.Misses != 0) {
			t.Fatalf("identical authored metadata did not retain warm hits: %+v", result.CacheStats)
		}
		if index == 2 {
			_, image := readPlanImage(t, layout)
			if image.Config.Labels["cache_probe"] != "authored-metadata" {
				t.Fatal("warm instruction hits lost the final metadata probe")
			}
		}
	}
}

func copyDirectoryCacheContext(t *testing.T) (root, contextDir string) {
	t.Helper()
	root = t.TempDir()
	contextDir = filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(contextDir, "busybox")
	if err := os.WriteFile(target, data, 0o755); err != nil {
		t.Fatal(err)
	}
	epoch := time.Unix(0, 0)
	if err := os.Chtimes(target, epoch, epoch); err != nil {
		t.Fatal(err)
	}
	return root, contextDir
}
