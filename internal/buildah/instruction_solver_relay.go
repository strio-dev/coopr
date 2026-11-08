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
	"strings"
	"syscall"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/cache"
	"golang.org/x/sys/unix"
)

const maxInstructionRelayDepth = 512

const instructionGraphSourcesName = "graph-sources.json"

type instructionGraphSource struct {
	binding digest.Digest
	source  *cache.GraphSource
}

// Binding indexes select only the read stores supplied by this parent. The
// envelope carries immutable metadata, never worker-supplied source authority.
type instructionGraphSourceRelay struct {
	Binding  int                       `json:"binding"`
	Root     digest.Digest             `json:"root"`
	Metadata cache.GraphSourceMetadata `json:"metadata"`
}

type instructionGraphSourceFile struct {
	Digest digest.Digest `json:"digest"`
	Size   int64         `json:"size"`
}

func instructionReadBindingID(spec CacheSpec) digest.Digest {
	return digest.FromString("coopr-read-cache-binding-v1\n" + spec.Transport + "\n" + spec.Reference)
}

func (p *instructionGraphPortable) rememberSource(binding digest.Digest, source *cache.GraphSource) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, previous := range p.sources {
		if previous.binding == binding && previous.source == source {
			return
		}
	}
	p.sources = append(p.sources, instructionGraphSource{binding: binding, source: source})
}

func (s *instructionSolver) relayInstructionSources(bindings []digest.Digest) ([]instructionGraphSourceRelay, error) {
	if s == nil || s.portable == nil {
		return nil, nil
	}
	p := s.portable
	p.mu.Lock()
	sources := slices.Clone(p.sources)
	p.mu.Unlock()
	if len(sources) > 4096 {
		return nil, errors.New("too many relayed graph sources")
	}
	relays := make([]instructionGraphSourceRelay, 0, len(sources))
	for _, source := range sources {
		index := slices.Index(bindings, source.binding)
		if index < 0 || !validInstructionRelayDigest(source.binding) {
			return nil, errors.New("graph source has no parent-approved read binding")
		}
		record, err := source.source.Record()
		if err != nil {
			return nil, err
		}
		relays = append(relays, instructionGraphSourceRelay{Binding: index, Root: record.Root, Metadata: source.source.Metadata()})
	}
	return relays, nil
}

func (s *instructionSolver) acceptInstructionSources(ctx context.Context, relays []instructionGraphSourceRelay) error {
	if len(relays) == 0 {
		return nil
	}
	if s == nil || s.portable == nil || len(relays) > 4096 {
		return errors.New("graph sources have no owner or exceed limits")
	}
	p := s.portable
	reader, err := p.graphReader()
	if err != nil {
		return err
	}
	for _, relay := range relays {
		if relay.Binding < 0 || relay.Binding >= len(p.cache.readStores) || relay.Binding >= len(p.cache.readBindingIDs) || !validInstructionRelayDigest(relay.Root) {
			return errors.New("invalid parent-approved graph source binding")
		}
		store, ok := p.cache.readStores[relay.Binding].(*cache.OCIStore)
		if !ok {
			return errors.New("relayed graph source binding is not an OCI store")
		}
		source, err := reader.RestoreMetadata(ctx, store, relay.Root, relay.Metadata)
		if err != nil {
			return fmt.Errorf("validate relayed graph metadata: %w", err)
		}
		if err := p.restoreSource(ctx, source); err != nil {
			return fmt.Errorf("restore relayed graph metadata: %w", err)
		}
		p.rememberSource(p.cache.readBindingIDs[relay.Binding], source)
	}
	return nil
}

func writeInstructionGraphSources(resultPath string, relays []instructionGraphSourceRelay) (*instructionGraphSourceFile, error) {
	if len(relays) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(relays)
	if err != nil {
		return nil, err
	}
	if len(data) > workerMaxJSON {
		return nil, errors.New("graph source handoff exceeds size limit")
	}
	path := filepath.Join(filepath.Dir(resultPath), instructionGraphSourcesName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return nil, err
	}
	return &instructionGraphSourceFile{Digest: digest.FromBytes(data), Size: int64(len(data))}, nil
}

