package oci

import (
	"context"
	"encoding/json"
	"testing"

	"oras.land/oras-go/v2/content/oci"
)

func TestReplaceImageAnnotationsLayout(t *testing.T) {
	fixture := newConfigRewriteFixture(t)
	updated, err := ReplaceImageAnnotationsLayout(context.Background(), fixture.layout, false, []string{"new=value", "empty="}, []string{"vendor.manifest"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Digest == fixture.oldManifest.Digest {
		t.Fatal("annotation update did not change manifest digest")
	}
	_, root, err := layoutRoot(fixture.layout)
	if err != nil {
		t.Fatal(err)
	}
	if root.Digest != updated.Digest {
		t.Fatalf("layout root = %s, want %s", root.Digest, updated.Digest)
	}
	store, err := oci.New(fixture.layout)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readLayerMetadata(context.Background(), store, root)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Annotations) != 2 || manifest.Annotations["new"] != "value" || manifest.Annotations["empty"] != "" {
		t.Fatalf("annotations = %#v", manifest.Annotations)
	}
}
