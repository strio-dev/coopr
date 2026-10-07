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
	"strings"

	"github.com/containerd/platforms"
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

// AssembleImageIndex combines complete single-platform OCI layouts into one
// multi-platform OCI layout. The child manifests and their graphs are copied
// without rewriting them.
func AssembleImageIndex(ctx context.Context, outputPath string, variants []ImageVariant, format string) (v1.Descriptor, []byte, error) {
	root, indexData, err := ImageIndexDescriptor(variants, format)
	if err != nil {
		return v1.Descriptor{}, nil, err
	}
	if err := RestoreImageIndex(ctx, outputPath, root, indexData, variants); err != nil {
		return v1.Descriptor{}, nil, err
	}
	return root, indexData, nil
}

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

// RestoreImageIndex recreates an exact image index from retained bytes and
// independently stored child image graphs. It never re-marshals indexData, so
// the returned layout preserves root's digest exactly.
func RestoreImageIndex(ctx context.Context, outputPath string, root v1.Descriptor, indexData []byte, variants []ImageVariant) (retErr error) {
	normalized, err := normalizeImageVariants(variants)
	if err != nil {
		return err
	}
	if err := validateImageIndex(root, indexData, normalized); err != nil {
		return err
	}
	return restoreIndexGraph(ctx, outputPath, root, indexData, normalized, "image", validateImageVariant)
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
	key := platforms.FormatAll(platform)
	if len(platform.OSFeatures) != 0 {
		key += "[" + strings.Join(platform.OSFeatures, ",") + "]"
	}
	return key
}

func platformPointer(platform v1.Platform) *v1.Platform {
	copy := platform
	return &copy
}

func validateImageIndex(root v1.Descriptor, indexData []byte, variants []ImageVariant) error {
	if err := checkDescriptor(root); err != nil {
		return fmt.Errorf("invalid image index descriptor: %w", err)
	}
	if root.MediaType != v1.MediaTypeImageIndex && root.MediaType != dockerIndexType {
		return fmt.Errorf("unsupported image index media type %q", root.MediaType)
	}
	if int64(len(indexData)) != root.Size || root.Digest != root.Digest.Algorithm().FromBytes(indexData) {
		return errors.New("image index bytes differ from root descriptor")
	}
	if len(indexData) > maxMetadataBytes {
		return fmt.Errorf("image index exceeds %d bytes", maxMetadataBytes)
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return fmt.Errorf("decode image index: %w", err)
	}
	if index.SchemaVersion != 2 || index.MediaType != root.MediaType {
		return fmt.Errorf("image index schema or media type differs from root descriptor")
	}
	if len(index.Manifests) != len(variants) {
		return fmt.Errorf("image index has %d manifests, want %d", len(index.Manifests), len(variants))
	}
	matched := make([]bool, len(variants))
	for i, child := range index.Manifests {
		found := false
		for j, variant := range variants {
			if matched[j] {
				continue
			}
			expected := variant.Manifest
			expected.Platform = platformPointer(variant.Platform)
			if descriptorJSONEqual(child, expected) {
				matched[j] = true
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("image index manifest %d does not match an exact image variant", i)
		}
	}
	return nil
}

func descriptorJSONEqual(a, b v1.Descriptor) bool {
	aData, aErr := json.Marshal(a)
	bData, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && bytes.Equal(aData, bData)
}

func validateImageVariant(ctx context.Context, variant ImageVariant) (*orasoci.Store, error) {
	if variant.Layout == "" {
		return nil, errors.New("empty OCI layout path")
	}
	if variant.Manifest.MediaType != v1.MediaTypeImageManifest && variant.Manifest.MediaType != dockerManifestType {
		return nil, fmt.Errorf("unsupported image manifest media type %q", variant.Manifest.MediaType)
	}
	if err := checkDescriptor(variant.Manifest); err != nil {
		return nil, fmt.Errorf("invalid image manifest descriptor: %w", err)
	}
	_, layoutDescriptor, err := layoutRoot(variant.Layout)
	if err != nil {
		return nil, fmt.Errorf("read OCI layout root: %w", err)
	}
	if !sameDescriptor(layoutDescriptor, variant.Manifest) {
		return nil, errors.New("OCI layout root differs from image manifest descriptor")
	}
	source, err := orasoci.NewWithContext(ctx, variant.Layout)
	if err != nil {
		return nil, fmt.Errorf("open OCI layout: %w", err)
	}
	resolved, err := source.Resolve(ctx, variant.Manifest.Digest.String())
	if err != nil || !sameDescriptor(resolved, variant.Manifest) {
		return nil, fmt.Errorf("resolve image manifest: %v", err)
	}
	if err := verifyArchiveGraph(ctx, source, variant.Manifest); err != nil {
		return nil, fmt.Errorf("invalid image graph: %w", err)
	}
	manifestData, err := readLayerMetadata(ctx, source, variant.Manifest)
	if err != nil {
		return nil, fmt.Errorf("read image manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("decode image manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != "" && manifest.MediaType != variant.Manifest.MediaType {
		return nil, errors.New("image manifest schema or media type differs from descriptor")
	}
	wantConfigType := v1.MediaTypeImageConfig
	if variant.Manifest.MediaType == dockerManifestType {
		wantConfigType = dockerConfigType
	}
	if manifest.Config.MediaType != wantConfigType {
		return nil, fmt.Errorf("image config has media type %q, want %q", manifest.Config.MediaType, wantConfigType)
	}
	configData, err := readLayerMetadata(ctx, source, manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("read image config: %w", err)
	}
	var image v1.Image
	if err := json.Unmarshal(configData, &image); err != nil {
		return nil, fmt.Errorf("decode image config: %w", err)
	}
	actual := normalizeImagePlatform(image.Platform)
	if !platformEqual(actual, variant.Platform) {
		return nil, fmt.Errorf("image config platform %s differs from requested %s", platformKey(actual), platformKey(variant.Platform))
	}
	return source, nil
}
