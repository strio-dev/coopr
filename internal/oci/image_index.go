package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// IndexVariant identifies a complete single-platform graph and the platform
// under which its manifest will appear in an OCI index.
type IndexVariant struct {
	Layout   string
	Manifest v1.Descriptor
	Platform v1.Platform
}

type ImageVariant = IndexVariant

// ImageIndexDescriptor assembles validated platform metadata without copying
// image layers or creating a filesystem layout.
func ImageIndexDescriptor(variants []ImageVariant, format string) (v1.Descriptor, []byte, error) {
	mediaType, err := imageIndexMediaType(format)
	if err != nil {
		return v1.Descriptor{}, nil, err
	}
	normalized, err := normalizeImageVariants(variants)
	if err != nil {
		return v1.Descriptor{}, nil, err
	}
	return assembleIndexDescriptor(normalized, mediaType)
}

func assembleIndexDescriptor(variants []ImageVariant, mediaType string) (v1.Descriptor, []byte, error) {
	children := make([]v1.Descriptor, len(variants))
	for i, variant := range variants {
		children[i] = variant.Manifest
		children[i].Platform = platformPointer(variant.Platform)
	}
	indexData, err := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: mediaType,
		Manifests: children,
	})
	if err != nil {
		return v1.Descriptor{}, nil, fmt.Errorf("marshal image index: %w", err)
	}
	root := Descriptor(mediaType, indexData)
	return root, indexData, nil
}

func restoreIndexGraph(ctx context.Context, outputPath string, root v1.Descriptor, indexData []byte, variants []ImageVariant, kind string, validate func(context.Context, ImageVariant) (*orasoci.Store, error)) (retErr error) {
	if outputPath == "" || !filepath.IsAbs(outputPath) {
		return fmt.Errorf("OCI layout output must be an absolute path: %q", outputPath)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	sources := make([]*orasoci.Store, len(variants))
	for i, variant := range variants {
		source, err := validate(ctx, variant)
		if err != nil {
			return fmt.Errorf("%s variant %s: %w", kind, platformKey(variant.Platform), err)
		}
		sources[i] = source
	}

	parent := filepath.Dir(outputPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create OCI layout parent: %w", err)
	}
	if _, err := os.Lstat(outputPath); err == nil {
		return fmt.Errorf("OCI layout output already exists: %s", outputPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect OCI layout output: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".coopr-image-index-*")
	if err != nil {
		return fmt.Errorf("create staged OCI layout: %w", err)
	}
	defer func() {
		if staging != "" {
			retErr = errors.Join(retErr, os.RemoveAll(staging))
		}
	}()

	target, err := orasoci.NewWithContext(ctx, staging)
	if err != nil {
		return fmt.Errorf("open staged OCI layout: %w", err)
	}
	storage, err := orasoci.NewStorage(staging)
	if err != nil {
		return fmt.Errorf("open staged OCI content store: %w", err)
	}
	for i, variant := range variants {
		if err := oras.CopyGraph(ctx, sources[i], storage, variant.Manifest, oras.CopyGraphOptions{}); err != nil {
			return fmt.Errorf("copy %s variant %s: %w", kind, platformKey(variant.Platform), err)
		}
	}
	// Push and anchor the index only after every complete child graph exists.
	if err := target.Push(ctx, root, bytes.NewReader(indexData)); err != nil {
		return fmt.Errorf("write image index: %w", err)
	}
	if err := target.Tag(ctx, root, root.Digest.String()); err != nil {
		return fmt.Errorf("anchor image index: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(staging, outputPath); err != nil {
		return fmt.Errorf("publish OCI layout: %w", err)
	}
	staging = ""
	return nil
}

func imageIndexMediaType(format string) (string, error) {
	switch format {
	case "", "oci":
		return v1.MediaTypeImageIndex, nil
	case "docker":
		return dockerIndexType, nil
	default:
		return "", fmt.Errorf("unsupported image index format %q (want oci or docker)", format)
	}
}

func normalizeImageVariants(variants []ImageVariant) ([]ImageVariant, error) {
	if len(variants) == 0 {
		return nil, errors.New("image index requires at least one variant")
	}
	normalized := make([]ImageVariant, len(variants))
	seen := make(map[string]struct{}, len(variants))
	for i, variant := range variants {
		variant.Platform = normalizeImagePlatform(variant.Platform)
		if err := checkPlatform(variant.Platform); err != nil {
			return nil, fmt.Errorf("image variant %d: %w", i, err)
		}
		key := platformKey(variant.Platform)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate image platform %s", key)
		}
		seen[key] = struct{}{}
		normalized[i] = variant
	}
	return normalized, nil
}

func normalizeImagePlatform(platform v1.Platform) v1.Platform {
	platform = platforms.Normalize(platform)
	platform.Variant = normalizeVariant(platform)
	if len(platform.OSFeatures) == 0 {
		platform.OSFeatures = nil
	} else {
		platform.OSFeatures = slices.Clone(platform.OSFeatures)
		slices.Sort(platform.OSFeatures)
	}
	return platform
}

func platformKey(platform v1.Platform) string {
	return platforms.FormatAll(platform)
}

func platformPointer(platform v1.Platform) *v1.Platform {
	copy := platform
	return &copy
}

// Rewrite only member identities; retain index metadata and ordered duplicate
// entries even when distinct input compression variants normalize identically.
func RewriteImageIndex(root v1.Descriptor, original []byte, members []v1.Descriptor) (v1.Descriptor, []byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(original, &fields); err != nil {
		return root, nil, err
	}
	data, err := json.Marshal(members)
	if err != nil {
		return root, nil, err
	}
	fields["manifests"] = data
	data, err = json.Marshal(fields)
	if err != nil {
		return root, nil, err
	}
	root.Digest = digest.FromBytes(data)
	root.Size = int64(len(data))
	if len(root.Data) != 0 {
		root.Data = data
	}
	return root, data, nil
}
