package oci

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"coopr/internal/componentstore"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestComponentPullSaveLoadPreservesEveryPlatform(t *testing.T) {
	ctx := context.Background()
	_, _, publisher, remote := testRepository(t, registry.New())
	var variants []IndexVariant
	for _, arch := range []string{"amd64", "arm64"} {
		meta, paths := publicationMetadata(t, packageTar(t, arch), true)
		meta.Platform.Architecture = arch
		meta.Component.Platform = "linux/" + arch
		meta.Packages[0].Config = packageImageConfig(t, meta.Platform, packageTar(t, arch))
		layout := filepath.Join(t.TempDir(), "component")
		root, err := WriteComponentLayout(ctx, layout, meta, paths)
		if err != nil {
			t.Fatal(err)
		}
		variants = append(variants, IndexVariant{Layout: layout, Manifest: root, Platform: meta.Platform})
	}
	layout := filepath.Join(t.TempDir(), "index")
	root, _, err := AssembleComponentIndex(ctx, layout, variants)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishLayout(ctx, remote+":multi", layout, root); err != nil {
		t.Fatal(err)
	}
	pull, err := NewResolver(Options{TLSVerify: new(false), ComponentStoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := pull.PullComponent(ctx, remote+":multi", "shared")
	if err != nil || got.Digest != root.Digest {
		t.Fatalf("pull root=%s err=%v", got.Digest, err)
	}
	store, err := componentstore.Open(ctx, pull.ComponentStoreDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "component.tar")
	if err := componentstore.WriteArchive(ctx, store, root, archive); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewResolver(Options{ComponentStoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got, err = loaded.LoadComponent(ctx, archive, "restored")
	if err != nil || got.Digest != root.Digest {
		t.Fatalf("load root=%s err=%v", got.Digest, err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		resolved, err := loaded.Resolve(ctx, "local:restored", v1.Platform{OS: "linux", Architecture: arch}, Component)
		if err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(t.TempDir(), "package.tar")
		if err := loaded.Download(ctx, resolved, resolved.Layers[0], output); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(output)
		if err != nil || !bytes.Equal(data, packageTar(t, arch)) {
			t.Fatalf("package %s changed: %v", arch, err)
		}
	}
}

func TestComponentLoadRejectsImageBeforeMovingTag(t *testing.T) {
	ctx := context.Background()
	resolver, err := NewResolver(Options{ComponentStoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	meta, paths := publicationMetadata(t, nil, false)
	layout := filepath.Join(t.TempDir(), "valid")
	root, err := WriteComponentLayout(ctx, layout, meta, paths)
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.storeComponent(ctx, source, root, "stable"); err != nil {
		t.Fatal(err)
	}
	imageLayout, imageRoot := imageIndexTestLayout(t, ctx, v1.Platform{OS: "linux", Architecture: "amd64"}, "ordinary-image", false)
	imageSource, err := orasoci.New(imageLayout)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "image.tar")
	if err := componentstore.WriteArchive(ctx, imageSource, imageRoot, archive); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.LoadComponent(ctx, archive, "stable"); err == nil {
		t.Fatal("image loaded as component")
	}
	stored, err := componentstore.Open(ctx, resolver.ComponentStoreDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := stored.Resolve(ctx, "stable")
	if err != nil || got.Digest != root.Digest {
		t.Fatalf("failed load moved tag: %s %v", got.Digest, err)
	}
}
