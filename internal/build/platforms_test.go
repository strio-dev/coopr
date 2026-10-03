package build

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/imagecatalog"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestAllPlatformsDiscoversPartiallyCachedRegistryIndexOffline(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const reference = "registry.invalid/team/base:latest"
	rootData := []byte("remote-index-without-local-index-blob")
	root := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(rootData), Size: int64(len(rootData))}
	for _, arch := range []string{"amd64", "arm64"} {
		config := json.RawMessage(`{"os":"linux","architecture":"` + arch + `"}`)
		manifest := []byte("manifest-" + arch)
		selected := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}
		if err := imagecatalog.Commit(ctx, dir, reference, v1.Platform{OS: "linux", Architecture: arch}, imagecatalog.Selection{
			Root: root, Manifest: selected, ImageID: digest.FromBytes(config).Encoded(), ConfigData: config,
		}); err != nil {
			t.Fatal(err)
		}
	}
	def, err := definition.Parse(strings.NewReader("from \"" + reference + "\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := discoverBuildPlatforms(ctx, def, planner.Options{Mode: planner.Build}, Options{PullPolicy: "never"}, dir, "")
	if err != nil || !slices.Equal(got, []string{"linux/amd64", "linux/arm64"}) {
		t.Fatalf("offline cached index platforms = %v, %v", got, err)
	}
}
