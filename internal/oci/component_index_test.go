package oci

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/componentstore"
	"coopr/internal/definition"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestAssembleComponentIndexResolvesBothPlatforms(t *testing.T) {
	ctx := context.Background()
	def, err := definition.Parse(strings.NewReader("extend\nenv COMPONENT=multi\n"))
	if err != nil {
		t.Fatal(err)
	}
	variants := make([]IndexVariant, 0, 2)
	for _, arch := range []string{"arm64", "amd64"} {
		platform := v1.Platform{OS: "linux", Architecture: arch}
		publication, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: "linux/" + arch})
		if err != nil {
			t.Fatal(err)
		}
		layout := filepath.Join(t.TempDir(), arch)
		manifest, err := WriteComponentLayout(ctx, layout, ComponentMetadata{
			Version: ComponentVersion, Platform: platform, Component: *publication.Component,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		variants = append(variants, IndexVariant{Layout: layout, Manifest: manifest, Platform: platform})
	}
	output := filepath.Join(t.TempDir(), "index")
	root, data, err := AssembleComponentIndex(ctx, output, variants)
	if err != nil {
		t.Fatal(err)
	}
	if root.MediaType != v1.MediaTypeImageIndex || !sameDescriptor(root, Descriptor(v1.MediaTypeImageIndex, data)) {
		t.Fatalf("component index descriptor = %+v", root)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil || len(index.Manifests) != 2 || index.Manifests[0].Platform.Architecture != "arm64" {
		t.Fatalf("component index = %+v, %v", index, err)
	}
	storeDir := filepath.Join(t.TempDir(), "components")
	source, err := orasoci.NewWithContext(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range index.Manifests {
		if err := verifyArchiveGraph(ctx, source, child); err != nil {
			t.Fatalf("incomplete component graph %s: %v", child.Digest, err)
		}
		if _, err := content.FetchAll(ctx, source, child); err != nil {
			t.Fatal(err)
		}
	}
	if err := componentstore.Put(ctx, storeDir, source, root, "multi"); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(Options{ComponentStoreDir: storeDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		resolved, err := resolver.Resolve(ctx, "local:multi", v1.Platform{OS: "linux", Architecture: arch}, Component)
		if err != nil || resolved.Root.Digest != root.Digest || resolved.Component == nil || resolved.Component.Platform.Architecture != arch {
			t.Fatalf("resolve component for %s = %+v, %v", arch, resolved, err)
		}
	}
}
