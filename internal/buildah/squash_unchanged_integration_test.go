package buildah

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestSquashUnchangedAndMetadataOnlyRetainsBaseLayers(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := writeComponentTestPolicy(t, root)
	var baseline []string
	for index, tail := range []string{"", "label changed=\"yes\"\n"} {
		plan := testPlan(t, fmt.Sprintf("from %q\n%s", base.reference, tail))
		for _, squash := range []bool{false, true} {
			layout := filepath.Join(root, fmt.Sprintf("output-%d-%t", index, squash))
			_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: root, ImageStoreDir: base.imageStoreDir, SignaturePolicyPath: policy, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout, Squash: squash, DisableCompression: true}, Stdout: io.Discard, Stderr: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			manifest, _ := readPlanImage(t, layout)
			if baseline == nil {
				for _, layer := range manifest.Layers {
					baseline = append(baseline, layer.Digest.String())
				}
			}
			if len(manifest.Layers) != len(baseline) {
				t.Fatalf("metadata tail %d squash=%t layers=%d want %d", index, squash, len(manifest.Layers), len(baseline))
			}
			for i, layer := range manifest.Layers {
				if layer.Digest.String() != baseline[i] {
					t.Fatalf("metadata tail %d squash=%t base layer %d changed: %s want %s", index, squash, i, layer.Digest, baseline[i])
				}
			}
		}
	}
	var baselineDiffIDs []string
	for index, tail := range []string{"", "label changed=\"yes\"\n"} {
		plan := testPlan(t, fmt.Sprintf("from %q\n%s", base.reference, tail))
		for _, squash := range []bool{false, true} {
			layout := filepath.Join(root, fmt.Sprintf("gzip-output-%d-%t", index, squash))
			_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: root, ImageStoreDir: base.imageStoreDir, SignaturePolicyPath: policy, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout, Squash: squash}, Stdout: io.Discard, Stderr: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			manifest, image := readPlanImage(t, layout)
			if baselineDiffIDs == nil {
				for _, diffID := range image.RootFS.DiffIDs {
					baselineDiffIDs = append(baselineDiffIDs, diffID.String())
				}
			}
			if len(manifest.Layers) != len(baselineDiffIDs) || len(image.RootFS.DiffIDs) != len(baselineDiffIDs) {
				t.Fatalf("gzip metadata tail %d squash=%t layers=%d diffIDs=%d want %d", index, squash, len(manifest.Layers), len(image.RootFS.DiffIDs), len(baselineDiffIDs))
			}
			for i, diffID := range image.RootFS.DiffIDs {
				if diffID.String() != baselineDiffIDs[i] {
					t.Fatalf("gzip metadata tail %d squash=%t base diffID %d changed: %s want %s", index, squash, i, diffID, baselineDiffIDs[i])
				}
			}
		}
	}
}
