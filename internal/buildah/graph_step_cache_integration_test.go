package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestBuildPlanCachesWorkdir(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	plan := testPlan(t, "from \"scratch\"\nworkdir \"/workspace/nested\"\n")
	var coldLayer digest.Digest
	for attempt := range 2 {
		layout := filepath.Join(root, fmt.Sprintf("layout-%d", attempt))
		result, err := BuildPlan(ctx, plan, PlanOptions{
			Store: store, Isolation: "rootless", Output: Output{Path: layout},
		})
		if err != nil {
			t.Fatal(err)
		}
		if count := instructionCacheRecordCount(t, store); count != 1 {
			t.Fatalf("build %d WORKDIR cache records = %d, want 1", attempt+1, count)
		}
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 {
			t.Fatalf("build %d WORKDIR layers = %d, want 1", attempt+1, len(manifest.Layers))
		}
		if image.Config.WorkingDir != "/workspace/nested" {
			t.Fatalf("build %d WORKDIR config = %q, want /workspace/nested", attempt+1, image.Config.WorkingDir)
		}
		if attempt == 0 {
			coldLayer = manifest.Layers[0].Digest
		} else if manifest.Layers[0].Digest != coldLayer {
			t.Fatalf("warm WORKDIR layer = %s, want %s (result %s)", manifest.Layers[0].Digest, coldLayer, result.ManifestDigest)
		}
	}
}

