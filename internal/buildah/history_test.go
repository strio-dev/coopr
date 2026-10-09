package buildah

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPlannedOperationHistoryUsesContainerfileInstructions(t *testing.T) {
	tests := []struct {
		name string
		op   planner.Operation
		want string
	}{
		{
			name: "shell run with declared arguments",
			op: planner.Operation{
				Instruction:      definition.Instruction{Name: "run", Form: "shell", Arguments: []string{"printf '%s' \"$release\""}},
				ArgumentsInScope: map[string]string{"release": "stable", "arch": "amd64"},
			},
			want: "RUN |2 arch=amd64 release=stable printf '%s' \"$release\"",
		},
		{
			name: "exec command",
			op: planner.Operation{Instruction: definition.Instruction{
				Name: "cmd", Form: "exec", Arguments: []string{"/bin/echo", "hello world"},
			}},
			want: `CMD ["/bin/echo","hello world"]`,
		},
		{
			name: "copy flags are stable",
			op: planner.Operation{Instruction: definition.Instruction{
				Name: "copy", Arguments: []string{"source", "/target"},
				Properties: map[string]string{"chown": "1:2", "chmod": "0755"},
			}},
			want: "COPY --chmod=0755 --chown=1:2 source /target",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := plannedOperationHistory(test.op, nil)
			if !ok || got != test.want {
				t.Fatalf("plannedOperationHistory() = %q, %v; want %q, true", got, ok, test.want)
			}
		})
	}
}

func TestPlannedOperationHistoryOmitsRuntimeProxyValues(t *testing.T) {
	op := planner.Operation{
		Instruction:      definition.Instruction{Name: "run", Form: "shell", Arguments: []string{"test -n \"$HTTP_PROXY\""}},
		ArgumentsInScope: map[string]string{"release": "stable"},
	}
	got, ok := plannedOperationHistory(op, nil)
	if !ok {
		t.Fatal("RUN did not produce history")
	}
	if strings.Contains(got, "proxy.example") || got != `RUN |1 release=stable test -n "$HTTP_PROXY"` {
		t.Fatalf("RUN history = %q", got)
	}

	op.ArgumentsInScope["HTTP_PROXY"] = "http://proxy.example"
	got, _ = plannedOperationHistory(op, nil)
	if !strings.Contains(got, "HTTP_PROXY=http://proxy.example") {
		t.Fatalf("explicit proxy ARG missing from RUN history: %q", got)
	}
}

func TestMergeHistoryOperationsValidatesAndPreservesPositions(t *testing.T) {
	operations := []planner.Operation{
		{Instruction: definition.Instruction{Name: "env", Properties: map[string]string{"one": "1"}}},
		{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}}},
	}
	history := []planner.HistoryOperation{
		{Before: 0, Operation: planner.Operation{Instruction: definition.Instruction{Name: "arg", Arguments: []string{"first", "1"}}}},
		{Before: 1, Operation: planner.Operation{Instruction: definition.Instruction{Name: "arg", Arguments: []string{"second"}}}},
	}
	merged, err := mergeHistoryOperations(operations, history)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(merged))
	for index := range merged {
		got[index] = merged[index].Name
	}
	if strings.Join(got, ",") != "arg,env,arg,run" {
		t.Fatalf("merged operation order = %v", got)
	}

	history[1].Operation.Name = "run"
	if _, err := mergeHistoryOperations(operations, history); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("invalid history instruction error = %v", err)
	}
}

