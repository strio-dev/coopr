// Package componentstore manages Coopr's local OCI image-layout component store.
package componentstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/localstore"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func DefaultDir() (string, error) { return localstore.DefaultComponentDir() }

// NormalizeTag gives component names the conventional implicit latest tag and
// familiar Docker spelling. An empty tag leaves the artifact digest-only.
func NormalizeTag(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	normalized, err := NormalizeReference(value)
	if err != nil {
		return "", err
	}
	named, err := reference.ParseNormalizedNamed(normalized)
	if err != nil {
		return "", err
	}
	if _, ok := named.(reference.Digested); ok || strings.HasPrefix(normalized, "sha256:") {
		return "", fmt.Errorf("component tag %q must not include a digest", value)
	}
	return normalized, nil
}

// NormalizeReference accepts conventional named references and immutable local
// sha256 digests. Local and remote lookups share the same named spelling.
func NormalizeReference(value string) (string, error) {
	if strings.HasPrefix(value, "sha256:") {
		parsed, err := digest.Parse(value)
		if err != nil {
			return "", fmt.Errorf("invalid component digest %q: %w", value, err)
		}
		return parsed.String(), nil
	}
	named, err := reference.ParseNormalizedNamed(value)
	if err != nil {
		return "", fmt.Errorf("invalid component reference %q: %w", value, err)
	}
	return reference.FamiliarString(reference.TagNameOnly(named)), nil
}

// LocalSelector returns the exact OCI-layout selector for a component name or
// digest. A named digest selects immutable content rather than its mutable tag.
func LocalSelector(value string) (string, error) {
	normalized, err := NormalizeReference(value)
	if err != nil {
		return "", err
	}
	if at := strings.LastIndexByte(normalized, '@'); at >= 0 {
		return normalized[at+1:], nil
	}
	return normalized, nil
}

func ValidateTag(tag string) error {
	_, err := NormalizeTag(tag)
	return err
}

func Put(ctx context.Context, rootDir string, source content.ReadOnlyStorage, root v1.Descriptor, tag string) error {
	return PutWithBinding(ctx, rootDir, source, root, tag, nil)
}

func Open(ctx context.Context, rootDir string) (*orasoci.Store, error) {
	return localstore.Open(ctx, rootDir)
}

func WriteArchive(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, output string) error {
	root, auxiliary, err := archiveDescriptors(root)
	if err != nil {
		return err
	}
	return localstore.WriteArchive(ctx, source, root, output, auxiliary...)
}
