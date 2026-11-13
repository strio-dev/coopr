package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/storage"
)

// Keep the executor's native graph names at the existing local boundary.
type instructionGraphMetadata = cache.GraphMetadata
type instructionGraphNode = cache.GraphNode
type instructionGraphInput = cache.GraphInput
type instructionGraphStore interface {
	PutGraph(context.Context, digest.Digest, cache.GraphRecord, string) (v1.Descriptor, error)
}
type instructionGraphPortable struct {
	mu               sync.Mutex
	executor         *graphExecutor
	engine           *instructionSolver
	cache            *portableInstructionCache
	roots            map[digest.Digest]instructionGraphRoot
	read             map[digest.Digest]bool // attempted roots; true means metadata was admitted
	reader           *cache.GraphReader
	sources          []instructionGraphSource
	relayBindings    []digest.Digest
	relayBindingsSet bool
}
type instructionGraphRoot struct {
	vertex *instructionVertex
	result string
}

func newInstructionGraphPortable(executor *graphExecutor, engine *instructionSolver) *instructionGraphPortable {
	if executor.instructionPortableCache == nil {
		return nil
	}
	return &instructionGraphPortable{executor: executor, engine: engine, cache: executor.instructionPortableCache, roots: map[digest.Digest]instructionGraphRoot{}, read: map[digest.Digest]bool{}}
}

func (s *instructionSolver) closePortable() error {
	if s.portable == nil {
		return nil
	}
	return s.portable.reader.Close()
}

// Source ownership outlives selected imports and success-gated publication.
func (p *instructionGraphPortable) graphReader() (*cache.GraphReader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reader == nil {
		reader, err := cache.NewGraphReader(p.cache.stagingDir)
		if err != nil {
			return nil, err
		}
		p.reader = reader
	}
	return p.reader, nil
}

func (s *instructionSolver) preparePortable(ctx context.Context, vertex *instructionVertex) error {
	p := s.portable
	if p == nil {
		return nil
	}
	root, err := vertex.portableDigest()
	if err != nil {
		return err
	}
	p.mu.Lock()
	if admitted, read := p.read[root]; read {
		p.mu.Unlock()
		if admitted {
			return s.linkPortableKeys(vertex)
		}
		return s.keys.Err()
	}
	p.read[root] = false
	p.mu.Unlock()
	admitted := false
	for index, candidate := range p.cache.readStores {
		store, ok := candidate.(*cache.OCIStore)
		if !ok {
			continue
		}
		reader, err := p.graphReader()
		if err != nil {
			return err
		}
		source, err := reader.LookupMetadata(ctx, store, root)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if !errors.Is(err, cache.ErrMiss) {
				packageCacheWarning("read graph cache", err)
				p.mu.Lock()
				p.cache.stats.Errors++
				p.mu.Unlock()
			}
			continue
		}
		binding := digest.Digest("")
		if index < len(p.cache.readBindingIDs) {
			binding = p.cache.readBindingIDs[index]
		}
		// A source absent from the parent's opened binding set cannot escape
		// as a deferred worker capability. Preserve the prior complete/eager
		// handoff for that unusual independently available local read cache.
		if p.relayBindingsSet && !slices.Contains(p.relayBindings, binding) {
			var layout string
			layout, err = source.EnsureAll(ctx)
			if err == nil {
				var record cache.GraphRecord
				record, err = source.Record()
				if err == nil {
					err = p.restoreRecord(ctx, &record, layout, nil, true)
				}
			}
		} else {
			err = p.restoreSource(ctx, source)
			if err == nil {
				p.rememberSource(binding, source)
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			packageCacheWarning("restore graph cache", err)
			p.mu.Lock()
			p.cache.stats.Errors++
			p.mu.Unlock()
			continue
		}
		// Only metadata is admitted before matching. BuildKit loads the selected
		// result on demand; IgnoreCache still controls execution.
		admitted = true
	}
	p.mu.Lock()
	p.read[root] = admitted
	p.mu.Unlock()
	if !admitted {
		return s.keys.Err()
	}
	return s.linkPortableKeys(vertex)
}

