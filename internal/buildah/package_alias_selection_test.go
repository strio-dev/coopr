package buildah

import (
	"context"
	"go.podman.io/image/v5/types"
	orasoci "oras.land/oras-go/v2/content/oci"
	"runtime"
	"testing"

	"coopr/internal/oci"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPackageSelectionKeepsExternalSourceBeforeLaterAlias(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	platform := v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	platformString := runtime.GOOS + "/" + runtime.GOARCH
	store := cacheTestStore(root)
	sourceDir := t.TempDir()
	source, err := orasoci.NewWithContext(ctx, sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := sourceTestImage(t, ctx, source, platform, "base")
	lease, err := acquireStore(store)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ImportSelectedImage(ctx, lease.store, &types.SystemContext{BigFilesTemporaryDir: t.TempDir()}, sourceDir, manifest)
	if err != nil {
		_ = lease.Close()
		t.Fatal(err)
	}
	if err := lease.store.AddNames(id, []string{"localhost/base:latest"}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{NativeStore: NativeStoreOptions(store), PullPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	stages := []planner.Stage{
		{ID: "0", Kind: "from", Name: "producer", Source: "base", Platform: platformString},
		{ID: "1", Kind: "from", Name: "base", Source: "scratch", Platform: platformString},
	}
	selected, err := selectPublicationBases(ctx, PlanOptions{Resolver: resolver}, stages)
	if err != nil {
		t.Fatal(err)
	}
	key := ResolvedBaseKey{Reference: "base", Platform: platformString}
	if selected[key].Selected.Digest != manifest.Digest {
		t.Fatalf("external base before later alias was not selected: %+v", selected)
	}
	bases, err := resolvePackageClosureBases(stages, selected)
	if err != nil {
		t.Fatal(err)
	}
	if len(bases) != 1 {
		t.Fatalf("package key omitted earlier external base: %+v", bases)
	}
}
