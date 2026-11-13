package apibackend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/client"
	"github.com/strio-dev/coopr/internal/build"
	"github.com/strio-dev/coopr/internal/buildah"
)

func TestNativeOperationProgressColdWarmAndFailure(t *testing.T) {
	f := newNativeAPIFixture(t)
	f.once.Do(func() { close(f.release) })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var first string
	for i, store := range []string{"shared", "fresh"} {
		request := f.request("progress-"+store, "progress", store)
		var mu sync.Mutex
		var events []api.OperationProgress
		observed := api.WithProgressObserver(ctx, func(event api.OperationProgress) {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		})
		result, err := Execute(observed, request, io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if result.ProgressDropped != 0 {
			t.Fatalf("small build truncated %d phases", result.ProgressDropped)
		}
		data, err := os.ReadFile(filepath.Join(result.Filesystem, "proof"))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = string(data)
		} else if string(data) != first {
			t.Fatal("warm progress instrumentation changed the random output proof")
		}
		completed := checkProgressPairs(t, events)
		lookups, executions, restores, exports, stageHits := 0, 0, 0, 0, 0
		for _, event := range completed {
			if event.Phase == "lookup" && (event.Instruction == "copy" || event.Instruction == "run") {
				lookups++
				want := "miss"
				if i == 1 {
					want = "hit"
				}
				if event.Cache != want {
					t.Fatalf("lookup cache decision %q, want %q", event.Cache, want)
				}
			}
			if event.Phase == "execute" && (event.Instruction == "copy" || event.Instruction == "run") {
				executions++
			}
			if event.Phase == "restore" {
				restores++
			}
			if event.Phase == "export" {
				exports++
			}
			if event.State == "completed" && event.Instruction == "stage" && event.Phase == "solve" && event.Cache == "hit" {
				stageHits++
			}
		}
		wantLookups, wantExecutions := 2, 2
		if i == 1 {
			wantLookups, wantExecutions = 0, 0
			if restores != 1 || stageHits != 1 || result.Cache == nil || result.Cache.Hits != 1 || result.Cache.Misses != 0 {
				t.Fatalf("fresh warm restore: restores=%d stageHits=%d cache=%+v", restores, stageHits, result.Cache)
			}
		}
		if lookups != wantLookups || executions != wantExecutions || exports == 0 {
			t.Fatalf("phase counts lookup=%d execute=%d restore=%d export=%d", lookups, executions, restores, exports)
		}
		t.Logf("%s: %d paired phases; lookup=%d execute=%d restore=%d export=%d", store, len(completed), lookups, executions, restores, exports)
	}
	request := f.request("failed-progress", "failure", "shared")
	last := &request.Program.States[0].Operations[len(request.Program.States[0].Operations)-1]
	last.Args = []string{"/bin/busybox", "sh", "-c", "exit 19"}
	var events []api.OperationProgress
	_, err := Execute(api.WithProgressObserver(ctx, func(event api.OperationProgress) { events = append(events, event) }), request, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("failing RUN succeeded")
	}
	found := false
	for _, event := range checkProgressPairs(t, events) {
		if event.Instruction == "run" && event.Phase == "execute" && event.State == "failed" && event.Error != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed RUN observation missing: %+v", events)
	}
}

