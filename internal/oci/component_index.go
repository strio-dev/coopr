package oci

import (
	"context"
	"fmt"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// AssembleComponentIndex combines independently built component artifacts for
// several Linux platforms. Each child remains an ordinary component manifest,
// so existing component resolution selects and verifies it by platform.
func AssembleComponentIndex(ctx context.Context, outputPath string, variants []IndexVariant) (v1.Descriptor, []byte, error) {
	normalized, err := normalizeImageVariants(variants, false)
	if err != nil {
		return v1.Descriptor{}, nil, err
	}
	root, indexData, err := assembleIndexDescriptor(normalized, v1.MediaTypeImageIndex, false)
	if err != nil {
		return v1.Descriptor{}, nil, err
	}
	if err := validateImageIndex(root, indexData, normalized); err != nil {
		return v1.Descriptor{}, nil, err
	}
	if err := restoreIndexGraph(ctx, outputPath, root, indexData, normalized, "component", validateComponentVariant); err != nil {
		return v1.Descriptor{}, nil, err
	}
	return root, indexData, nil
}

func validateComponentVariant(ctx context.Context, variant IndexVariant) (*orasoci.Store, error) {
	if variant.Manifest.MediaType != v1.MediaTypeImageManifest {
		return nil, fmt.Errorf("component manifest has media type %q", variant.Manifest.MediaType)
	}
	_, layoutDescriptor, err := layoutRoot(variant.Layout)
	if err != nil {
		return nil, fmt.Errorf("read OCI layout root: %w", err)
	}
	if !sameDescriptor(layoutDescriptor, variant.Manifest) {
		return nil, fmt.Errorf("OCI layout root differs from component manifest")
	}
	source, err := orasoci.NewWithContext(ctx, variant.Layout)
	if err != nil {
		return nil, fmt.Errorf("open component OCI layout: %w", err)
	}
	if err := verifyArchiveGraph(ctx, source, variant.Manifest); err != nil {
		return nil, fmt.Errorf("invalid component graph: %w", err)
	}
	if _, err := (&Resolver{}).resolveRoot(ctx, source, variant.Manifest.Digest.String(), "layout", variant.Manifest, variant.Platform, Component); err != nil {
		return nil, fmt.Errorf("invalid component variant: %w", err)
	}
	return source, nil
}
