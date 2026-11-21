package buildah

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInstructionGraphPublishesNativeHitWithoutReadPayload(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	input := filepath.Join(root, "context")
	if err := os.Mkdir(input, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "payload"), []byte("native cache publication proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, `from "scratch" { copy "payload" "/payload"; }`)
	producerStore := cacheTestStore(filepath.Join(root, "native-store"))
	source := CacheSpec{Transport: "oci-layout", Reference: filepath.Join(root, "source")}
	mirror := CacheSpec{Transport: "oci-layout", Reference: filepath.Join(root, "mirror")}
	policy := writeComponentTestPolicy(t, root)
	build := func(lane string, store StoreOptions, from, to []CacheSpec) Result {
		t.Helper()
		var logs bytes.Buffer
		layout := filepath.Join(root, lane)
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: input, Isolation: "rootless", Runtime: "crun",
			CacheFrom: from, CacheTo: to, SignaturePolicyPath: policy,
			Output: Output{Path: layout}, Stdout: &logs, Stderr: &logs,
		})
		if err != nil || result.CacheStats.Errors != 0 {
			t.Fatalf("%s: stats=%+v err=%v logs=%s", lane, result.CacheStats, err, logs.String())
		}
		return result
	}
	cold := build("cold", producerStore, nil, []CacheSpec{source})
	if cold.CacheStats.Misses == 0 || cold.CacheStats.Stored == 0 {
		t.Fatalf("producer did not populate native and portable caches: %+v", cold.CacheStats)
	}
	manifest, _ := readPlanImage(t, cold.Layout)
	// Remove a payload, not the graph metadata or the manifest descriptor, so
	// metadata admission still sees the same optional source.
	config := manifest.Config.Digest
	if err := os.Remove(filepath.Join(source.Reference, "blobs", config.Algorithm().String(), config.Encoded())); err != nil {
		t.Fatal(err)
	}
	warm := build("native-hit", producerStore, []CacheSpec{source}, []CacheSpec{mirror})
	if warm.ManifestDigest != cold.ManifestDigest || warm.CacheStats.Hits == 0 ||
		warm.CacheStats.Misses != 0 || warm.CacheStats.Stored == 0 {
		t.Fatalf("verified native hit did not publish to the empty destination: cold=%+v warm=%+v", cold, warm)
	}
	offline := build("mirror-hit", cacheTestStore(filepath.Join(root, "fresh-store")), []CacheSpec{mirror}, nil)
	if offline.ManifestDigest != cold.ManifestDigest || offline.CacheStats.Hits == 0 ||
		offline.CacheStats.Misses != 0 || localComponentLastFile(t, offline.Layout, "payload") != "native cache publication proof\n" {
		t.Fatalf("fresh native store could not restore the exact mirror payload: %+v", offline)
	}
}