func TestNativeDefinitionMetadataContainsOperationTimings(t *testing.T) {
	f := newNativeAPIFixture(t)
	definition := filepath.Join(f.context, "image.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\" {\n copy \"busybox\" \"/bin/busybox\"\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(f.root, "metadata.json")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	controls, err := buildah.ParseRunControls(buildah.RunControlInput{HTTPProxy: true, Runtime: "crun", Isolation: "chroot"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = build.Run(ctx, build.Options{File: definition, Context: f.context, MetadataFile: metadata,
		BuildStore:  buildah.StoreOptions{RunRoot: filepath.Join(f.root, "shared", "run"), GraphRoot: filepath.Join(f.root, "shared", "graph"), GraphDriverName: "vfs"},
		RunControls: controls, CacheLocalDir: f.cache, SignaturePolicyPath: f.policy, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var events []api.OperationProgress
	if err := json.Unmarshal(fields["coopr.operation.progress"], &events); err != nil {
		t.Fatalf("native CLI metadata did not retain timings: %v", err)
	}
	if len(checkProgressPairs(t, events)) == 0 {
		t.Fatal("native definition produced no observed phases")
	}
}

func TestNativeHandledFailureRetainsProgressTruncation(t *testing.T) {
	for _, kind := range []string{"build", "component"} {
		t.Run(kind, func(t *testing.T) {
			f := newNativeAPIFixture(t)
			var source strings.Builder
			source.WriteString("from \"scratch\" as=\"producer\" {\n copy \"busybox\" \"/bin/busybox\"\n")
			for i := range 520 {
				fmt.Fprintf(&source, " env VALUE%d=\"value\"\n", i)
			}
			source.WriteString(" run {\n  exec \"/bin/busybox\" \"sh\" \"-c\" \"exit 19\"\n }\n}\n")
			if kind == "component" {
				source.WriteString("package as=\"assets\" {\n copy \"/proof\" \"/proof\" from=\"producer\"\n}\nextend {\n copy \"/proof\" \"/proof\" from=\"assets\"\n}\n")
			}
			definition := filepath.Join(f.context, "failure.coopr")
			if err := os.WriteFile(definition, []byte(source.String()), 0600); err != nil {
				t.Fatal(err)
			}
			request := f.request("truncated-failure", "unused", "shared")
			request.Kind, request.Program = kind, nil
			request.Build = &api.Build{Files: []string{definition}}
			request.Options.Jobs = 1
			if kind == "component" {
				request.Options.Output.Filesystem = ""
			}
			count := 0
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			result, err := Execute(api.WithProgressObserver(ctx, func(api.OperationProgress) { count++ }), request, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "19") {
				t.Fatalf("expected the handled RUN failure, got %v", err)
			}
			if result.ProgressDropped == 0 || count == 0 || count > 2048 {
				t.Fatalf("handled %s failure lost bounded capture accounting: events=%d dropped=%d", kind, count, result.ProgressDropped)
			}
			t.Logf("%s failure: events=%d dropped=%d", kind, count, result.ProgressDropped)
		})
	}
}

func checkProgressPairs(t *testing.T, events []api.OperationProgress) []api.OperationProgress {
	t.Helper()
	started := make(map[string]api.OperationProgress)
	var completed []api.OperationProgress
	for _, event := range events {
		if event.ID == "" || event.Phase == "" || event.Started.IsZero() {
			t.Fatalf("incomplete observation: %+v", event)
		}
		if event.State == "started" {
			if _, exists := started[event.ID]; exists {
				t.Fatalf("duplicate start %s", event.ID)
			}
			started[event.ID] = event
			continue
		}
		start, exists := started[event.ID]
		if !exists || event.Completed == nil || event.DurationNanos < 0 || !event.Started.Equal(start.Started) {
			t.Fatalf("unpaired observation: %+v", event)
		}
		delete(started, event.ID)
		completed = append(completed, event)
	}
	if len(started) != 0 {
		t.Fatalf("unfinished observed phases: %v", started)
	}
	return completed
}

func TestPublicClientDefinitionProgressColdWarm(t *testing.T) {
	f := newNativeAPIFixture(t)
	c, _ := publicNativeClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	publicNativeWrite(t, filepath.Join(f.context, "white space ☃.txt"), "context whitespace and Unicode\n", 0600)
	file := filepath.Join(f.context, "image.coopr")
	publicNativeWrite(t, file, `from "scratch" {
    copy "busybox" "/bin/busybox"
    copy "white space ☃.txt" "/native.txt"
    run {
        exec "/bin/busybox" "sh" "-eu" "-c" "printf 'native frontend run\\n' > /native-run.txt; echo API_NATIVE_RUN_EXECUTED"
    }
}
`, 0600)
	var first string
	for i, label := range []string{"definition-cold", "definition-warm"} {
		r := publicNativeDefinition(f, file, "build", label, filepath.Join(f.root, "components"))
		r.Options.Network, r.Options.Isolation = "none", "rootless"
		var logs bytes.Buffer
		var events []api.OperationProgress
		result, err := c.Execute(ctx, r, client.StreamOptions{Stdout: &logs, Stderr: &logs, Progress: func(event api.OperationProgress) { events = append(events, event) }})
		if err != nil {
			t.Fatal(err, logs.String())
		}
		if result.Cache != nil || len(result.Metadata) == 0 || result.ManifestDigest == "" || result.ProgressDropped != 0 {
			t.Fatalf("native metadata/progress/cache contract: %+v", result)
		}
		if publicNativeRead(t, filepath.Join(result.Filesystem, "native.txt")) != "context whitespace and Unicode\n" || publicNativeRead(t, filepath.Join(result.Filesystem, "native-run.txt")) != "native frontend run\n" {
			t.Fatal("native COPY/RUN output changed")
		}
		var index v1.Index
		if err := json.Unmarshal([]byte(publicNativeRead(t, filepath.Join(result.OCI, "index.json"))), &index); err != nil {
			t.Fatal(err)
		}
		if len(index.Manifests) != 1 || index.Manifests[0].Digest.String() != result.ManifestDigest {
			t.Fatal("native result digest does not identify exported OCI artifact")
		}
		executed := slices.Contains(strings.Split(logs.String(), "\n"), "API_NATIVE_RUN_EXECUTED")
		if executed != (i == 0) {
			t.Fatalf("native RUN marker: cold=%t logs=%q", i == 0, logs.String())
		}
		observed := 0
		for _, event := range checkProgressPairs(t, events) {
			if event.State == "completed" && ((i == 0 && event.Instruction == "run" && event.Phase == "execute") || (i == 1 && event.Instruction == "stage" && event.Phase == "solve" && event.Cache == "hit")) {
				observed++
			}
		}
		if observed != 1 {
			t.Fatalf("missing native RUN execution/stage cache-hit progress: %+v", events)
		}
		if i == 0 {
			first = result.ManifestDigest
		} else if result.ManifestDigest != first {
			t.Fatal("warm native artifact identity changed in the same explicit store/cache")
		}
	}
}
