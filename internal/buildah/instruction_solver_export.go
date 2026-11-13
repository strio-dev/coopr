package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// One invocation can publish many graph prefixes containing the same complete
// results. Export each native manifest once, sharing verified blobs across all
// prefix artifacts instead of repeatedly materializing every ancestor image.
type instructionGraphExport struct {
	dir     string
	layout  oras.Target
	images  map[digest.Digest]v1.Descriptor
	results map[string]cache.GraphResult
}

// An imported alternative with proven invalid bytes cannot be included in a
// complete graph. This does not classify native export or destination failures.
type instructionGraphInputIntegrityError struct {
	id  string
	err error
}

func (err *instructionGraphInputIntegrityError) Error() string {
	return fmt.Sprintf("invalid imported graph result %s: %v", err.id, err.err)
}
func (err *instructionGraphInputIntegrityError) Unwrap() error { return err.err }

// A mixed failure is not merely invalid input: preserve cancellation, source
// Close errors and storage faults even when another cause reports corruption.
func instructionGraphInputIntegrityOnly(err error) bool {
	if errors.Is(err, cache.ErrGraphSourceClose) {
		return false
	}
	if err == content.ErrMismatchedDigest || err == content.ErrTrailingData || err == io.ErrUnexpectedEOF {
		return true
	}
	switch cause := err.(type) {
	case interface{ Unwrap() []error }:
		causes := cause.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, nested := range causes {
			if !instructionGraphInputIntegrityOnly(nested) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return instructionGraphInputIntegrityOnly(cause.Unwrap())
	default:
		return false
	}
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
	r, readErr := portable.engine.results.read(id)
	pending := portable.engine.results.lazy.get(id)
	if pending != nil && (pending.failed.Load() || pending.record.ID != id || pending.value.ID() != id) {
		pending = nil
	}
	if readErr != nil && pending == nil {
		return cache.GraphResult{}, readErr
	}
	if readErr == nil && pending != nil && r.CreatedAt.Before(pending.value.CreatedAt) {
		r.CreatedAt = pending.value.CreatedAt
	}
	// Importing a portable hit keeps its verified OCI source alive. Reuse those
	// exact image bytes while preserving the current native result metadata.
	if pending != nil && (readErr != nil || pending.record.Image.Digest == r.Entry.ManifestDigest) {
		if _, exists := e.images[pending.record.Image.Digest]; !exists {
			layout, _, err := pending.ensureLayout(ctx)
			if err != nil {
				if ctx.Err() == nil && instructionGraphInputIntegrityOnly(err) {
					return cache.GraphResult{}, &instructionGraphInputIntegrityError{id: id, err: err}
				}
				return cache.GraphResult{}, err
			}
			source, err := orasoci.New(layout)
			if err != nil {
				return cache.GraphResult{}, err
			}
			if err := cache.CopyGraphImages(ctx, source, e.layout, []v1.Descriptor{pending.record.Image}); err != nil {
				return cache.GraphResult{}, err
			}
			e.images[pending.record.Image.Digest] = pending.record.Image
		}
		if readErr != nil {
			e.results[id] = pending.record
			return pending.record, nil
		}
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
		if err := cache.CopyGraphImages(ctx, source, e.layout, []v1.Descriptor{descriptor}); err != nil {
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
