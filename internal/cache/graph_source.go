package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
)

const maxGraphReaderMetadataBytes = 64 << 20

// ErrGraphSourceClose preserves stream-close failures alongside verification
// errors so callers cannot classify them as invalid cache bytes alone.
var ErrGraphSourceClose = errors.New("cache graph source close failed")

type graphSourceKey struct {
	store    *OCIStore
	artifact digest.Digest
}

// GraphSource binds verified graph metadata to one approved source store and
// immutable artifact. Unmaterialized image closures are neither verified nor
// guaranteed to remain available. The owning reader must outlive all retrieval.
type GraphSource struct {
	reader       *GraphReader
	store        *OCIStore
	artifact     v1.Descriptor
	indexJSON    []byte
	recordJSON   []byte
	manifestJSON []byte
	results      map[string]GraphResult
	images       []v1.Descriptor
}

// LookupMetadata verifies the artifact, record and every sidecar without
// fetching image manifests or payloads. TTL applies to admission, not later
// materialization of an already admitted source. Repeated immutable sources
// share a handle; retained metadata is bounded for the reader's lifetime.
func (reader *GraphReader) LookupMetadata(ctx context.Context, store *OCIStore, root digest.Digest) (*GraphSource, error) {
	if err := reader.enter(ctx); err != nil {
		return nil, err
	}
	defer reader.leave()
	if err := validateGraphLookup(ctx, store, root); err != nil {
		return nil, err
	}
	target, release, err := store.operationTarget(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	metadata, err := store.lookupGraphMetadata(ctx, target, root)
	if err != nil {
		return nil, err
	}
	return reader.admitMetadata(store, metadata)
}

type graphMetadata struct {
	record                              GraphRecord
	artifact                            v1.Descriptor
	indexJSON, manifestJSON, recordJSON []byte
	bytes                               int
}

// admitMetadata requires the owning reader gate.
func (reader *GraphReader) admitMetadata(store *OCIStore, metadata *graphMetadata) (*GraphSource, error) {
	record, artifact := &metadata.record, metadata.artifact
	key := graphSourceKey{store: store, artifact: artifact.Digest}
	if previous := reader.sources[key]; previous != nil {
		return previous, nil
	}
	if len(reader.sources) >= maxGraphEntries || metadata.bytes > maxGraphReaderMetadataBytes-reader.metadataBytes {
		return nil, errors.New("graph reader metadata limit exceeded")
	}
	images := graphImages(*record)
	for _, image := range images {
		for _, previous := range []map[digest.Digest]v1.Descriptor{reader.declared, reader.verified} {
			if known, exists := previous[image.Digest]; exists && !content.Equal(known, image) {
				return nil, errors.New("graph image conflicts with an admitted descriptor")
			}
		}
	}
	source := &GraphSource{
		reader: reader, store: store,
		// Strict metadata descriptors contain no mutable selection fields.
		artifact:  v1.Descriptor{MediaType: artifact.MediaType, Digest: artifact.Digest, Size: artifact.Size},
		indexJSON: metadata.indexJSON, recordJSON: metadata.recordJSON, manifestJSON: metadata.manifestJSON, results: make(map[string]GraphResult, len(record.Results)), images: images,
	}
	for _, result := range record.Results {
		source.results[result.ID] = result
	}
	for _, image := range images {
		reader.declared[image.Digest] = image
	}
	reader.sources[key] = source
	reader.metadataBytes += metadata.bytes
	return source, nil
}

// Record returns a defensive copy of the admitted metadata. It does not assert
// that all result images have been fetched or verified.
func (source *GraphSource) Record() (GraphRecord, error) {
	if source == nil {
		return GraphRecord{}, errors.New("nil graph source")
	}
	var record GraphRecord
	err := json.Unmarshal(source.recordJSON, &record)
	return record, err
}

// Artifact returns the exact immutable graph artifact admitted by the reader.
func (source *GraphSource) Artifact() v1.Descriptor {
	if source == nil {
		return v1.Descriptor{}
	}
	return source.artifact
}

// EnsureResult retrieves and verifies only the selected result image and its
// optional restoration base. It never resolves the mutable graph tag again.
func (source *GraphSource) EnsureResult(ctx context.Context, resultID string) (string, error) {
	if source == nil {
		return "", errors.New("nil graph source")
	}
	result, exists := source.results[resultID]
	if !exists {
		return "", fmt.Errorf("result %q is not bound by admitted graph", resultID)
	}
	return source.ensureImages(ctx, graphImages(GraphRecord{Results: []GraphResult{result}}))
}

// EnsureAll retrieves every retained alternative for complete export/seeding.
// A failure leaves no claim of complete ownership or successful publication.
func (source *GraphSource) EnsureAll(ctx context.Context) (string, error) {
	if source == nil {
		return "", errors.New("nil graph source")
	}
	return source.ensureImages(ctx, source.images)
}

func (source *GraphSource) ensureImages(ctx context.Context, images []v1.Descriptor) (string, error) {
	reader := source.reader
	if err := reader.enter(ctx); err != nil {
		return "", err
	}
	defer reader.leave()
	var retained, missing []v1.Descriptor
	for _, image := range images {
		if previous, exists := reader.verified[image.Digest]; exists {
			if !content.Equal(previous, image) {
				return "", errors.New("graph image conflicts with a retained descriptor")
			}
			retained = append(retained, image)
		} else {
			missing = append(missing, image)
		}
	}
	// Successful materialization owns a complete private closure. Revalidate
	// that retained content without requiring its original source to survive.
	if len(retained) != 0 {
		local := graphDeclaredSource{Target: reader.layout, declared: reader.declared}
		if err := materializeGraphImages(ctx, local, retained, reader.layout, reader.verified); err != nil {
			return "", err
		}
	}
	if len(missing) == 0 {
		return reader.path, nil
	}
	// Reopen the approved source under its operation-scoped lock. The retained
	// metadata fixes all identities even if its tag changed or was removed.
	target, release, err := source.store.operationTarget(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	checked := graphDeclaredSource{Target: target, declared: reader.declared}
	if err := materializeGraphImages(ctx, checked, missing, reader.layout, reader.verified); err != nil {
		return "", err
	}
	return reader.path, nil
}

// Admitted roots retain their descriptor role even before their payloads are
// requested; another graph cannot reinterpret one as an opaque leaf.
type graphDeclaredSource struct {
	oras.Target
	declared map[digest.Digest]v1.Descriptor
}

func (source graphDeclaredSource) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	if previous, exists := source.declared[descriptor.Digest]; exists && !content.Equal(previous, descriptor) {
		return nil, errors.New("graph payload conflicts with an admitted image descriptor")
	}
	stream, err := source.Target.Fetch(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	return graphDeclaredStream{Reader: &contextReader{ctx: ctx, reader: stream}, Closer: stream}, nil
}

type graphDeclaredStream struct {
	io.Reader
	io.Closer
}

func (stream graphDeclaredStream) Close() error {
	if err := stream.Closer.Close(); err != nil {
		return errors.Join(ErrGraphSourceClose, err)
	}
	return nil
}
