package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	GraphRecordVersion   = "coopr.strio.dev/graph-cache-record/v3"
	GraphArtifactType    = "application/vnd.coopr.strio.dev.graph-cache.v3+json"
	GraphConfigMediaType = "application/vnd.coopr.strio.dev.graph-cache.config.v3+json"
	maxGraphRecordBytes  = 8 << 20
	maxGraphEntries      = 4096
	maxGraphDepth        = 512
)

// GraphResult retains a complete immutable image and executor-owned restoration
// metadata, bound together by the graph record's config digest. Identical images
// may have distinct restoration metadata.
type GraphResult struct {
	ID        string          `json:"id"`
	Image     v1.Descriptor   `json:"image"`
	BaseImage *v1.Descriptor  `json:"base_image,omitempty"`
	Metadata  json.RawMessage `json:"metadata"`
}

func (r GraphResult) validate() error {
	if r.ID == "" || len(r.ID) > 1024 || strings.ContainsRune(r.ID, '\x00') {
		return errors.New("invalid graph result ID")
	}
	if err := validGraphImage(r.Image); err != nil {
		return err
	}
	if r.BaseImage != nil {
		if err := validGraphImage(*r.BaseImage); err != nil {
			return err
		}
	}
	if len(r.Metadata) == 0 || !json.Valid(r.Metadata) {
		return errors.New("invalid graph result metadata")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > maxGraphRecordBytes {
		return errors.New("graph result too large")
	}
	return nil
}

func validGraphImage(d v1.Descriptor) error {
	if err := validDescriptor(d); err != nil {
		return err
	}
	if d.MediaType != v1.MediaTypeImageManifest && d.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		return fmt.Errorf("invalid graph result image media type %q", d.MediaType)
	}
	return nil
}

// GraphMetadata preserves native BuildKit key IDs and operation links, which
// cannot be recovered from its output-normalized remote cache encoding.
type GraphMetadata struct {
	Roots []string    `json:"roots"`
	Nodes []GraphNode `json:"nodes"`
}
type GraphNode struct {
	ID      string               `json:"id"`
	Inputs  []GraphInput         `json:"inputs,omitempty"`
	Results []solver.CacheResult `json:"results,omitempty"`
}
type GraphInput struct {
	Source string               `json:"source"`
	Link   solver.CacheInfoLink `json:"link"`
}