func TestNormalizeZeroLayerExecutorConfigOnlyRepairsEmptyRootFS(t *testing.T) {
	raw := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers"},"history":[{"created_by":"LABEL x=y","empty_layer":true}]}`)
	normalized, err := normalizeZeroLayerExecutorConfig(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	var image struct {
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(normalized, &image); err != nil {
		t.Fatal(err)
	}
	if image.RootFS.DiffIDs == nil || len(image.RootFS.DiffIDs) != 0 {
		t.Fatalf("normalized diff IDs = %#v, want present empty list", image.RootFS.DiffIDs)
	}

	invalid := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers"},"history":[{"created_by":"RUN true"}]}`)
	unchanged, err := normalizeZeroLayerExecutorConfig(invalid, true)
	if err != nil || string(unchanged) != string(invalid) {
		t.Fatalf("layered invalid config changed: %s, %v", unchanged, err)
	}

	missingHistory := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers"}}`)
	unchanged, err = normalizeZeroLayerExecutorConfig(missingHistory, false)
	if err != nil || string(unchanged) != string(missingHistory) {
		t.Fatalf("unproven zero-layer config changed: %s, %v", unchanged, err)
	}
}

func TestBuildDefinitionRecordsInstructionHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live instruction-history coverage in short mode")
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
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`
from %q
arg "channel" "stable"
env release="stable"
label "org.example.release" "stable"
run "printf history >/proof" network="none"
user "0"
workdir "/workspace"
cmd { exec "/bin/true" }
`, base.reference)))
	if err != nil {
		t.Fatal(err)
	}
	epoch := int64(1_700_000_000)
	layout := filepath.Join(root, "layout")
	_, err = BuildDefinitionSupervised(ctx, def, planner.Options{
		Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Arguments: map[string]string{"SOURCE_DATE_EPOCH": fmt.Sprint(epoch)},
	}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Output:              Output{Path: layout},
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	want := []struct {
		prefix string
		empty  bool
	}{
		{"ARG channel=stable", true},
		{"ENV release=stable", true},
		{"LABEL org.example.release=stable", true},
		{"RUN |1 channel=stable --network=none printf history >/proof", false},
		{"USER 0", true},
		{"WORKDIR /workspace", false},
		{`CMD ["/bin/true"]`, true},
	}
	if len(image.History) < len(want) {
		t.Fatalf("history has %d entries, want at least %d: %+v", len(image.History), len(want), image.History)
	}
	tail := image.History[len(image.History)-len(want):]
	for index, expected := range want {
		entry := tail[index]
		if entry.CreatedBy != expected.prefix || entry.EmptyLayer != expected.empty {
			t.Errorf("history[%d] = {%q empty=%v}, want {%q empty=%v}", index, entry.CreatedBy, entry.EmptyLayer, expected.prefix, expected.empty)
		}
		if entry.Created == nil || entry.Created.Unix() != epoch {
			t.Errorf("history[%d] created = %v, want epoch %d", index, entry.Created, epoch)
		}
	}
	nonempty := 0
	for _, entry := range image.History {
		if !entry.EmptyLayer {
			nonempty++
		}
	}
	if nonempty != len(manifest.Layers) {
		t.Fatalf("history has %d filesystem entries for %d layers: %+v", nonempty, len(manifest.Layers), image.History)
	}
}

func TestBuildPlanCacheRewritesCurrentMetadataHistory(t *testing.T) {
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
	var proof string
	tests := []struct {
		metadata string
		want     []string
	}{
		{metadata: `label release="one"
cmd { exec "/bin/echo" "one" }`, want: []string{`LABEL release=one`, `CMD ["/bin/echo","one"]`}},
		{metadata: `label release="two"`, want: []string{`LABEL release=two`}},
		{metadata: `label release="three"
cmd { exec "/bin/echo" "three" }
entrypoint { exec "/bin/sh" }`, want: []string{`LABEL release=three`, `CMD ["/bin/echo","three"]`, `ENTRYPOINT ["/bin/sh"]`}},
		{metadata: "", want: nil},
	}
	for attempt, test := range tests {
		plan := testPlan(t, fmt.Sprintf(`
from %q
%s
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \n' >/proof" network="none"
`, base.reference, test.metadata))
		layout := filepath.Join(root, fmt.Sprintf("layout-%d", attempt))
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output:              Output{Path: layout},
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		gotProof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded()), "proof")
		if attempt == 0 {
			proof = gotProof
		} else if gotProof != proof {
			t.Fatalf("warm RUN executed again: %q != %q", gotProof, proof)
		}
		suffixLength := len(test.want) + 1
		tail := image.History[len(image.History)-suffixLength:]
		for index, want := range test.want {
			if tail[index].CreatedBy != want || !tail[index].EmptyLayer {
				t.Fatalf("build %d history[%d] = %+v, want metadata %q", attempt+1, index, tail[index], want)
			}
		}
		if !strings.HasPrefix(tail[len(tail)-1].CreatedBy, "RUN ") || tail[len(tail)-1].EmptyLayer {
			t.Fatalf("build %d filesystem history = %+v", attempt+1, tail[len(tail)-1])
		}
	}
}

