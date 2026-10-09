package buildah

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestBuildPlanReusesFinalRunWithoutChangingImage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live final instruction cache build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"
`, base.reference))
	var firstProof string
	var firstImage v1.Image
	firstLayerCount := 0
	for attempt := range 2 {
		layout := filepath.Join(root, "final-layout-"+string(rune('1'+attempt)))
		var stderr strings.Builder
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, Pull: false,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stderr.String(), "STEP 1/2: FROM ") || !strings.Contains(stderr.String(), "STEP 2/2: RUN ") || strings.Count(stderr.String(), "COMMIT\n") != 1 {
			t.Fatalf("build %d instruction/final progress = %q", attempt+1, stderr.String())
		}
		if attempt == 1 && !strings.Contains(stderr.String(), "--> Using cache ") {
			t.Fatalf("warm build did not report instruction cache hit: %q", stderr.String())
		}
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != len(image.RootFS.DiffIDs) {
			t.Fatalf("build %d layers=%d diffIDs=%d", attempt+1, len(manifest.Layers), len(image.RootFS.DiffIDs))
		}
		last := manifest.Layers[len(manifest.Layers)-1]
		proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
		if attempt == 0 {
			firstProof, firstImage, firstLayerCount = proof, image, len(manifest.Layers)
			continue
		}
		if len(manifest.Layers) != firstLayerCount {
			t.Fatalf("final cached layer count = %d, want %d", len(manifest.Layers), firstLayerCount)
		}
		if proof != firstProof {
			t.Fatalf("final cached RUN proof changed: first %q, second %q", firstProof, proof)
		}
		if !reflect.DeepEqual(image, firstImage) {
			t.Fatalf("final cached image config changed:\nfirst:  %#v\nsecond: %#v", firstImage, image)
		}
	}

	plan = testPlan(t, fmt.Sprintf(`
from %q
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof; printf tail >>/proof" network="none"
label cached="yes"
`, base.reference))
	firstProof = ""
	firstImage = v1.Image{}
	for attempt := range 2 {
		layout := filepath.Join(root, "tail-layout-"+string(rune('1'+attempt)))
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, Pull: false,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != firstLayerCount || len(image.RootFS.DiffIDs) != firstLayerCount {
			t.Fatalf("build %d layers=%d diffIDs=%d, want %d", attempt+1, len(manifest.Layers), len(image.RootFS.DiffIDs), firstLayerCount)
		}
		if image.Config.Labels["cached"] != "yes" {
			t.Fatalf("build %d labels = %#v", attempt+1, image.Config.Labels)
		}
		last := manifest.Layers[len(manifest.Layers)-1]
		proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
		if attempt == 0 {
			firstProof, firstImage = proof, image
			continue
		}
		if proof != firstProof || !reflect.DeepEqual(image, firstImage) {
			t.Fatalf("config-tail cache changed output: proof %q/%q image equal=%v", firstProof, proof, reflect.DeepEqual(image, firstImage))
		}
	}
}

func TestWarmHostNetworkRunCacheStillRequiresEntitlement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live instruction cache build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf("from %q\nrun \"printf cached-host-run >/proof\" network=\"host\"\n", base.reference))
	common := SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Pull: false, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	}
	allowed := common
	allowed.Allow = []string{"network.host"}
	allowed.Output = Output{Path: filepath.Join(root, "allowed-layout")}
	if _, err := BuildPlanSupervised(ctx, plan, allowed); err != nil {
		t.Fatalf("populate host-network RUN cache: %v", err)
	}
	denied := common
	denied.Output = Output{Path: filepath.Join(root, "denied-layout")}
	if _, err := BuildPlanSupervised(ctx, plan, denied); err == nil || !strings.Contains(err.Error(), "--allow network.host") {
		t.Fatalf("warm cache without entitlement error = %v", err)
	}
	buildWide := common
	buildWide.Network = "host"
	buildWide.Output = Output{Path: filepath.Join(root, "build-wide-layout")}
	if _, err := BuildPlanSupervised(ctx, plan, buildWide); err != nil {
		t.Fatalf("warm cache with build-wide host network: %v", err)
	}
}

func TestWarmInsecureRunCacheStillRequiresEntitlement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live insecure instruction cache build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf("from %q\nrun \"test -r /proc/self/status && printf cached-insecure-run >/proof\" network=\"none\" security=\"insecure\"\nlabel after=\"insecure-run\"\n", base.reference))
	common := SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Pull: false, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	}
	allowed := common
	allowed.Allow = []string{"security.insecure"}
	allowed.Output = Output{Path: filepath.Join(root, "allowed-layout")}
	_, err := BuildPlanSupervised(ctx, plan, allowed)
	if err != nil {
		t.Fatalf("populate insecure RUN cache: %v", err)
	}
	if records := instructionCacheRecordCount(t, store); records != 1 {
		t.Fatalf("cold insecure RUN cache records = %d, want 1", records)
	}
	denied := common
	denied.Output = Output{Path: filepath.Join(root, "denied-layout")}
	if _, err := BuildPlanSupervised(ctx, plan, denied); err == nil || !strings.Contains(err.Error(), "--allow security.insecure") {
		t.Fatalf("warm cache without entitlement error = %v", err)
	}
}

func TestWarmDeviceRunCacheStillRequiresEntitlement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live CDI instruction cache build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cdiDir := filepath.Join(root, "cdi")
	if err := os.MkdirAll(cdiDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdiDir, "env.yaml"), []byte(`cdiVersion: "0.6.0"
