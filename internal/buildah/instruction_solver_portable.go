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
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	cachetypes "github.com/moby/buildkit/cache/remotecache/v1/types"
	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// Portable metadata retains native key IDs and unnormalized operation links.
// BuildKit's portable graph uses output-normalized digests, which are not
// reversible. Both representations are validated against each other on import.
type instructionGraphMetadata struct {
	Roots []string               `json:"roots"`
	Nodes []instructionGraphNode `json:"nodes"`
}
type instructionGraphNode struct {
	ID      string                  `json:"id"`
	Inputs  []instructionGraphInput `json:"inputs,omitempty"`
	Results []solver.CacheResult    `json:"results,omitempty"`
}
type instructionGraphInput struct {
	Source string               `json:"source"`
	Link   solver.CacheInfoLink `json:"link"`
}
type instructionGraphStore interface {
	LookupGraph(context.Context, digest.Digest) (*cache.GraphRecord, string, error)
	PutGraph(context.Context, digest.Digest, cache.GraphRecord, string) (v1.Descriptor, error)
}
type instructionGraphPortable struct {
	mu         sync.Mutex
	executor   *graphExecutor
	engine     *instructionSolver
	cache      *portableInstructionCache
	roots      map[digest.Digest]instructionGraphRoot
	read       map[digest.Digest]bool
	readLayout *orasoci.Store
	readPath   string
}
type instructionGraphRoot struct {
	vertex *instructionVertex
	result string
}

func newInstructionGraphPortable(_ context.Context, executor *graphExecutor, engine *instructionSolver) (*instructionGraphPortable, error) {
	if executor.instructionPortableCache == nil {
		return nil, nil
	}
	return &instructionGraphPortable{executor: executor, engine: engine, cache: executor.instructionPortableCache, roots: map[digest.Digest]instructionGraphRoot{}, read: map[digest.Digest]bool{}}, nil
}

func (s *instructionSolver) closePortable() error {
	if s.portable == nil || s.portable.readPath == "" {
		return nil
	}
	return os.RemoveAll(s.portable.readPath)
}

// Keep one private read layout per invocation: graph prefixes commonly contain
// the same ancestor variants. Holding every LookupGraph layout until close
// would otherwise duplicate their blobs quadratically across a long stage.
func (p *instructionGraphPortable) retainImages(ctx context.Context, record *cache.GraphRecord, layout string) (string, error) {
	p.mu.Lock()
	if p.readLayout == nil {
		dir, err := os.MkdirTemp(p.cache.stagingDir, "graph-read-*")
		if err != nil {
			p.mu.Unlock()
			return "", err
		}
		target, err := orasoci.New(dir)
		if err != nil {
			p.mu.Unlock()
			return "", errors.Join(err, os.RemoveAll(dir))
		}
		p.readLayout, p.readPath = target, dir
	}
	target, path := p.readLayout, p.readPath
	p.mu.Unlock()
	source, err := orasoci.New(layout)
	if err != nil {
		return "", err
	}
	for _, result := range record.Results {
		if err := oras.CopyGraph(ctx, source, target, result.Image, oras.DefaultCopyGraphOptions); err != nil {
			return "", err
		}
		if err := target.Tag(ctx, result.Image, result.Image.Digest.String()); err != nil {
			return "", err
		}
	}
	return path, nil
}

func (s *instructionSolver) preparePortable(ctx context.Context, vertex *instructionVertex) error {
	p := s.portable
	if p == nil {
		return nil
	}
	root := vertex.portableDigest()
	p.mu.Lock()
	if p.read[root] {
		p.mu.Unlock()
		return nil
	}
	p.read[root] = true
	p.mu.Unlock()
	for _, candidate := range p.cache.readStores {
		store, ok := candidate.(instructionGraphStore)
		if !ok {
			continue
		}
		record, layout, err := store.LookupGraph(ctx, root)
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
		err = p.restore(ctx, record, layout)
		err = errors.Join(err, os.RemoveAll(layout))
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
	}
	return nil
}

func (s *instructionSolver) stagePortable(vertex *instructionVertex, result *instructionSolverResult) {
	if s.portable != nil && result != nil {
		s.portable.mu.Lock()
		defer s.portable.mu.Unlock()
		s.portable.roots[vertex.portableDigest()] = instructionGraphRoot{vertex: vertex, result: result.ID()}
		if !vertex.ignore {
			if vertex.executed.Load() {
				s.portable.cache.stats.Misses++
			} else {
				s.portable.cache.stats.Hits++
			}
		}
	}
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
	p.mu.Unlock()
	slices.Sort(roots)
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, layout, err := p.snapshot(ctx, root, selected[root], exports)
		if err != nil {
			return fmt.Errorf("stage graph cache: %w", err)
		}
		var writeErr error
		for _, candidate := range p.cache.writeStores {
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
		writeErr = errors.Join(writeErr, os.RemoveAll(filepath.Dir(layout)))
		if writeErr != nil {
			return fmt.Errorf("write graph cache: %w", writeErr)
		}
	}
	return nil
}