// Independently published prefixes can use different native key IDs for the
// same authored graph. A completed solver edge retains its earlier IDs when a
// later graph is admitted. Requery the exact authored graph so BuildKit's Query
// associates every matching parent key with its matching children before that
// completed edge is reused. This reads key metadata, never result payloads.
func (s *instructionSolver) linkPortableKeys(vertex *instructionVertex) error {
	vertices, err := instructionGraphOrder(vertex)
	if err != nil {
		return err
	}
	for _, current := range vertices {
		if current.ignore {
			return s.keys.Err()
		}
	}
	matched := map[*instructionVertex][]solver.CacheKeyWithSelector{}
	for _, current := range vertices {
		var candidates map[string]*solver.CacheKey
		inputs := current.inputVertices()
		for slot := 0; slot < max(1, len(inputs)); slot++ {
			var dependencies []solver.CacheKeyWithSelector
			if len(inputs) != 0 {
				dependencies = matched[inputs[slot]]
				if len(dependencies) == 0 {
					candidates = nil
					break
				}
			}
			keys, err := s.manager.Query(dependencies, solver.Index(slot), current.key, 0)
			if err != nil {
				return err
			}
			found := make(map[string]*solver.CacheKey, len(keys))
			for _, key := range keys {
				if slot == 0 || candidates[key.ID] != nil {
					found[key.ID] = key
				}
			}
			candidates = found
			if len(candidates) == 0 {
				break
			}
		}
		for _, key := range candidates {
			matched[current] = append(matched[current], solver.CacheKeyWithSelector{CacheKey: solver.ExportableCacheKey{CacheKey: key}})
		}
	}
	return s.keys.Err()
}

// A shared input is visited once, after its own inputs. Reject cycles before
// reconciling links or making a publication decision.
func instructionGraphOrder(root *instructionVertex) ([]*instructionVertex, error) {
	var vertices []*instructionVertex
	states := map[*instructionVertex]uint8{}
	var visit func(*instructionVertex) error
	visit = func(vertex *instructionVertex) error {
		if vertex == nil {
			return errors.New("portable graph has no vertex")
		}
		switch states[vertex] {
		case 1:
			return errors.New("portable graph contains a cycle")
		case 2:
			return nil
		}
		if len(states) >= 4096 {
			return errors.New("portable graph exceeds record limit")
		}
		states[vertex] = 1
		for _, input := range vertex.inputVertices() {
			if err := visit(input); err != nil {
				return err
			}
		}
		states[vertex] = 2
		vertices = append(vertices, vertex)
		return nil
	}
	err := visit(root)
	return vertices, err
}

func (s *instructionSolver) stagePortable(vertex *instructionVertex, result *instructionSolverResult) error {
	if s.portable != nil && result != nil {
		root, err := vertex.portableDigest()
		if err != nil {
			return err
		}
		s.portable.mu.Lock()
		defer s.portable.mu.Unlock()
		s.portable.roots[root] = instructionGraphRoot{vertex: vertex, result: result.ID()}
		if !vertex.ignore {
			if vertex.executed.Load() {
				// Stage callbacks reuse instruction subgraphs; counting their
				// misses again would report execution that never happened.
				if vertex.run == nil {
					s.portable.cache.stats.Misses++
				}
			} else {
				s.portable.cache.stats.Hits++
			}
		}
	}
	return nil
}

