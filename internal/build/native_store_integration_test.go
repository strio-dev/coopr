package build

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/transfer"
	"go.podman.io/storage"
)

func TestBuildAndCopyUseExplicitNativeStoreOptions(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live native-store coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	definition := filepath.Join(root, "image.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\"\nenv STORED=\"yes\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(root, "catalog")
	store := buildah.StoreOptions{
		RunRoot: filepath.Join(root, "runtime"), GraphRoot: filepath.Join(root, "arbitrary-native-root"),
		ImageStore: filepath.Join(root, "split-images"), GraphDriverName: "vfs",
		GraphDriverOptions: []string{"vfs.ignore_chown_errors=true"}, TransientStore: true,
	}
	name := "explicit-store:latest"
	if _, err := Run(ctx, Options{
		File: definition, Context: root, StoreDir: catalog, BuildStore: store,
		Platform: "linux/" + runtime.GOARCH, Tag: name,
	}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "image.tar")
	if _, err := transfer.Copy(ctx, oci.Image, name, transfer.Destination{Transport: "oci-archive", Name: archive}, transfer.Options{
		ImageStoreDir: catalog, BuildStore: store,
	}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
		t.Fatalf("exported archive: info=%v err=%v", info, err)
	}
	backend, err := storage.GetStore(buildah.NativeStoreOptions(store))
	if err != nil {
		t.Fatal(err)
	}
	images, err := backend.Images()
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range images {
		if _, err := backend.DeleteImage(image.ID, true); err != nil {
			t.Errorf("delete test image %s: %v", image.ID, err)
		}
	}
	if _, err := backend.Shutdown(true); err != nil {
		t.Errorf("shutdown test store: %v", err)
	}
}
