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

	"github.com/strio-dev/coopr/internal/oci"
)

func TestInstructionGraphComponentAggregateBoundaryAcrossFreshStores(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	base, _ := newLiveBusyBoxRegistry(t, ctx)
	_ = localConfigComponentResolver(t, ctx, root, `extend {
    run "printf module-data > /module" network="none"
}
`)
	policy := writeComponentTestPolicy(t, root)
	build := func(name, project, variant string) (string, string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(contextDir, "project"), []byte(project), 0o600); err != nil {
			t.Fatal(err)
		}
		command := fmt.Sprintf("printf %s > /proof; od -An -N16 -tx1 /dev/urandom >> /proof; cat /module /project >> /proof", variant)
		plan := testPlan(t, fmt.Sprintf(`from %q {
    component "config"
    copy "project" "/project"
    run %q network="none"
}
`, base, command))
		lane := filepath.Join(root, name)
		layout := filepath.Join(lane, "layout")
		var progress strings.Builder
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: cacheTestStore(lane), ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
			Output:            Output{Path: layout, Format: "oci", CompressionFormat: "gzip"},
			ComponentStoreDir: filepath.Join(root, "components"), CacheLocalDir: filepath.Join(root, "cache"),
			SignaturePolicyPath: policy, TLSVerify: new(false), Stdout: io.Discard, Stderr: &progress,
		})
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, progress.String())
		}
		if result.CacheStats.Errors != 0 {
			t.Fatalf("%s cache errors: %+v\n%s", name, result.CacheStats, progress.String())
		}
		selected, err := oci.LayoutRoot(layout)
		if err != nil || selected.Digest.String() != result.ManifestDigest {
			t.Fatalf("%s selected manifest is incoherent: %s / %s, %v", name, selected.Digest, result.ManifestDigest, err)
		}
		return localComponentLastFile(t, layout, "proof"), progress.String()
	}
	cold, _ := build("cold", "project-one", "first")
	warm, progress := build("warm", "project-one", "first")
	if !strings.Contains(progress, "Using component cache") {
		t.Fatalf("fresh-store warm build did not use aggregate component cache:\n%s", progress)
	}
	if warm != cold {
		t.Fatalf("aggregate hit changed downstream identity and reexecuted RUN: cold=%q warm=%q\n%s", cold, warm, progress)
	}
	// A changed continuation must export the imported component source again.
	// This exercises exact compressed-manifest retention, beyond a cache hit.
	changed, progress := build("changed", "project-two", "second")
	if !strings.Contains(progress, "Using component cache") || changed == cold || !strings.Contains(changed, "module-dataproject-two") {
		t.Fatalf("changed continuation did not use the current inputs: %q\n%s", changed, progress)
	}
	warmChanged, progress := build("changed-warm", "project-two", "second")
	if !strings.Contains(progress, "Using component cache") || warmChanged != changed {
		t.Fatalf("new continuation was not retained coherently: changed=%q warm=%q\n%s", changed, warmChanged, progress)
	}
}
