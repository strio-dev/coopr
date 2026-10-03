package buildah

import (
	"context"
	"errors"
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
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for native pull fallback")
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
	var damageCatalog atomic.Bool
	var newLayer atomic.Value
	newLayer.Store("")
	mutation := make(chan error, 1)
	catalogPath := filepath.Join(root, "consumer-images", "catalog.json")
	registry := registryserver.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && blocked.Load().(string) != "" && strings.HasSuffix(r.URL.Path, "/blobs/"+blocked.Load().(string)) {
			failures.Add(1)
			http.Error(w, "layer temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blobs/"+newLayer.Load().(string)) && damageCatalog.CompareAndSwap(true, false) {
			err := os.Rename(catalogPath, catalogPath+".backup")
			if err == nil {
				err = os.Mkdir(catalogPath, 0700)
			}
			mutation <- err
		}
		registry.ServeHTTP(w, r)
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	reference := authority + "/coopr/newer:latest"
	registryOptions := oci.Options{ImageStoreDir: filepath.Join(root, "consumer-images"), PlainHTTPRegistries: []string{authority}, SignaturePolicyPath: policy, RetrySet: true}
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
			Store: producerStore, ContextDir: root, ImageStoreDir: base.imageStoreDir, Isolation: "rootless", SignaturePolicyPath: policy,
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
	makeParent("new")
	newLayers, _ := readPlanImage(t, filepath.Join(root, "parent-new"))
	blocked.Store(newLayers.Layers[len(newLayers.Layers)-1].Digest.String())
	newLayer.Store(blocked.Load())
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
	options := SupervisedPlanOptions{Store: consumerStore, ContextDir: root, ImageStoreDir: registryOptions.ImageStoreDir, Isolation: "rootless", SignaturePolicyPath: policy, PlainHTTPRegistries: []string{authority}, PullPolicy: string(oci.PullNewer), RetrySet: true}
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
	// An actual pull failure may reuse the local image; a successful pull's
	// verification or catalog failure must remain visible to the caller.
	blocked.Store("")
	damageCatalog.Store(true)
	lease, err = acquireStore(consumerStore)
	if err != nil {
		t.Fatal(err)
	}
	_, admissionErr := ResolveImageSource(ctx, resolver, reference, platform, lease.store, system)
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-mutation:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("catalog failure fixture did not reach layer download: %v", admissionErr)
	}
	if err := errors.Join(os.Remove(catalogPath), os.Rename(catalogPath+".backup", catalogPath)); err != nil {
		t.Fatal(err)
	}
	if admissionErr == nil || !strings.Contains(admissionErr.Error(), "catalog imported base image") {
		t.Fatalf("catalog failure silently reused old image: %v", admissionErr)
	}
}
