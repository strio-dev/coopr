package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// One invocation can publish many graph prefixes containing the same complete
// results. Export each native manifest once, sharing verified blobs across all
// prefix artifacts instead of repeatedly materializing every ancestor image.
type instructionGraphExport struct {
	dir     string
	layout  *orasoci.Store
	images  map[digest.Digest]v1.Descriptor
	results map[string]cache.GraphResult
}

func newInstructionGraphExport(staging string) (*instructionGraphExport, error) {
	dir, err := os.MkdirTemp(staging, "graph-export-*")
	if err != nil {
		return nil, err
	}
	layout, err := orasoci.New(filepath.Join(dir, "layout"))
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	return &instructionGraphExport{dir: dir, layout: layout, images: map[digest.Digest]v1.Descriptor{}, results: map[string]cache.GraphResult{}}, nil
}

func (e *instructionGraphExport) result(ctx context.Context, portable *instructionGraphPortable, id string) (cache.GraphResult, error) {
	if result, ok := e.results[id]; ok {
		return result, nil
	}
	r, err := portable.engine.results.read(id)
	if err != nil {
		pending := portable.engine.results.lazy.get(id)
		if pending == nil || pending.failed.Load() {
			return cache.GraphResult{}, err
		}
		source, err := orasoci.New(pending.layout)
		if err != nil {
			return cache.GraphResult{}, err
		}
		if err := oras.CopyGraph(ctx, source, e.layout, pending.record.Image, oras.DefaultCopyGraphOptions); err != nil {
			return cache.GraphResult{}, err
		}
		e.results[id] = pending.record
		e.images[pending.record.Image.Digest] = pending.record.Image
		return pending.record, nil
	}
	if pending := portable.engine.results.lazy.get(id); pending != nil && !pending.failed.Load() && r.CreatedAt.Before(pending.value.CreatedAt) {
		r.CreatedAt = pending.value.CreatedAt
	}
	descriptor, ok := e.images[r.Entry.ManifestDigest]
	if !ok {
		dir, err := os.MkdirTemp(e.dir, "image-*")
		if err != nil {
			return cache.GraphResult{}, err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		output := Output{Path: filepath.Join(dir, "layout"), Format: portable.executor.options.Output.Format, DisableCompression: true}
		exported, err := copyStoredOutputSelected(ctx, portable.executor.store, r.Entry.ImageID, output, portable.executor.system, optionalDigest(r.Entry.ManifestDigest))
		if err != nil {
			return cache.GraphResult{}, err
		}
		descriptor, err = oci.LayoutRoot(exported.Layout)
		if err != nil {
			return cache.GraphResult{}, err
		}
		if descriptor.Digest != r.Entry.ManifestDigest {
			return cache.GraphResult{}, fmt.Errorf("graph export changed immutable image manifest %s", r.Entry.ManifestDigest)
		}
		source, err := orasoci.New(exported.Layout)
		if err != nil {
			return cache.GraphResult{}, err
		}
		if err := oras.CopyGraph(ctx, source, e.layout, descriptor, oras.DefaultCopyGraphOptions); err != nil {
			return cache.GraphResult{}, err
		}
		e.images[descriptor.Digest] = descriptor
	}
	value := *r
	value.Entry.ImageID = ""
	raw, err := json.Marshal(value)
	if err != nil {
		return cache.GraphResult{}, err
	}
	result := cache.GraphResult{ID: id, Image: descriptor, Metadata: raw}
	e.results[id] = result
	return result, nil
}
