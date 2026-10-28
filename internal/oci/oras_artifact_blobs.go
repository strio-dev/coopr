package oci

import (
	"context"
	"io"
	"os"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/pkg/retry"
	"go.podman.io/image/v5/docker"
	nativereference "go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/pkg/blobinfocache"
	nativecredentials "go.podman.io/image/v5/pkg/docker/config"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2/registry"
)

// orasArtifactBlobs uses the native shared blob cache as location hints while
// configured ORAS clients own every registry request.
type orasArtifactBlobs struct {
	resolver *Resolver
	ref      registry.Reference
}

// Cache coordinates match containers/image v5.41.2's Docker transport:
// registry domains are scopes and normalized repository names are locations.
// Keep this mapping isolated when updating containers/image; cache lookup and
// recording use its public API, while all network access stays with ORAS.
func (r orasArtifactBlobs) cacheCoordinates() (types.BlobInfoCache, types.BICTransportScope, nativereference.Named, error) {
	named, err := nativereference.ParseNormalizedNamed(r.ref.Registry + "/" + r.ref.Repository)
	if err != nil {
		return nil, types.BICTransportScope{}, nil, err
	}
	return blobinfocache.DefaultCache(r.resolver.SystemContext()), types.BICTransportScope{Opaque: nativereference.Domain(named)}, named, nil
}

func (r orasArtifactBlobs) Exists(ctx context.Context, descriptor v1.Descriptor) (bool, error) {
	if err := checkDescriptor(descriptor); err != nil {
		return false, err
	}
	var exists bool
	err := retry.IfNecessary(ctx, func() error {
		repo, err := r.resolver.orasRepository(ctx, r.ref.Registry+"/"+r.ref.Repository, nil, r.resolver.SystemContext(), 0)
		if err != nil {
			return err
		}
		exists, err = repo.Blobs().Exists(ctx, descriptor)
		return err
	}, r.resolver.artifactRetryOptions())
	if err == nil && exists {
		cache, scope, named, cacheErr := r.cacheCoordinates()
		if cacheErr != nil {
			return false, cacheErr
		}
		cache.RecordKnownLocation(docker.Transport, scope, descriptor.Digest, types.BICLocationReference{Opaque: named.Name()})
	}
	return exists, err
}

func (r orasArtifactBlobs) Push(ctx context.Context, descriptor v1.Descriptor, reader io.Reader) error {
	file, err := spoolArtifact(reader, descriptor)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	defer func() { _ = file.Close() }()
	cache, scope, destination, err := r.cacheCoordinates()
	if err != nil {
		return err
	}
	record := func() {
		cache.RecordKnownLocation(docker.Transport, scope, descriptor.Digest, types.BICLocationReference{Opaque: destination.Name()})
	}
	return retry.IfNecessary(ctx, func() error {
		repo, err := r.resolver.orasRepository(ctx, r.ref.Registry+"/"+r.ref.Repository, nil, r.resolver.SystemContext(), 0)
		if err != nil {
			return err
		}
		exists, err := repo.Blobs().Exists(ctx, descriptor)
		if err != nil {
			return err
		}
		if exists {
			record()
			return nil
		}
		sourceSystem := *r.resolver.SystemContext()
		credentialsBound := false
		for _, candidate := range cache.CandidateLocations(docker.Transport, scope, descriptor.Digest, false) {
			if candidate.Digest != descriptor.Digest {
				continue
			}
			named, err := nativereference.ParseNormalizedNamed(candidate.Location.Opaque)
			if err != nil || named.Name() != candidate.Location.Opaque || nativereference.Domain(named) != scope.Opaque || named.Name() == destination.Name() {
				continue
			}
			if !credentialsBound {
				destinationCredentials, err := nativecredentials.GetCredentialsForRef(&sourceSystem, destination)
				if err != nil {
					return err
				}
				// Native reuse expands destination credentials, not a cached source entry.
				sourceSystem.DockerAuthConfig = &destinationCredentials
				credentialsBound = true
			}
			source, err := r.resolver.orasRepository(ctx, named.Name(), nil, &sourceSystem, 0)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				continue
			}
			exists, err := source.Blobs().Exists(ctx, descriptor)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil || !exists {
				continue
			}
			uploadStarted := false
			err = repo.Mount(ctx, descriptor, nativereference.Path(named), func() (io.ReadCloser, error) {
				uploadStarted = true
				if _, err := file.Seek(0, io.SeekStart); err != nil {
					return nil, err
				}
				return io.NopCloser(file), nil
			})
			if err == nil {
				record()
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if uploadStarted {
				// A declined mount is now an upload; only the outer budget may replay it.
				return err
			}
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := repo.Blobs().Push(ctx, descriptor, io.NopCloser(file)); err != nil {
			return err
		}
		record()
		return nil
	}, r.resolver.artifactRetryOptions())
}
