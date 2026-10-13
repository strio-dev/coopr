package transfer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/oci"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestNativePushTransports(t *testing.T) {
	for _, value := range []string{"dir:/tmp/image", "oci:/tmp/layout:app", "docker-archive:/tmp/image.tar:example.com/app:dev"} {
		t.Run(value, func(t *testing.T) {
			got, err := ParsePushDestination(value, oci.Image)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != value {
				t.Fatalf("got %s", got.String())
			}
			if _, err := ParsePushDestination(value, oci.Component); err == nil {
				t.Fatal("component transport accepted")
			}
		})
	}
}

func TestLiteralEngineLikeStoredSource(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	selected := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "literal")
	if err := nameNativeTestImage(ctx, store, "docker-daemon:dev", selected); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, oci.Image, "docker-daemon:dev", Destination{Transport: "local", Name: "literal:latest"}, Options{BuildStore: store}); err != nil {
		t.Fatal(err)
	}
}

func TestNativePushTransportOutputs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	selected := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "push")
	if err := nameNativeTestImage(ctx, store, "push:latest", selected); err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"dir", "oci", "docker-archive"} {
		t.Run(transport, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "output")
			value := transport + ":" + path
			if transport == "oci" {
				value += ":app"
			}
			if transport == "docker-archive" {
				value += ":example.com/app:dev"
			}
			dst, err := ParsePushDestination(value, oci.Image)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Copy(ctx, oci.Image, "push:latest", dst, Options{BuildStore: store}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
			ref, err := alltransports.ParseImageName(dst.String())
			if err != nil {
				t.Fatal(err)
			}
			src, err := ref.NewImageSource(ctx, &types.SystemContext{})
			if err != nil {
				t.Fatal(err)
			}
			raw, _, err := src.GetManifest(ctx, nil)
			closeErr := src.Close()
			if err != nil {
				t.Fatal(err)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			var output v1.Manifest
			if err := json.Unmarshal(raw, &output); err != nil {
				t.Fatal(err)
			}
			var input v1.Manifest
			err = buildah.WithStore(store, func(backend storage.Store) error {
				data, err := backend.ImageBigData(selected.ImageID, storage.ImageDigestBigDataKey)
				if err != nil {
					return err
				}
				return json.Unmarshal(data, &input)
			})
			if err != nil {
				t.Fatal(err)
			}
			if output.Config.Digest != input.Config.Digest {
				t.Fatal("output config changed")
			}
			if transport == "docker-archive" {
				if _, err := buildah.LoadImages(ctx, nativeTestStore(t.TempDir()), path, oci.Options{}, nil); err != nil {
					t.Fatal(err)
				}
			}

		})
	}
}

func TestNativePushFilesystemUsesNativeDestinationPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	selected := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "protected")
	if err := nameNativeTestImage(ctx, store, "protected:latest", selected); err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"dir", "oci", "docker-archive"} {
		for _, root := range []string{store.GraphRoot, store.RunRoot} {
			value := transport + ":" + filepath.Join(root, "export-"+transport)
			if transport != "dir" {
				value += ":example.com/app:dev"
			}
			dst, err := ParsePushDestination(value, oci.Image)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Copy(ctx, oci.Image, "protected:latest", dst, Options{BuildStore: store}); err != nil {
				t.Fatalf("native output destination %s: %v", value, err)
			}
		}
	}
}

