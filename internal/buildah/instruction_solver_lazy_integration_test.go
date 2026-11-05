package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The forced lanes retain several physical RUN results under one logical graph.
// A fresh consumer needs only the selected COPY and RUN snapshots, not every
// historical result exported by the producer.
func TestInstructionGraphRestoreImportsOnlySelectedSnapshots(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root, input := copyDirectoryCacheContext(t)
	policy := writeComponentTestPolicy(t, root)
	cacheDir := filepath.Join(root, "cache")
	plan := testPlan(t, `from "scratch" {
    copy "busybox" "/bin/busybox"
    run network="none" { exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox od -An -N16 -tx1 /dev/urandom > /proof"; }
}`)
	build := func(lane string, force bool) (Result, string) {
		t.Helper()
		layout := filepath.Join(root, lane, "layout")
		var logs bytes.Buffer
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: cacheTestStore(filepath.Join(root, lane)), ContextDir: input,
			Isolation: "rootless", Runtime: "crun", NoCache: force,
			CacheLocalDir: cacheDir, SignaturePolicyPath: policy,
			Output: Output{Path: layout}, Stdout: io.Discard, Stderr: &logs,
		})
		if err != nil {
			t.Fatalf("%s: %v\n%s", lane, err, logs.String())
		}
		if result.CacheStats.Errors != 0 {
			t.Fatalf("%s cache errors: %+v\n%s", lane, result.CacheStats, logs.String())
		}
		return result, localComponentLastFile(t, layout, "proof")
	}
	var latest string
	for i := range 3 {
		_, proof := build(fmt.Sprintf("producer-%d", i), i != 0)
		if proof == latest {
			t.Fatal("forced producer did not execute its nondeterministic RUN")
		}
		latest = proof
	}
	warm, proof := build("consumer", false)
	if warm.CacheStats.Hits != 2 || warm.CacheStats.Misses != 0 || proof != latest {
		t.Fatalf("warm result lost selected graph: proof=%q want=%q stats=%+v", proof, latest, warm.CacheStats)
	}
	lease, err := acquireStore(cacheTestStore(filepath.Join(root, "consumer")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	images, err := lease.store.Images()
	if err != nil {
		t.Fatal(err)
	}
	retained := 0
	for _, image := range images {
		for _, name := range image.Names {
			if strings.HasPrefix(name, graphResultNamePrefix) {
				retained++
				break
			}
		}
	}
	t.Logf("fresh consumer retained %d native graph snapshots after three producer variants", retained)
	if retained != 2 {
		t.Fatalf("fresh consumer imported %d native graph snapshots, want only its selected COPY and RUN", retained)
	}
}

func TestInstructionGraphParallelWorkersPreserveHistoricalVariants(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	root, input := copyDirectoryCacheContext(t)
	policy := writeComponentTestPolicy(t, root)
	source, mirror := filepath.Join(root, "source"), filepath.Join(root, "mirror")
	plan := testPlan(t, `from "scratch" as="left" {
 copy "busybox" "/bin/busybox"
 run network="none" { exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox od -An -N16 -tx1 /dev/urandom > /proof"; }
}
from "scratch" as="right" {
 copy "busybox" "/bin/busybox"
 run network="none" { exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox od -An -N16 -tx1 /dev/urandom > /other"; }
}
from "scratch" {
 copy "/other" "/other" from="right"
 copy "/proof" "/proof" from="left"
}`)
	build := func(lane string, force bool, to string) (Result, string) {
		t.Helper()
		layout := filepath.Join(root, lane, "layout")
		var logs bytes.Buffer
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: cacheTestStore(filepath.Join(root, lane)), ContextDir: input, Isolation: "rootless", Runtime: "crun", Jobs: 2, NoCache: force,
			CacheFrom: []CacheSpec{{Transport: "oci-layout", Reference: source}}, CacheTo: []CacheSpec{{Transport: "oci-layout", Reference: to}},
			SignaturePolicyPath: policy, Output: Output{Path: layout}, Stdout: io.Discard, Stderr: &logs,
		})
		if err != nil {
			t.Fatalf("%s: %v\n%s", lane, err, logs.String())
		}
		if result.CacheStats.Errors != 0 {
			t.Fatalf("%s cacheerrors %+v\n%s", lane, result.CacheStats, logs.String())
		}
		return result, localComponentLastFile(t, layout, "proof")
	}
	var latest string
	for i := range 3 {
		_, latest = build(fmt.Sprintf("producer-%d", i), i != 0, source)
	}
	before := instructionGraphArtifactResults(t, source)
	warm, proof := build("mirror-consumer", false, mirror)
	if proof != latest || warm.CacheStats.Misses != 0 || warm.CacheStats.Hits == 0 {
		t.Fatalf("parallel warm graph changed: %q != %q stats=%+v", proof, latest, warm.CacheStats)
	}
	after := instructionGraphArtifactResults(t, mirror)
	historical := false
	for root, ids := range after {
		want, exists := before[root]
		if !exists {
			t.Fatalf("mirror published unknown graph root %s", root)
		}
		for _, id := range want {
			if !slices.Contains(ids, id) {
				t.Fatalf("parallel mirror discarded alternative %s for %s: got%d wantatleast%d", id, root, len(ids), len(want))
			}
		}
		historical = historical || len(ids) >= 3
	}
	if !historical {
		t.Fatal("parallel fixture did not retain historical physical variants")
	}
	t.Logf("parallel mirror retained all historical alternatives across %d selected graph roots", len(after))
}

func instructionGraphArtifactResults(t *testing.T, root string) map[digest.Digest][]string {
	t.Helper()
	read := func(d digest.Digest) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, "blobs", "sha256", d.Encoded()))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b, err := os.ReadFile(filepath.Join(root, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(b, &index); err != nil {
		t.Fatal(err)
	}
	results := map[digest.Digest][]string{}
	for _, descriptor := range index.Manifests {
		var manifest v1.Manifest
		if err := json.Unmarshal(read(descriptor.Digest), &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.ArtifactType != cache.GraphArtifactType {
			continue
		}
		var record cache.GraphRecord
		if err := json.Unmarshal(read(manifest.Config.Digest), &record); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, result := range record.Results {
			ids = append(ids, result.ID)
		}
		slices.Sort(ids)
		results[record.Root] = ids
	}
	return results
}
