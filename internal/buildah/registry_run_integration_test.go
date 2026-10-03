package buildah

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coopr/internal/oci"
	"go.podman.io/image/v5/types"
)

func TestBuildPlanRunsWithAndWithoutNetworkFromRegistryBase(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live registry and runtime build")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	base, authority := newLiveBusyBoxRegistry(t, ctx)
	resolver, err := oci.NewResolver(oci.Options{
		ImageStoreDir: filepath.Join(root, "images"), Pull: true, PlainHTTPRegistries: []string{authority},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q
run "printf default >/default"
run "printf isolated >/isolated" network="none"
`, base))
	layout := filepath.Join(root, "layout")
	_, err = BuildPlan(ctx, plan, PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout}, Resolver: resolver,
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) < 3 || len(image.RootFS.DiffIDs) != len(manifest.Layers) {
		t.Fatalf("RUN image layers=%d diffIDs=%d, want base plus two RUN layers", len(manifest.Layers), len(image.RootFS.DiffIDs))
	}
}

func TestBuildPlanAddsCustomHostToRun(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live custom host build")
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
	plan := testPlan(t, fmt.Sprintf("from %q\nrun \"found=; while read ip name rest; do if test x$ip = x127.0.0.42 && test x$name = xcoopr.test; then found=yes; fi; done </etc/hosts; test x$found = xyes || exit 1; printf custom-host >/proof\"\n", base.reference))
	layout := filepath.Join(root, "layout")
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Output: Output{Path: layout}, ImageStoreDir: base.imageStoreDir, Pull: false,
		Network: "host", AddHosts: []string{"Coopr.Test:127.0.0.42"}, Allow: []string{"network.host"},
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	last := manifest.Layers[len(manifest.Layers)-1]
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof"); got != "custom-host" {
		t.Fatalf("custom host proof = %q", got)
	}
}

func TestBuildPlanSupervisedKeepsCacheMountAcrossBuilds(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live registry and runtime build")
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
run "if test -e /cache/proof; then printf hit >/result; else printf miss >/result; fi; touch /cache/proof" network="none" {
  mount "cache" target="/cache"
}
`, base.reference))
	for attempt, want := range []string{"miss", "hit"} {
		layout := filepath.Join(root, "layout-"+string(rune('1'+attempt)))
		// Re-execute the second RUN: a normal instruction-cache hit would skip
		// the command, while the cache mount itself must still retain its data.
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
			ImageStoreDir: base.imageStoreDir, Pull: false, NoCache: attempt > 0, SignaturePolicyPath: policy,
			Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "result"); got != want {
			t.Fatalf("build %d cache result = %q, want %q", attempt+1, got, want)
		}
	}
}

func TestBuildPlanSupervisedRunsIndependentJobsConcurrently(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live concurrent RUN builds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	warm := testPlan(t, "from \""+base.reference+"\"\n")
	_, err := BuildPlanSupervised(ctx, warm, SupervisedPlanOptions{
		Store: store, Isolation: "rootless", Runtime: "crun", Output: Output{Path: filepath.Join(root, "warm")},
		ImageStoreDir: base.imageStoreDir, Pull: false, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	type jobResult struct {
		name string
		err  error
	}
	start := make(chan struct{})
	results := make(chan jobResult, 2)
	for _, name := range []string{"a", "b"} {
		name := name
		other := "a"
		if name == "a" {
			other = "b"
		}
		plan := testPlan(t, "from \""+base.reference+"\"\n"+
			"run \"touch /cache/"+name+"; for i in $(seq 1 15); do if test -e /cache/"+other+"; then echo concurrent >/proof; exit 0; fi; sleep 1; done; exit 1\" network=\"none\" {\n"+
			"  mount \"cache\" target=\"/cache\" id=\"coopr-concurrency-proof\" sharing=\"shared\"\n}\n")
		go func() {
			<-start
			_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
				Store: store, Isolation: "rootless", Runtime: "crun", Output: Output{Path: filepath.Join(root, "layout-"+name)},
				ImageStoreDir: base.imageStoreDir, Pull: false, SignaturePolicyPath: policy,
				Stdout: io.Discard, Stderr: io.Discard,
			})
			results <- jobResult{name: name, err: err}
		}()
	}
	close(start)
	for range 2 {
		job := <-results
		if job.err != nil {
			t.Fatalf("concurrent build %s: %v", job.name, job.err)
		}
		manifest, _ := readPlanImage(t, filepath.Join(root, "layout-"+job.name))
		last := manifest.Layers[len(manifest.Layers)-1]
		if got := readLayerFile(t, filepath.Join(root, "layout-"+job.name, "blobs", "sha256", last.Digest.Encoded()), "proof"); got != "concurrent\n" {
			t.Fatalf("build %s overlap proof = %q", job.name, got)
		}
	}
}

func readLayerFile(t *testing.T, blobPath, wanted string) string {
	t.Helper()
	file, err := os.Open(blobPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck
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
		defer decoded.Close() //nolint:errcheck
		reader = decoded
	}
	archive := tar.NewReader(reader)
	for {
		entry, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(entry.Name, "./") != wanted {
			continue
		}
		contents, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		return string(contents)
	}
	t.Fatalf("layer %s has no %s", blobPath, wanted)
	return ""
}
