package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

func graphTag(root digest.Digest) string { return "graph-" + root.Encoded() }

// PutGraph publishes a verified graph artifact only after all result sidecars
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
	layers := append([]v1.Descriptor{}, graphs...)
	for _, result := range record.Results {
		desc, _ := result.Descriptor()
		value, err := json.Marshal(result)
		if err != nil {
			return v1.Descriptor{}, err
		}
		if err := pushMetadata(desc, value); err != nil {
			return v1.Descriptor{}, err
		}
		layers = append(layers, desc)
	}
	config := oci.Descriptor(GraphConfigMediaType, data)
	manifestData, err := json.Marshal(oci.VersionedManifest(config, layers, GraphArtifactType))
	if err != nil {
		return v1.Descriptor{}, err
	}
	if len(manifestData) > maxGraphRecordBytes {
		return v1.Descriptor{}, errors.New("graph artifact manifest too large")
	}
	artifact := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := pushMetadata(config, data); err != nil {
		return v1.Descriptor{}, err
	}
	if err := pushMetadata(artifact, manifestData); err != nil {
		return v1.Descriptor{}, err
	}
	if err := target.Tag(ctx, artifact, graphTag(root)); err != nil {
		return v1.Descriptor{}, err
	}
	return artifact, nil
}

// LookupGraph verifies graph metadata, sidecar bindings and each complete image
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
func (s *OCIStore) lookupGraphMetadata(ctx context.Context, target oras.ReadOnlyTarget, root digest.Digest) (*graphMetadata, error) {
	artifact, err := target.Resolve(ctx, graphTag(root))
	if errors.Is(err, errdef.ErrNotFound) {
		return nil, ErrMiss
	}
	if err != nil {
		return nil, err
	}
	if len(artifact.Annotations) == 1 && artifact.Annotations[v1.AnnotationRefName] == graphTag(root) {
		artifact.Annotations = nil
	}
	if artifact.MediaType != v1.MediaTypeImageManifest {
		return nil, errors.New("invalid graph artifact media type")
	}
	if err := validDescriptor(artifact); err != nil {
		return nil, err
	}
	manifestData, err := fetchVerified(ctx, target, artifact, maxGraphRecordBytes)
	if err != nil {
		return nil, err
	}
	var manifest v1.Manifest
	if err := strictJSON(manifestData, &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != "" && manifest.MediaType != v1.MediaTypeImageManifest || manifest.ArtifactType != GraphArtifactType || manifest.Config.MediaType != GraphConfigMediaType || manifest.Subject != nil || len(manifest.Layers) > 3*maxGraphEntries {
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
	expected := append([]v1.Descriptor{}, graphs...)
	for _, result := range record.Results {
		desc, _ := result.Descriptor()
		expected = append(expected, desc)
	}
	if len(expected) != len(manifest.Layers) {
		return nil, errors.New("graph artifact result count differs from record")
	}
	for i, desc := range expected {
		if err := validDescriptor(manifest.Layers[i]); err != nil {
			return nil, err
		}
		if !content.Equal(desc, manifest.Layers[i]) {
			return nil, errors.New("graph artifact result differs from record")
		}
	}
	metadataBytes := len(manifestData) + len(data)
	for _, result := range record.Results {
		desc, _ := result.Descriptor()
		value, err := fetchVerified(ctx, target, desc, maxGraphRecordBytes)
		if err != nil {
			return nil, err
		}
		metadataBytes += len(value)
		want, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(value, want) {
			return nil, errors.New("graph result sidecar differs from record")
		}
	}
	if !RecordFresh(record.CreatedAt, s.ttl, time.Now()) {
		return nil, ErrMiss
	}
	return &graphMetadata{record: record, artifact: artifact, manifestJSON: manifestData, recordJSON: data, bytes: metadataBytes}, nil
}

func materializeGraphImages(ctx context.Context, target oras.Target, graphs []v1.Descriptor, destination *orasoci.Store, reusable map[digest.Digest]v1.Descriptor) error {
	if err := verifyAndCopyGraphImagesReusable(ctx, target, graphs, destination, reusable); err != nil {
		return fmt.Errorf("verify cached graph images: %w", err)
	}
	var copyTarget content.Storage = destination
	if reusable != nil {
		copyTarget = graphReaderDestination{Store: destination, verified: reusable}
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
	return verifyAndCopyGraphImagesReusable(ctx, target, images, destination, nil)
}

func verifyAndCopyGraphImagesReusable(ctx context.Context, target oras.Target, images []v1.Descriptor, destination *orasoci.Store, reusable map[digest.Digest]v1.Descriptor) error {
	roots := map[digest.Digest]v1.Descriptor{}
	for _, image := range images {
		if err := validDescriptor(image); err != nil {
			return err
		}
		if previous, ok := roots[image.Digest]; ok && !content.Equal(previous, image) {
			return errors.New("graph images have conflicting descriptors")
		}
		if previous, ok := reusable[image.Digest]; ok && !content.Equal(previous, image) {
			return errors.New("graph image conflicts with a previously verified payload")
		}
		roots[image.Digest] = image
	}
	imageDescriptors := make([][]v1.Descriptor, 0, len(images))
	for _, image := range images {
		descriptors, err := imageGraphDescriptors(ctx, target, image)
		if err != nil {
			return err
		}
		imageDescriptors = append(imageDescriptors, descriptors)
		for _, descriptor := range descriptors {
			if previous, ok := reusable[descriptor.Digest]; ok && !content.Equal(previous, descriptor) {
				return errors.New("graph payload conflicts with a previously verified descriptor")
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
				return errors.New("graph image and payload have conflicting descriptors")
			}
			if previous, ok := seen[descriptor.Digest]; ok {
				if !content.Equal(previous, descriptor) {
					return errors.New("shared graph blob has conflicting descriptors")
				}
				continue
			}
			if _, ok := reusable[descriptor.Digest]; ok && destination != nil {
				if err := verifyGraphBlob(ctx, destination, descriptor); err != nil {
					return fmt.Errorf("previously verified blob %s: %w", descriptor.Digest, err)
				}
				seen[descriptor.Digest] = descriptor
				continue
			}
			stream, err := target.Fetch(ctx, descriptor)
			if err != nil {
				return err
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
				return fmt.Errorf("blob %s: %w", descriptor.Digest, err)
			}
			seen[descriptor.Digest] = descriptor
			if reusable != nil && destination != nil {
				reusable[descriptor.Digest] = descriptor
			}
		}
	}
	return nil
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

func pushGraphBlob(ctx context.Context, target oras.Target, desc v1.Descriptor, data []byte) error {
	if err := target.Push(ctx, desc, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	return nil
}
