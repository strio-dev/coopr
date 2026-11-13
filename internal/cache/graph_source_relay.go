package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
)

// GraphSourceMetadata preserves exact admitted metadata across a trusted
// same-invocation worker handoff. Byte slices deliberately use base64 in JSON:
// re-encoding embedded JSON would change its content-addressed identity.
// It carries no repository, credentials, filesystem path or image payloads.
type GraphSourceMetadata struct {
	Artifact    v1.Descriptor `json:"artifact"`
	Index       []byte        `json:"index"`
	Manifest    []byte        `json:"manifest"`
	Record      []byte        `json:"record"`
	UsedResults []string      `json:"used_results,omitempty"` // Trusted selected-import receipts.
}

func (source *GraphSource) Metadata() GraphSourceMetadata {
	if source == nil {
		return GraphSourceMetadata{}
	}
	snapshot := GraphSourceMetadata{Artifact: source.Artifact(), Index: slices.Clone(source.indexJSON), Manifest: slices.Clone(source.manifestJSON), Record: slices.Clone(source.recordJSON)}
	source.used.Range(func(id, _ any) bool {
		snapshot.UsedResults = append(snapshot.UsedResults, id.(string))
		return true
	})
	slices.Sort(snapshot.UsedResults)
	return snapshot
}

// RestoreMetadata verifies an already admitted snapshot against its immutable
// artifact and rebinds deferred payload retrieval to the caller-approved store.
// The caller must own the trusted invocation handoff; this is not a new cache
// lookup or an authentication mechanism. As for a completed native worker
// result, TTL cannot expire a snapshot admitted earlier in the same invocation.
func (reader *GraphReader) RestoreMetadata(ctx context.Context, store *OCIStore, root digest.Digest, snapshot GraphSourceMetadata) (*GraphSource, error) {
	if err := reader.enter(ctx); err != nil {
		return nil, err
	}
	defer reader.leave()
	if err := validateGraphLookup(ctx, store, root); err != nil {
		return nil, err
	}
	if err := validDescriptor(snapshot.Artifact); err != nil {
		return nil, err
	}
	if snapshot.Artifact.MediaType != v1.MediaTypeImageIndex || len(snapshot.Index) == 0 || len(snapshot.Index) > maxGraphRecordBytes || len(snapshot.Manifest) == 0 || len(snapshot.Manifest) > maxGraphRecordBytes || len(snapshot.Record) == 0 || len(snapshot.Record) > maxGraphRecordBytes {
		return nil, errors.New("graph metadata handoff outside limits")
	}
	// Retained sources must not alias a caller-owned relay decoding buffer.
	snapshot.Index = slices.Clone(snapshot.Index)
	snapshot.Manifest = slices.Clone(snapshot.Manifest)
	snapshot.Record = slices.Clone(snapshot.Record)
	var index v1.Index
	if err := strictJSON(snapshot.Index, &index); err != nil {
		return nil, err
	}
	if len(index.Manifests) == 0 {
		return nil, errors.New("graph metadata handoff lacks its metadata manifest")
	}
	var manifest v1.Manifest
	if err := strictJSON(snapshot.Manifest, &manifest); err != nil {
		return nil, err
	}
	var record GraphRecord
	if err := strictJSON(snapshot.Record, &record); err != nil {
		return nil, err
	}
	if err := record.Validate(root); err != nil {
		return nil, err
	}
	if len(snapshot.UsedResults) > len(record.Results) {
		return nil, errors.New("too many graph source use receipts")
	}
	target := graphMetadataSnapshotTarget{root: root, artifact: snapshot.Artifact, blobs: map[digest.Digest][]byte{
		snapshot.Artifact.Digest:  snapshot.Index,
		index.Manifests[0].Digest: snapshot.Manifest,
		manifest.Config.Digest:    snapshot.Record,
	}}
	used := make(map[string]bool, len(snapshot.UsedResults))
	for _, id := range snapshot.UsedResults {
		if used[id] || !slices.ContainsFunc(record.Results, func(result GraphResult) bool { return result.ID == id }) {
			return nil, errors.New("unbound or duplicate graph source use receipt")
		}
		used[id] = true
	}
	admitted := *store
	admitted.ttl = nil
	metadata, err := admitted.lookupGraphMetadata(ctx, target, root)
	if err != nil {
		return nil, err
	}
	source, err := reader.admitMetadata(store, metadata)
	if err != nil {
		return nil, err
	}
	for id := range used {
		source.used.Store(id, true)
	}
	return source, nil
}

// A handoff can supply metadata only. Shared validation must not fall through
// to network reads or resolve a mutable graph tag while reconstructing it.
type graphMetadataSnapshotTarget struct {
	root     digest.Digest
	artifact v1.Descriptor
	blobs    map[digest.Digest][]byte
}

func (target graphMetadataSnapshotTarget) Resolve(ctx context.Context, reference string) (v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	if reference != graphTag(target.root) {
		return v1.Descriptor{}, errdef.ErrNotFound
	}
	return target.artifact, nil
}

func (target graphMetadataSnapshotTarget) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, exists := target.blobs[descriptor.Digest]
	if !exists {
		return nil, errdef.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (target graphMetadataSnapshotTarget) Exists(ctx context.Context, descriptor v1.Descriptor) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, exists := target.blobs[descriptor.Digest]
	return exists, nil
}
