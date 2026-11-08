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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/planner"
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
		for _, request := range []any{planWorkerRequest{
			workerExecutionOptions: workerExecutionOptions{
				TLSVerify: verify,
			},
		}, stageWorkerRequest{
			workerExecutionOptions: workerExecutionOptions{
				TLSVerify: verify,
			},
		}} {
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

func TestOrdinaryBuildWithoutExecutableOrigin(t *testing.T) {
	if len(os.Args) >= 4 && os.Args[len(os.Args)-3] == "--" {
		mode, origin := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
		if mode != "normalize" && mode != "supervised" {
			t.Fatalf("unexpected subprocess mode %q", mode)
		}
		if !filepath.IsAbs(origin) || origin != currentRunSequenceOrigin() {
			t.Fatalf("subprocess unlink path %q is not its launch origin", origin)
		}
		if err := os.Remove(origin); err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "normalize":
			if _, err := normalizePlanOptions(PlanOptions{}); err != nil {
				t.Fatalf("ordinary options require removed executable origin: %v", err)
			}
		case "supervised":
			root := t.TempDir()
			plan := testPlan(t, `from "scratch" {}`)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}, ContextDir: root, Isolation: "rootless", Output: Output{Path: filepath.Join(root, "layout")}, Stdout: io.Discard, Stderr: io.Discard})
			if err != nil {
				t.Fatalf("ordinary supervised build requires removed executable origin: %v", err)
			}
			if result.ManifestDigest == "" {
				t.Fatal("scratch build emitted no manifest")
			}
		}
		if _, _, _, err := prepareRunSequence(nativeBuilder{executableOrigin: origin}, [][]string{{"first"}, {"second"}}); err == nil || !strings.Contains(err.Error(), "RUN sequence helper") {
			t.Fatalf("multi-command RUN missing origin error = %v", err)
		}
		return
	}
	for _, mode := range []string{"normalize", "supervised"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "supervised" && testing.Short() {
				t.Skip("requires live rootless worker")
			}
			origin := filepath.Join(t.TempDir(), "deleted-origin.test")
			source, err := os.Open("/proc/self/exe")
			if err != nil {
				t.Fatal(err)
			}
			destination, err := os.OpenFile(origin, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
			if err != nil {
				t.Fatal(errors.Join(err, source.Close()))
			}
			_, copyErr := io.Copy(destination, source)
			if err := errors.Join(copyErr, source.Close(), destination.Close()); err != nil {
				t.Fatal(err)
			}
			args := []string{"-test.run=^TestOrdinaryBuildWithoutExecutableOrigin$"}
			if testing.Short() {
				args = append(args, "-test.short")
			}
			command := exec.Command(origin, append(args, "--", mode, origin)...)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("%s without executable origin: %v\n%s", mode, err, output)
			}
		})
	}
}

