package buildah

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestWorkerJobIDValidation(t *testing.T) {
	id, err := newWorkerJobID()
	if err != nil || !validWorkerJobID(id) {
		t.Fatalf("generated worker ID = %q, %v", id, err)
	}
	for _, invalid := range []string{"", "abc", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/"} {
		if validWorkerJobID(invalid) {
			t.Fatalf("accepted invalid worker ID %q", invalid)
		}
	}
}

func TestWorkerRequestsPreserveTLSVerifyPolicy(t *testing.T) {
	for _, verify := range []*bool{nil, new(true), new(false)} {
		for _, request := range []any{planWorkerRequest{TLSVerify: verify}, stageWorkerRequest{TLSVerify: verify}} {
			data, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				TLSVerify *bool `json:"tls_verify,omitempty"`
			}
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if verify == nil {
				if decoded.TLSVerify != nil {
					t.Fatalf("%T changed omitted TLS policy", request)
				}
			} else if decoded.TLSVerify == nil || *decoded.TLSVerify != *verify {
				t.Fatalf("%T lost explicit TLS policy %v", request, *verify)
			}
		}
	}
}

func TestStoredTransferTLSVerifyPolicy(t *testing.T) {
	for _, test := range []struct {
		verify *bool
		want   types.OptionalBool
	}{
		{nil, types.OptionalBoolUndefined},
		{new(true), types.OptionalBoolFalse},
		{new(false), types.OptionalBoolTrue},
	} {
		system, err := storedTransferSystemContext(StoredTransferOptions{TLSVerify: test.verify}, "")
		if err != nil {
			t.Fatal(err)
		}
		if system.DockerInsecureSkipTLSVerify != test.want {
			t.Fatalf("native TLS policy = %v, want %v", system.DockerInsecureSkipTLSVerify, test.want)
		}
	}
}

func TestPlanWorkerMessagePreservesRequestExecutionOptions(t *testing.T) {
	root := t.TempDir()
	request := planWorkerRequest{
		Mode: "build", NoCache: true, Network: "none", AddHosts: []string{"example.test:127.0.0.1"}, RewriteTimestamp: true, Allow: []string{"network.host"},
		RunControls: RunControls{HTTPProxy: true, Memory: 64 << 20, DNSServers: []string{"1.1.1.1"}}, Jobs: 3,
		AuthFile: "/tmp/auth.json", CertDir: "/tmp/certs", TLSVerify: new(false),
		PullPolicy:    string(oci.PullNever),
		CacheFrom:     []CacheSpec{{Transport: "registry", Reference: "registry.example/read"}},
		CacheTo:       []CacheSpec{{Transport: "registry", Reference: "registry.example/write"}},
		BuildContexts: []buildcontext.Spec{{Name: "base", Kind: buildcontext.DockerImage, Reference: "example.com/base:latest"}},
		ResultPath:    filepath.Join(root, "result.json"),
	}
	path := filepath.Join(root, "request.json")
	if err := writeWorkerJSON(path, request); err != nil {
		t.Fatal(err)
	}
	var decoded planWorkerRequest
	if err := readWorkerJSON(path, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.NoCache {
		t.Fatal("plan worker request lost no-cache setting")
	}
	if decoded.Network != "none" || !slices.Equal(decoded.AddHosts, request.AddHosts) {
		t.Fatalf("plan worker request network options = %q, %#v", decoded.Network, decoded.AddHosts)
	}
	if !decoded.RunControls.HTTPProxy || decoded.RunControls.Memory != 64<<20 || decoded.Jobs != 3 {
		t.Fatalf("plan worker request controls = %+v, jobs=%d", decoded.RunControls, decoded.Jobs)
	}
	if decoded.AuthFile != request.AuthFile || decoded.CertDir != request.CertDir || decoded.TLSVerify == nil || *decoded.TLSVerify {
		t.Fatalf("plan worker request registry options = %q, %q, %v", decoded.AuthFile, decoded.CertDir, decoded.TLSVerify)
	}
	if decoded.PullPolicy != request.PullPolicy || !slices.Equal(decoded.CacheFrom, request.CacheFrom) || !slices.Equal(decoded.CacheTo, request.CacheTo) {
		t.Fatalf("plan worker request pull/cache options = %q, %#v, %#v", decoded.PullPolicy, decoded.CacheFrom, decoded.CacheTo)
	}
	if !decoded.RewriteTimestamp {
		t.Fatal("plan worker request lost rewrite-timestamp setting")
	}
	if !slices.Equal(decoded.Allow, []string{"network.host"}) {
		t.Fatalf("plan worker request allow = %#v", decoded.Allow)
	}
	if !slices.Equal(decoded.BuildContexts, request.BuildContexts) {
		t.Fatalf("plan worker request build contexts = %#v", decoded.BuildContexts)
	}
}

func TestPlanWorkerRequestRequiresExactlyOneBuildInput(t *testing.T) {
	def := parseWorkerDefinition(t, `from "scratch"`)
	plan, err := planner.Create(def, planner.Options{Mode: planner.Build})
	if err != nil {
		t.Fatal(err)
	}
	buildOptions := planner.Options{Mode: planner.Build}
	base := planWorkerRequest{Mode: "build", ResultPath: "/result", Plan: plan}
	if err := validatePlanWorkerRequest(base); err != nil {
		t.Fatalf("planned request rejected: %v", err)
	}
	raw := planWorkerRequest{Mode: "build", ResultPath: "/result", Definition: def, PlannerOptions: &buildOptions}
	if err := validatePlanWorkerRequest(raw); err != nil {
		t.Fatalf("raw request rejected: %v", err)
	}
	for name, request := range map[string]planWorkerRequest{
		"neither input":         {Mode: "build", ResultPath: "/result"},
		"both inputs":           {Mode: "build", ResultPath: "/result", Plan: plan, Definition: def, PlannerOptions: &buildOptions},
		"definition no options": {Mode: "build", ResultPath: "/result", Definition: def},
		"plan with options":     {Mode: "build", ResultPath: "/result", Plan: plan, PlannerOptions: &buildOptions},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validatePlanWorkerRequest(request); err == nil {
				t.Fatal("invalid request was accepted")
			}
		})
	}
}