func TestBuildPlanNoOpRunCachePreservesHistoryAfterCopy(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, format := range []string{outputFormatOCI, outputFormatDocker} {
		for _, intermediate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/intermediate=%t", format, intermediate), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				root := t.TempDir()
				store := StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "overlay"}
				base := newLiveBusyBoxStorage(t, ctx, root, store)
				policy := writeComponentTestPolicy(t, root)
				if err := os.WriteFile(filepath.Join(root, "payload"), []byte("unchanged payload\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "marker"), []byte("after read-only RUN\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				afterRun := ""
				cacheHits := 3
				if intermediate {
					afterRun = "copy \"marker\" \"/marker\""
					cacheHits++
				}
				var coldImage v1.Image
				var coldLayers []v1.Descriptor
				for attempt, release := range []string{"cold", "warm"} {
					plan := testPlan(t, fmt.Sprintf(`
from %q
run "/bin/busybox true" network="none"
copy "payload" "/payload"
env channel="stable"
label release="%s"
run "/bin/busybox test -s /payload" network="none"
%s
cmd { exec "/bin/cat" "/payload" }
`, base.reference, release, afterRun))
					layout := filepath.Join(root, release)
					var progress strings.Builder
					_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
						Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
						Output:              Output{Path: layout, Format: format},
						SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: &progress,
					})
					if err != nil {
						t.Fatalf("%s build: %v\n%s", release, err, progress.String())
					}
					if attempt == 1 && strings.Count(progress.String(), "--> Using cache ") != cacheHits {
						t.Fatalf("warm instructions did not all hit cache:\n%s", progress.String())
					}
					manifest, image := readPlanImage(t, layout)
					if image.Config.Labels["release"] != release {
						t.Fatalf("%s release label = %q", release, image.Config.Labels["release"])
					}
					if len(manifest.Layers) < 3 {
						t.Fatalf("%s has %d layers, want BusyBox, prepared runtime files, and copied payload", release, len(manifest.Layers))
					}
					if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[2].Digest.Encoded()), "payload"); got != "unchanged payload\n" {
						t.Fatalf("%s payload = %q", release, got)
					}
					if intermediate {
						last := manifest.Layers[len(manifest.Layers)-1]
						if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "marker"); got != "after read-only RUN\n" {
							t.Fatalf("%s marker = %q", release, got)
						}
					}
					filesystemEntries := 0
					for _, entry := range image.History {
						if !entry.EmptyLayer {
							filesystemEntries++
						}
					}
					if filesystemEntries != len(manifest.Layers) || len(image.RootFS.DiffIDs) != len(manifest.Layers) {
						t.Fatalf("%s history filesystem entries=%d layers=%d diffIDs=%d", release, filesystemEntries, len(manifest.Layers), len(image.RootFS.DiffIDs))
					}
					want := []string{"COPY payload /payload", "ENV channel=stable", "LABEL release=" + release, "RUN --network=none /bin/busybox test -s /payload", `CMD ["/bin/cat","/payload"]`}
					if intermediate {
						want = append(want[:4], "COPY marker /marker", want[4])
					}
					t.Logf("%s layers=%d history=%+v", release, len(manifest.Layers), image.History)
					if len(image.History) < len(want) {
						t.Fatalf("%s history = %+v", release, image.History)
					}
					tail := image.History[len(image.History)-len(want):]
					// OCI omits an empty diff; Docker represents it as a layer.
					if format == outputFormatOCI && !tail[3].EmptyLayer {
						t.Fatalf("%s read-only RUN produced a filesystem layer: %+v", release, tail[3])
					}
					for index, instruction := range want {
						if tail[index].CreatedBy != instruction {
							t.Fatalf("%s history[%d] = %q, want %q", release, index, tail[index].CreatedBy, instruction)
						}
					}
					if attempt == 0 {
						coldImage, coldLayers = image, manifest.Layers
						continue
					}
					if len(image.History) != len(coldImage.History) || !digestSlicesEqual(image.RootFS.DiffIDs, coldImage.RootFS.DiffIDs) || len(manifest.Layers) != len(coldLayers) {
						t.Fatalf("warm no-op RUN changed history or filesystem counts: cold history=%d layers=%d; warm history=%d layers=%d", len(coldImage.History), len(coldLayers), len(image.History), len(manifest.Layers))
					}
					for index, layer := range manifest.Layers {
						if layer.Digest != coldLayers[index].Digest {
							t.Fatalf("warm layer %d changed from %s to %s", index, coldLayers[index].Digest, layer.Digest)
						}
					}
					for index, entry := range image.History {
						if entry.EmptyLayer != coldImage.History[index].EmptyLayer {
							t.Fatalf("warm history[%d] empty-layer flag changed", index)
						}
					}
				}
			})
		}
	}
}