func TestBuildPlanCachesDefaultNetworkRun(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof"
`, base.reference))
	var coldProof string
	for attempt := range 2 {
		layout := filepath.Join(root, fmt.Sprintf("layout-%d", attempt))
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, ImageStoreDir: base.imageStoreDir, Pull: false,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
		if attempt == 0 {
			coldProof = proof
		} else if proof != coldProof {
			t.Fatalf("warm default-network RUN executed again: proof %q, want cached %q", proof, coldProof)
		}
	}
	if count := instructionCacheRecordCount(t, store); count != 1 {
		t.Fatalf("default-network RUN cache records = %d, want 1", count)
	}
}

func TestBuildPlanReusesRunAcrossOutputMetadataChanges(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(attempt int, metadata string) (string, v1.Image) {
		t.Helper()
		plan := testPlan(t, fmt.Sprintf(`
from %q
label "org.example.release" %q
cmd { exec "/bin/echo" %q }
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"
`, base.reference, metadata, metadata))
		layout := filepath.Join(root, fmt.Sprintf("metadata-%d", attempt))
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, ImageStoreDir: base.imageStoreDir, Pull: false,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
		return proof, image
	}

	coldProof, _ := build(1, "one")
	warmProof, warmImage := build(2, "two")
	if warmProof != coldProof {
		t.Fatalf("output-only metadata change reexecuted RUN: cold %q, warm %q", coldProof, warmProof)
	}
	if got := warmImage.Config.Labels["org.example.release"]; got != "two" {
		t.Fatalf("warm image label = %q, want two", got)
	}
	if got := warmImage.Config.Cmd; len(got) != 2 || got[0] != "/bin/echo" || got[1] != "two" {
		t.Fatalf("warm image command = %#v, want [/bin/echo two]", got)
	}
	if count := instructionCacheRecordCount(t, store); count != 1 {
		t.Fatalf("output-only metadata builds created %d RUN cache records, want 1", count)
	}
}

func TestBuildPlanReusesRunAcrossSourceStageMetadataChanges(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(attempt int, metadata string) (string, v1.Image) {
		t.Helper()
		plan := testPlan(t, fmt.Sprintf(`
from %q as="source"
label "org.example.release" %q
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"
from "source"
label "org.example.consumer" "yes"
`, base.reference, metadata))
		layout := filepath.Join(root, fmt.Sprintf("source-metadata-%d", attempt))
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, ImageStoreDir: base.imageStoreDir, Pull: false,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
		return proof, image
	}

	coldProof, _ := build(1, "one")
	warmProof, warmImage := build(2, "two")
	if warmProof != coldProof {
		t.Fatalf("source-stage output-only metadata change reexecuted RUN: cold %q, warm %q", coldProof, warmProof)
	}
	if got := warmImage.Config.Labels["org.example.release"]; got != "two" {
		t.Fatalf("warm consumer inherited source-stage label %q, want two", got)
	}
	if got := warmImage.Config.Labels["org.example.consumer"]; got != "yes" {
		t.Fatalf("warm consumer label = %q, want yes", got)
	}
	if count := instructionCacheRecordCount(t, store); count != 1 {
		t.Fatalf("source-stage output-only metadata builds created %d RUN cache records, want 1", count)
	}
}

func TestBuildPlanInvalidatesRunForEnvironmentChanges(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(attempt int, value string) string {
		t.Helper()
		plan := testPlan(t, fmt.Sprintf(`
from %q
env CACHE_VALUE=%q
run "printf '%%s:' \"$CACHE_VALUE\" >/proof; od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >>/proof" network="none"
`, base.reference, value))
		layout := filepath.Join(root, fmt.Sprintf("environment-%d", attempt))
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, ImageStoreDir: base.imageStoreDir, Pull: false,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		return readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
	}

	first := build(1, "one")
	second := build(2, "two")
	if first == second {
		t.Fatalf("ENV change reused RUN proof %q", second)
	}
	if !strings.HasPrefix(first, "one:") || !strings.HasPrefix(second, "two:") {
		t.Fatalf("RUN environment proofs = %q and %q, want one:/two: prefixes", first, second)
	}
	if count := instructionCacheRecordCount(t, store); count != 2 {
		t.Fatalf("ENV-changing builds created %d RUN cache records, want 2", count)
	}
}

func TestBuildPlanCachesStageCopy(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	value := filepath.Join(contextDir, "value")
	fixed := time.Unix(1_700_000_000, 0)
	write := func(contents string) {
		t.Helper()
		if err := os.WriteFile(value, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(value, fixed, fixed); err != nil {
			t.Fatal(err)
		}
	}
	write("one\n")
	store := cacheTestStore(root)
	plan := testPlan(t, `
from "scratch" as="source"
copy "value" "/value"
from "scratch"
copy "/value" "/copied" from="source"
`)
	build := func(name, want string) (int, digest.Digest) {
		t.Helper()
		layout := filepath.Join(root, name)
		if _, err := BuildPlan(ctx, plan, PlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: layout},
		}); err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 {
			t.Fatalf("%s stage COPY layers = %d, want 1", name, len(manifest.Layers))
		}
		layer := manifest.Layers[0]
		if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", layer.Digest.Encoded()), "copied"); got != want {
			t.Fatalf("%s stage COPY output = %q, want %q", name, got, want)
		}
		return instructionCacheRecordCount(t, store), layer.Digest
	}
	coldRecords, coldLayer := build("cold", "one\n")
	warmRecords, warmLayer := build("warm", "one\n")
	if coldRecords != 2 || warmRecords != 2 || warmLayer != coldLayer {
		t.Fatalf("warm stage COPY records/layer = %d/%s, cold = %d/%s", warmRecords, warmLayer, coldRecords, coldLayer)
	}
	write("two\n")
	changedRecords, changedLayer := build("changed", "two\n")
	if changedRecords != 4 || changedLayer == coldLayer {
		t.Fatalf("changed stage COPY records/layer = %d/%s, cold layer %s", changedRecords, changedLayer, coldLayer)
	}
}

func TestBuildPlanCachesRunWithContextBind(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.MkdirAll(filepath.Join(contextDir, "input"), 0o700); err != nil {
		t.Fatal(err)
	}
	value := filepath.Join(contextDir, "input", "value")
	fixed := time.Unix(1_700_000_000, 0)
	write := func(contents string) {
		t.Helper()
		if err := os.WriteFile(value, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(value, fixed, fixed); err != nil {
			t.Fatal(err)
		}
	}
	write("one\n")
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q
run "cat /input/value >/proof" network="none" {
  mount "bind" source="input" target="/input" readonly="true"
}
`, base.reference))
	build := func(name, want string) (int, digest.Digest) {
		t.Helper()
		layout := filepath.Join(root, name)
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, ImageStoreDir: base.imageStoreDir, Pull: false,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof"); got != want {
			t.Fatalf("%s context-bind RUN proof = %q, want %q", name, got, want)
		}
		return instructionCacheRecordCount(t, store), last.Digest
	}
	coldRecords, coldLayer := build("cold", "one\n")
	warmRecords, warmLayer := build("warm", "one\n")
	if coldRecords != 1 || warmRecords != 1 || warmLayer != coldLayer {
		t.Fatalf("warm context-bind RUN records/layer = %d/%s, cold = %d/%s", warmRecords, warmLayer, coldRecords, coldLayer)
	}
	write("two\n")
	changedRecords, changedLayer := build("changed", "two\n")
	if changedRecords != 2 || changedLayer == coldLayer {
		t.Fatalf("changed context-bind RUN records/layer = %d/%s, cold layer %s", changedRecords, changedLayer, coldLayer)
	}
}