func (s *instructionSolver) publishPortable(ctx context.Context) (err error) {
	if s == nil || s.portable == nil {
		return nil
	}
	p := s.portable
	if len(p.cache.writeStores) == 0 {
		return nil
	}
	ctx, complete := ObserveOperationPhase(ctx, OperationProgress{Phase: "export", Instruction: "cache"})
	defer func() { complete("portable", err) }()
	exports, err := newInstructionGraphExport(p.cache.stagingDir)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(exports.dir) }()
	p.mu.Lock()
	roots := make([]digest.Digest, 0, len(p.roots))
	selected := make(map[digest.Digest]instructionGraphRoot, len(p.roots))
	for root := range p.roots {
		roots = append(roots, root)
		selected[root] = p.roots[root]
	}
	sources := slices.Clone(p.sources)
	p.mu.Unlock()
	slices.Sort(roots)
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		writers, err := p.graphPublicationWriters(ctx, root, selected[root], sources)
		if err != nil {
			return err
		}
		if len(writers) == 0 {
			continue
		}
		record, layout, err := p.snapshot(ctx, root, selected[root], exports)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var invalid *instructionGraphInputIntegrityError
			if errors.As(err, &invalid) {
				// Reject the entire graph rather than publishing a subset or
				// turning an invalid optional read into a failed required build.
				p.cache.stats.Errors++
				packageCacheWarning("skip graph cache publication", fmt.Errorf("graph %s: %w", root, err))
				continue
			}
			return fmt.Errorf("stage graph cache: %w", err)
		}
		var writeErr error
		for _, candidate := range writers {
			store, ok := candidate.(instructionGraphStore)
			if !ok {
				continue
			}
			if _, err := store.PutGraph(ctx, root, record, layout); err != nil {
				p.cache.stats.Errors++
				writeErr = errors.Join(writeErr, err)
			} else {
				p.cache.stats.Stored++
			}
		}
		if writeErr != nil {
			return fmt.Errorf("write graph cache: %w", writeErr)
		}
	}
	return nil
}

func (p *instructionGraphPortable) graphPublicationWriters(ctx context.Context, root digest.Digest, selected instructionGraphRoot, sources []instructionGraphSource) ([]instructionCacheStore, error) {
	if selected.vertex == nil {
		return p.cache.writeStores, nil
	}
	vertices, err := instructionGraphOrder(selected.vertex)
	if err != nil {
		return nil, err
	}
	for _, vertex := range vertices {
		if vertex.ignore || vertex.executed.Load() {
			return p.cache.writeStores, nil
		}
	}
	pending := p.engine.results.lazy.get(selected.result)
	if pending != nil && pending.failed.Load() {
		return p.cache.writeStores, nil
	}
	value, err := p.engine.results.read(selected.result)
	if err != nil || value.Boundary != "" {
		return p.cache.writeStores, nil
	}
	if pending != nil && value.CreatedAt.Before(pending.value.CreatedAt) {
		value.CreatedAt = pending.value.CreatedAt
	}
	var image v1.Descriptor
	if pending != nil {
		image = pending.record.Image
	} else {
		data, err := p.executor.store.ImageBigData(value.Entry.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+value.Entry.ManifestDigest.String())
		if err != nil {
			return nil, err
		}
		image = v1.Descriptor{MediaType: manifest.GuessMIMEType(data), Digest: digest.FromBytes(data), Size: int64(len(data))}
		if image.Digest != value.Entry.ManifestDigest {
			return nil, errors.New("native graph publication manifest changed")
		}
	}
	value.Entry.ImageID = ""
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	current := cache.GraphResult{ID: selected.result, Image: image, Metadata: raw}
	writers := make([]instructionCacheStore, 0, len(p.cache.writeStores))
	for _, writer := range p.cache.writeStores {
		reuse := false
		if destination, ok := writer.(*cache.OCIStore); ok {
			for _, admitted := range sources {
				reuse, err = admitted.source.CanReuseAt(ctx, destination, root, current)
				if err != nil {
					return nil, err
				}
				if reuse {
					break
				}
			}
			if !reuse && pending == nil {
				reuse, err = destination.CanReuseGraph(ctx, root, current)
				if err != nil {
					return nil, err
				}
			}
		}
		if !reuse {
			writers = append(writers, writer)
		}
	}
	return writers, nil
}

