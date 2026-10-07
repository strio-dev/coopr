package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/docker/distribution/registry/api/errcode"
	v2 "github.com/docker/distribution/registry/api/v2"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/pkg/retry"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/pkg/blobinfocache"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
)

// ApplyTLSVerify preserves native registries.conf policy when verify is omitted.
func ApplyTLSVerify(system *types.SystemContext, verify *bool) {
	system.DockerInsecureSkipTLSVerify = types.OptionalBoolUndefined
	if verify != nil {
		system.DockerInsecureSkipTLSVerify = types.NewOptionalBool(!*verify)
	}
}

// nativeArtifactRepository exposes raw registry content without image conversion.
// Each operation owns its native source/destination, so ORAS can copy concurrently.
type nativeArtifactRepository struct {
	name     string
	resolver *Resolver
	mu       sync.Mutex
	// Native sources probe a manifest before fetching blobs. Anchor each blob
	// to its own parent manifest, keeping independent cache graphs isolated.
	selector string
	parents  map[digest.Digest]string
	uploaded map[digest.Digest][]byte
}

var _ oras.Target = (*nativeArtifactRepository)(nil)

func (r *Resolver) repository(ref registry.Reference) (*nativeArtifactRepository, error) {
	return &nativeArtifactRepository{name: ref.Registry + "/" + ref.Repository, resolver: r, selector: ref.Reference, parents: make(map[digest.Digest]string), uploaded: make(map[digest.Digest][]byte)}, nil
}
func (r *nativeArtifactRepository) reference(selector string) (types.ImageReference, error) {
	separator := ":"
	if d := digest.Digest(selector); d.Validate() == nil {
		separator = "@"
	}
	return docker.ParseReference("//" + r.name + separator + selector)
}
func isArtifactManifest(desc v1.Descriptor) bool {
	switch desc.MediaType {
	case v1.MediaTypeImageManifest, v1.MediaTypeImageIndex, dockerManifestType, dockerIndexType:
		return true
	}
	return false
}
func (r *nativeArtifactRepository) manifest(ctx context.Context, selector string) (data []byte, media string, err error) {
	ref, err := r.reference(selector)
	if err != nil {
		return nil, "", err
	}
	err = retry.IfNecessary(ctx, func() error {
		src, e := ref.NewImageSource(ctx, r.resolver.SystemContext())
		if e != nil {
			return e
		}
		defer func() { _ = src.Close() }()
		data, media, e = src.GetManifest(ctx, nil)
		return e
	}, r.resolver.retryOptions)
	if err == nil {
		if requested := digest.Digest(selector); requested.Validate() == nil && requested.Algorithm().FromBytes(data) != requested {
			return nil, "", fmt.Errorf("registry manifest differs from requested digest %s", requested)
		}
		if media == v1.MediaTypeImageManifest || media == dockerManifestType {
			var manifest v1.Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				return nil, "", err
			}
			parent := digest.FromBytes(data).String()
			r.mu.Lock()
			r.parents[manifest.Config.Digest] = parent
			for _, layer := range manifest.Layers {
				r.parents[layer.Digest] = parent
			}
			r.mu.Unlock()
		}
	}
	return data, media, artifactError(err)
}
func (r *nativeArtifactRepository) Resolve(ctx context.Context, selector string) (v1.Descriptor, error) {
	data, media, err := r.manifest(ctx, selector)
	if err != nil {
		return v1.Descriptor{}, err
	}
	return Descriptor(media, data), nil
}
func (r *nativeArtifactRepository) Fetch(ctx context.Context, desc v1.Descriptor) (io.ReadCloser, error) {
	if isArtifactManifest(desc) {
		data, _, err := r.manifest(ctx, desc.Digest.String())
		if err != nil {
			return nil, err
		}
		return io.NopCloser(content.NewVerifyReader(bytes.NewReader(data), desc)), nil
	}
	r.mu.Lock()
	selector := r.parents[desc.Digest]
	if selector == "" {
		selector = r.selector
	}
	r.mu.Unlock()
	ref, err := r.reference(selector)
	if err != nil {
		return nil, err
	}
	var stream io.ReadCloser
	err = retry.IfNecessary(ctx, func() error {
		src, e := ref.NewImageSource(ctx, r.resolver.SystemContext())
		if e != nil {
			return e
		}
		body, _, e := src.GetBlob(ctx, types.BlobInfo{Digest: desc.Digest, Size: desc.Size, MediaType: desc.MediaType}, blobinfocache.DefaultCache(r.resolver.SystemContext()))
		if e != nil {
			_ = src.Close()
			return e
		}
		stream = &artifactReadCloser{ReadCloser: body, source: src}
		return nil
	}, r.resolver.retryOptions)
	return stream, artifactError(err)
}

type artifactReadCloser struct {
	io.ReadCloser
	source types.ImageSource
}