func readInstructionGraphSources(resultPath string, summary *instructionGraphSourceFile) ([]instructionGraphSourceRelay, error) {
	if summary == nil {
		return nil, nil
	}
	if !validInstructionRelayDigest(summary.Digest) || summary.Size <= 0 || summary.Size > workerMaxJSON {
		return nil, errors.New("invalid graph source handoff summary")
	}
	path := filepath.Join(filepath.Dir(resultPath), instructionGraphSourcesName)
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || owner.Uid != uint32(os.Getuid()) || owner.Nlink != 1 || info.Size() != summary.Size {
		return nil, errors.New("unsafe graph source handoff file")
	}
	data, err := io.ReadAll(io.LimitReader(file, workerMaxJSON+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != summary.Size || digest.FromBytes(data) != summary.Digest {
		return nil, errors.New("graph source handoff bytes differ from summary")
	}
	var relays []instructionGraphSourceRelay
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&relays); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("graph source handoff contains trailing data")
	}
	if len(relays) == 0 || len(relays) > 4096 {
		return nil, errors.New("graph source handoff count outside limits")
	}
	return relays, nil
}

// A stage worker shares the native image store and durable result metadata with
// its parent. The relay carries graph identity, rather than another image copy.
type instructionVertexRelay struct {
	Root   digest.Digest                `json:"root"`
	Serial uint64                       `json:"serial,omitempty"`
	Nodes  []instructionVertexRelayNode `json:"nodes"`
}

type instructionVertexRelayNode struct {
	Key      digest.Digest `json:"key"`
	Digest   digest.Digest `json:"digest"`
	Name     string        `json:"name"`
	Ignore   bool          `json:"ignore,omitempty"`
	ResultID string        `json:"result_id"`
}

type instructionGraphRootRelay struct {
	Root     digest.Digest           `json:"root"`
	Vertex   *instructionVertexRelay `json:"vertex"`
	ResultID string                  `json:"result_id"`
}

func encodeInstructionVertexRelay(vertex *instructionVertex) (*instructionVertexRelay, error) {
	if vertex == nil {
		return nil, nil
	}
	relay := &instructionVertexRelay{}
	if vertex.owner != nil {
		relay.Serial = vertex.owner.serial.Load()
	}
	seen := map[*instructionVertex]bool{}
	for current := vertex; current != nil; current = current.parent {
		if seen[current] || len(relay.Nodes) >= maxInstructionRelayDepth {
			return nil, errors.New("instruction graph relay is cyclic or too deep")
		}
		seen[current] = true
		result := current.completed
		if result == nil {
			result = current.source
		}
		if result == nil {
			return nil, errors.New("instruction graph relay contains an incomplete vertex")
		}
		if current.completed == nil && current.source != nil {
			// A downstream cache hit can match source keys without executing
			// that source. Scratch has no portable image sidecar, so ensure its
			// cheap immutable metadata exists before relaying the result ID.
			if current.owner == nil || current.owner.results == nil {
				return nil, errors.New("instruction graph relay source has no result storage")
			}
			results := *current.owner.results
			results.ttl = nil
			if _, err := results.read(result.ID()); err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("validate instruction graph relay source: %w", err)
				}
				created := result.CreatedAt
				if created.IsZero() {
					created = time.Now()
				}
				if _, err := results.Save(result, created); err != nil {
					return nil, fmt.Errorf("persist instruction graph relay source: %w", err)
				}
			}
		}
		relay.Nodes = append(relay.Nodes, instructionVertexRelayNode{
			Key: current.key, Digest: current.dgst, Name: current.name,
			Ignore: current.ignore, ResultID: result.ID(),
		})
	}
	slices.Reverse(relay.Nodes)
	relay.Root = vertex.portableDigest()
	return relay, nil
}

func validInstructionRelayDigest(value digest.Digest) bool {
	return value.Validate() == nil && value.Algorithm() == digest.SHA256
}

