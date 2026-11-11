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

func TestInstructionGraphParallelWorkersExportSelectedVariants(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	root, input := copyDirectoryCacheContext(t)
	policy := writeComponentTestPolicy(t, root)
	mirror := filepath.Join(root, "mirror")
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
	var from []CacheSpec
	build := func(lane string, force bool, to string) (Result, string) {
		t.Helper()
		layout := filepath.Join(root, lane, "layout")
		var logs bytes.Buffer
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: cacheTestStore(filepath.Join(root, lane)), ContextDir: input, Isolation: "rootless", Runtime: "crun", Jobs: 2, NoCache: force,
			CacheFrom: from, CacheTo: []CacheSpec{{Transport: "oci-layout", Reference: to}},
			SignaturePolicyPath: policy, Output: Output{Path: layout}, Stdout: io.Discard, Stderr: &logs,
		})
		if err != nil {
			t.Fatalf("%s: %v\n%s", lane, err, logs.String())
		}
		if result.CacheStats.Errors != 0 {
			t.Fatalf("%s cacheerrors %+v\n%s", lane, result.CacheStats, logs.String())
		}
		manifest, _ := readPlanImage(t, layout)
		if len(manifest.Layers) != 2 {
			t.Fatalf("final stage must contain its two authored COPY layers: %+v", manifest.Layers)
		}
		other := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()), "other")
		return result, localComponentLastFile(t, layout, "proof") + "\x00" + other
	}
	var latest string
	before := map[digest.Digest][]string{}
	for i := range 3 {
		source := filepath.Join(root, fmt.Sprintf("source-%d", i))
		_, latest = build(fmt.Sprintf("producer-%d", i), i != 0, source)
		from = append(from, CacheSpec{Transport: "oci-layout", Reference: source})
		for key, ids := range instructionGraphArtifactResults(t, source) {
			for _, id := range ids {
				if !slices.Contains(before[key], id) {
					before[key] = append(before[key], id)
				}
			}
		}
	}
	warm, proof := build("mirror-consumer", false, mirror)
	if proof != latest || warm.CacheStats.Misses != 0 || warm.CacheStats.Hits == 0 {
		t.Fatalf("parallel warm graph changed: %q != %q stats=%+v", proof, latest, warm.CacheStats)
	}
	after := instructionGraphArtifactResults(t, mirror)
	selected := instructionGraphStoredResultIDs(t, cacheTestStore(filepath.Join(root, "mirror-consumer")))
	omitted := false
	rebound := 0
	for root, ids := range after {
		want, exists := before[root]
		if !exists {
			t.Fatalf("mirror published unknown graph root %s", root)
		}
		for _, id := range ids {
			if !selected[id] {
				t.Fatalf("parallel export included an unmaterialized source alternative %s for %s", id, root)
			}
			if !slices.Contains(want, id) {
				// Cached filesystems can be recommitted when current authored
				// history/configuration is rebound. Export that actual native
				// result, rather than requiring a stale source manifest ID.
				rebound++
			}
		}
		omitted = omitted || len(ids) < len(want)
	}
	if len(after) == 0 || !omitted {
		t.Fatal("parallel export mirrored every unused source alternative")
	}
	for _, source := range from {
		if err := os.Rename(source.Reference, source.Reference+"-unavailable"); err != nil {
			t.Fatal(err)
		}
	}
	from = []CacheSpec{{Transport: "oci-layout", Reference: mirror}}
	offline, proof := build("offline-consumer", false, filepath.Join(root, "offline-cache"))
	if proof != latest || offline.CacheStats.Misses != 0 || offline.CacheStats.Hits == 0 {
		t.Fatalf("selected parallel graph was not complete offline: %q != %q stats=%+v", proof, latest, offline.CacheStats)
	}
	t.Logf("parallel selected export retained exact outputs across %d graph roots without unused alternatives; %d exported associations use recommitted native metadata", len(after), rebound)
}

