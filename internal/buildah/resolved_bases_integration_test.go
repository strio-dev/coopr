package buildah

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"coopr/internal/oci"
)

func TestBuildPlanUsesPinnedBaseWithoutResolvingTagAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah pinned-base test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	baseLayout := filepath.Join(root, "base")
	base, err := BuildPlan(ctx, testPlan(t, "from \"scratch\"\nenv BASE=\"before\"\n"), PlanOptions{
		Store: store, Isolation: "rootless", Output: Output{Path: baseLayout},
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := oci.ReadImageConfigLayout(ctx, baseLayout)
	if err != nil {
		t.Fatal(err)
	}
	reference := "registry.invalid/coopr/test:mutable"
	childLayout := filepath.Join(root, "child")
	_, err = BuildPlan(ctx, testPlan(t, "from \""+reference+"\"\nenv CHILD=\"after\"\n"), PlanOptions{
		Store: store, Isolation: "rootless", Output: Output{Path: childLayout},
		ResolvedBases: map[ResolvedBaseKey]ResolvedImageSource{
			{Reference: reference, Platform: "linux/amd64"}: {ImageID: base.ImageID, ConfigData: config},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, image := readPlanImage(t, childLayout)
	if !slices.Contains(image.Config.Env, "BASE=before") || !slices.Contains(image.Config.Env, "CHILD=after") {
		t.Fatalf("pinned base environment was not inherited: %v", image.Config.Env)
	}
}
