package oci

import (
	"fmt"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// LayoutRoot reads the sole manifest descriptor from a completed OCI layout.
func LayoutRoot(path string) (v1.Descriptor, error) {
	_, root, err := layoutRoot(path)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if root.MediaType != v1.MediaTypeImageManifest && root.MediaType != v1.MediaTypeImageIndex && root.MediaType != dockerManifestType && root.MediaType != dockerIndexType {
		return v1.Descriptor{}, fmt.Errorf("unsupported OCI layout root media type %q", root.MediaType)
	}
	if err := checkDescriptor(root); err != nil {
		return v1.Descriptor{}, err
	}
	return root, nil
}
