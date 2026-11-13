package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/moby/buildkit/util/compression"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"golang.org/x/sync/errgroup"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

func graphTag(root digest.Digest) string { return "graph-v3-" + root.Encoded() }

// CanReuseGraph checks an unchanged native result only for publication. It does
// not admit execution inputs, refresh age, or trust mutable local layout files.
func (s *OCIStore) CanReuseGraph(ctx context.Context, root digest.Digest, current GraphResult) (bool, error) {
	if s == nil || s.localRoot != "" {
		return false, nil
	}
	if err := validateGraphLookup(ctx, s, root); err != nil {
		return false, err
	}
	target, release, err := s.operationTarget(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	primary, ok := target.(interface{ ResolvedFromPrimary(v1.Descriptor) bool })
	if !ok {
		return false, nil
	}
	miss := func(err error) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		var response *errcode.ErrorResponse
		if errors.Is(err, ErrMiss) || errors.Is(err, errdef.ErrNotFound) || errors.As(err, &response) && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
			return false, nil // A write target need not permit this optional read.
		}
		return false, err
	}
	metadata, err := s.lookupGraphMetadata(ctx, target, root)
	if err != nil {
		return miss(err)
	}
	if !primary.ResolvedFromPrimary(metadata.artifact) {
		return false, nil
	}
	matched, err := graphRootMatchesResult(metadata.record, root, current)
	if err != nil || !matched {
		return matched, err
	}
	remaining := maxGraphReaderMetadataBytes - metadata.bytes
	leaves := make(map[digest.Digest]v1.Descriptor)
	for _, image := range graphImages(metadata.record) {
		if image.Size > int64(remaining) {
			return false, nil
		}
		remaining -= int(image.Size)
		descriptors, err := imageGraphDescriptors(ctx, target, image)
		if err != nil {
			return miss(err)
		}
		if !primary.ResolvedFromPrimary(image) {
			return false, nil
		}
		for _, descriptor := range descriptors {
			if !isImageGraphLeaf(descriptor.MediaType) {
				return false, nil // Unknown/nested content retains normal publication.
			}
			if previous, ok := leaves[descriptor.Digest]; ok {
				if !sameGraphPayload(previous, descriptor) {
					return false, nil // Conflicting roles retain full publication validation.
				}
				continue
			}
			if descriptor.MediaType == v1.MediaTypeImageConfig || descriptor.MediaType == "application/vnd.docker.container.image.v1+json" {
				if descriptor.Size > int64(remaining) {
					return false, nil
				}
				remaining -= int(descriptor.Size)
			}
			leaves[descriptor.Digest] = descriptor
		}
	}
	checks, checkCtx := errgroup.WithContext(ctx)
	checks.SetLimit(3) // Match ORAS's default graph-transfer concurrency.
	for _, descriptor := range leaves {
		checks.Go(func() error {
			if descriptor.MediaType == v1.MediaTypeImageConfig || descriptor.MediaType == "application/vnd.docker.container.image.v1+json" {
				if _, err := fetchVerified(checkCtx, target, descriptor, maxImageRecordBytes); err != nil {
					return err
				}
			}
			exists, err := target.Exists(checkCtx, descriptor)
			if err == nil && !exists {
				return ErrMiss
			}
			return err
		})
	}
	if err := checks.Wait(); err != nil {
		return miss(err)
	}
	return true, ctx.Err()
}