func TestBuildPlanOrdersMetadataAfterNonCacheableRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live non-cacheable RUN history coverage in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	secret := filepath.Join(root, "secret")
	if err := os.WriteFile(secret, []byte("proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q
run "cat /run/secrets/token >/proof" network="none" { mount "secret" id="token" required="true" }
label after="run"
`, base.reference))
	layout := filepath.Join(root, "layout")
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Output:  Output{Path: layout},
		Secrets: []string{"id=token,src=" + secret}, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, image := readPlanImage(t, layout)
	tail := image.History[len(image.History)-2:]
	if !strings.HasPrefix(tail[0].CreatedBy, "RUN ") || tail[0].EmptyLayer || tail[1].CreatedBy != "LABEL after=run" || !tail[1].EmptyLayer {
		t.Fatalf("RUN/LABEL history order = %+v", tail)
	}
}

func TestBuildPlanLinkedCopyUsesPlannedHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live linked COPY history coverage in short mode")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, `from "scratch"
copy "payload" "/payload" link="true"
`)
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(context.Background(), plan, PlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, image := readPlanImage(t, layout)
	last := image.History[len(image.History)-1]
	if last.CreatedBy != "COPY --link=true payload /payload" || last.EmptyLayer {
		t.Fatalf("linked COPY history = %+v", last)
	}
}

func TestBuildPlanComponentMetadataTailKeepsLayerHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live component history coverage in short mode")
	}
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
	layout := filepath.Join(root, "layout")
	options := componentTestOptions(root, layout, resolver, policy)
	if _, err := BuildPlan(ctx, plan, options); err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	nonempty := 0
	for _, entry := range image.History {
		if !entry.EmptyLayer {
			nonempty++
		}
	}
	if nonempty != len(manifest.Layers) || nonempty != 1 {
		t.Fatalf("component history has %d filesystem entries for %d layers: %+v", nonempty, len(manifest.Layers), image.History)
	}
	copyIndex := -1
	for i, entry := range image.History {
		if !entry.EmptyLayer && strings.HasPrefix(entry.CreatedBy, "COPY ") {
			copyIndex = i
		}
	}
	last := image.History[len(image.History)-1]
	if copyIndex < 0 || copyIndex >= len(image.History)-1 || last.CreatedBy != "LABEL final=yes" || !last.EmptyLayer {
		t.Fatalf("selected COPY/final LABEL history order=%+v", image.History)
	}

}
