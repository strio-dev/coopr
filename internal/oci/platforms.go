package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"go/types"
	"slices"
	"strings"

	"github.com/containerd/platforms"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// ImagePlatforms inspects verified registry metadata without downloading image
// layers. Auxiliary artifacts are excluded from the runnable Linux platforms.
func (r *Resolver) ImagePlatforms(ctx context.Context, reference string) ([]v1.Platform, error) {
	source, root, _, _, err := r.openNativeImage(ctx, reference, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve image platforms for %s: %w", reference, err)
	}
	return imagePlatforms(ctx, source, root)
}

// LayoutImagePlatforms inspects an OCI layout using the same runnable-image
// validation as registry platform discovery.
func LayoutImagePlatforms(ctx context.Context, path, selector string) ([]v1.Platform, error) {
	source, err := orasoci.NewWithContext(ctx, path)
	if err != nil {
		return nil, err
	}
	root, err := source.Resolve(ctx, selector)
	if err != nil {
		return nil, err
	}
	return imagePlatforms(ctx, source, root)
}

func imagePlatforms(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor) ([]v1.Platform, error) {
	if root.MediaType != v1.MediaTypeImageIndex && root.MediaType != dockerIndexType {
		return singleImagePlatform(ctx, source, root)
	}
	data, err := fetchMetadata(ctx, source, root)
	if err != nil {
		return nil, err
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, err
	}
	if index.SchemaVersion != 2 || index.MediaType != "" && index.MediaType != root.MediaType {
		return nil, fmt.Errorf("invalid image index during platform discovery")
	}
	seen := map[string]bool{}
	result := []v1.Platform{}
	for _, manifest := range index.Manifests {
		if manifest.Platform == nil || manifest.Platform.OS != "linux" || manifest.Platform.Architecture == "" || manifest.Platform.Architecture == "unknown" || manifest.ArtifactType != "" {
			continue
		}
		if manifest.MediaType != v1.MediaTypeImageManifest && manifest.MediaType != dockerManifestType {
			continue
		}
		platform := platforms.Normalize(*manifest.Platform)
		if types.SizesFor("gc", platform.Architecture) == nil {
			continue
		}
		key := platforms.Format(platform)
		if seen[key] {
			return nil, fmt.Errorf("image index has duplicate platform %s", key)
		}
		// Inspect each selected child using the ordinary image admission path;
		// a platform label on an attestation is not proof of a runnable image.
		if _, err := (&Resolver{}).resolveRoot(ctx, source, "platform-discovery", "", manifest, platform, Image); err != nil {
			return nil, fmt.Errorf("inspect image platform %s: %w", key, err)
		}
		seen[key] = true
		result = append(result, platform)
	}
	slices.SortFunc(result, func(a, b v1.Platform) int {
		return strings.Compare(platforms.Format(a), platforms.Format(b))
	})
	if len(result) == 0 {
		return nil, fmt.Errorf("image has no runnable Linux platforms")
	}
	return result, nil
}

func singleImagePlatform(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor) ([]v1.Platform, error) {
	if root.MediaType != v1.MediaTypeImageManifest && root.MediaType != dockerManifestType {
		return nil, fmt.Errorf("platform discovery requires an image manifest or index")
	}
	data, err := fetchMetadata(ctx, source, root)
	if err != nil {
		return nil, err
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	data, err = fetchMetadata(ctx, source, manifest.Config)
	if err != nil {
		return nil, err
	}
	var image v1.Image
	if err := json.Unmarshal(data, &image); err != nil {
		return nil, err
	}
	platform := platforms.Normalize(image.Platform)
	if platform.OS != "linux" || types.SizesFor("gc", platform.Architecture) == nil {
		return nil, fmt.Errorf("image has no runnable Linux platform")
	}
	if _, err := (&Resolver{}).resolveRoot(ctx, source, "platform-discovery", "", root, platform, Image); err != nil {
		return nil, err
	}
	return []v1.Platform{platform}, nil
}
