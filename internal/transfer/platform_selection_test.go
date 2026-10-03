package transfer

import (
	"context"
	"runtime"
	"testing"

	"coopr/internal/imagecatalog"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCopySelectsSoleForeignTagWithoutPlatform(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	native := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	foreign := v1.Platform{OS: "linux", Architecture: "arm64"}
	if native.Architecture == foreign.Architecture {
		foreign.Architecture = "amd64"
	}
	root := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromString("index"), Size: 5}
	foreignSelection := catalogSelection(root, foreign)
	if err := imagecatalog.Commit(ctx, storeDir, "app:latest", foreign, foreignSelection); err != nil {
		t.Fatal(err)
	}

	selected, platform, found, err := lookupStoredImage(ctx, storeDir, "app:latest", native, false)
	if err != nil || !found || selected.Manifest.Digest != foreignSelection.Manifest.Digest || platform.Architecture != foreign.Architecture {
		t.Fatalf("implicit foreign tag = %+v, %+v, found=%t, err=%v", selected, platform, found, err)
	}
	if _, _, found, err := lookupStoredImage(ctx, storeDir, "app:latest", native, true); err != nil || found {
		t.Fatalf("explicit native selection found foreign tag: found=%t, err=%v", found, err)
	}
	if _, platform, found, err := lookupStoredImage(ctx, storeDir, root.Digest.String(), native, false); err != nil || found {
		t.Fatalf("index digest should still require native selection: platform=%+v, found=%t, err=%v", platform, found, err)
	}
	if selected, platform, found, err := lookupStoredImage(ctx, storeDir, foreignSelection.Manifest.Digest.String(), native, false); err != nil || !found || selected.Manifest.Digest != foreignSelection.Manifest.Digest || platform.Architecture != foreign.Architecture {
		t.Fatalf("exact manifest digest = %+v, %+v, found=%t, err=%v", selected, platform, found, err)
	}

	other := v1.Platform{OS: "linux", Architecture: "ppc64le"}
	if other.Architecture == native.Architecture || other.Architecture == foreign.Architecture {
		other.Architecture = "s390x"
	}
	if err := imagecatalog.Commit(ctx, storeDir, "app:latest", other, catalogSelection(root, other)); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := lookupStoredImage(ctx, storeDir, "app:latest", native, false); err != nil || found {
		t.Fatalf("multi-platform tag without native selection: found=%t, err=%v", found, err)
	}

	nativeSelection := catalogSelection(root, native)
	if err := imagecatalog.Commit(ctx, storeDir, "app:latest", native, nativeSelection); err != nil {
		t.Fatal(err)
	}
	selected, platform, found, err = lookupStoredImage(ctx, storeDir, "app:latest", native, false)
	if err != nil || !found || selected.Manifest.Digest != nativeSelection.Manifest.Digest || platform.Architecture != native.Architecture {
		t.Fatalf("multi-platform tag native default = %+v, %+v, found=%t, err=%v", selected, platform, found, err)
	}
}

func catalogSelection(root v1.Descriptor, platform v1.Platform) imagecatalog.Selection {
	config := []byte(`{"os":"linux","architecture":"` + platform.Architecture + `"}`)
	return imagecatalog.Selection{
		Root:       root,
		Manifest:   v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("manifest:" + platform.Architecture), Size: 1},
		ImageID:    digest.FromBytes(config).Encoded(),
		ConfigData: config,
	}
}