func TestBuildPlanCachesRunWithStageBind(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	value := filepath.Join(contextDir, "value")
	fixed := time.Unix(1_700_000_000, 0)
	write := func(contents string) {
		t.Helper()
		if err := os.WriteFile(value, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(value, fixed, fixed); err != nil {
			t.Fatal(err)
		}
	}
	write("one\n")
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from "scratch" as="source"
copy "value" "/input/value"
from %q
run "cat /input/value >/proof" network="none" {
  mount "bind" from="source" source="/input" target="/input" readonly="true"
}
`, base.reference))
	build := func(name, want string) (int, digest.Digest) {
		t.Helper()
		layout := filepath.Join(root, name)
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, ImageStoreDir: base.imageStoreDir, Pull: false,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof"); got != want {
			t.Fatalf("%s stage-bind RUN proof = %q, want %q", name, got, want)
		}
		return instructionCacheRecordCount(t, store), last.Digest
	}
	coldRecords, coldLayer := build("cold", "one\n")
	warmRecords, warmLayer := build("warm", "one\n")
	if coldRecords != 2 || warmRecords != 2 || warmLayer != coldLayer {
		t.Fatalf("warm stage-bind RUN records/layer = %d/%s, cold = %d/%s", warmRecords, warmLayer, coldRecords, coldLayer)
	}
	write("two\n")
	changedRecords, changedLayer := build("changed", "two\n")
	if changedRecords != 4 || changedLayer == coldLayer {
		t.Fatalf("changed stage-bind RUN records/layer = %d/%s, cold layer %s", changedRecords, changedLayer, coldLayer)
	}
}

func TestBuildPlanCachesComponentBeforeMetadataTail(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	resolver, _, _ := localComponentResolver(t, ctx, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, `
from "scratch"
component "local:tool" channel="stable"
label final="yes"
`)
	cacheDir := filepath.Join(root, "cache")
	var coldLayer digest.Digest
	for attempt := range 2 {
		layout := filepath.Join(root, fmt.Sprintf("layout-%d", attempt))
		options := componentTestOptions(root, layout, resolver, policy)
		options.CacheLocalDir = cacheDir
		result, err := BuildPlan(ctx, plan, options)
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 || image.Config.Labels["final"] != "yes" {
			t.Fatalf("build %d component metadata tail: layers=%d labels=%v", attempt+1, len(manifest.Layers), image.Config.Labels)
		}
		if len(image.Config.Env) != 1 || image.Config.Env[0] != "CHANNEL=stable" {
			t.Fatalf("build %d component environment = %v", attempt+1, image.Config.Env)
		}
		if attempt == 0 {
			if result.CacheStats.Misses < 1 || result.CacheStats.Stored < 1 || result.CacheStats.Hits != 0 {
				t.Fatalf("cold component cache stats = %+v", result.CacheStats)
			}
			coldLayer = manifest.Layers[0].Digest
			continue
		}
		if result.CacheStats.Hits != 1 || result.CacheStats.Misses != 0 {
			t.Fatalf("warm component cache stats = %+v", result.CacheStats)
		}
		if manifest.Layers[0].Digest != coldLayer {
			t.Fatalf("warm component layer = %s, want %s", manifest.Layers[0].Digest, coldLayer)
		}
	}
}

func TestBuildPlanNoCacheReexecutesRunAndReplacesCachedResult(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"
`, base.reference))
	build := func(attempt int, noCache bool) string {
		t.Helper()
		layout := filepath.Join(root, fmt.Sprintf("layout-%d", attempt))
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", NoCache: noCache,
			Output: Output{Path: layout}, ImageStoreDir: base.imageStoreDir, Pull: false,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		return readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
	}
	cold := build(1, false)
	warm := build(2, false)
	if warm != cold {
		t.Fatalf("normal warm build reexecuted RUN: cold %q, warm %q", cold, warm)
	}
	if count := instructionCacheRecordCount(t, store); count != 1 {
		t.Fatalf("normal RUN cache records = %d, want 1", count)
	}
	bypassed := build(3, true)
	if bypassed == cold {
		t.Fatalf("NoCache build reused RUN proof %q", bypassed)
	}
	if count := instructionCacheRecordCount(t, store); count != 1 {
		t.Fatalf("NoCache build instruction cache records = %d, want replacement record", count)
	}
	refreshed := build(4, false)
	if refreshed != bypassed {
		t.Fatalf("warm build proof %q, want NoCache result %q", refreshed, bypassed)
	}
}

