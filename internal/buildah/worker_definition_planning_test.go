package buildah

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestWorkerBuildPlanningSelectsBaseAndFileMetadataWithoutImport(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	root := t.TempDir()
	reference, authority := newLiveBusyBoxRegistry(t, ctx)
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	expected, err := resolver.ResolveRemoteImage(ctx, reference, platform)
	if err != nil {
		t.Fatal(err)
	}
	fileReference := authority + "/coopr/busybox@" + expected.Selected.Digest.String()
	definition := parseWorkerDefinition(t, fmt.Sprintf(`from %q {
    copy "/bin/busybox" "/busybox" from=%q
}`, reference, fileReference))
	planning := planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}
	store := cacheTestStore(filepath.Join(root, "planning"))
	_, _, selected, err := planDefinitionInWorker(ctx, planWorkerRequest{
		workerExecutionOptions: workerExecutionOptions{Store: store, ContextDir: root, TLSVerify: new(false)},
		Mode:                   "build", Definition: definition, PlannerOptions: &planning,
		ResultPath: filepath.Join(root, "result.json"), SignaturePolicyPath: writeComponentTestPolicy(t, root), RetrySet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{reference, fileReference} {
		selection := selected.all[ResolvedBaseKey{Reference: reference, Platform: planning.Platform}]
		if !selection.Remote || selection.Artifact || selection.Selected.Digest != expected.Selected.Digest || !bytes.Equal(selection.ConfigData, expected.ConfigData) {
			t.Fatalf("planning failed to retain exact remote metadata for %s: %+v", reference, selection)
		}
	}
	lease, err := acquireStore(store)
	if err != nil {
		t.Fatal(err)
	}
	images, imageErr := lease.store.Images()
	containers, containerErr := lease.store.Containers()
	if err := errors.Join(imageErr, containerErr, lease.Close()); err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 || len(containers) != 0 {
		t.Fatalf("metadata planning materialized images=%d containers=%d", len(images), len(containers))
	}
}
