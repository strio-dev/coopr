package imagestore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	ocilayout "go.podman.io/image/v5/oci/layout"
	imagestorage "go.podman.io/image/v5/storage"
)

// ReadLayout copies an engine-owned image into an OCI layout without changing
// its engine name. An empty platform retains every available index instance.
func (s *Store) ReadLayout(ctx context.Context, name, layout string, platform v1.Platform) (v1.Descriptor, error) {
	if s == nil || s.backend == nil {
		return v1.Descriptor{}, errors.New("image store is not initialized")
	}
	selector := name
	if pinned := digest.Digest(name); pinned.Validate() == nil {
		selector = pinned.Encoded()
	}
	image, err := s.backend.Image(selector)
	if err != nil {
		normalized, normalizeErr := NormalizeTag(name)
		if normalizeErr != nil {
			return v1.Descriptor{}, fmt.Errorf("resolve engine image %q: %w", name, err)
		}
		image, err = s.backend.Image(normalized)
	}
	if err != nil {
		return v1.Descriptor{}, fmt.Errorf("resolve engine image %q: %w", name, err)
	}
	source, err := imagestorage.Transport.NewStoreReference(s.backend, nil, image.ID)
	if err != nil {
		return v1.Descriptor{}, err
	}
	target, err := ocilayout.NewReference(layout, "")
	if err != nil {
		return v1.Descriptor{}, err
	}
	system := *s.system
	selection := imagecopy.CopyAllImages
	if platform.Architecture != "" {
		system.OSChoice, system.ArchitectureChoice, system.VariantChoice = platform.OS, platform.Architecture, platform.Variant
		selection = imagecopy.CopySystemImage
	}
	if err := CopyImages(ctx, &system, source, target, true, selection); err != nil {
		return v1.Descriptor{}, fmt.Errorf("read Podman image %s: %w", strings.TrimSpace(name), err)
	}
	return oci.LayoutRoot(layout)
}
