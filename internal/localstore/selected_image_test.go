package localstore

import (
	"context"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

type unvisitedImageSource struct{ content.ReadOnlyStorage }

func TestCopySelectedImageRejectsMalformedDigestsBeforeCopy(t *testing.T) {
	valid := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("selected"), Size: 8}
	for _, value := range []string{"", "sha256", "malformed", "sha256:", "sha256:bad"} {
		t.Run(value, func(t *testing.T) {
			bad := valid
			bad.Digest = digest.Digest(value)
			for _, test := range []struct {
				name           string
				root, selected v1.Descriptor
				want           string
			}{
				{"root", bad, valid, "invalid image root"},
				{"selected", valid, bad, "invalid selected image manifest"},
			} {
				t.Run(test.name, func(t *testing.T) {
					err := CopySelectedImage(context.Background(), t.TempDir(), unvisitedImageSource{}, test.root, test.selected)
					if err == nil || !strings.Contains(err.Error(), test.want) {
						t.Fatalf("error = %v, want %q", err, test.want)
					}
				})
			}
		})
	}
}