func (p *instructionGraphPortable) snapshot(ctx context.Context, root digest.Digest, selected instructionGraphRoot, exports *instructionGraphExport) (cache.GraphRecord, string, error) {
	metadata := instructionGraphMetadata{}
	nodes := map[string]*instructionGraphNode{}
	// Reconstruct the selected graph from its authored vertices and forward
	// matches. Upstream WalkBacklinks returns output-normalized digests, not
	// the original operation digests required by AddLink during import.
	var visit func(*instructionVertex) ([]string, error)
	visit = func(vertex *instructionVertex) ([]string, error) {
		if vertex == nil {
			return nil, errors.New("portable graph has no vertex")
		}
		var parents []string
		var err error
		if vertex.parent != nil {
			parents, err = visit(vertex.parent)
			if err != nil {
				return nil, err
			}
		}
		ids, err := instructionGraphKeyIDs(p.engine.keys, vertex)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			node := nodes[id]
			if node == nil {
				if len(nodes) >= 4096 {
					return nil, errors.New("portable graph exceeds record limit")
				}
				node = &instructionGraphNode{ID: id}
				nodes[id] = node
				if err := p.engine.keys.WalkResults(id, func(result solver.CacheResult) error {
					r, err := p.engine.results.describe(result.ID)
					if err == nil && r.Boundary == "" {
						node.Results = append(node.Results, result)
					}
					return nil
				}); err != nil {
					return nil, err
				}
			}
			link := solver.CacheInfoLink{Digest: vertex.key}
			for _, parent := range parents {
				if p.engine.keys.HasLink(parent, link, id) {
					input := instructionGraphInput{Source: parent, Link: link}
					if !slices.Contains(node.Inputs, input) {
						node.Inputs = append(node.Inputs, input)
					}
				}
			}
		}
		return ids, nil
	}
	var graphErr error
	metadata.Roots, graphErr = visit(selected.vertex)
	if graphErr != nil {
		return cache.GraphRecord{}, "", graphErr
	}
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
	dir, err := os.MkdirTemp(p.cache.stagingDir, "graph-*")
	if err != nil {
		return cache.GraphRecord{}, "", err
	}
	layout := filepath.Join(dir, "layout")
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	destination, err := orasoci.New(layout)
	if err != nil {
		return cache.GraphRecord{}, "", err
	}
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
			if err := oras.CopyGraph(ctx, exports.layout, destination, portable.Image, oras.DefaultCopyGraphOptions); err != nil {
				return cache.GraphRecord{}, "", err
			}
			record.Results = append(record.Results, portable)
		}
	}
	record.Metadata, err = json.Marshal(metadata)
	if err != nil {
		return cache.GraphRecord{}, "", err
	}
	record.Config, err = instructionGraphConfig(metadata, record.Results)
	if err != nil {
		return cache.GraphRecord{}, "", err
	}
	if err := record.Validate(root); err != nil {
		return cache.GraphRecord{}, "", err
	}
	keep = true
	return record, layout, nil
}