func (s *instructionSolver) restoreInstructionVertexRelay(relay *instructionVertexRelay) (*instructionVertex, error) {
	if relay == nil {
		return nil, nil
	}
	if s == nil || s.results == nil {
		return nil, errors.New("instruction graph relay has no owning solver")
	}
	if len(relay.Nodes) == 0 || len(relay.Nodes) > maxInstructionRelayDepth || !validInstructionRelayDigest(relay.Root) || relay.Serial > 1<<32 {
		return nil, errors.New("invalid instruction graph relay shape")
	}
	// TTL controls future cache reuse. It cannot invalidate the immutable
	// output of a stage that just completed in this same invocation.
	results := *s.results
	results.ttl = nil
	var parent *instructionVertex
	forced := false
	for index, node := range relay.Nodes {
		if !validInstructionRelayDigest(node.Key) || !validInstructionRelayDigest(node.Digest) || !validInstructionRelayDigest(digest.Digest(node.ResultID)) || len(node.Name) > 4096 || strings.ContainsRune(node.Name, '\x00') {
			return nil, errors.New("invalid instruction graph relay vertex")
		}
		normal := node.Key
		if parent != nil {
			normal = digest.FromString(instructionSolverVersion + "\n" + node.Key.String() + "\n" + parent.Digest().String())
		}
		if (!node.Ignore && node.Digest != normal) || (node.Ignore && node.Digest == normal) {
			return nil, errors.New("instruction graph relay force status differs from vertex identity")
		}
		forced = forced || node.Ignore
		result, err := results.read(node.ResultID)
		if err != nil {
			return nil, fmt.Errorf("load completed instruction graph relay result: %w", err)
		}
		if index == 0 && result.Boundary != node.Key {
			return nil, errors.New("instruction graph relay source differs from its result")
		}
		// Completed upstream operations return their selected whole image if
		// the new solver needs to materialize them. Their original IgnoreCache
		// flags still force newly admitted consumers to execute.
		parent = &instructionVertex{owner: s, key: node.Key, dgst: node.Digest, name: node.Name, ignore: node.Ignore, parent: parent, source: result, completed: result}
	}
	if forced && relay.Serial == 0 {
		return nil, errors.New("forced instruction graph relay lacks execution serial")
	}
	if parent.portableDigest() != relay.Root {
		return nil, errors.New("instruction graph relay root differs from its vertices")
	}
	for previous := s.serial.Load(); previous < relay.Serial; previous = s.serial.Load() {
		if s.serial.CompareAndSwap(previous, relay.Serial) {
			break
		}
	}
	return parent, nil
}

func (s *instructionSolver) relayInstructionGraphs() ([]instructionGraphRootRelay, error) {
	if s == nil || s.portable == nil {
		return nil, nil
	}
	p := s.portable
	p.mu.Lock()
	roots := make(map[digest.Digest]instructionGraphRoot, len(p.roots))
	for root, selected := range p.roots {
		roots[root] = selected
	}
	p.mu.Unlock()
	ordered := make([]digest.Digest, 0, len(roots))
	for root := range roots {
		ordered = append(ordered, root)
	}
	slices.Sort(ordered)
	relays := make([]instructionGraphRootRelay, 0, len(roots))
	for _, root := range ordered {
		selected := roots[root]
		vertex, err := encodeInstructionVertexRelay(selected.vertex)
		if err != nil {
			return nil, err
		}
		if vertex == nil || vertex.Root != root || vertex.Nodes[len(vertex.Nodes)-1].ResultID != selected.result {
			return nil, errors.New("instruction graph root relay differs from completed result")
		}
		relays = append(relays, instructionGraphRootRelay{Root: root, Vertex: vertex, ResultID: selected.result})
	}
	return relays, nil
}

func (s *instructionSolver) acceptInstructionGraphs(relays []instructionGraphRootRelay) error {
	if len(relays) == 0 {
		return nil
	}
	if s == nil || s.portable == nil || len(relays) > 4096 {
		return errors.New("instruction graph roots have no owning portable cache or exceed limits")
	}
	selected := make(map[digest.Digest]instructionGraphRoot, len(relays))
	for _, relay := range relays {
		if relay.Vertex == nil || relay.Root != relay.Vertex.Root || !validInstructionRelayDigest(relay.Root) {
			return errors.New("invalid instruction graph root relay")
		}
		if _, duplicate := selected[relay.Root]; duplicate {
			return errors.New("duplicate instruction graph root relay")
		}
		vertex, err := s.restoreInstructionVertexRelay(relay.Vertex)
		if err != nil {
			return err
		}
		if vertex.completed.ID() != relay.ResultID {
			return errors.New("instruction graph root relay result differs from endpoint")
		}
		selected[relay.Root] = instructionGraphRoot{vertex: vertex, result: relay.ResultID}
	}
	p := s.portable
	p.mu.Lock()
	defer p.mu.Unlock()
	for root, result := range selected {
		p.roots[root] = result
	}
	return nil
}
