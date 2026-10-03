package buildah

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestBuildPlanRunsIndependentStagesInOneRootlessGraph(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live parallel Buildah graph")
	}
	previous := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previous)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	resolver, err := oci.NewResolver(oci.Options{ImageStoreDir: base.imageStoreDir, Pull: false})
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q as="left-base"
run "sleep 2; printf left >/left" network="none"
from "left-base" as="left"
env LEFT="yes"
from %q as="right-base"
run "sleep 2; printf right >/right" network="none"
from "right-base" as="right"
env RIGHT="yes"
from "scratch"
copy "/left" "/left" from="left"
copy "/right" "/right" from="right"
`, base.reference, base.reference))
	layout := filepath.Join(root, "layout")
	options := PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Resolver: resolver,
		Jobs:          2,
		Output:        Output{Path: layout},
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	}
	stages, outputID, err := validateStandaloneGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	observations := make([]string, 0, len(stages))
	_, err = executePlanGraph(ctx, plan, options, stages, outputID, nil, func(_ storage.Store, stage planner.Stage, _ string, _ *imageconfig.Config, _ *PackageRootMetadata) error {
		observations = append(observations, stage.ID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantObservations := make([]string, 0, len(stages))
	for _, stage := range stages {
		wantObservations = append(wantObservations, stage.ID)
	}
	if !reflect.DeepEqual(observations, wantObservations) {
		t.Fatalf("observations = %v, want source order %v", observations, wantObservations)
	}
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) != 2 || len(image.RootFS.DiffIDs) != 2 {
		t.Fatalf("final image layers=%d diffIDs=%d, want two COPY layers", len(manifest.Layers), len(image.RootFS.DiffIDs))
	}
	for index, proof := range []struct{ path, want string }{{"left", "left"}, {"right", "right"}} {
		blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[index].Digest.Encoded())
		if got := readLayerFile(t, blob, proof.path); got != proof.want {
			t.Fatalf("layer %d %s = %q, want %q", index, proof.path, got, proof.want)
		}
	}
}

func TestBuildPlanStartsReadyDescendantBeforeIndependentStageFinishes(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live dependency-ready Buildah graph")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	workerTemp, err := prepareWorkerTemp(store.GraphRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", workerTemp)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	resolver, err := oci.NewResolver(oci.Options{ImageStoreDir: base.imageStoreDir})
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q as="a"
run "sleep 1; touch /a" network="none"
from "a" as="a-next"
run "sleep 10; touch /a-next; touch /cache/mounted" network="none" {
  mount "cache" target="/cache" id="dependency-ready" sharing="shared"
}
from %q as="b"
run "sleep 12; touch /b" network="none"
from "scratch"
copy "/a-next" "/a-next" from="a-next"
copy "/b" "/b" from="b"
`, base.reference, base.reference))
	started := time.Now()
	_, err = BuildPlan(ctx, plan, PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Resolver: resolver, Jobs: 2,
		Output:        Output{Path: filepath.Join(root, "layout")},
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatalf("dependency-ready build: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 20*time.Second {
		t.Fatalf("dependency-ready descendant waited for an independent stage: %s", elapsed)
	}
}

func TestBuildPlanOverlapsIndependentRemoteAddStages(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live parallel ADD coverage")
	}
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
			_, _ = writer.Write([]byte(strings.TrimPrefix(request.URL.Path, "/")))
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	go func() {
		<-arrived
		select {
		case <-arrived:
		case <-time.After(20 * time.Second):
		}
		close(release)
	}()
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	plan := testPlan(t, `
from "scratch" as="left"
add "`+server.URL+`/left" "/proof"
from "scratch" as="right"
add "`+server.URL+`/right" "/proof"
from "scratch"
copy "/proof" "/left" from="left"
copy "/proof" "/right" from="right"
`)
	if _, err := BuildPlan(ctx, plan, PlanOptions{
		Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Jobs: 2,
		Output: Output{Path: filepath.Join(root, "layout")},
	}); err != nil {
		t.Fatalf("parallel remote ADD stages: %v", err)
	}
}

func TestBuildPlanSupervisedCancelsIndependentStagesAndCleansBuilders(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live parallel stage cancellation")
	}
	t.Setenv("GOMAXPROCS", "2")
	root := t.TempDir()
	storeOptions := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, context.Background(), root, storeOptions)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q as="left"
run "trap '' TERM; sleep 60; touch /left" network="none"
from %q as="right"
run "trap '' TERM; sleep 60; touch /right" network="none"
from "scratch"
copy "/left" "/left" from="left"
copy "/right" "/right" from="right"
`, base.reference, base.reference))
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: storeOptions, ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		Jobs:          2,
		ImageStoreDir: base.imageStoreDir, Pull: false, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "build worker exited after cleanup") {
		t.Fatalf("cancelled parallel graph error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 12*time.Second {
		t.Fatalf("parallel stage cancellation took %s", elapsed)
	}
	if _, err := os.Lstat(layout); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled output layout exists: %v", err)
	}
	store, err := storage.GetStore(storage.StoreOptions{
		RunRoot: storeOptions.RunRoot, GraphRoot: storeOptions.GraphRoot, GraphDriverName: storeOptions.GraphDriverName,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := store.Shutdown(false); err != nil {
			t.Errorf("shutdown cancelled build store: %v", err)
		}
	}()
	containers, err := store.Containers()
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 0 {
		t.Fatalf("cancelled graph left %d working containers", len(containers))
	}
}

func TestBuildPlanSupervisedStopsSiblingRunAfterStageFailure(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live sibling failure cancellation")
	}
	t.Setenv("GOMAXPROCS", "2")
	root := t.TempDir()
	storeOptions := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, context.Background(), root, storeOptions)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q as="fails"
run "sleep 2; exit 17" network="none"
from %q as="sleeps"
run "printf SIBLING_READY_TOKEN; trap '' TERM; sleep 60; touch /never" network="none"
from "scratch"
copy "/etc" "/failed-stage" from="fails"
copy "/never" "/never" from="sleeps"
`, base.reference, base.reference))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	var logs bytes.Buffer
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: storeOptions, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Jobs:   2,
		Output: Output{Path: filepath.Join(root, "layout")}, ImageStoreDir: base.imageStoreDir,
		SignaturePolicyPath: policy, Stdout: &logs, Stderr: io.Discard,
	})
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stage failure did not stop sibling RUN: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 12*time.Second {
		t.Fatalf("stage failure waited %s for sibling RUN", elapsed)
	}
	if !strings.Contains(logs.String(), "SIBLING_READY_TOKEN") {
		t.Fatalf("sibling RUN did not report readiness before failure: %q", logs.String())
	}
	if _, err := os.Lstat(filepath.Join(root, "layout")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed build left output layout: %v", err)
	}
	store, err := storage.GetStore(storage.StoreOptions{
		RunRoot: storeOptions.RunRoot, GraphRoot: storeOptions.GraphRoot, GraphDriverName: storeOptions.GraphDriverName,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := store.Shutdown(false); err != nil {
			t.Errorf("shutdown failed build store: %v", err)
		}
	}()
	containers, err := store.Containers()
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 0 {
		t.Fatalf("stage failure left %d working containers", len(containers))
	}
}