func TestPlanWorkerImportRequestRequiresLayoutAndManifest(t *testing.T) {
	request := planWorkerRequest{
		Mode: "import", ResultPath: "/result", Output: Output{Path: "/layout"},
		ManifestDescriptor: v1.Descriptor{Digest: digest.FromString("manifest"), MediaType: v1.MediaTypeImageManifest, Size: 1},
	}
	if err := validatePlanWorkerRequest(request); err != nil {
		t.Fatalf("valid import request rejected: %v", err)
	}
	request.Output.Path = ""
	if err := validatePlanWorkerRequest(request); err == nil {
		t.Fatal("import request without layout accepted")
	}
}

func TestDefinitionSupervisedRequiresMatchingPlannerMode(t *testing.T) {
	def := parseWorkerDefinition(t, `from "scratch"`)
	root := t.TempDir()
	options := SupervisedPlanOptions{
		Store:  StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		Output: Output{Path: filepath.Join(root, "layout")},
	}
	if _, err := BuildDefinitionSupervised(context.Background(), def, planner.Options{Mode: planner.Publish}, options); err == nil || !strings.Contains(err.Error(), `requires planner mode "build"`) {
		t.Fatalf("build mode mismatch error = %v", err)
	}
	if _, err := PublishDefinitionSupervised(context.Background(), def, planner.Options{Mode: planner.Build}, options); err == nil || !strings.Contains(err.Error(), `requires planner mode "publish"`) {
		t.Fatalf("publish mode mismatch error = %v", err)
	}
}

func TestPromoteCompleteOutputNeverReplacesExistingDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	for _, directory := range []string{source, destination} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(destination, "owned"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := promoteCompleteOutput(source, destination); !errors.Is(err, os.ErrExist) {
		t.Fatalf("promote over existing destination: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source removed after failed promotion: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(destination, "owned"))
	if err != nil || string(contents) != "keep" {
		t.Fatalf("existing output changed: %q, %v", contents, err)
	}
}

func TestBuildPlanSupervisedPromotesCompleteLayout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah worker in short mode")
	}
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("supervised\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, "from \"scratch\"\ncopy \"proof\" \"/proof\"\n")
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Layout != layout || result.ManifestDigest == "" {
		t.Fatalf("supervised result = %+v", result)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("supervised layers = %d, want 1", len(manifest.Layers))
	}
}