// GraphRecord binds the native dependency topology in Metadata to complete
// immutable images and restoration metadata. Multiple results remain explicit.
type GraphRecord struct {
	Version   string          `json:"version"`
	CreatedAt time.Time       `json:"created_at"`
	Root      digest.Digest   `json:"root"`
	Results   []GraphResult   `json:"results"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

// Validate checks metadata without materializing image payloads.
func (r GraphRecord) Validate(root digest.Digest) error {
	_, err := r.NativeMetadata(root)
	return err
}

// NativeMetadata admits the complete native topology and every result
// before importing images or exposing any dependency link to the solver.
func (r GraphRecord) NativeMetadata(root digest.Digest) (metadata GraphMetadata, err error) {
	if err := validSHA(root); err != nil {
		return metadata, err
	}
	if r.Version != GraphRecordVersion || r.Root != root {
		return metadata, errors.New("graph cache version or root differs from request")
	}
	if r.CreatedAt.IsZero() {
		return metadata, errors.New("graph cache lacks creation time")
	}
	if len(r.Metadata) == 0 || len(r.Metadata) > maxGraphRecordBytes {
		return metadata, errors.New("graph cache metadata outside size limit")
	}
	if err := strictJSON(r.Metadata, &metadata); err != nil {
		return metadata, err
	}
	if len(metadata.Nodes) == 0 || len(metadata.Nodes) > maxGraphEntries || len(metadata.Roots) == 0 || len(metadata.Roots) > maxGraphEntries || len(r.Results) == 0 || len(r.Results) > maxGraphEntries {
		return metadata, errors.New("graph cache entry count outside limits")
	}
	ids := map[string]bool{}
	images := map[digest.Digest]v1.Descriptor{}
	metadataBytes := len(r.Metadata)
	for _, result := range r.Results {
		metadataBytes += len(result.Metadata) + len(result.ID)
		if metadataBytes > maxGraphRecordBytes {
			return metadata, errors.New("graph cache metadata too large")
		}
		if err := result.validate(); err != nil {
			return metadata, err
		}
		if ids[result.ID] {
			return metadata, errors.New("duplicate graph result identity")
		}
		ids[result.ID] = true
		for _, image := range []*v1.Descriptor{&result.Image, result.BaseImage} {
			if image == nil {
				continue
			}
			if previous, ok := images[image.Digest]; ok && (previous.Size != image.Size || previous.MediaType != image.MediaType) {
				return metadata, errors.New("conflicting graph image descriptors")
			}
			images[image.Digest] = *image
		}
	}
	nodes := make(map[string]GraphNode, len(metadata.Nodes))
	for _, node := range metadata.Nodes {
		if node.ID == "" || len(node.ID) > 1024 || strings.ContainsRune(node.ID, '\x00') {
			return metadata, errors.New("invalid native graph key")
		}
		if _, exists := nodes[node.ID]; exists {
			return metadata, errors.New("duplicate native graph key")
		}
		nodes[node.ID] = node
	}
	for _, root := range metadata.Roots {
		if _, exists := nodes[root]; !exists {
			return metadata, errors.New("native graph root is missing")
		}
	}
	used, links := map[string]bool{}, 0
	for _, node := range metadata.Nodes {
		if len(node.Results) > maxGraphEntries {
			return metadata, errors.New("graph record too large")
		}
		if len(node.Inputs) == 0 {
			if err := validSHA(digest.Digest(node.ID)); err != nil {
				return metadata, fmt.Errorf("native graph source key: %w", err)
			}
		}
		var operation digest.Digest
		var ordinals [65]int
		last := -1
		for _, input := range node.Inputs {
			links++
			_, exists := nodes[input.Source]
			if links > 4*maxGraphEntries || !exists || input.Link.Input < 0 || input.Link.Input > 64 || input.Link.Output != 0 || validSHA(input.Link.Digest) != nil {
				return metadata, errors.New("invalid native graph dependency")
			}
			if input.Link.Selector != "" && (len(input.Link.Selector) > 4096 || input.Link.Selector.Validate() != nil) {
				return metadata, errors.New("invalid native graph selector")
			}
			if operation != "" && operation != input.Link.Digest {
				return metadata, errors.New("native graph key has conflicting operation identities")
			}
			operation = input.Link.Digest
			ordinals[input.Link.Input]++
			if ordinals[input.Link.Input] > maxGraphEntries {
				return metadata, errors.New("native graph input count outside limits")
			}
			if int(input.Link.Input) > last {
				last = int(input.Link.Input)
			}
		}
		for _, count := range ordinals[:last+1] {
			if count == 0 {
				return metadata, errors.New("native graph has an empty input ordinal")
			}
		}
		for _, result := range node.Results {
			if !ids[result.ID] || result.CreatedAt.IsZero() {
				return metadata, errors.New("native graph result is missing or lacks creation time")
			}
			used[result.ID] = true
		}
	}
	if len(used) != len(ids) {
		return metadata, errors.New("unreferenced graph result")
	}
	return metadata, validateGraphDepth(nodes)
}

// A temporary visiting mark detects cycles; completed nodes retain their longest
// ancestry. Validate links first so every recursive lookup is an admitted node.
func validateGraphDepth(nodes map[string]GraphNode) error {
	depths := make(map[string]int, len(nodes))
	var visit func(string, int) (int, error)
	visit = func(index string, ancestors int) (int, error) {
		if ancestors >= maxGraphDepth {
			return 0, errors.New("graph dependency chain too deep")
		}
		if depths[index] == -1 {
			return 0, errors.New("cyclic graph dependency")
		}
		if depths[index] > 0 {
			if ancestors+depths[index] > maxGraphDepth {
				return 0, errors.New("graph dependency chain too deep")
			}
			return depths[index], nil
		}
		depths[index] = -1
		depth := 1
		for _, input := range nodes[index].Inputs {
			parentDepth, err := visit(input.Source, ancestors+1)
			if err != nil {
				return 0, err
			}
			if parentDepth+1 > depth {
				depth = parentDepth + 1
			}
			if depth > maxGraphDepth {
				return 0, errors.New("graph dependency chain too deep")
			}
		}
		depths[index] = depth
		return depth, nil
	}
	for index := range nodes {
		if _, err := visit(index, 0); err != nil {
			return err
		}
	}
	return nil
}
