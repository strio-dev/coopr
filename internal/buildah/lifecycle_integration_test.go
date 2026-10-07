package buildah

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/oci"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/image/v5/types"
)

func TestLifecycleNoLayersBuildsSingleNewLayerWithoutInstructionCache(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for native lifecycle builds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf("from %q\nrun \"printf first >/one\" network=\"none\"\nrun \"od -An -N16 -tx1 /dev/urandom >/proof\" network=\"none\"\nlabel mode=\"single\"\n", base.reference))
	var previous string
	for attempt := range 2 {
		layout := filepath.Join(root, fmt.Sprintf("output-%d", attempt))
		var logs strings.Builder
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", SignaturePolicyPath: policy, Output: Output{Path: layout}, Lifecycle: LifecycleControls{NoLayers: true}, Stdout: io.Discard, Stderr: &logs})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != 2 {
			t.Fatalf("layers=%d, want base plus one new layer", len(manifest.Layers))
		}
		if strings.Contains(logs.String(), "cache hit RUN") {
			t.Fatal("nonlayered build reused RUN")
		}
		proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded()), "proof")
		if attempt > 0 && proof == previous {
			t.Fatal("nonlayered RUN reused random output")
		}
		previous = proof
		if image.Config.Labels["mode"] != "single" {
			t.Fatalf("labels=%v", image.Config.Labels)
		}
	}
}

func TestLifecycleNoLayersPreservesCallerAcrossComponentAsOneLayer(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for native lifecycle builds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "before"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "after"), []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, _, _ := localComponentResolver(t, ctx, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \"scratch\"\ncopy \"before\" \"/before\"\ncomponent \"local:tool\" channel=\"first\"\ncopy \"after\" \"/after\"\ncomponent \"local:tool\" channel=\"second\"\n")
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	for attempt := range 2 {
		layout := filepath.Join(root, fmt.Sprintf("layout-%d", attempt))
		result, err := BuildPlan(ctx, plan, PlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}, Resolver: resolver,
			Lifecycle: LifecycleControls{NoLayers: true}, CacheLocalDir: filepath.Join(root, "cache"),
			SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.CacheStats.Hits != 0 || result.CacheStats.Misses != 0 {
			t.Fatalf("attempt %d read component cache with --layers=false: %+v", attempt, result.CacheStats)
		}
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 {
			t.Fatalf("attempt %d --layers=false output has %d new layers across component boundaries, want 1", attempt, len(manifest.Layers))
		}
		layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
		for file, want := range map[string]string{"before": "before\n", "after": "after\n", "artifact": "package payload\n"} {
			if got := readLayerFile(t, layer, file); got != want {
				t.Fatalf("attempt %d %s mutation lost or split: %q", attempt, file, got)
			}
		}
		if !strings.Contains(strings.Join(image.Config.Env, "\n"), "CHANNEL=second") {
			t.Fatalf("attempt %d final component config = %#v", attempt, image.Config.Env)
		}
	}
}

func TestLifecycleNoLayersAddsOneLayerAcrossComponentBoundaries(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for native lifecycle builds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	for name := range map[string]bool{"before": true, "after": true} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	componentResolver, _, _ := localComponentResolver(t, ctx, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
	resolver, err := oci.NewResolver(oci.Options{
		ComponentStoreDir: componentResolver.ComponentStoreDir(), Pull: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, fmt.Sprintf("from %q\ncopy \"before\" \"/before\"\ncomponent \"local:tool\" channel=\"first\"\ncopy \"after\" \"/after\"\ncomponent \"local:tool\" channel=\"second\"\n", base.reference))
	layout := filepath.Join(root, "layout")
	result, err := BuildPlan(ctx, plan, PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}, Resolver: resolver,
		Lifecycle: LifecycleControls{NoLayers: true}, CacheLocalDir: filepath.Join(root, "cache"),
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.CacheStats.Hits != 0 || result.CacheStats.Misses != 0 {
		t.Fatalf("read component cache with --layers=false: %+v", result.CacheStats)
	}
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) != 2 {
		t.Fatalf("--layers=false output has %d layers across component boundaries, want base plus one", len(manifest.Layers))
	}
	layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
	for file, want := range map[string]string{"before": "before\n", "after": "after\n", "artifact": "package payload\n"} {
		if got := readLayerFile(t, layer, file); got != want {
			t.Fatalf("%s mutation lost or split: %q", file, got)
		}
	}
	if !strings.Contains(strings.Join(image.Config.Env, "\n"), "CHANNEL=second") {
		t.Fatalf("final component config = %#v", image.Config.Env)
	}
}

func TestLifecycleRetainsSelectedIntermediateContainers(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for native lifecycle builds")
	}
	for _, tc := range []struct {
		name         string
		controls     LifecycleControls
		fail, retain bool
	}{
		{"remove-success", LifecycleControls{}, false, false},
		{"retain-success", LifecycleControls{KeepIntermediate: true}, false, true},
		{"retain-failure", LifecycleControls{KeepFailed: true}, true, true},
		{"remove-failure", LifecycleControls{}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			root := t.TempDir()
			store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
			base := newLiveBusyBoxStorage(t, ctx, root, store)
			policy := filepath.Join(root, "policy.json")
			if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			command := "printf two >/two"
			if tc.fail {
				command = "exit 9"
			}
			plan := testPlan(t, fmt.Sprintf("from %q\nrun \"printf one >/one\" network=\"none\"\nrun %q network=\"none\"\n", base.reference, command))
			_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", SignaturePolicyPath: policy, Output: Output{Path: filepath.Join(root, "output")}, Lifecycle: tc.controls, Stdout: io.Discard, Stderr: io.Discard})
			if (err != nil) != tc.fail {
				t.Fatalf("build error=%v fail=%v", err, tc.fail)
			}
			lease, err := acquireStore(store)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close() //nolint:errcheck
			containers, err := lease.store.Containers()
			if err != nil {
				t.Fatal(err)
			}
			if (len(containers) > 0) != tc.retain {
				t.Fatalf("containers=%d retain=%v", len(containers), tc.retain)
			}
			for _, container := range containers {
				builder, err := upstream.OpenBuilder(lease.store, container.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := builder.Delete(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
