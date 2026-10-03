package oci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/pkg/retry"
	dockertransport "go.podman.io/image/v5/docker"
	dockerreference "go.podman.io/image/v5/docker/reference"
	imageapi "go.podman.io/image/v5/image"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/pkg/blobinfocache"
	"go.podman.io/image/v5/pkg/shortnames"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/types"
)

// resolveNativeImage keeps image inputs on the containers/image Docker
// transport. That transport applies registries.conf mirrors, remapping,
// blocking, insecure endpoints, and short-name policy before any metadata is
// admitted. Component artifacts intentionally continue to use ORAS.
func (r *Resolver) resolveNativeImage(ctx context.Context, reference string, platform v1.Platform) (*Resolved, error) {
	if ctx == nil {
		return nil, errors.New("image resolve context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkPlatform(platform); err != nil {
		return nil, err
	}
	storage, root, resolvedReference, repository, err := r.openNativeImage(ctx, reference, &platform)
	if err != nil {
		return nil, err
	}
	result, err := r.resolveRoot(ctx, storage, resolvedReference, repository, root, platform, Image)
	if err != nil {
		return nil, err
	}
	if err := enforceNativeImagePolicy(ctx, storage, result.Selected); err != nil {
		return nil, err
	}
	return result, nil
}

func enforceNativeImagePolicy(ctx context.Context, storage *nativeImageStorage, selected v1.Descriptor) error {
	return retry.IfNecessary(ctx, func() (retErr error) {
		source, err := storage.reference.NewImageSource(ctx, storage.system)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, source.Close()) }()
		instance := selected.Digest
		policy, err := signature.DefaultPolicy(storage.system)
		if err != nil {
			return fmt.Errorf("load image policy: %w", err)
		}
		policyContext, err := signature.NewPolicyContext(policy)
		if err != nil {
			return fmt.Errorf("initialize image policy: %w", err)
		}
		defer func() { retErr = errors.Join(retErr, policyContext.Destroy()) }()
		allowed, err := policyContext.IsRunningImageAllowed(ctx, imageapi.UnparsedInstance(source, &instance))
		if err != nil {
			return err
		}
		if !allowed {
			return errors.New("source image rejected by policy")
		}
		return nil
	}, storage.retryOptions)
}

func (r *Resolver) openNativeImage(ctx context.Context, reference string, platform *v1.Platform) (*nativeImageStorage, v1.Descriptor, string, string, error) {
	system := r.SystemContext()
	if platform != nil {
		system.OSChoice = platform.OS
		system.ArchitectureChoice = platform.Architecture
		system.VariantChoice = platform.Variant
	}
	resolvedName, err := shortnames.Resolve(system, reference)
	if err != nil {
		return nil, v1.Descriptor{}, "", "", fmt.Errorf("resolve image name %q: %w", reference, err)
	}
	var candidateErrors []error
	for _, candidate := range resolvedName.PullCandidates {
		candidateSystem := *system
		if r.usePlainHTTP(dockerreference.Domain(candidate.Value)) {
			candidateSystem.DockerInsecureSkipTLSVerify = types.OptionalBoolTrue
		}
		// The Docker transport expects either a tag or a digest. The native
		// parser accepts repo:tag@digest and selects the immutable digest.
		name, err := dockerreference.ParseDockerRef(candidate.Value.String())
		if err != nil {
			candidateErrors = append(candidateErrors, err)
			continue
		}
		imageReference, err := dockertransport.NewReference(name)
		if err != nil {
			candidateErrors = append(candidateErrors, err)
			continue
		}
		storage := &nativeImageStorage{reference: imageReference, system: &candidateSystem, retryOptions: r.retryOptions}
		root, err := storage.resolveRoot(ctx)
		if err != nil {
			candidateErrors = append(candidateErrors, err)
			continue
		}
		storage.root = root
		return storage, root, name.String(), name.Name(), nil
	}
	return nil, v1.Descriptor{}, "", "", resolvedName.FormatPullErrors(candidateErrors)
}

type nativeImageStorage struct {
	reference    types.ImageReference
	system       *types.SystemContext
	root         v1.Descriptor
	retryOptions *retry.Options
}

func (s *nativeImageStorage) resolveRoot(ctx context.Context) (v1.Descriptor, error) {
	data, mediaType, err := s.getManifest(ctx, nil)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if mediaType == "" {
		mediaType = manifest.GuessMIMEType(data)
	}
	return v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}, nil
}

func (s *nativeImageStorage) Fetch(ctx context.Context, target v1.Descriptor) (io.ReadCloser, error) {
	if target.MediaType == v1.MediaTypeImageIndex || target.MediaType == dockerIndexType ||
		target.MediaType == v1.MediaTypeImageManifest || target.MediaType == dockerManifestType {
		var instance *digest.Digest
		if target.Digest != s.root.Digest {
			d := target.Digest
			instance = &d
		}
		data, _, err := s.getManifest(ctx, instance)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	var source types.ImageSource
	var stream io.ReadCloser
	err := retry.IfNecessary(ctx, func() error {
		candidate, err := s.reference.NewImageSource(ctx, s.system)
		if err != nil {
			return err
		}
		candidateStream, _, err := candidate.GetBlob(ctx, types.BlobInfo{
			Digest: target.Digest, Size: target.Size, MediaType: target.MediaType,
		}, blobinfocache.DefaultCache(s.system))
		if err != nil {
			_ = candidate.Close()
			return err
		}
		source, stream = candidate, candidateStream
		return nil
	}, s.retryOptions)
	if err != nil {
		return nil, err
	}
	return &nativeBlobReader{ReadCloser: stream, source: source}, nil
}

func (s *nativeImageStorage) getManifest(ctx context.Context, instance *digest.Digest) (data []byte, mediaType string, retErr error) {
	err := retry.IfNecessary(ctx, func() (attemptErr error) {
		source, err := s.reference.NewImageSource(ctx, s.system)
		if err != nil {
			return err
		}
		defer func() { attemptErr = errors.Join(attemptErr, source.Close()) }()
		data, mediaType, err = source.GetManifest(ctx, instance)
		return err
	}, s.retryOptions)
	return data, mediaType, err
}

func (s *nativeImageStorage) Exists(ctx context.Context, target v1.Descriptor) (bool, error) {
	stream, err := s.Fetch(ctx, target)
	if err != nil {
		return false, err
	}
	return true, stream.Close()
}

type nativeBlobReader struct {
	io.ReadCloser
	source types.ImageSource
}

func (r *nativeBlobReader) Close() error {
	return errors.Join(r.ReadCloser.Close(), r.source.Close())
}

var _ interface {
	Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error)
	Exists(context.Context, v1.Descriptor) (bool, error)
} = (*nativeImageStorage)(nil)