func (p *instructionGraphPortable) snapshot(ctx context.Context, root digest.Digest, selected instructionGraphRoot, exports *instructionGraphExport) (cache.GraphRecord, string, error) {
	if selected.result == "" {
		return cache.GraphRecord{}, "", errors.New("portable graph has no selected result")
	}
	metadata := instructionGraphMetadata{}
	nodes := map[string]*instructionGraphNode{}
	// Reconstruct the selected graph from its authored vertices and forward
	// matches. Upstream WalkBacklinks returns output-normalized digests, not
	// the original operation digests required by AddLink during import.
	vertices, err := instructionGraphOrder(selected.vertex)
	if err != nil {
		return cache.GraphRecord{}, "", err
	}
	matched := map[*instructionVertex][]string{}
	for _, vertex := range vertices {
		inputs := make([][]string, len(vertex.inputVertices()))
		for slot, input := range vertex.inputVertices() {
			inputs[slot] = matched[input]
		}
		ids, err := instructionGraphLinkedIDs(p.engine.keys, vertex.key, inputs)
		if err != nil {
			return cache.GraphRecord{}, "", err
		}
		matched[vertex] = ids
		resultID := ""
		if vertex == selected.vertex {
			resultID = selected.result
		}
		if resultID == "" && vertex.completed != nil && vertex.completed.Boundary == "" {
			resultID = vertex.completed.ID()
		}
		// Immutable source/barrier vertices carry native filesystem boundaries,
		// not exportable operation results. A solved root always has an exact ID.
		includeResult := resultID != "" || vertex.source == nil
		associated := false
		for _, id := range ids {
			node := nodes[id]
			if node == nil {
				if len(nodes) >= 4096 {
					return cache.GraphRecord{}, "", errors.New("portable graph exceeds record limit")
				}
				node = &instructionGraphNode{ID: id}
				nodes[id] = node
			}
			if includeResult {
				result, err := p.snapshotResult(id, resultID)
				if err != nil {
					return cache.GraphRecord{}, "", err
				}
				if result != nil {
					associated = true
					if !slices.Contains(node.Results, *result) {
						node.Results = append(node.Results, *result)
					}
				}
			}
			for slot, parents := range inputs {
				link := solver.CacheInfoLink{Input: solver.Index(slot), Digest: vertex.key}
				for _, parent := range parents {
					if p.engine.keys.HasLink(parent, link, id) {
						input := instructionGraphInput{Source: parent, Link: link}
						if !slices.Contains(node.Inputs, input) {
							node.Inputs = append(node.Inputs, input)
						}
					}
				}
			}
		}
		if resultID != "" && !associated {
			return cache.GraphRecord{}, "", fmt.Errorf("selected graph result %s has no persisted key association", resultID)
		}
	}
	metadata.Roots = matched[selected.vertex]
	if len(metadata.Roots) == 0 {
		return cache.GraphRecord{}, "", errors.New("solver result has no persisted graph association")
	}
	for _, node := range nodes {
		sort.Slice(node.Inputs, func(i, j int) bool {
			a, _ := json.Marshal(node.Inputs[i])
			b, _ := json.Marshal(node.Inputs[j])
			return bytes.Compare(a, b) < 0
		})
		slices.SortFunc(node.Results, func(a, b solver.CacheResult) int { return bytes.Compare([]byte(a.ID), []byte(b.ID)) })
		metadata.Nodes = append(metadata.Nodes, *node)
	}
	slices.SortFunc(metadata.Nodes, func(a, b instructionGraphNode) int { return bytes.Compare([]byte(a.ID), []byte(b.ID)) })
	record := cache.GraphRecord{Version: cache.GraphRecordVersion, CreatedAt: time.Now().UTC(), Root: root}
	seen := map[string]bool{}
	for _, node := range metadata.Nodes {
		for _, result := range node.Results {
			if seen[result.ID] {
				continue
			}
			seen[result.ID] = true
			portable, err := exports.result(ctx, p, result.ID)
			if err != nil {
				return cache.GraphRecord{}, "", err
			}
			record.Results = append(record.Results, portable)
		}
	}
	record.Metadata, err = json.Marshal(metadata)
	if err != nil {
		return cache.GraphRecord{}, "", err
	}
	if err := record.Validate(root); err != nil {
		return cache.GraphRecord{}, "", err
	}
	// PutGraph verifies the selected closure in the invocation-owned export layout.
	if err := ctx.Err(); err != nil {
		return cache.GraphRecord{}, "", err
	}
	return record, filepath.Join(exports.dir, "layout"), nil
}

