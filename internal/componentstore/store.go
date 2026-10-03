// Package componentstore manages Coopr's local OCI image-layout component store.
package componentstore

import (
	"context"
	"fmt"
	"regexp"

	"coopr/internal/localstore"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func DefaultDir() (string, error) { return localstore.DefaultComponentDir() }

func ValidateTag(tag string) error {
	if tag != "" && !tagPattern.MatchString(tag) {
		return fmt.Errorf("invalid local component tag %q", tag)
	}
	return nil
}

func Put(ctx context.Context, rootDir string, source content.ReadOnlyStorage, root v1.Descriptor, tag string) error {
	if err := ValidateTag(tag); err != nil {
		return err
	}
	return localstore.Put(ctx, rootDir, source, root, tag)
}

func Open(ctx context.Context, rootDir string) (*orasoci.Store, error) {
	return localstore.Open(ctx, rootDir)
}

func WriteArchive(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, output string) error {
	return localstore.WriteArchive(ctx, source, root, output)
}