func TestBuildDefinitionSupervisedMatchesPlannedBuildBehavior(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah worker in short mode")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "proof"), []byte("raw supervised\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	def := parseWorkerDefinition(t, "from \"scratch\"\ncopy \"proof\" \"/proof\"\n")
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := BuildDefinitionSupervised(ctx, def, planner.Options{
		Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}, SupervisedPlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Layout != layout || result.ManifestDigest == "" {
		t.Fatalf("raw supervised result = %+v", result)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("raw supervised layers = %d, want 1", len(manifest.Layers))
	}
}

func TestPublishPlanSupervisedPromotesSortedComponentLayout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah worker in short mode")
	}
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("supervised publication\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPublicationPlan(t, `
package as="z-last"
copy "proof" "/z"
package as="a-first"
copy "proof" "/a"
extend
copy "/z" "/z" from="z-last"
copy "/a" "/a" from="a-first"
`)
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := PublishPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Layout != layout || result.Root.Digest == "" {
		t.Fatalf("supervised publication result = %+v", result)
	}
	rootDescriptor, err := oci.LayoutRoot(layout)
	if err != nil {
		t.Fatal(err)
	}
	if rootDescriptor.Digest != result.Root.Digest {
		t.Fatalf("published root = %s, returned %s", rootDescriptor.Digest, result.Root.Digest)
	}
	metadata := readComponentMetadata(t, ctx, layout, rootDescriptor)
	if len(metadata.Packages) != 2 || metadata.Packages[0].Stage != "a-first" || metadata.Packages[1].Stage != "z-last" {
		t.Fatalf("component packages are not sorted: %+v", metadata.Packages)
	}
}

func TestPublishDefinitionSupervisedMatchesPlannedPublicationBehavior(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah worker in short mode")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "proof"), []byte("raw publication\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	def := parseWorkerDefinition(t, `
package as="z-last"
copy "proof" "/z"
package as="a-first"
copy "proof" "/a"
extend
copy "/z" "/z" from="z-last"
copy "/a" "/a" from="a-first"
`)
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := PublishDefinitionSupervised(ctx, def, planner.Options{
		Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}, SupervisedPlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Layout != layout || result.Root.Digest == "" {
		t.Fatalf("raw supervised publication result = %+v", result)
	}
	metadata := readComponentMetadata(t, ctx, layout, result.Root)
	if len(metadata.Packages) != 2 || metadata.Packages[0].Stage != "a-first" || metadata.Packages[1].Stage != "z-last" {
		t.Fatalf("raw component packages are not sorted: %+v", metadata.Packages)
	}
}

func parseWorkerDefinition(t *testing.T, source string) *definition.Definition {
	t.Helper()
	def, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func readComponentMetadata(t *testing.T, ctx context.Context, layout string, root v1.Descriptor) oci.ComponentMetadata {
	t.Helper()
	store, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	manifestData, err := content.FetchAll(ctx, store, root)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	configData, err := content.FetchAll(ctx, store, manifest.Config)
	if err != nil {
		t.Fatal(err)
	}
	var metadata oci.ComponentMetadata
	if err := json.Unmarshal(configData, &metadata); err != nil {
		t.Fatal(err)
	}
	return metadata
}

func TestBuildPlanSupervisedCopyDotExcludesCooprArtifacts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless context build in short mode")
	}
	root := t.TempDir()
	images := filepath.Join(root, "images")
	if err := os.Mkdir(images, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		"included": "include\n", "ignored": "ignore\n", ".dockerignore": "ignored\n",
		filepath.Join("images", "secret"): "never copy\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan := testPlan(t, "from \"scratch\"\ncopy \".\" \"/ctx/\"\n")
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: images, GraphDriverName: "vfs"},
		ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("COPY . layers = %d, want 1", len(manifest.Layers))
	}
	file, err := os.Open(filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close context layer: %v", err)
		}
	}()
	buffered := bufio.NewReader(file)
	header, err := buffered.Peek(2)
	if err != nil {
		t.Fatal(err)
	}
	var reader io.Reader = buffered
	if header[0] == 0x1f && header[1] == 0x8b {
		decoded, err := gzip.NewReader(buffered)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := decoded.Close(); err != nil {
				t.Errorf("close decoded context layer: %v", err)
			}
		}()
		reader = decoded
	}
	archive := tar.NewReader(reader)
	foundIncluded := false
	for {
		entry, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(entry.Name)), "./")
		if name == "ctx/included" {
			foundIncluded = true
		}
		if strings.HasPrefix(name, "ctx/images/") || strings.HasPrefix(name, "ctx/graph/") || strings.HasPrefix(name, "ctx/run/") || strings.Contains(name, ".coopr-build-worker-") || name == "ctx/ignored" {
			t.Fatalf("COPY . included protected or ignored path %q", name)
		}
	}
	if !foundIncluded {
		t.Fatal("COPY . omitted the ordinary input file")
	}
}

func TestBuildPlanSupervisedCancelsRunAndCleansBuilder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live worker cancellation in short mode")
	}
	root := t.TempDir()
	fixtureContext := context.Background()
	storeOptions := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, fixtureContext, root, storeOptions)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q
run "trap '' TERM; sleep 60" network="none"
`, base.reference))
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: storeOptions, ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		Pull: false, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled build error = %v", err)
	}
	if !strings.Contains(err.Error(), "build worker exited after cleanup") {
		t.Fatalf("worker did not report an orderly cancellation: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 12*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
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
		t.Fatalf("cancelled build left %d working containers", len(containers))
	}
}