func instructionGraphStoredResultIDs(t *testing.T, options StoreOptions) map[string]bool {
	t.Helper()
	lease, err := acquireStore(options)
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
	selected := map[string]bool{}
	for _, image := range images {
		for _, name := range image.Names {
			if strings.HasPrefix(name, graphResultNamePrefix) {
				selected["sha256:"+strings.TrimPrefix(name, graphResultNamePrefix)] = true
			}
		}
	}
	return selected
}

func TestInstructionGraphSelectedExportPreservesOlderBranches(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root, input := copyDirectoryCacheContext(t)
	policy := writeComponentTestPolicy(t, root)
	cacheDir := filepath.Join(root, "cache")
	build := func(lane, branch string, force bool) (Result, string) {
		t.Helper()
		plan := testPlan(t, fmt.Sprintf(`from "scratch" {
 copy "busybox" "/bin/busybox"
 run network="none" { exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox od -An -N16 -tx1 /dev/urandom > /proof; printf '%%s' '%s' > /branch"; }
}`, branch))
		layout := filepath.Join(root, lane, "layout")
		var logs bytes.Buffer
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: cacheTestStore(filepath.Join(root, lane)), ContextDir: input,
			Isolation: "rootless", Runtime: "crun", NoCache: force,
			CacheLocalDir: cacheDir, SignaturePolicyPath: policy,
			Output: Output{Path: layout}, Stdout: io.Discard, Stderr: &logs,
		})
		if err != nil || result.CacheStats.Errors != 0 {
			t.Fatalf("%s: %v stats=%+v\n%s", lane, err, result.CacheStats, logs.String())
		}
		if got := localComponentLastFile(t, layout, "branch"); got != branch {
			t.Fatalf("%s changed branch: %q != %q", lane, got, branch)
		}
		return result, localComponentLastFile(t, layout, "proof")
	}
	_, first := build("producer-A", "A", false)
	before := instructionGraphArtifactResults(t, cacheDir)
	// Native COPY keys exclude source mtime. Force a distinct physical COPY
	// under the same declared content key, rather than relying on clock timing.
	stamp := time.Unix(123, 0)
	if err := os.Chtimes(filepath.Join(input, "busybox"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, second := build("producer-B", "B", true); first == second {
		t.Fatal("forced sibling did not produce a fresh output")
	}
	after := instructionGraphArtifactResults(t, cacheDir)
	preserved, replacedPrefix := false, false
	for key, ids := range before {
		// Shared prefixes may be replaced; A's downstream root must retain
		// its complete selected output and intermediate associations.
		preserved = preserved || len(ids) == 2 && slices.Equal(ids, after[key])
		replacedPrefix = replacedPrefix || len(ids) == 1 && len(after[key]) == 1 && !slices.Equal(ids, after[key])
	}
	if !preserved || !replacedPrefix {
		t.Fatalf("forced sibling qualification failed: older branch preserved=%t, shared physical prefix replaced=%t; before=%v after=%v", preserved, replacedPrefix, before, after)
	}
	warm, restored := build("fresh-store-A", "A", false)
	if restored != first || warm.CacheStats.Hits != 2 || warm.CacheStats.Misses != 0 {
		t.Fatalf("older branch lost exact cached output: %q != %q stats=%+v", restored, first, warm.CacheStats)
	}
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
		if descriptor.MediaType != v1.MediaTypeImageIndex {
			continue
		}
		var envelope v1.Index
		if err := json.Unmarshal(read(descriptor.Digest), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.ArtifactType != cache.GraphArtifactType || len(envelope.Manifests) == 0 {
			continue
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(read(envelope.Manifests[0].Digest), &manifest); err != nil {
			t.Fatal(err)
		}
		var record cache.GraphRecord
		if err := json.Unmarshal(read(manifest.Config.Digest), &record); err != nil {
			t.Fatal(err)
		}
		// OCI layouts can retain untagged previous manifests. Only the active
		// graph tag describes this cache key's current advertised snapshot.
		if descriptor.Annotations[v1.AnnotationRefName] != "graph-v2-"+record.Root.Encoded() {
			continue
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
