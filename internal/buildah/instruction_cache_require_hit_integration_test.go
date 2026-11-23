package buildah

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstructionRequireHitAcrossSupervisedParallelPlan(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root, input := copyDirectoryCacheContext(t)
	policy := writeComponentTestPolicy(t, root)
	source := CacheSpec{Transport: "oci-layout", Reference: filepath.Join(root, "cache")}
	plan := testPlan(t, `
from "scratch" as="left" {
    copy "busybox" "/bin/busybox"
    run network="none" { exec "/bin/busybox" "sh" "-ec" "echo REQUIRE_HIT_LEFT; /bin/busybox od -An -N16 -tx1 /dev/urandom > /left"; }
}
from "scratch" as="right" {
    copy "busybox" "/bin/busybox"
    run network="none" { exec "/bin/busybox" "sh" "-ec" "echo REQUIRE_HIT_RIGHT; /bin/busybox od -An -N16 -tx1 /dev/urandom > /right"; }
}
from "scratch" {
    copy "/left" "/left" from="left"
    copy "/right" "/right" from="right"
}
`)
	build := func(lane string, requireHit bool, from, to []CacheSpec) (Result, string, string, error) {
		t.Helper()
		var logs bytes.Buffer
		layout := filepath.Join(root, lane, "layout")
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: cacheTestStore(filepath.Join(root, lane)), ContextDir: input, Jobs: 2,
			Isolation: "rootless", Runtime: "crun", RequireCacheHit: requireHit,
			CacheFrom: from, CacheTo: to, SignaturePolicyPath: policy,
			Output: Output{Path: layout}, Stdout: &logs, Stderr: io.Discard,
		})
		return result, layout, logs.String(), err
	}
	cold, _, logs, err := build("producer", false, nil, []CacheSpec{source})
	if err != nil || cold.CacheStats.Stored == 0 || cold.CacheStats.Errors != 0 ||
		!strings.Contains(logs, "REQUIRE_HIT_LEFT\n") || !strings.Contains(logs, "REQUIRE_HIT_RIGHT\n") {
		t.Fatalf("producer did not run both branches and publish the graph: stats=%+v err=%v stdout=%s", cold.CacheStats, err, logs)
	}

	warm, _, logs, err := build("hit", true, []CacheSpec{source}, nil)
	if err != nil || warm.ManifestDigest != cold.ManifestDigest || warm.CacheStats.Hits == 0 ||
		warm.CacheStats.Misses != 0 || warm.CacheStats.Stored != 0 || warm.CacheStats.Errors != 0 ||
		strings.Contains(logs, "REQUIRE_HIT_") {
		t.Fatalf("require-hit did not restore the exact graph without running branches: stats=%+v err=%v stdout=%s", warm.CacheStats, err, logs)
	}

	_, layout, logs, err := build("miss", true, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "required cache result unavailable") || strings.Contains(logs, "REQUIRE_HIT_") {
		t.Fatalf("require-hit miss did not stop before branch execution: err=%v stdout=%s", err, logs)
	}
	if _, err := os.Stat(layout); !os.IsNotExist(err) {
		t.Fatalf("require-hit miss left output at %s: %v", layout, err)
	}
}