func TestVerifyStoredImageSupervisedRejectsMissingImage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah storage checks in short mode")
	}
	root := t.TempDir()
	options := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	if err := VerifyStoredImageSupervised(context.Background(), options, strings.Repeat("0", 64)); err == nil {
		t.Fatal("missing storage image was accepted")
	}
}

func TestCleanupNamedBuildersOnlyRemovesItsJob(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah cleanup in short mode")
	}
	root := t.TempDir()
	options := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close cleanup test store: %v", err)
		}
	}()
	network, err := newNetworkInterface(lease.store)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := newWorkerJobID()
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := newWorkerJobID()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{jobID, otherID} {
		builderOptions, err := newBuilderOptions("scratch", define.IsolationChroot, rootCapabilities(), network, root, "")
		if err != nil {
			t.Fatal(err)
		}
		builderOptions.Container = "coopr-" + id + "-0"
		builder, err := upstream.NewBuilder(context.Background(), lease.store, builderOptions)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := builder.Mount(builder.MountLabel); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupNamedBuilders(options, jobID); err != nil {
		t.Fatal(err)
	}
	if err := cleanupNamedBuilders(options, jobID); err != nil {
		t.Fatalf("second cleanup was not idempotent: %v", err)
	}
	containers, err := lease.store.Containers()
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || len(containers[0].Names) != 1 || containers[0].Names[0] != "coopr-"+otherID+"-0" {
		t.Fatalf("cleanup touched another job or left its own container: %+v", containers)
	}
	if err := cleanupNamedBuilders(options, otherID); err != nil {
		t.Fatal(err)
	}
}

func TestBuildPlanSupervisedCancelsStalledRemoteAdd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live stalled ADD cancellation in short mode")
	}
	started := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		once.Do(func() { close(started) })
		<-request.Context().Done()
	}))
	defer func() {
		server.CloseClientConnections()
		server.Close()
	}()
	root := t.TempDir()
	plan := testPlan(t, fmt.Sprintf("from \"scratch\"\nadd %q \"/hang\"\n", server.URL+"/hang"))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	layout := filepath.Join(root, "layout")
	storeOptions := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: storeOptions, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stalled ADD cancellation error = %v", err)
	}
	if _, err := os.Lstat(layout); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stalled ADD published output: %v", err)
	}
	store, err := storage.GetStore(storage.StoreOptions{
		RunRoot: storeOptions.RunRoot, GraphRoot: storeOptions.GraphRoot, GraphDriverName: storeOptions.GraphDriverName,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := store.Shutdown(false); err != nil {
			t.Errorf("shutdown stalled ADD store: %v", err)
		}
		store.Free()
	}()
	containers, err := store.Containers()
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 0 {
		t.Fatalf("stalled ADD left %d working containers", len(containers))
	}
}

func TestPublishPlanSupervisedCancelsStalledRemoteAdd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live stalled component ADD cancellation in short mode")
	}
	started := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		once.Do(func() { close(started) })
		<-request.Context().Done()
	}))
	defer func() {
		server.CloseClientConnections()
		server.Close()
	}()
	root := t.TempDir()
	plan := testPublicationPlan(t, fmt.Sprintf(`
package as="payload"
add %q "/payload"
extend
copy "/payload" "/payload" from="payload"
`, server.URL+"/hang"))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	layout := filepath.Join(root, "layout")
	storeOptions := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	_, err := PublishPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: storeOptions, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stalled component ADD cancellation error = %v", err)
	}
	if _, err := os.Lstat(layout); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stalled component ADD published output: %v", err)
	}
	store, err := storage.GetStore(storage.StoreOptions{
		RunRoot: storeOptions.RunRoot, GraphRoot: storeOptions.GraphRoot, GraphDriverName: storeOptions.GraphDriverName,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := store.Shutdown(false); err != nil {
			t.Errorf("shutdown stalled publication store: %v", err)
		}
		store.Free()
	}()
	containers, err := store.Containers()
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 0 {
		t.Fatalf("stalled publication left %d working containers", len(containers))
	}
}