func TestNativePushForeignContainerStorage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	foreign := nativeTestStore(t.TempDir())
	selected := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "storage")
	if err := nameNativeTestImage(ctx, store, "source:latest", selected); err != nil {
		t.Fatal(err)
	}
	dst, err := ParsePushDestination("containers-storage:[vfs@"+foreign.GraphRoot+"+"+foreign.RunRoot+"]localhost/output:dev", oci.Image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, oci.Image, "source:latest", dst, Options{BuildStore: store}); err != nil {
		t.Fatal(err)
	}
	err = buildah.WithStore(foreign, func(backend storage.Store) error {
		_, err := backend.Image("localhost/output:dev")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativePushCompleteIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	selections := map[string]oci.StoredSelection{}
	var members []v1.Descriptor
	for _, arch := range []string{"amd64", "arm64"} {
		platform := v1.Platform{OS: "linux", Architecture: arch}
		selected := nativeEmptyImageFixture(t, store, platform, arch)
		selections["linux/"+arch] = selected
		member := selected.Manifest
		member.Platform = &platform
		members = append(members, member)
	}
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: members, Annotations: map[string]string{"fixture": "complete"}})
	if err != nil {
		t.Fatal(err)
	}
	root := oci.Descriptor(v1.MediaTypeImageIndex, data)
	if err := nativeTestIndex(ctx, store, "index:latest", root, data, selections); err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"dir", "oci", "docker-archive"} {
		t.Run(transport, func(t *testing.T) {
			value := transport + ":" + filepath.Join(t.TempDir(), "output")
			if transport == "oci" {
				value += ":app"
			}
			dst, err := ParsePushDestination(value, oci.Image)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Copy(ctx, oci.Image, "index:latest", dst, Options{BuildStore: store})
			if transport == "docker-archive" {
				if err == nil {
					t.Fatal("Docker archive silently flattened complete index")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ref, err := alltransports.ParseImageName(dst.String())
			if err != nil {
				t.Fatal(err)
			}
			src, err := ref.NewImageSource(ctx, &types.SystemContext{})
			if err != nil {
				t.Fatal(err)
			}
			raw, _, err := src.GetManifest(ctx, nil)
			closeErr := src.Close()
			if err != nil {
				t.Fatal(err)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			var index v1.Index
			if err := json.Unmarshal(raw, &index); err != nil {
				t.Fatal(err)
			}
			if len(index.Manifests) != 2 || index.Annotations["fixture"] != "complete" {
				t.Fatalf("index metadata lost: %s", raw)
			}
		})
	}

}

func TestExplicitContainerStorageSource(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	selected := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "source")
	if err := nameNativeTestImage(ctx, store, "source:latest", selected); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, oci.Image, "containers-storage:localhost/source:latest", Destination{Transport: "local", Name: "alias:latest"}, Options{BuildStore: store}); err != nil {
		t.Fatal(err)
	}
}

func TestContainerStorageDestinationPreservesImplicitStore(t *testing.T) {
	dst, err := ParsePushDestination("containers-storage:localhost/app:dev", oci.Image)
	if err != nil {
		t.Fatal(err)
	}
	if dst.Name != "localhost/app:dev" || dst.String() != "containers-storage:localhost/app:dev" {
		t.Fatalf("parser selected a store: %+v", dst)
	}
}

func TestContainerStorageSourceRetainsPinnedImageID(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	old := nativeEmptyImageFixture(t, store, platform, "old")
	current := nativeEmptyImageFixture(t, store, platform, "current")
	if err := nameNativeTestImage(ctx, store, "source:old", old); err != nil {
		t.Fatal(err)
	}
	for _, selection := range []oci.StoredSelection{old, current} {
		if err := nameNativeTestImage(ctx, store, "source:latest", selection); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Copy(ctx, oci.Image, "containers-storage:localhost/source:latest@"+old.ImageID, Destination{Transport: "local", Name: "pinned:latest"}, Options{BuildStore: store}); err != nil {
		t.Fatal(err)
	}
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		image, err := backend.Image("localhost/pinned:latest")
		if err != nil {
			return err
		}
		if image.ID != old.ImageID {
			t.Fatalf("copied retargeted image %s", image.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	foreign := nativeTestStore(t.TempDir())
	other := nativeEmptyImageFixture(t, foreign, platform, "foreign")
	if err := nameNativeTestImage(ctx, foreign, "source:latest", other); err != nil {
		t.Fatal(err)
	}
	source := "containers-storage:[vfs@" + foreign.GraphRoot + "+" + foreign.RunRoot + "]localhost/source:latest"
	if _, err := Copy(ctx, oci.Image, source, Destination{Transport: "local", Name: "foreign:latest"}, Options{BuildStore: store}); err == nil {
		t.Fatal("foreign source silently read current store")
	}
}
