package localstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

const (
	dockerManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	dockerIndexMediaType    = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// CopySelectedImage copies the selected manifest graph into a temporary OCI
// layout. For a multi-platform root it also copies the verified index blob,
// without copying unrelated platform manifests.
func CopySelectedImage(ctx context.Context, dir string, source content.ReadOnlyStorage, root, selected v1.Descriptor) error {
	if source == nil {
		return errors.New("nil OCI image source")
	}
	if err := validateImageSourceDescriptor(root); err != nil {
		return fmt.Errorf("invalid image root: %w", err)
	}
	if err := validateImageSourceDescriptor(selected); err != nil {
		return fmt.Errorf("invalid selected image manifest: %w", err)
	}
	if !isManifestMediaType(selected.MediaType) {
		return fmt.Errorf("selected image has unsupported media type %q", selected.MediaType)
	}
	if !isManifestMediaType(root.MediaType) && !isIndexMediaType(root.MediaType) {
		return fmt.Errorf("image root has unsupported media type %q", root.MediaType)
	}
	dir, err := prepareRoot(dir)
	if err != nil {
		return err
	}
	target, err := orasoci.NewWithContext(ctx, dir)
	if err != nil {
		return err
	}
	target.AutoSaveIndex = false
	if err := oras.CopyGraph(ctx, source, target, selected, oras.CopyGraphOptions{}); err != nil {
		return fmt.Errorf("copy selected OCI image graph: %w", err)
	}
	if root.Digest == selected.Digest {
		return nil
	}
	data, err := verifiedBlob(ctx, source, root)
	if err != nil {
		return fmt.Errorf("read OCI image index: %w", err)
	}
	if err := target.Push(ctx, root, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return fmt.Errorf("copy OCI image index: %w", err)
	}
	return nil
}

func validateImageSourceDescriptor(desc v1.Descriptor) error {
	if desc.MediaType == "" || desc.Size < 0 || desc.Digest.Algorithm() != digest.SHA256 || desc.Digest.Validate() != nil {
		return errors.New("descriptor requires a media type, non-negative size, and sha256 digest")
	}
	return nil
}

func isManifestMediaType(mediaType string) bool {
	return mediaType == v1.MediaTypeImageManifest || mediaType == dockerManifestMediaType
}

func isIndexMediaType(mediaType string) bool {
	return mediaType == v1.MediaTypeImageIndex || mediaType == dockerIndexMediaType
}

func verifiedBlob(ctx context.Context, store content.ReadOnlyStorage, desc v1.Descriptor) ([]byte, error) {
	data, err := content.FetchAll(ctx, store, desc)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != desc.Size || digest.FromBytes(data) != desc.Digest {
		return nil, fmt.Errorf("blob %s does not match its descriptor", desc.Digest)
	}
	return data, nil
}