// Export solved outputs rather than every result ever associated with a key.
// Ancestors bypassed by a cache hit may have no completed vertex result; follow
// upstream remote-cache selection there by choosing the newest valid result.
// Selection reads metadata only. The exporter verifies the chosen OCI closure.
func (p *instructionGraphPortable) snapshotResult(key, selected string) (*solver.CacheResult, error) {
	if selected != "" {
		result, err := p.engine.keys.Load(key, selected)
		if errors.Is(err, solver.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		value, err := p.engine.results.describe(selected)
		if err != nil {
			return nil, err
		}
		if value.Boundary != "" {
			return nil, nil
		}
		return &result, nil
	}
	var newest *solver.CacheResult
	err := p.engine.keys.WalkResults(key, func(result solver.CacheResult) error {
		value, err := p.engine.results.describe(result.ID)
		if err != nil || value.Boundary != "" {
			return nil
		}
		if newest == nil || result.CreatedAt.After(newest.CreatedAt) || result.CreatedAt.Equal(newest.CreatedAt) && result.ID < newest.ID {
			newest = &result
		}
		return nil
	})
	return newest, err
}

func instructionGraphKeyIDs(keys solver.CacheKeyStorage, vertex *instructionVertex) ([]string, error) {
	vertices, err := instructionGraphOrder(vertex)
	if err != nil {
		return nil, err
	}
	matched := map[*instructionVertex][]string{}
	for _, current := range vertices {
		inputs := make([][]string, len(current.inputVertices()))
		for slot, input := range current.inputVertices() {
			inputs[slot] = matched[input]
		}
		matched[current], err = instructionGraphLinkedIDs(keys, current.key, inputs)
		if err != nil {
			return nil, err
		}
	}
	return matched[vertex], nil
}

func instructionGraphLinkedIDs(keys solver.CacheKeyStorage, key digest.Digest, inputs [][]string) ([]string, error) {
	if len(inputs) == 0 {
		return []string{solver.NewCacheKey(key, "", 0).ID}, nil
	}
	var candidates map[string]bool
	for slot, parents := range inputs {
		found := map[string]bool{}
		for _, parent := range parents {
			if err := keys.WalkLinks(parent, solver.CacheInfoLink{Input: solver.Index(slot), Digest: key}, func(id string) error {
				if slot == 0 || candidates[id] {
					found[id] = true
				}
				return nil
			}); err != nil {
				return nil, err
			}
		}
		candidates = found
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func decodeInstructionGraphJSON(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > 8<<20 {
		return errors.New("native graph metadata outside size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("native graph metadata contains trailing data")
	}
	return nil
}

func (p *instructionGraphPortable) restoreSource(ctx context.Context, source *cache.GraphSource) error {
	record, err := source.Record()
	if err != nil {
		return err
	}
	return p.restoreRecord(ctx, &record, "", source, p.executor != nil && p.executor.options.eagerPortableImports)
}

// restoreRecord accepts complete owned layouts or admitted deferred sources.
func (p *instructionGraphPortable) restoreRecord(ctx context.Context, record *cache.GraphRecord, layout string, source *cache.GraphSource, eager bool) error {
	if record == nil || layout == "" && source == nil {
		return errors.New("incomplete portable graph")
	}
	metadata, err := record.NativeMetadata(record.Root)
	if err != nil {
		return err
	}
	// Validate every opaque result before importing images or exposing links.
	results := make([]*instructionSolverResult, len(record.Results))
	for i, result := range record.Results {
		var r instructionSolverResult
		if err := decodeInstructionGraphJSON(result.Metadata, &r); err != nil {
			return err
		}
		if r.ID() != result.ID || r.Entry.ManifestDigest != result.Image.Digest || r.Entry.ImageID != "" || r.ParentLayers < 0 || r.ParentLayers > 2048 || r.CreatedAt.IsZero() || result.BaseImage != nil || validateInstructionResultState(&r) != nil {
			return errors.New("portable graph result identity or restoration metadata differs")
		}
		if r.Boundary != "" && r.Boundary.Validate() != nil {
			return errors.New("invalid portable graph result boundary")
		}
		if _, valid := cacheablePackageRootMetadata(r.Entry.RootMetadata); !valid {
			return errors.New("missing or nonportable graph root metadata")
		}
		results[i] = &r
	}
	if eager {
		if source != nil {
			var err error
			layout, err = source.EnsureAll(ctx)
			if err != nil {
				return err
			}
		}
		return p.restoreEager(ctx, metadata, record, results, layout)
	}
	pending := make([]*instructionPortableResult, len(record.Results))
	for i, result := range record.Results {
		item := &instructionPortableResult{value: *results[i], record: result, layout: layout, source: source, owner: p, gate: make(chan struct{}, 1)}
		item.gate <- struct{}{}
		item.importImage = func(ctx context.Context) (string, error) {
			selected, chosen, err := item.ensureLayout(ctx)
			if err != nil {
				return "", err
			}
			image, err := importInstructionCacheImage(ctx, p.executor.store, p.executor.system, selected, result.Image)
			if err == nil {
				chosen.MarkUsed(result.ID)
			}
			return image, err
		}
		pending[i] = item
	}
	// Persist links, but expose unresolved result associations only through the
	// invocation overlay. Native associations become durable after image import.
	for _, node := range metadata.Nodes {
		for _, input := range node.Inputs {
			if err := p.engine.keys.AddLink(input.Source, input.Link, node.ID); err != nil {
				return err
			}
		}
	}
	if err := p.engine.results.lazy.add(pending, metadata.Nodes); err != nil {
		return err
	}
	return p.engine.keys.Err()
}

// Eager restoration is retained only for sources that cannot be rebound to a
// parent-approved read binding. Ordinary isolated workers relay owned metadata
// and selected native results instead of importing every historical image.
func (p *instructionGraphPortable) restoreEager(ctx context.Context, metadata instructionGraphMetadata, record *cache.GraphRecord, results []*instructionSolverResult, layout string) error {
	return p.restoreEagerImages(ctx, metadata, record, results, func(ctx context.Context, result cache.GraphResult) (string, error) {
		return importInstructionCacheImage(ctx, p.executor.store, p.executor.system, layout, result.Image)
	})
}

func (p *instructionGraphPortable) restoreEagerImages(ctx context.Context, metadata instructionGraphMetadata, record *cache.GraphRecord, results []*instructionSolverResult, importImage func(context.Context, cache.GraphResult) (string, error)) error {
	materialized := make(map[string]solver.CacheResult, len(record.Results))
	for i, result := range record.Results {
		native, err := p.engine.results.materializePortable(ctx, results[i], func(ctx context.Context) (string, error) {
			return importImage(ctx, result)
		})
		if err != nil {
			return err
		}
		materialized[result.ID] = solver.CacheResult{ID: result.ID, CreatedAt: native.CreatedAt}
	}
	for _, node := range metadata.Nodes {
		for _, result := range node.Results {
			if saved := materialized[result.ID]; result.CreatedAt.Before(saved.CreatedAt) {
				result.CreatedAt = saved.CreatedAt
			}
			if err := p.engine.keys.AddResult(node.ID, result); err != nil {
				return err
			}
		}
		for _, input := range node.Inputs {
			if err := p.engine.keys.AddLink(input.Source, input.Link, node.ID); err != nil {
				return err
			}
		}
	}
	return p.engine.keys.Err()
}
