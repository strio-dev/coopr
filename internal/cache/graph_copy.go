package cache

import (
	"context"
	"errors"
	"io"
	"sync"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
)

// CopyGraphImages validates and copies complete immutable cache images without
// ORAS's default exclusion of nondistributable layers. Source URLs are not
// followed: each payload must exist in the supplied content-addressed source.
// The destination retains its normal leaf-content existence contract; existing
// metadata is verified and traversed, rather than assumed to imply closure.
func CopyGraphImages(ctx context.Context, source oras.Target, destination content.Storage, images []v1.Descriptor) error {
	return copyGraphImages(ctx, source, destination, images, true, false)
}

// CopyVerifiedGraphImages skips the initial image-validation pass when its
// caller has already performed it. Transfer still visits metadata descendants,
// retains foreign payloads and reports observed source-close failures.
func CopyVerifiedGraphImages(ctx context.Context, source oras.Target, destination content.Storage, images []v1.Descriptor) error {
	return copyGraphImages(ctx, source, destination, images, false, false)
}

func copyGraphImages(ctx context.Context, source oras.Target, destination content.Storage, images []v1.Descriptor, validate, verifyExistingPayloads bool) error {
	if source == nil || destination == nil {
		return errors.New("copy graph images requires source and destination")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tracked := &graphCopySource{Target: source}
	if validate {
		if err := verifyGraphImages(ctx, tracked, images); err != nil {
			return errors.Join(err, tracked.closeError())
		}
	}
	target := graphCopyDestination{Storage: destination, verifyExistingPayloads: verifyExistingPayloads}
	options := oras.DefaultCopyGraphOptions
	options.FindSuccessors = func(ctx context.Context, fetcher content.Fetcher, descriptor v1.Descriptor) ([]v1.Descriptor, error) {
		successors, err := content.Successors(ctx, fetcher, descriptor)
		if err != nil {
			return nil, err
		}
		for _, successor := range successors {
			validated := successor
			validated.Annotations = nil // Preserve authored compression metadata.
			// Nested OCI indexes carry selection metadata that strict cache
			// record descriptors omit. Validate the content identity without
			// rewriting the descriptor passed to upstream traversal/copy.
			validated.Platform, validated.ArtifactType, validated.Data = nil, "", nil
			if successor.Platform != nil && (successor.Platform.OS == "" || successor.Platform.Architecture == "") {
				return nil, errors.New("nested image platform requires OS and architecture")
			}
			if len(successor.Data) != 0 && (int64(len(successor.Data)) != successor.Size || digest.FromBytes(successor.Data) != successor.Digest) {
				return nil, errors.New("nested inline descriptor data differs from identity")
			}
			if err := validDescriptor(validated); err != nil {
				return nil, err
			}
			if isGraphForeignLayer(successor.MediaType) {
				// Upstream filters foreign successors after this callback, but
				// does not filter the root passed to a separate CopyGraph call.
				// Reuse its transfer/verification path without changing descriptors.
				if err := oras.CopyGraph(ctx, tracked, target, successor, options); err != nil {
					return nil, err
				}
			}
		}
		return successors, nil
	}
	for _, image := range images {
		if err := oras.CopyGraph(ctx, tracked, target, image, options); err != nil {
			return errors.Join(err, tracked.closeError())
		}
	}
	return errors.Join(ctx.Err(), tracked.closeError())
}

type graphCopyDestination struct {
	content.Storage
	verifyExistingPayloads bool
}

func (target graphCopyDestination) Exists(ctx context.Context, descriptor v1.Descriptor) (bool, error) {
	exists, err := target.Storage.Exists(ctx, descriptor)
	metadata := isGraphCopyMetadata(descriptor.MediaType)
	if err != nil || !exists || (!metadata && !target.verifyExistingPayloads) {
		return exists, err
	}
	// A prior interrupted copy or a payload/manifest alias can leave a root's
	// bytes present while its descendants are missing. Reject corrupt metadata
	// and let upstream visit its successors even when the root already exists.
	// Local publication also verifies existing payload bytes before reusing them.
	if err := verifyGraphBlob(ctx, target.Storage, descriptor); err != nil {
		return false, err
	}
	return !metadata, nil
}

func isGraphCopyMetadata(mediaType string) bool {
	switch mediaType {
	case v1.MediaTypeImageManifest, v1.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.cncf.oras.artifact.manifest.v1+json":
		return true
	default:
		return false
	}
}

//nolint:staticcheck // Complete cache graphs retain existing nondistributable layer types.
func isGraphForeignLayer(mediaType string) bool {
	switch mediaType {
	case v1.MediaTypeImageLayerNonDistributable,
		v1.MediaTypeImageLayerNonDistributableGzip,
		v1.MediaTypeImageLayerNonDistributableZstd,
		"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip":
		return true
	default:
		return false
	}
}

// ORAS deliberately ignores source Close errors in its ordinary copy path.
// Cache publication cannot report success if any observed source close fails.
type graphCopySource struct {
	oras.Target
	mu  sync.Mutex
	err error
}

func (source *graphCopySource) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	stream, err := source.Target.Fetch(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	return &graphCopyStream{ReadCloser: stream, source: source}, nil
}

func (source *graphCopySource) closeError() error {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.err
}

type graphCopyStream struct {
	io.ReadCloser
	source *graphCopySource
}

func (stream *graphCopyStream) Close() error {
	err := stream.ReadCloser.Close()
	if err != nil {
		stream.source.mu.Lock()
		if stream.source.err == nil {
			stream.source.err = err
		}
		stream.source.mu.Unlock()
	}
	return err
}
