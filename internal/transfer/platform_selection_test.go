package transfer

import (
	"context"
	"runtime"
	"testing"

	"coopr/internal/oci"
	"github.com/containerd/platforms"
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
	store := nativeTestStore(storeDir)
	foreignSelection := nativeEmptyImageFixture(t, store, foreign, "foreign")
	if err := nameNativeTestImage(ctx, store, "app:latest", foreignSelection); err != nil {
		t.Fatal(err)
	}

	selected, platform, found, err := lookupStoredImage(ctx, Options{BuildStore: nativeTestStore(storeDir)}, "app:latest", native, false)
	if err != nil || !found || selected.Manifest.Digest != foreignSelection.Manifest.Digest || platform.Architecture != foreign.Architecture {
		t.Fatalf("implicit foreign tag = %+v, %+v, found=%t, err=%v", selected, platform, found, err)
	}
	if _, _, found, err := lookupStoredImage(ctx, Options{BuildStore: nativeTestStore(storeDir)}, "app:latest", native, true); err != nil || found {
		t.Fatalf("explicit native selection found foreign tag: found=%t, err=%v", found, err)
	}
	if selected, platform, found, err := lookupStoredImage(ctx, Options{BuildStore: nativeTestStore(storeDir)}, foreignSelection.Manifest.Digest.String(), native, false); err != nil || !found || selected.Manifest.Digest != foreignSelection.Manifest.Digest || platform.Architecture != foreign.Architecture {
		t.Fatalf("exact manifest digest = %+v, %+v, found=%t, err=%v", selected, platform, found, err)
	}

	other := v1.Platform{OS: "linux", Architecture: "ppc64le"}
	if other.Architecture == native.Architecture || other.Architecture == foreign.Architecture {
		other.Architecture = "s390x"
	}
	otherSelection := nativeEmptyImageFixture(t, store, other, "other")
	makeIndex := func(selected []oci.StoredSelection, members []v1.Platform) v1.Descriptor {
		t.Helper()
		variants := make([]oci.ImageVariant, len(selected))
		selections := map[string]oci.StoredSelection{}
		for i, selection := range selected {
			variants[i] = oci.ImageVariant{Manifest: selection.Manifest, Platform: members[i]}
			selections[platforms.Format(members[i])] = selection
		}
		root, data, err := oci.ImageIndexDescriptor(variants, "oci")
		if err != nil {
			t.Fatal(err)
		}
		if err := nativeTestIndex(ctx, store, "app:latest", root, data, selections); err != nil {
			t.Fatal(err)
		}
		return root
	}
	root := makeIndex([]oci.StoredSelection{foreignSelection, otherSelection}, []v1.Platform{foreign, other})
	if _, _, found, err := lookupStoredImage(ctx, Options{BuildStore: store}, "app:latest", native, false); err != nil || found {
		t.Fatalf("multi-platform tag missing native found=%v err=%v", found, err)
	}
	if _, _, found, err := lookupStoredImage(ctx, Options{BuildStore: store}, root.Digest.String(), native, false); err != nil || found {
		t.Fatalf("index digest missing native found=%v err=%v", found, err)
	}
	nativeSelection := nativeEmptyImageFixture(t, store, native, "native")
	makeIndex([]oci.StoredSelection{foreignSelection, otherSelection, nativeSelection}, []v1.Platform{foreign, other, native})

	selected, platform, found, err = lookupStoredImage(ctx, Options{BuildStore: nativeTestStore(storeDir)}, "app:latest", native, false)
	if err != nil || !found || selected.Manifest.Digest != nativeSelection.Manifest.Digest || platform.Architecture != native.Architecture {
		t.Fatalf("multi-platform tag native default = %+v, %+v, found=%t, err=%v", selected, platform, found, err)
	}
}
