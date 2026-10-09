package build

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"coopr/internal/localstore"
)

func TestAllPlatformsDiscoversLocalBasesAndIntersectsPlatforms(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	ctx := context.Background()
	workspace := dockerEngineWorkspace(t)
	storeDir := filepath.Join(workspace, "images")
	base := filepath.Join(workspace, "base.coopr")
	consumer := filepath.Join(workspace, "consumer.coopr")
	if err := os.WriteFile(base, []byte("from \"scratch\"\nlabel purpose=\"discovery\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, Options{File: base, BuildStore: nativeBuildTestStore(storeDir), Tag: "base", Platforms: []string{"linux/amd64", "linux/arm64"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(consumer, []byte("arg \"base\"\nfrom \"$base\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := Options{File: consumer, BuildStore: nativeBuildTestStore(storeDir), Tag: "discovered", AllPlatforms: true, PullPolicy: "never", Args: map[string]string{"base": "base"}}
	if _, err := Run(ctx, opts); err != nil {
		t.Fatal(err)
	}
	name, err := localstore.NormalizeImageTag("discovered")
	if err != nil {
		t.Fatal(err)
	}
	_, _, selections, found, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), name)
	if err != nil || !found || len(selections) != 2 {
		t.Fatalf("discovered index found=%v platforms=%v err=%v", found, selections, err)
	}
	if _, err := Run(ctx, Options{File: base, BuildStore: nativeBuildTestStore(storeDir), Tag: "single", Platform: "linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(consumer, []byte("from \"base\" as=\"multi\"\nfrom \"single\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts.Tag = "intersection"
	if _, err := Run(ctx, opts); err != nil {
		t.Fatal(err)
	}
	name, err = localstore.NormalizeImageTag("intersection")
	if err != nil {
		t.Fatal(err)
	}
	_, platform, found, err := testStoredSoleImage(ctx, nativeBuildTestStore(storeDir), name)
	if err != nil || !found || platform.Architecture != "amd64" {
		t.Fatalf("common platform=%+v found=%v err=%v", platform, found, err)
	}
	if _, err := Run(ctx, Options{File: base, BuildStore: nativeBuildTestStore(storeDir), AllPlatforms: true}); err == nil || !strings.Contains(err.Error(), "non-scratch") {
		t.Fatalf("scratch-only discovery error=%v", err)
	}
}

func TestManifestAppendsConcurrentPlatformsAndReplacesExistingPlatform(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	ctx := context.Background()
	workspace := dockerEngineWorkspace(t)
	storeDir := filepath.Join(workspace, "images")
	definition := filepath.Join(workspace, "image.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\"\narg \"revision\"\nlabel revision=\"$revision\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Run(ctx, Options{File: definition, BuildStore: nativeBuildTestStore(storeDir), Manifest: map[string]string{"linux/amd64": "combined", "linux/arm64": "localhost/combined:latest"}[platform], Platform: platform, Args: map[string]string{"revision": "first"}})
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	name, err := localstore.NormalizeImageTag("combined")
	if err != nil {
		t.Fatal(err)
	}
	_, _, previous, found, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), name)
	if err != nil || !found || len(previous) != 2 {
		t.Fatalf("concurrent manifest found=%v platforms=%v err=%v", found, previous, err)
	}
	if _, err := Run(ctx, Options{File: definition, BuildStore: nativeBuildTestStore(storeDir), Manifest: "combined", Platform: "linux/amd64", Args: map[string]string{"revision": "replacement"}}); err != nil {
		t.Fatal(err)
	}
	_, _, replaced, found, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), name)
	if err != nil || !found || len(replaced) != 2 {
		t.Fatalf("replacement manifest found=%v platforms=%v err=%v", found, replaced, err)
	}
	if previous["linux/amd64"].Manifest.Digest == replaced["linux/amd64"].Manifest.Digest || previous["linux/arm64"].Manifest.Digest != replaced["linux/arm64"].Manifest.Digest {
		t.Fatal("replacement changed the wrong manifest instances")
	}
	consumer := filepath.Join(workspace, "consumer.coopr")
	if err := os.WriteFile(consumer, []byte("from \"combined\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, Options{File: consumer, BuildStore: nativeBuildTestStore(storeDir), AllPlatforms: true, PullPolicy: "never", Tag: "consumed"}); err != nil {
		t.Fatalf("consume appended manifest offline: %v", err)
	}
}
