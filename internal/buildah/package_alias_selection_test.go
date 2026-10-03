package buildah

import (
	"context"
	"runtime"
	"testing"

	"coopr/internal/imagecatalog"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPackageSelectionKeepsExternalSourceBeforeLaterAlias(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	platform := v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	platformString := runtime.GOOS + "/" + runtime.GOARCH
	manifest := oci.Descriptor(v1.MediaTypeImageManifest, []byte("selected base manifest"))
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	if err := imagecatalog.Commit(ctx, root, "base:latest", platform, imagecatalog.Selection{
		Root: manifest, Manifest: manifest, ImageID: digest.FromBytes(config).Encoded(), ConfigData: config,
	}); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{ImageStoreDir: root})
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
