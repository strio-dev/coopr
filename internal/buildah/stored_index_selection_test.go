package buildah

import (
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
)

func TestStoredIndexMemberSelectionPrefersExactManifest(t *testing.T) {
	member := oci.Descriptor(v1.MediaTypeImageManifest, []byte("exact resident manifest"))
	transformed := oci.Descriptor(v1.MediaTypeImageManifest, []byte("transformed resident manifest"))
	selections := map[string]oci.StoredSelection{
		"origin": {ImageID: "transformed", Manifest: transformed, SourceManifest: &member},
		"exact":  {ImageID: "exact", Manifest: member},
	}
	for range 100 {
		selected, found := storedIndexMemberSelection(selections, member)
		if !found || selected.ImageID != "exact" {
			t.Fatalf("origin displaced exact resident selection: %+v, found=%t", selected, found)
		}
	}
	delete(selections, "exact")
	if selected, found := storedIndexMemberSelection(selections, member); !found || selected.ImageID != "transformed" {
		t.Fatalf("source descriptor did not select its runnable transformation: %+v, found=%t", selected, found)
	}
	mismatched := member
	mismatched.Size++
	if _, found := storedIndexMemberSelection(selections, mismatched); found {
		t.Fatal("accepted mismatched source descriptor")
	}
}
