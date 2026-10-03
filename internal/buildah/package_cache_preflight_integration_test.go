package buildah

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"coopr/internal/planner"
)

func TestPublishDefinitionPackageCacheHitDoesNotOpenFreshBuildStore(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah package cache")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	_, authority := newLiveBusyBoxRegistry(t, ctx)
	reference := authority + "/coopr/busybox:latest"
	cacheDir := filepath.Join(root, "cache")
	imageStoreDir := filepath.Join(root, "images")
	componentStoreDir := filepath.Join(root, "components")
	policy := writeComponentTestPolicy(t, root)
	component := parseWorkerDefinition(t, `
from "`+reference+`" as="producer"
package as="from-bundle"
copy "/bin/busybox" "/from-busybox" from="producer"
package as="copy-bundle"
copy "/bin/busybox" "/copy-busybox" from="`+reference+`"
extend
copy "/from-busybox" "/from-busybox" from="from-bundle"
copy "/copy-busybox" "/copy-busybox" from="copy-bundle"
`)
	build := func(name string) (string, StoreOptions) {
		storeRoot := filepath.Join(root, name)
		store := StoreOptions{
			RunRoot: filepath.Join(storeRoot, "run"), GraphRoot: filepath.Join(storeRoot, "graph"), GraphDriverName: "vfs",
		}
		result, err := PublishDefinitionSupervised(ctx, component, planner.Options{
			Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		}, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output:        Output{Path: filepath.Join(storeRoot, "component")},
			ImageStoreDir: imageStoreDir, ComponentStoreDir: componentStoreDir, CacheLocalDir: cacheDir,
			PlainHTTPRegistries: []string{authority}, SignaturePolicyPath: policy,
			Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Root.Digest.String(), store
	}
	first, _ := build("first")
	second, fresh := build("second")
	if first != second {
		t.Fatalf("cached component digest changed: first=%s second=%s", first, second)
	}
	for _, path := range []string{fresh.RunRoot, fresh.GraphRoot} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("portable package-cache hit opened build store %s: %v", path, err)
		}
	}
}
