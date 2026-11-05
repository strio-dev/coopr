package buildah

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/buildcontext"
)

func TestNamedContextImageAndStageDirectoryMountsAreReadOnly(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	named := filepath.Join(root, "named")
	for _, directory := range []string{contextDir, named, filepath.Join(named, "directory")} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, value := range map[string]string{filepath.Join(contextDir, "payload.txt"): "image-data", filepath.Join(named, "directory", "payload.txt"): "named-data"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := cacheTestStore(filepath.Join(root, "store"))
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := writeComponentTestPolicy(t, root)
	seed := testPlan(t, fmt.Sprintf(`from %q { copy "payload.txt" "/directory/image-payload" }`, base.reference))
	result, err := BuildPlanSupervised(ctx, seed, SupervisedPlanOptions{Store: store, ContextDir: contextDir, Runtime: "crun", Isolation: "rootless", Output: Output{Path: filepath.Join(root, "seed")}, PullPolicy: "never", SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	image := "containers-storage:" + result.ImageID
	files := "/named/directory/payload.txt /selected-named/payload.txt /stage/directory/image-payload /selected-stage/image-payload /image/directory/image-payload /selected-image/image-payload"
	command := `for file in ` + files + ` /metadata/payload.txt` + `; do if (printf mutation >>"$file") 2>/dev/null; then echo "writable:$file" >&2; exit 19; fi; done; /bin/busybox cat ` + files + ` /metadata/payload.txt >/proof`
	writable := `for file in ` + files + `; do printf disposable >>"$file" || exit 20; done`
	spec := buildcontext.Spec{Name: "assets", Kind: buildcontext.Local, Path: named}
	plan := namedContextGraphPlan(t, fmt.Sprintf(`from %q as="previous" {}
from %q {
 run %q network="none" {
  mount "bind" from="assets" target="/named" readonly=#false
  mount "bind" from="assets" source="directory" target="/selected-named" readonly=#false
  mount "bind" from="previous" target="/stage" readonly=#false
  mount "bind" from="previous" source="directory" target="/selected-stage" readonly=#false
  mount "bind" from=%q target="/image" readonly=#false
  mount "bind" from=%q source="directory" target="/selected-image" readonly=#false
 }
 run %q network="none" {
  mount "bind" from="assets" target="/named" readonly=#true
  mount "bind" from="assets" source="directory" target="/selected-named" readonly=#true
  mount "bind" from="assets" source="directory" target="/metadata" readonly=#true relabel="shared" u=#true
  mount "bind" from="previous" target="/stage" readonly=#true
  mount "bind" from="previous" source="directory" target="/selected-stage" readonly=#true
  mount "bind" from=%q target="/image" readonly=#true
  mount "bind" from=%q source="directory" target="/selected-image" readonly=#true
 }
}`, image, image, writable, image, image, command, image, image), spec)
	var log strings.Builder
	layout := filepath.Join(root, "readonly")
	if _, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: contextDir, BuildContexts: []buildcontext.Spec{spec}, Runtime: "crun", Isolation: "rootless", Output: Output{Path: layout}, PullPolicy: "never", SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: &log}); err != nil {
		t.Fatalf("read-only directory contract failed: %v\n%s", err, log.String())
	}
	manifest, _ := readPlanImage(t, layout)
	last := manifest.Layers[len(manifest.Layers)-1]
	if proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof"); proof != "named-datanamed-dataimage-dataimage-dataimage-dataimage-datanamed-data" {
		t.Fatalf("mounted read-only files changed: %q", proof)
	}
	if got, err := os.ReadFile(filepath.Join(named, "directory", "payload.txt")); err != nil || string(got) != "named-data" {
		t.Fatalf("caller named input changed: %q %v", got, err)
	}
}