// PutGraph publishes a verified graph artifact only after its record metadata
// and complete image graphs have been copied. The caller stages publication
// until its whole build succeeds, as for the other portable cache formats.
func (s *OCIStore) PutGraph(ctx context.Context, root digest.Digest, record GraphRecord, layoutPath string) (v1.Descriptor, error) {
	if s == nil || s.target == nil {
		return v1.Descriptor{}, errors.New("nil cache store")
	}
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	if record.Version == "" {
		record.Version = GraphRecordVersion
	}
	if err := record.Validate(root); err != nil {
		return v1.Descriptor{}, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if len(data) > maxGraphRecordBytes {
		return v1.Descriptor{}, errors.New("graph cache record too large")
	}
	source, err := orasoci.New(layoutPath)
	if err != nil {
		return v1.Descriptor{}, err
	}
	graphs := graphImages(record)
	if err := verifyGraphImages(ctx, source, graphs); err != nil {
		return v1.Descriptor{}, fmt.Errorf("verify graph result images: %w", err)
	}
	target, release, err := s.operationTarget(ctx)
	if err != nil {
		return v1.Descriptor{}, err
	}
	defer release()
	pushMetadata := func(descriptor v1.Descriptor, data []byte) error {
		if s.localRoot != "" {
			exists, err := target.Exists(ctx, descriptor)
			if err != nil {
				return err
			}
			if exists {
				return verifyGraphBlob(ctx, target, descriptor)
			}
		}
		return pushGraphBlob(ctx, target, descriptor, data)
	}
	// Local writers hold the layout lock. Revalidate existing local payloads:
	// filename existence alone cannot prove their bytes still match the digest.
	// Fail before retagging rather than claiming a corrupt graph was stored.
	if err := copyGraphImages(ctx, source, target, graphs, false, s.localRoot != ""); err != nil {
		return v1.Descriptor{}, err
	}
	config := oci.Descriptor(GraphConfigMediaType, data)
	manifestData, err := json.Marshal(oci.VersionedManifest(config, []v1.Descriptor{}, GraphArtifactType))
	if err != nil {
		return v1.Descriptor{}, err
	}
	if len(manifestData) > maxGraphRecordBytes {
		return v1.Descriptor{}, errors.New("graph artifact manifest too large")
	}
	metadata := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := pushMetadata(config, data); err != nil {
		return v1.Descriptor{}, err
	}
	if err := pushMetadata(metadata, manifestData); err != nil {
		return v1.Descriptor{}, err
	}
	manifests := append([]v1.Descriptor{metadata}, graphs...)
	indexData, err := json.Marshal(oci.VersionedIndex(manifests, GraphArtifactType))
	if err != nil {
		return v1.Descriptor{}, err
	}
	if len(indexData) > maxGraphRecordBytes {
		return v1.Descriptor{}, errors.New("graph artifact index too large")
	}
	if s.localRoot == "" {
		return oras.TagBytes(ctx, target, v1.MediaTypeImageIndex, indexData, graphTag(root))
	}
	artifact := oci.Descriptor(v1.MediaTypeImageIndex, indexData)
	if err := pushMetadata(artifact, indexData); err != nil {
		return v1.Descriptor{}, err
	}
	if err := target.Tag(ctx, artifact, graphTag(root)); err != nil {
		return v1.Descriptor{}, err
	}
	return artifact, nil
}

// LookupGraph verifies graph metadata bindings and each complete image
// graph, then returns a caller-owned OCI layout containing all results.
func (s *OCIStore) LookupGraph(ctx context.Context, root digest.Digest) (*GraphRecord, string, error) {
	if err := validateGraphLookup(ctx, s, root); err != nil {
		return nil, "", err
	}
	target, release, err := s.operationTarget(ctx)
	if err != nil {
		return nil, "", err
	}
	defer release()
	metadata, err := s.lookupGraphMetadata(ctx, target, root)
	if err != nil {
		return nil, "", err
	}
	dir, err := os.MkdirTemp(s.stagingDir, "coopr-graph-cache-*")
	if err != nil {
		return nil, "", err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	destination, err := orasoci.New(filepath.Clean(dir))
	if err != nil {
		return nil, "", err
	}
	if err := materializeGraphImages(ctx, target, graphImages(metadata.record), destination, nil); err != nil {
		return nil, "", err
	}
	keep = true
	return &metadata.record, dir, nil
}

func validateGraphLookup(ctx context.Context, store *OCIStore, root digest.Digest) error {
	if store == nil || store.target == nil {
		return errors.New("nil cache store")
	}
	if err := validSHA(root); err != nil {
		return err
	}
	return ctx.Err()
}

// lookupGraphMetadata admits only graph metadata. Image closure integrity and
// availability are checked when the selected images are materialized.
func (s *OCIStore) lookupGraphMetadata(ctx context.Context, target oras.ReadOnlyTarget, root digest.Digest) (_ *graphMetadata, retErr error) {
	var artifact v1.Descriptor
	var reader io.ReadCloser
	var err error
	if _, ok := target.(registry.ReferenceFetcher); ok {
		artifact, reader, err = oras.Fetch(ctx, target, graphTag(root), oras.FetchOptions{})
	} else {
		artifact, err = target.Resolve(ctx, graphTag(root))
	}
	if reader != nil {
		defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	}
	if errors.Is(err, errdef.ErrNotFound) {
		return nil, ErrMiss
	}
	if err != nil {
		return nil, err
	}
	if len(artifact.Annotations) == 1 && artifact.Annotations[v1.AnnotationRefName] == graphTag(root) {
		artifact.Annotations = nil
	}
	if artifact.MediaType != v1.MediaTypeImageIndex {
		return nil, errors.New("invalid graph artifact media type")
	}
	if err := validDescriptor(artifact); err != nil {
		return nil, err
	}
	if artifact.Size > maxGraphRecordBytes {
		return nil, errors.New("cache metadata exceeds size limit")
	}
	var indexData []byte
	if reader != nil {
		indexData, err = content.ReadAll(&contextReader{ctx: ctx, reader: reader}, artifact)
	} else {
		indexData, err = fetchVerified(ctx, target, artifact, maxGraphRecordBytes)
	}
	if err != nil {
		return nil, err
	}
	var index v1.Index
	if err := strictJSON(indexData, &index); err != nil {
		return nil, err
	}
	if index.SchemaVersion != 2 || index.MediaType != "" && index.MediaType != v1.MediaTypeImageIndex || index.ArtifactType != GraphArtifactType || index.Subject != nil || len(index.Manifests) < 2 || len(index.Manifests) > 2*maxGraphEntries+1 {
		return nil, errors.New("invalid graph cache index")
	}
	metadata := index.Manifests[0]
	if metadata.MediaType != v1.MediaTypeImageManifest {
		return nil, errors.New("invalid graph metadata manifest media type")
	}
	if err := validDescriptor(metadata); err != nil {
		return nil, err
	}
	manifestData, err := fetchVerified(ctx, target, metadata, maxGraphRecordBytes)
	if err != nil {
		return nil, err
	}
	var manifest v1.Manifest
	if err := strictJSON(manifestData, &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != "" && manifest.MediaType != v1.MediaTypeImageManifest || manifest.ArtifactType != GraphArtifactType || manifest.Config.MediaType != GraphConfigMediaType || manifest.Subject != nil || manifest.Layers == nil || len(manifest.Layers) != 0 {
		return nil, errors.New("invalid graph cache artifact")
	}
	if err := validDescriptor(manifest.Config); err != nil {
		return nil, err
	}
	data, err := fetchVerified(ctx, target, manifest.Config, maxGraphRecordBytes)
	if err != nil {
		return nil, err
	}
	var record GraphRecord
	if err := strictJSON(data, &record); err != nil {
		return nil, err
	}
	if err := record.Validate(root); err != nil {
		return nil, err
	}
	graphs := graphImages(record)
	if len(index.Manifests) != len(graphs)+1 {
		return nil, errors.New("graph index image count differs from record")
	}
	for i, image := range graphs {
		if err := validDescriptor(index.Manifests[i+1]); err != nil {
			return nil, err
		}
		if !content.Equal(image, index.Manifests[i+1]) {
			return nil, errors.New("graph index image differs from record")
		}
	}
	metadataBytes := len(indexData) + len(manifestData) + len(data)
	if !RecordFresh(record.CreatedAt, s.ttl, time.Now()) {
		return nil, ErrMiss
	}
	return &graphMetadata{record: record, artifact: artifact, indexJSON: indexData, manifestJSON: manifestData, recordJSON: data, bytes: metadataBytes}, nil
}

func materializeGraphImages(ctx context.Context, target oras.Target, graphs []v1.Descriptor, destination *orasoci.Store, reusable map[digest.Digest]v1.Descriptor) error {
	validated, err := verifyAndCopyGraphImagesReusable(ctx, target, graphs, destination, reusable)
	if err != nil {
		return fmt.Errorf("verify cached graph images: %w", err)
	}
	var copyTarget content.Storage = destination
	if reusable != nil {
		copyTarget = graphReaderDestination{Store: destination, verified: reusable, validatedPass: validated}
	}
	if err := CopyVerifiedGraphImages(ctx, target, copyTarget, graphs); err != nil {
		return err
	}
	for _, image := range graphs {
		if err := destination.Tag(ctx, image, image.Digest.String()); err != nil {
			return err
		}
		if reusable != nil {
			// Declared roots cannot later be reinterpreted as opaque payloads.
			reusable[image.Digest] = image
		}
	}
	return nil
}

// Complete checkpoints commonly share most layers. Verify each shared blob
// once per artifact operation, rather than downloading the same bytes for each
// ancestor image. Repeated digests must still agree on size and media type.
func verifyGraphImages(ctx context.Context, target oras.Target, images []v1.Descriptor) error {
	return verifyAndCopyGraphImages(ctx, target, images, nil)
}

// The optional destination must be a fresh private layout. It verifies leaf
// payloads during ingestion, avoiding a second source read when CopyGraph
// restores image roots. This also retains foreign layers filtered by ORAS.
// Artifacts with non-leaf or unknown media retain verification and normal graph copying.
func verifyAndCopyGraphImages(ctx context.Context, target oras.Target, images []v1.Descriptor, destination *orasoci.Store) error {
	_, err := verifyAndCopyGraphImagesReusable(ctx, target, images, destination, nil)
	return err
}

// The bool reports a complete leaf validation pass in the private destination.
// Nested or unknown graphs retain the full verification path during traversal.
func verifyAndCopyGraphImagesReusable(ctx context.Context, target oras.Target, images []v1.Descriptor, destination *orasoci.Store, reusable map[digest.Digest]v1.Descriptor) (bool, error) {
	roots := map[digest.Digest]v1.Descriptor{}
	for _, image := range images {
		if err := validDescriptor(image); err != nil {
			return false, err
		}
		if previous, ok := roots[image.Digest]; ok && !content.Equal(previous, image) {
			return false, errors.New("graph images have conflicting descriptors")
		}
		if previous, ok := reusable[image.Digest]; ok && !content.Equal(previous, image) {
			return false, errors.New("graph image conflicts with a previously verified payload")
		}
		roots[image.Digest] = image
	}
	imageDescriptors := make([][]v1.Descriptor, 0, len(images))
	for _, image := range images {
		descriptors, err := imageGraphDescriptors(ctx, target, image)
		if err != nil {
			return false, err
		}
		imageDescriptors = append(imageDescriptors, descriptors)
		for _, descriptor := range descriptors {
			if previous, ok := reusable[descriptor.Digest]; ok && !sameGraphPayload(previous, descriptor) {
				return false, errors.New("graph payload conflicts with a previously verified descriptor")
			}
			if !isImageGraphLeaf(descriptor.MediaType) {
				// Prepopulating any leaf could hide a conflicting descendant in
				// a nested graph. Retain the original copy path for this artifact.
				destination = nil
			}
		}
	}
	seen := map[digest.Digest]v1.Descriptor{}
	for _, descriptors := range imageDescriptors {
		for _, descriptor := range descriptors {
			if image, ok := roots[descriptor.Digest]; ok && !content.Equal(image, descriptor) {
				return false, errors.New("graph image and payload have conflicting descriptors")
			}
			if previous, ok := seen[descriptor.Digest]; ok {
				if !sameGraphPayload(previous, descriptor) {
					return false, errors.New("shared graph blob has conflicting descriptors")
				}
				continue
			}
			if _, ok := reusable[descriptor.Digest]; ok && destination != nil {
				if err := verifyGraphBlob(ctx, destination, descriptor); err != nil {
					return false, fmt.Errorf("previously verified blob %s: %w", descriptor.Digest, err)
				}
				seen[descriptor.Digest] = descriptor
				continue
			}
			stream, err := target.Fetch(ctx, descriptor)
			if err != nil {
				return false, err
			}
			if destination != nil && isImageGraphLeaf(descriptor.MediaType) {
				// A failed source Close can leave ingested bytes behind without
				// admitting them for reuse. A retry verifies both streams instead
				// of trusting Exists or deleting content used by an earlier graph.
				var exists bool
				exists, err = destination.Exists(ctx, descriptor)
				if err != nil {
					err = errors.Join(err, stream.Close())
				} else if exists {
					err = errors.Join(verifyGraphStream(stream, descriptor), verifyGraphBlob(ctx, destination, descriptor))
				} else {
					err = errors.Join(destination.Push(ctx, descriptor, stream), stream.Close())
				}
			} else {
				err = verifyGraphStream(stream, descriptor)
			}
			if err != nil {
				return false, fmt.Errorf("blob %s: %w", descriptor.Digest, err)
			}
			seen[descriptor.Digest] = descriptor
			if reusable != nil && destination != nil {
				reusable[descriptor.Digest] = descriptor
			}
		}
	}
	return destination != nil, nil
}

// OCI and Docker name the same uncompressed tar format differently. Normalize
// that payload alias only; graph roles, compression and byte identity stay strict.
func sameGraphPayload(left, right v1.Descriptor) bool {
	if compression.IsMediaType(compression.Uncompressed, left.MediaType) {
		left.MediaType = v1.MediaTypeImageLayer
	}
	if compression.IsMediaType(compression.Uncompressed, right.MediaType) {
		right.MediaType = v1.MediaTypeImageLayer
	}
	return content.Equal(left, right)
}

func verifyGraphBlob(ctx context.Context, target content.Fetcher, descriptor v1.Descriptor) error {
	stream, err := target.Fetch(ctx, descriptor)
	if err != nil {
		return err
	}
	return verifyGraphStream(struct {
		io.Reader
		io.Closer
	}{Reader: &contextReader{ctx: ctx, reader: stream}, Closer: stream}, descriptor)
}

func verifyGraphStream(stream io.ReadCloser, descriptor v1.Descriptor) error {
	verified := content.NewVerifyReader(stream, descriptor)
	_, copyErr := io.Copy(io.Discard, verified)
	return errors.Join(copyErr, verified.Verify(), stream.Close())
}

//nolint:staticcheck // Imported cache graphs may contain deprecated nondistributable layer types.
func isImageGraphLeaf(mediaType string) bool {
	switch mediaType {
	case v1.MediaTypeImageConfig,
		v1.MediaTypeImageLayer, v1.MediaTypeImageLayerGzip, v1.MediaTypeImageLayerZstd,
		v1.MediaTypeImageLayerNonDistributable, v1.MediaTypeImageLayerNonDistributableGzip, v1.MediaTypeImageLayerNonDistributableZstd,
		"application/vnd.docker.container.image.v1+json",
		"application/vnd.docker.image.rootfs.diff.tar",
		"application/vnd.docker.image.rootfs.diff.tar.gzip",
		"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip":
		return true
	default:
		return false
	}
}

func graphImages(record GraphRecord) []v1.Descriptor {
	var images []v1.Descriptor
	seen := map[digest.Digest]bool{}
	for _, result := range record.Results {
		for _, image := range []*v1.Descriptor{&result.Image, result.BaseImage} {
			if image != nil && !seen[image.Digest] {
				images = append(images, *image)
				seen[image.Digest] = true
			}
		}
	}
	return images
}

func pushGraphBlob(ctx context.Context, target content.Storage, desc v1.Descriptor, data []byte) error {
	if err := target.Push(ctx, desc, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	return nil
}