func TestOrdinaryBuildRejectsUnrelatedUnlinkPath(t *testing.T) {
	for _, mode := range []string{"normalize", "invalid-mode"} {
		t.Run(mode, func(t *testing.T) {
			unrelated := filepath.Join(t.TempDir(), "keep")
			if err := os.WriteFile(unrelated, []byte("keep this file"), 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("/proc/self/exe", "-test.short", "-test.run=^TestOrdinaryBuildWithoutExecutableOrigin$", "--", mode, unrelated)
			if output, err := command.CombinedOutput(); err == nil {
				t.Fatalf("accepted unrelated unlink path: %s", output)
			}
			data, err := os.ReadFile(unrelated)
			if err != nil || string(data) != "keep this file" {
				t.Fatalf("malformed subprocess changed unrelated file: %q, %v", data, err)
			}
		})
	}
}

func TestPlanOptionsPreserveExecutionOrigin(t *testing.T) {
	origin := currentRunSequenceOrigin()
	for _, explicit := range []string{"", "/opt/coopr/bin/coopr"} {
		options, err := normalizePlanOptions(PlanOptions{executableOrigin: explicit})
		if err != nil {
			t.Fatal(err)
		}
		want := explicit
		if want == "" {
			want = origin
		}
		if options.executableOrigin != want {
			t.Fatalf("executable origin = %q, want %q", options.executableOrigin, want)
		}
	}
}

func TestPlanWorkerRequestRequiresExactlyOneBuildInput(t *testing.T) {
	def := parseWorkerDefinition(t, `from "scratch" {}
`)
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

func TestPlanWorkerRequestRejectsRemovedModes(t *testing.T) {
	for _, mode := range []string{"import", "export"} {
		request := planWorkerRequest{Mode: mode, ResultPath: "/result", Output: Output{Path: "/layout"}, ImageID: strings.Repeat("0", 64), ManifestDescriptor: v1.Descriptor{Digest: digest.FromString("manifest"), MediaType: v1.MediaTypeImageManifest, Size: 1}}
		if err := validatePlanWorkerRequest(request); err == nil {
			t.Fatalf("removed %s mode accepted", mode)
		}
	}
}

func TestDefinitionSupervisedRequiresMatchingPlannerMode(t *testing.T) {
	def := parseWorkerDefinition(t, `from "scratch" {}
`)
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
	if err := promoteCompleteOutput(source, destination, true); !errors.Is(err, os.ErrExist) {
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
	for _, raw := range []bool{false, true} {
		t.Run(fmt.Sprintf("raw=%t", raw), func(t *testing.T) {
			root := t.TempDir()
			contextDir := filepath.Join(root, "context")
			if err := os.Mkdir(contextDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("supervised\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			def := parseWorkerDefinition(t, `from "scratch" { copy "proof" "/proof" }`)
			planning := planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH}
			layout := filepath.Join(root, "layout")
			options := SupervisedPlanOptions{
				Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
				ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard,
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var result Result
			var err error
			if raw {
				result, err = BuildDefinitionSupervised(ctx, def, planning, options)
			} else {
				plan, planErr := planner.Create(def, planning)
				if planErr != nil {
					t.Fatal(planErr)
				}
				result, err = BuildPlanSupervised(ctx, plan, options)
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Layout != layout || result.ManifestDigest == "" || result.Platform != planning.Platform {
				t.Fatalf("supervised result = %+v", result)
			}
			manifest, _ := readPlanImage(t, layout)
			if len(manifest.Layers) != 1 {
				t.Fatalf("supervised layers = %d, want 1", len(manifest.Layers))
			}
			layer := manifest.Layers[0]
			if got := readLayerFile(t, filepath.Join(layout, "blobs", layer.Digest.Algorithm().String(), layer.Digest.Encoded()), "proof"); got != "supervised\n" {
				t.Fatalf("promoted layer proof = %q", got)
			}
		})
	}
}

func TestPublishDefinitionSupervisedPromotesSortedComponentLayout(t *testing.T) {
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
	def := parseWorkerDefinition(t, `
package as="z-last" {
    copy "proof" "/z"
}
package as="a-first" {
    copy "proof" "/a"
}
extend {
    copy "/z" "/z" from="z-last"
    copy "/a" "/a" from="a-first"
}
`)
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := PublishDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH}, SupervisedPlanOptions{
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
	for _, pack := range metadata.Packages {
		name := map[string]string{"a-first": "a", "z-last": "z"}[pack.Stage]
		blob := filepath.Join(layout, "blobs", pack.Descriptor.Digest.Algorithm().String(), pack.Descriptor.Digest.Encoded())
		if got := readLayerFile(t, blob, name); got != "supervised publication\n" {
			t.Fatalf("published package %s proof = %q", pack.Stage, got)
		}
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
	plan := testPlan(t, `from "scratch" {
    copy "." "/ctx/"
}
`)
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
from %q {
    run "trap '' TERM; sleep 60" network="none"
}
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

func TestVerifyStoredImageSelectedSupervisedRejectsMissingImage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah storage checks in short mode")
	}
	root := t.TempDir()
	options := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	if err := VerifyStoredImageSelectedSupervised(context.Background(), options, strings.Repeat("0", 64), v1.Descriptor{}); err == nil {
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
		builderOptions, err := newBuilderOptionsWithHosts("scratch", define.IsolationChroot, rootCapabilities(), network, root, "", nil, RunControls{})
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
	for _, mode := range []planner.Mode{planner.Build, planner.Publish} {
		t.Run(string(mode), func(t *testing.T) {
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
			source := fmt.Sprintf(`from "scratch" { add %q "/hang" }`, server.URL+"/hang")
			if mode == planner.Publish {
				source = fmt.Sprintf(`package as="payload" { add %q "/payload" }; extend { copy "/payload" "/payload" from="payload" }`, server.URL+"/hang")
			}
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
			options := SupervisedPlanOptions{Store: storeOptions, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard}
			var err error
			if mode == planner.Build {
				_, err = BuildPlanSupervised(ctx, testPlan(t, source), options)
			} else {
				_, err = PublishDefinitionSupervised(ctx, parseWorkerDefinition(t, source), planner.Options{Mode: mode, Platform: runtime.GOOS + "/" + runtime.GOARCH}, options)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("stalled ADD cancellation error = %v", err)
			}
			if _, err := os.Lstat(layout); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stalled ADD published output: %v", err)
			}
			store, err := storage.GetStore(NativeStoreOptions(storeOptions))
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
		})
	}
}

// Corrupt the actual telemetry transport while a real worker waits for ADD.
// Neither a plan nor a stage progress failure invalidates its final cleanup proof.
func TestSupervisedWorkerProgressFailurePreservesCleanupProof(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native rootless worker execution")
	}
	for _, kind := range []string{"plan", "stage"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			corrupted := make(chan error, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				pattern := filepath.Join(root, ".coopr-build-worker-*", "progress.ndjson")
				if kind == "stage" {
					pattern = filepath.Join(root, ".coopr-build-worker-*", ".coopr-stage-worker-*", "progress.ndjson")
				}
				paths, err := filepath.Glob(pattern)
				if err == nil && len(paths) == 0 {
					err = errors.New("no active worker progress transport")
				}
				for _, path := range paths {
					file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
					if errors.Is(openErr, os.ErrNotExist) {
						continue // An unrelated stage may have completed meanwhile.
					}
					if openErr != nil {
						err = errors.Join(err, openErr)
						continue
					}
					_, writeErr := file.WriteString("{broken-progress}\n")
					err = errors.Join(err, writeErr, file.Close())
				}
				select {
				case corrupted <- err:
				default:
				}
				_, _ = w.Write([]byte("proof"))
			}))
			defer server.Close()
			source := fmt.Sprintf(`from "scratch" { add %q "/proof" }`, server.URL+"/proof")
			jobs := 1
			if kind == "stage" {
				jobs = 2
				source = fmt.Sprintf(`from "scratch" as="source" { add %q "/proof" }
from "scratch" as="other" { copy "local-proof" "/proof" }
from "scratch" {
 copy "/proof" "/source" from="source"
 copy "/proof" "/other" from="other"
}`, server.URL+"/proof")
				if err := os.WriteFile(filepath.Join(root, "local-proof"), []byte("proof"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(root, "output")
			_, err := BuildPlanSupervised(ctx, testPlan(t, source), SupervisedPlanOptions{
				Store: cacheTestStore(filepath.Join(root, "store")), ContextDir: root,
				Isolation: "rootless", Jobs: jobs, Progress: func(OperationProgress) {},
				Output: Output{Path: output}, Stdout: io.Discard, Stderr: io.Discard,
			})
			select {
			case corruptErr := <-corrupted:
				if corruptErr != nil {
					t.Fatal(corruptErr)
				}
			default:
				t.Fatalf("worker did not reach telemetry corruption fixture: %v", err)
			}
			if err == nil || !strings.Contains(err.Error(), "live worker progress") {
				t.Fatalf("lost telemetry failure: %v", err)
			}
			if errors.Is(err, ErrWorkerWaitIncomplete) {
				t.Fatalf("progress failure invalidated completed cleanup: %v", err)
			}
			if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed worker promoted output: %v", statErr)
			}
			remaining, globErr := filepath.Glob(filepath.Join(root, ".coopr-build-worker-*"))
			if globErr != nil || len(remaining) != 0 {
				t.Fatalf("completed worker files retained: %v (%v)", remaining, globErr)
			}
		})
	}
}