func TestPublishPlanPreservesRootMetadataAcrossWarmBuild(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name: ".", Typeflag: tar.TypeDir, Mode: 0o711, Uid: 1, Gid: 2,
		PAXRecords: map[string]string{"SCHILY.xattr.user.coopr": "root-metadata"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "root.tar"), archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "artifact"), []byte("package payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPublicationPlan(t, `
package as="payload"
add "root.tar" "/"
copy "artifact" "/artifact"
extend
copy "/artifact" "/artifact" from="payload"
`)
	store := cacheTestStore(root)
	for attempt := range 2 {
		path := filepath.Join(root, fmt.Sprintf("payload-%d.tar", attempt))
		packages, err := PublishPlan(ctx, plan, PublicationOptions{
			PlanOptions: PlanOptions{
				Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
			},
			PackagePaths: map[string]string{"payload": path},
		})
		if err != nil {
			t.Fatal(err)
		}
		pkg, ok := packages["payload"]
		if !ok {
			t.Fatalf("build %d package outputs = %v", attempt+1, packages)
		}
		rootHeader := packageTarRootHeader(t, path)
		if got := rootHeader.Mode & 0o7777; got != 0o711 || rootHeader.Uid != 1 || rootHeader.Gid != 2 {
			t.Fatalf("build %d package root metadata = mode %#o uid %d gid %d, want 0711/1/2", attempt+1, got, rootHeader.Uid, rootHeader.Gid)
		}
		if got := rootHeader.PAXRecords["SCHILY.xattr.user.coopr"]; got != "root-metadata" {
			t.Fatalf("build %d package root xattr = %q, want root-metadata", attempt+1, got)
		}
		if pkg.Descriptor.Digest == "" {
			t.Fatalf("build %d package descriptor has no digest", attempt+1)
		}
	}
	if count := instructionCacheRecordCount(t, store); count != 2 {
		t.Fatalf("package publication instruction cache records = %d, want 2", count)
	}
}