kind: "test.example/env"
devices:
  - name: proof
    containerEdits:
      env:
        - COOPR_CDI_PROOF=available
`), 0o600); err != nil {
		t.Fatal(err)
	}
	containersConf := filepath.Join(root, "containers.conf")
	if err := os.WriteFile(containersConf, []byte(fmt.Sprintf("[engine]\ncdi_spec_dirs=[%q]\n", cdiDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_CONF", containersConf)
	plan := testPlan(t, fmt.Sprintf(`
from %q
run "test \"$COOPR_CDI_PROOF\" = available && printf cached-device-run >/proof" network="none" {
  device "test.example/env=proof,required"
}
`, base.reference))
	common := SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Pull: false, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	}
	allowed := common
	allowed.Allow = []string{"device=test.example/env=proof"}
	allowed.Output = Output{Path: filepath.Join(root, "allowed-layout")}
	if _, err := BuildPlanSupervised(ctx, plan, allowed); err != nil {
		t.Fatalf("populate CDI RUN cache: %v", err)
	}
	denied := common
	denied.Output = Output{Path: filepath.Join(root, "denied-layout")}
	if _, err := BuildPlanSupervised(ctx, plan, denied); err == nil || !strings.Contains(err.Error(), "--allow device") {
		t.Fatalf("warm cache without device entitlement error = %v", err)
	}
}

func TestBuildPlanReusesClosedRunInstruction(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live instruction cache build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/cached" network="none"
run "cp /cached /proof" network="none"
`, base.reference))
	var first string
	durations := make([]time.Duration, 0, 2)
	for attempt := range 2 {
		layout := filepath.Join(root, "layout-"+string(rune('1'+attempt)))
		started := time.Now()
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, Pull: false,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		durations = append(durations, time.Since(started))
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
		if attempt == 0 {
			first = proof
			continue
		}
		if proof != first {
			t.Fatalf("cached RUN proof changed: first %q, second %q", first, proof)
		}
	}
	t.Logf("first build %s; second build with instruction cache hit %s", durations[0], durations[1])

	lease, err := acquireStore(store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close cache store: %v", err)
		}
	}()
	images, err := lease.store.Images()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, image := range images {
		for _, name := range image.BigDataNames {
			if strings.HasPrefix(name, instructionCacheBigData+"/") {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("persistent store has no instruction cache snapshot")
	}
}