func instructionGraphKeyIDs(keys solver.CacheKeyStorage, vertex *instructionVertex) ([]string, error) {
	if vertex.parent == nil {
		return []string{solver.NewCacheKey(vertex.key, "", 0).ID}, nil
	}
	parents, err := instructionGraphKeyIDs(keys, vertex.parent)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, parent := range parents {
		if err := keys.WalkLinks(parent, solver.CacheInfoLink{Digest: vertex.key}, func(id string) error {
			ids = append(ids, id)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

func instructionGraphConfig(metadata instructionGraphMetadata, results []cache.GraphResult) (cachetypes.CacheConfig, error) {
	var config cachetypes.CacheConfig
	indexes := map[string]int{}
	for i, node := range metadata.Nodes {
		if node.ID == "" || len(node.ID) > 1024 {
			return config, errors.New("invalid native graph key")
		}
		if _, exists := indexes[node.ID]; exists {
			return config, errors.New("duplicate native graph key")
		}
		indexes[node.ID] = i
	}
	layers := map[string]int{}
	for _, result := range results {
		desc, err := result.Descriptor()
		if err != nil {
			return config, err
		}
		layers[result.ID] = len(config.Layers)
		config.Layers = append(config.Layers, cachetypes.CacheLayer{Blob: desc.Digest, ParentIndex: -1})
	}
	for _, node := range metadata.Nodes {
		record := cachetypes.CacheRecord{}
		if len(node.Inputs) == 0 {
			record.Digest = digest.Digest(node.ID)
			if record.Digest.Validate() != nil || record.Digest.Algorithm() != digest.SHA256 {
				return config, errors.New("invalid native graph source key")
			}
		}
		for _, input := range node.Inputs {
			index, exists := indexes[input.Source]
			if !exists || input.Link.Input < 0 || input.Link.Input > 64 || input.Link.Output != 0 || input.Link.Digest.Validate() != nil {
				return config, errors.New("invalid native graph dependency")
			}
			if input.Link.Selector != "" && input.Link.Selector.Validate() != nil {
				return config, errors.New("invalid native graph selector")
			}
			normalized := digest.Digest(solver.NewCacheKey(input.Link.Digest, "", input.Link.Output).ID)
			if record.Digest != "" && record.Digest != normalized {
				return config, errors.New("native graph key has conflicting operation identities")
			}
			record.Digest = normalized
			for len(record.Inputs) <= int(input.Link.Input) {
				record.Inputs = append(record.Inputs, nil)
			}
			record.Inputs[input.Link.Input] = append(record.Inputs[input.Link.Input], cachetypes.CacheInput{Selector: input.Link.Selector.String(), LinkIndex: index})
		}
		for _, result := range node.Results {
			index, ok := layers[result.ID]
			if !ok {
				return config, errors.New("native graph references missing result")
			}
			record.Results = append(record.Results, cachetypes.CacheResult{LayerIndex: index, CreatedAt: result.CreatedAt})
		}
		config.Records = append(config.Records, record)
	}
	for _, id := range metadata.Roots {
		if _, ok := indexes[id]; !ok {
			return config, errors.New("native graph root is missing")
		}
	}
	if len(metadata.Roots) == 0 {
		return config, errors.New("native graph has no roots")
	}
	return config, nil
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

func (p *instructionGraphPortable) restore(ctx context.Context, record *cache.GraphRecord, layout string) error {
	if record == nil || layout == "" {
		return errors.New("incomplete portable graph")
	}
	if err := record.Validate(record.Root); err != nil {
		return err
	}
	var metadata instructionGraphMetadata
	if err := decodeInstructionGraphJSON(record.Metadata, &metadata); err != nil {
		return err
	}
	if len(metadata.Nodes) == 0 || len(metadata.Nodes) > 4096 || len(metadata.Roots) > 4096 {
		return errors.New("native graph count outside limits")
	}
	config, err := instructionGraphConfig(metadata, record.Results)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(config, record.Config) {
		return errors.New("native graph differs from portable dependency graph")
	}
	// Validate every opaque result before importing images or exposing links.
	results := make([]*instructionSolverResult, len(record.Results))
	for i, result := range record.Results {
		var r instructionSolverResult
		if err := decodeInstructionGraphJSON(result.Metadata, &r); err != nil {
			return err
		}
		if r.ID() != result.ID || r.Entry.ManifestDigest != result.Image.Digest || r.Entry.ImageID != "" || r.ParentLayers < 0 || r.ParentLayers > 2048 || r.CreatedAt.IsZero() || result.BaseImage != nil || r.Entry.BaseImageID != "" || len(r.Entry.LogicalConfig) != 0 || r.Entry.CommandSet != nil || r.Entry.RetainsCaller != nil {
			return errors.New("portable graph result identity or restoration metadata differs")
		}
		if r.Boundary != "" && r.Boundary.Validate() != nil {
			return errors.New("invalid portable graph result boundary")
		}
		if r.Entry.RootMetadata != nil {
			if _, valid := cacheablePackageRootMetadata(r.Entry.RootMetadata); !valid {
				return errors.New("nonportable graph root metadata")
			}
		}
		results[i] = &r
	}
	if p.executor.options.eagerPortableImports {
		return p.restoreEager(ctx, metadata, record, results, layout)
	}
	retained, err := p.retainImages(ctx, record, layout)
	if err != nil {
		return err
	}
	layout = retained
	pending := make([]*instructionPortableResult, len(record.Results))
	for i, result := range record.Results {
		item := &instructionPortableResult{value: *results[i], record: result, layout: layout, owner: p, gate: make(chan struct{}, 1)}
		item.gate <- struct{}{}
		item.importImage = func(ctx context.Context) (string, error) {
			return importInstructionCacheImage(ctx, p.executor.store, p.executor.system, layout, result.Image)
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
	p.engine.results.lazy.add(pending, metadata.Nodes)
	return p.engine.keys.Err()
}

// Isolated stage workers relay durable native results, not owned read layouts.
// Keep their established eager policy so every historical variant survives
// child shutdown and remains available for the parent's success-gated export.
func (p *instructionGraphPortable) restoreEager(ctx context.Context, metadata instructionGraphMetadata, record *cache.GraphRecord, results []*instructionSolverResult, layout string) error {
	for i, result := range record.Results {
		importCtx, complete := ObserveOperationPhase(ctx, OperationProgress{Phase: "restore", Instruction: "cache"})
		image, err := importInstructionCacheImage(importCtx, p.executor.store, p.executor.system, layout, result.Image)
		complete("portable", err)
		if err != nil {
			return err
		}
		results[i].Entry.ImageID = image
		if _, err := p.engine.results.Save(results[i], results[i].CreatedAt); err != nil {
			return err
		}
		if _, err := p.engine.results.read(result.ID); err != nil {
			return fmt.Errorf("verify imported graph result: %w", err)
		}
	}
	for _, node := range metadata.Nodes {
		for _, result := range node.Results {
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