func (r *artifactReadCloser) Close() error {
	return errors.Join(r.ReadCloser.Close(), r.source.Close())
}
func (r *nativeArtifactRepository) Exists(ctx context.Context, desc v1.Descriptor) (bool, error) {
	if isArtifactManifest(desc) {
		// Pull sources may include mirrors that contain content absent from the
		// write destination. Repushing immutable manifests is idempotent and
		// ensures graph copies visit every blob before publishing the root.
		return false, nil
	}
	ref, err := r.reference("coopr-content")
	if err != nil {
		return false, err
	}
	var exists bool
	err = retry.IfNecessary(ctx, func() error {
		dst, e := ref.NewImageDestination(ctx, r.resolver.SystemContext())
		if e != nil {
			return e
		}
		defer func() { _ = dst.Close() }()
		exists, _, e = dst.TryReusingBlob(ctx, types.BlobInfo{Digest: desc.Digest, Size: desc.Size, MediaType: desc.MediaType}, blobinfocache.DefaultCache(r.resolver.SystemContext()), false)
		return e
	}, r.resolver.retryOptions)
	return exists, artifactError(err)
}
func (r *nativeArtifactRepository) Push(ctx context.Context, desc v1.Descriptor, reader io.Reader) error {
	// Spool once to verify content and rewind failed uploads without retaining layers in memory.
	file, err := os.CreateTemp("", "coopr-registry-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	defer func() { _ = file.Close() }()
	if _, err = io.Copy(file, content.NewVerifyReader(reader, desc)); err != nil {
		return err
	}
	return retry.IfNecessary(ctx, func() error {
		if _, e := file.Seek(0, io.SeekStart); e != nil {
			return e
		}
		ref, e := r.reference(desc.Digest.String())
		if e != nil {
			return e
		}
		dst, e := ref.NewImageDestination(ctx, r.resolver.SystemContext())
		if e != nil {
			return e
		}
		defer func() { _ = dst.Close() }()
		if isArtifactManifest(desc) {
			data, e := io.ReadAll(file)
			if e != nil {
				return e
			}
			if err := dst.PutManifest(ctx, data, nil); err != nil {
				return err
			}
			r.mu.Lock()
			r.uploaded[desc.Digest] = data
			r.mu.Unlock()
			return nil
		}
		info, e := dst.PutBlob(ctx, file, types.BlobInfo{Digest: desc.Digest, Size: desc.Size, MediaType: desc.MediaType}, blobinfocache.DefaultCache(r.resolver.SystemContext()), false)
		if e == nil && (info.Digest != desc.Digest || info.Size != desc.Size) {
			return fmt.Errorf("uploaded blob descriptor changed")
		}
		return e
	}, r.resolver.retryOptions)
}
func (r *nativeArtifactRepository) Tag(ctx context.Context, desc v1.Descriptor, selector string) error {
	r.mu.Lock()
	data := r.uploaded[desc.Digest]
	r.mu.Unlock()
	if data == nil {
		ref, err := r.reference(desc.Digest.String())
		if err != nil {
			return err
		}
		// Standalone retagging requires a manifest at the write destination.
		err = retry.IfNecessary(ctx, func() error { _, e := docker.GetDigest(ctx, r.resolver.SystemContext(), ref); return e }, r.resolver.retryOptions)
		if err != nil {
			return err
		}
		data, _, err = r.manifest(ctx, desc.Digest.String())
		if err != nil {
			return err
		}
	}
	if _, err := io.Copy(io.Discard, content.NewVerifyReader(bytes.NewReader(data), desc)); err != nil {
		return err
	}
	ref, err := r.reference(selector)
	if err != nil {
		return err
	}
	return retry.IfNecessary(ctx, func() error {
		dst, e := ref.NewImageDestination(ctx, r.resolver.SystemContext())
		if e != nil {
			return e
		}
		defer func() { _ = dst.Close() }()
		return dst.PutManifest(ctx, data, nil)
	}, r.resolver.retryOptions)
}
func tlsVerifyOption(value types.OptionalBool) *bool {
	if value == types.OptionalBoolUndefined {
		return nil
	}
	verify := value == types.OptionalBoolFalse
	return &verify
}
func artifactError(err error) error {
	if err == nil {
		return nil
	}
	var status docker.UnexpectedHTTPStatusError
	if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %v", errdef.ErrNotFound, err)
	}
	var registryErrors errcode.Errors
	if errors.As(err, &registryErrors) {
		for _, e := range registryErrors {
			var coded errcode.Error
			if errors.As(e, &coded) && (coded.Code == v2.ErrorCodeManifestUnknown || coded.Code == v2.ErrorCodeBlobUnknown || coded.Code == v2.ErrorCodeNameUnknown) {
				return fmt.Errorf("%w: %v", errdef.ErrNotFound, err)
			}
		}
	}
	var coded errcode.Error
	if errors.As(err, &coded) && (coded.Code == v2.ErrorCodeManifestUnknown || coded.Code == v2.ErrorCodeBlobUnknown || coded.Code == v2.ErrorCodeNameUnknown) {
		return fmt.Errorf("%w: %v", errdef.ErrNotFound, err)
	}
	return err
}
