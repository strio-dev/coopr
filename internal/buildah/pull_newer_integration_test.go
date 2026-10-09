package buildah

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"coopr/internal/oci"
	"coopr/internal/planner"
	registryserver "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
)

func TestPullNewerCopyFailureBindsCachedConfigBeforeBuildAndPublication(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native pull fallback in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	producerStore := StoreOptions{GraphRoot: filepath.Join(root, "producer-graph"), RunRoot: filepath.Join(root, "producer-run"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, producerStore)
	policy := writeComponentTestPolicy(t, root)
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
	var blocked atomic.Value
	blocked.Store("")
	var failures atomic.Int32
	registry := registryserver.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && blocked.Load().(string) != "" && strings.HasSuffix(r.URL.Path, "/blobs/"+blocked.Load().(string)) {
			failures.Add(1)
			http.Error(w, "layer temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		registry.ServeHTTP(w, r)
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	reference := authority + "/coopr/newer:latest"
	registryOptions := oci.Options{TLSVerify: new(false), SignaturePolicyPath: policy, RetrySet: true}
	resolver, err := oci.NewResolver(registryOptions)
	if err != nil {
		t.Fatal(err)
	}
	makeParent := func(revision string) v1.Descriptor {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, revision), []byte(revision), 0600); err != nil {
			t.Fatal(err)
		}
		layout := filepath.Join(root, "parent-"+revision)
		_, err := BuildDefinitionSupervised(ctx, parseWorkerDefinition(t, fmt.Sprintf("from %q\ncopy %q \"/base-value\"\nenv revision=%q\nonbuild { run %q network=\"none\" }\n", base.reference, revision, revision, "printf "+revision+" > /onbuild-proof")), planner.Options{Mode: planner.Build}, SupervisedPlanOptions{
			Store: producerStore, ContextDir: root, Isolation: "rootless", SignaturePolicyPath: policy,
			Output: Output{Path: layout, Format: outputFormatDocker},
		})
		if err != nil {
			t.Fatal(err)
		}
		selected, err := oci.LayoutRoot(layout)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolver.PublishLayout(ctx, reference, layout, selected); err != nil {
			t.Fatal(err)
		}
		return selected
	}
	oldManifest := makeParent("old")
	consumerStore := StoreOptions{GraphRoot: filepath.Join(root, "consumer-graph"), RunRoot: filepath.Join(root, "consumer-run"), GraphDriverName: "vfs"}
	lease, err := acquireStore(consumerStore)
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	old, err := ResolveImageSource(ctx, resolver, reference, platform, lease.store, system)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	newManifest := makeParent("new")
	newLayers, _ := readPlanImage(t, filepath.Join(root, "parent-new"))
	blocked.Store(newLayers.Layers[len(newLayers.Layers)-1].Digest.String())
	registryOptions.PullPolicy = string(oci.PullNewer)
	resolver, err = oci.NewResolver(registryOptions)
	if err != nil {
		t.Fatal(err)
	}
	lease, err = acquireStore(consumerStore)
	if err != nil {
		t.Fatal(err)
	}
	fallback, fallbackErr := ResolveImageSource(ctx, resolver, reference, platform, lease.store, system)
	closeErr := lease.Close()
	if fallbackErr != nil || closeErr != nil {
		t.Fatalf("fallback=%v close=%v", fallbackErr, closeErr)
	}
	if fallback.ImageID != old.ImageID || fallback.Selected.Digest != oldManifest.Digest || string(fallback.ConfigData) != string(old.ConfigData) {
		t.Fatalf("copy failure selected newer metadata: %+v", fallback)
	}
	options := SupervisedPlanOptions{Store: consumerStore, ContextDir: root, Isolation: "rootless", SignaturePolicyPath: policy, TLSVerify: new(false), PullPolicy: string(oci.PullNewer), RetrySet: true}
	options.Output = Output{Path: filepath.Join(root, "child"), Format: outputFormatDocker}
	if _, err := BuildDefinitionSupervised(ctx, parseWorkerDefinition(t, fmt.Sprintf("from %q\nlabel selected=\"$revision\"\n", reference)), planner.Options{Mode: planner.Build}, options); err != nil {
		t.Fatal(err)
	}
	childManifest, childConfig := readPlanImage(t, options.Output.Path)
	if childConfig.Config.Labels["selected"] != "old" {
		t.Fatalf("planned config bound newer ENV: %+v", childConfig.Config.Labels)
	}
	childLayer := filepath.Join(options.Output.Path, "blobs", "sha256", childManifest.Layers[len(childManifest.Layers)-1].Digest.Encoded())
	if got := readLayerFile(t, childLayer, "onbuild-proof"); got != "old" {
		t.Fatalf("planned ONBUILD bound newer config: %q", got)
	}
	options.Output = Output{Path: filepath.Join(root, "component")}
	component := parseWorkerDefinition(t, fmt.Sprintf("from %q as=\"producer\"\npackage as=\"bundle\"\ncopy \"/onbuild-proof\" \"/proof\" from=\"producer\"\nextend\ncopy \"/proof\" \"/proof\" from=\"bundle\"\n", reference))
	published, err := PublishDefinitionSupervised(ctx, component, planner.Options{Mode: planner.Publish}, options)
	if err != nil {
		t.Fatal(err)
	}
	metadata := readComponentMetadata(t, ctx, options.Output.Path, published.Root)
	if len(metadata.Packages) != 1 {
		t.Fatalf("packages=%d", len(metadata.Packages))
	}
	packageLayer := filepath.Join(options.Output.Path, "blobs", "sha256", metadata.Packages[0].Descriptor.Digest.Encoded())
	if got := readLayerFile(t, packageLayer, "proof"); got != "old" {
		t.Fatalf("package ONBUILD bound newer config: %q", got)
	}
	if failures.Load() < 3 {
		t.Fatalf("expected failing layer copies in resolution, build and publication, got %d", failures.Load())
	}
	// Once the missing layer is available, newer must replace the native name
	// and bind the new configuration rather than continuing the fallback.
	blocked.Store("")
	lease, err = acquireStore(consumerStore)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, refreshErr := ResolveImageSource(ctx, resolver, reference, platform, lease.store, system)
	if refreshErr != nil {
		_ = lease.Close()
		t.Fatal(refreshErr)
	}
	if refreshed.Selected.Digest != newManifest.Digest || refreshed.ImageID == old.ImageID || string(refreshed.ConfigData) == string(old.ConfigData) {
		t.Fatalf("successful refresh retained cached selection: %+v", refreshed)
	}
	registryOptions.PullPolicy = string(oci.PullNever)
	offline, err := oci.NewResolver(registryOptions)
	if err != nil {
		_ = lease.Close()
		t.Fatal(err)
	}
	cached, cachedErr := ResolveImageSource(ctx, offline, reference, platform, lease.store, system)
	pinned, pinnedErr := ResolveImageSource(ctx, offline, oldManifest.Digest.String(), platform, lease.store, system)
	closeErr = lease.Close()
	if cachedErr != nil || pinnedErr != nil || closeErr != nil {
		t.Fatalf("offline cached=%v pinned=%v close=%v", cachedErr, pinnedErr, closeErr)
	}
	if cached.Selected.Digest != newManifest.Digest || pinned.Selected.Digest != oldManifest.Digest || string(pinned.ConfigData) != string(old.ConfigData) {
		t.Fatalf("offline native selections: cached=%+v pinned=%+v", cached, pinned)
	}
}
