package buildah

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
)

const maxInstructionRelayDepth = 512

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
