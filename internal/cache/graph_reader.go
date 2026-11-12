package cache

import (
	"context"
	"errors"
	"os"
	"sync"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// GraphReader owns admitted immutable graph sources and verified image data for
// one invocation. Complete lookups and selective materialization share a private
// layout and cancelable gate. Returned paths belong to the reader until Close.
type GraphReader struct {
	gate          chan struct{}
	path          string
	layout        *orasoci.Store
	verified      map[digest.Digest]v1.Descriptor
	declared      map[digest.Digest]v1.Descriptor
	sources       map[graphSourceKey]*GraphSource
	metadataBytes int
	closing       chan struct{}
	closeOnce     sync.Once
	closed        bool
	closeErr      error
}

func NewGraphReader(stagingDir string) (*GraphReader, error) {
	path, err := os.MkdirTemp(stagingDir, "coopr-graph-reader-*")
	if err != nil {
		return nil, err
	}
	layout, err := orasoci.New(path)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(path))
	}
	reader := &GraphReader{
		gate: make(chan struct{}, 1), path: path, layout: layout,
		verified: map[digest.Digest]v1.Descriptor{}, declared: map[digest.Digest]v1.Descriptor{},
		sources: map[graphSourceKey]*GraphSource{}, closing: make(chan struct{}),
	}
	reader.gate <- struct{}{}
	return reader, nil
}

func (reader *GraphReader) Lookup(ctx context.Context, store *OCIStore, root digest.Digest) (*GraphRecord, string, error) {
	source, err := reader.LookupMetadata(ctx, store, root)
	if err != nil {
		return nil, "", err
	}
	layout, err := source.EnsureAll(ctx)
	if err != nil {
		return nil, "", err
	}
	record, err := source.Record()
	if err != nil {
		return nil, "", err
	}
	return &record, layout, nil
}

func (reader *GraphReader) enter(ctx context.Context) error {
	if reader == nil {
		return errors.New("nil graph reader")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-reader.closing:
		return errors.New("graph reader is closed")
	case <-reader.gate:
	}
	select {
	case <-reader.closing:
		reader.leave()
		return errors.New("graph reader is closed")
	default:
	}
	if err := ctx.Err(); err != nil {
		reader.leave()
		return err
	}
	return nil
}

func (reader *GraphReader) leave() { reader.gate <- struct{}{} }

func (reader *GraphReader) Close() error {
	if reader == nil {
		return nil
	}
	reader.closeOnce.Do(func() { close(reader.closing) })
	<-reader.gate
	defer reader.leave()
	if !reader.closed {
		reader.closed = true
		reader.closeErr = os.RemoveAll(reader.path)
	}
	return reader.closeErr
}

type graphReaderDestination struct {
	*orasoci.Store
	verified      map[digest.Digest]v1.Descriptor
	validatedPass bool
}

func (destination graphReaderDestination) Exists(ctx context.Context, descriptor v1.Descriptor) (bool, error) {
	// Existing manifest bytes do not establish that its entire subgraph was
	// copied: another image may have called those bytes an opaque config leaf.
	// Always let upstream traverse non-leaf nodes, retaining nested graph and
	// unknown-media behavior instead of treating raw blob existence as closure.
	previous, known := destination.verified[descriptor.Digest]
	if known && !content.Equal(previous, descriptor) {
		return false, errors.New("graph copy conflicts with a previously verified descriptor")
	}
	exists, err := destination.Store.Exists(ctx, descriptor)
	if err != nil || !exists {
		return false, err
	}
	if destination.validatedPass && known && isImageGraphLeaf(descriptor.MediaType) {
		return true, nil
	}
	// ORAS ignores Push's AlreadyExists response. Unadmitted bytes, including
	// opaque leaves copied through recursive traversal, still need verification.
	if err := verifyGraphBlob(ctx, destination.Store, descriptor); err != nil {
		return false, err
	}
	return known && isImageGraphLeaf(descriptor.MediaType), nil
}
