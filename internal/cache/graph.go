package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	cacheimport "github.com/moby/buildkit/cache/remotecache/v1"
	cachetypes "github.com/moby/buildkit/cache/remotecache/v1/types"
	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
)

const (
	GraphRecordVersion   = "coopr.strio.dev/graph-cache-record/v1"
	GraphArtifactType    = "application/vnd.coopr.strio.dev.graph-cache.v1+json"
	GraphConfigMediaType = "application/vnd.coopr.strio.dev.graph-cache.config.v1+json"
	GraphResultMediaType = "application/vnd.coopr.strio.dev.graph-cache.result.v1+json"
	maxGraphRecordBytes  = 8 << 20
	maxGraphEntries      = 4096
	maxGraphDepth        = 512
)

// GraphResult retains a complete immutable image and executor-owned restoration
// metadata. Its descriptor is the portable identity used by BuildKit's graph
// result references; identical images may have distinct restoration metadata.
type GraphResult struct {
	ID        string          `json:"id"`
	Image     v1.Descriptor   `json:"image"`
	BaseImage *v1.Descriptor  `json:"base_image,omitempty"`
	Metadata  json.RawMessage `json:"metadata"`
}

func (r GraphResult) Descriptor() (v1.Descriptor, error) {
	if r.ID == "" || len(r.ID) > 1024 || strings.ContainsRune(r.ID, '\x00') {
		return v1.Descriptor{}, errors.New("invalid graph result ID")
	}
	if err := validGraphImage(r.Image); err != nil {
		return v1.Descriptor{}, err
	}
	if r.BaseImage != nil {
		if err := validGraphImage(*r.BaseImage); err != nil {
			return v1.Descriptor{}, err
		}
	}
	if len(r.Metadata) == 0 || !json.Valid(r.Metadata) {
		return v1.Descriptor{}, errors.New("invalid graph result metadata")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if len(data) > maxGraphRecordBytes {
		return v1.Descriptor{}, errors.New("graph result too large")
	}
	return oci.Descriptor(GraphResultMediaType, data), nil
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

// GraphRecord uses BuildKit's portable dependency graph, with result-sidecar
// descriptors in Layers. It deliberately keeps every result rather than using
// CacheChains.Marshal, which selects one best result for each graph record.
type GraphRecord struct {
	Version   string                 `json:"version"`
	CreatedAt time.Time              `json:"created_at"`
	Root      digest.Digest          `json:"root"`
	Config    cachetypes.CacheConfig `json:"config"`
	Results   []GraphResult          `json:"results"`
	Metadata  json.RawMessage        `json:"metadata,omitempty"`
}

// Validate checks the complete graph before invoking the upstream parser. The
// parser permits absent blobs as partial cache records; Coopr artifacts require
// every result binding to be present, so those checks are performed here.
func (r GraphRecord) Validate(root digest.Digest) error {
	if err := validSHA(root); err != nil {
		return err
	}
	if r.Version != GraphRecordVersion || r.Root != root {
		return errors.New("graph cache version or root differs from request")
	}
	if r.CreatedAt.IsZero() {
		return errors.New("graph cache lacks creation time")
	}
	if len(r.Metadata) != 0 && !json.Valid(r.Metadata) {
		return errors.New("invalid graph cache metadata")
	}
	if len(r.Config.Records) == 0 || len(r.Config.Records) > maxGraphEntries || len(r.Config.Layers) > maxGraphEntries || len(r.Results) == 0 || len(r.Results) > maxGraphEntries {
		return errors.New("graph cache entry count outside limits")
	}
	provider := cacheimport.DescriptorProvider{}
	ids := map[string]bool{}
	images := map[digest.Digest]v1.Descriptor{}
	metadataBytes := len(r.Metadata)
	for _, result := range r.Results {
		metadataBytes += len(result.Metadata) + len(result.ID)
		if metadataBytes > maxGraphRecordBytes {
			return errors.New("graph cache metadata too large")
		}
		desc, err := result.Descriptor()
		if err != nil {
			return err
		}
		if ids[result.ID] {
			return errors.New("duplicate graph result ID")
		}
		ids[result.ID] = true
		if _, ok := provider[desc.Digest]; ok {
			return errors.New("duplicate graph result descriptor")
		}
		provider[desc.Digest] = cacheimport.DescriptorProviderPair{Descriptor: desc}
		for _, image := range []*v1.Descriptor{&result.Image, result.BaseImage} {
			if image == nil {
				continue
			}
			if previous, ok := images[image.Digest]; ok && (previous.Size != image.Size || previous.MediaType != image.MediaType) {
				return errors.New("conflicting graph image descriptors")
			}
			images[image.Digest] = *image
		}
	}
	layers := map[digest.Digest]bool{}
	for index, layer := range r.Config.Layers {
		if _, ok := provider[layer.Blob]; !ok {
			return errors.New("graph layer lacks result sidecar")
		}
		if layers[layer.Blob] {
			return errors.New("duplicate graph layer")
		}
		layers[layer.Blob] = true
		if layer.ParentIndex < -1 || layer.ParentIndex >= len(r.Config.Layers) {
			return errors.New("invalid graph layer parent")
		}
		if layer.Annotations != nil {
			return errors.New("result sidecar layer cannot carry filesystem annotations")
		}
		if err := markGraphLayer(r.Config.Layers, index, map[int]bool{}); err != nil {
			return err
		}
	}
	if len(layers) != len(provider) {
		return errors.New("unbound graph result sidecar")
	}
	used := map[int]bool{}
	links := 0
	for _, record := range r.Config.Records {
		if err := validSHA(record.Digest); err != nil {
			return fmt.Errorf("graph operation: %w", err)
		}
		if len(record.Inputs) > maxGraphEntries || len(record.Results)+len(record.ChainedResults) > maxGraphEntries {
			return errors.New("graph record too large")
		}
		for _, inputs := range record.Inputs {
			links += len(inputs)
			if links > 4*maxGraphEntries {
				return errors.New("graph cache has too many links")
			}
			if len(inputs) == 0 || len(inputs) > maxGraphEntries {
				return errors.New("invalid graph input count")
			}
			for _, input := range inputs {
				if input.LinkIndex < 0 || input.LinkIndex >= len(r.Config.Records) || len(input.Selector) > 4096 || strings.ContainsRune(input.Selector, '\x00') {
					return errors.New("invalid graph input")
				}
			}
		}
		for _, result := range record.Results {
			if result.CreatedAt.IsZero() {
				return errors.New("graph result lacks creation time")
			}
			if err := markGraphLayer(r.Config.Layers, result.LayerIndex, used); err != nil {
				return err
			}
		}
		for _, result := range record.ChainedResults {
			if result.CreatedAt.IsZero() || len(result.LayerIndexes) == 0 || len(result.LayerIndexes) > maxGraphEntries {
				return errors.New("invalid graph result chain")
			}
			for _, index := range result.LayerIndexes {
				if index < 0 || index >= len(r.Config.Layers) {
					return errors.New("invalid chained result layer")
				}
				used[index] = true
			}
		}
	}
	if len(used) != len(r.Config.Layers) {
		return errors.New("unreferenced graph result layer")
	}
	if err := validateGraphDepth(r.Config.Records); err != nil {
		return err
	}
	// ParseConfig checks dependency recursion, layer chains and reconstructs all
	// result associations using BuildKit's public codec, without worker/daemon APIs.
	return cacheimport.ParseConfig(r.Config, provider, &graphValidationTarget{})
}

func markGraphLayer(layers []cachetypes.CacheLayer, index int, used map[int]bool) error {
	seen := map[int]bool{}
	for index != -1 {
		if index < 0 || index >= len(layers) || seen[index] {
			return errors.New("invalid graph result layer chain")
		}
		seen[index], used[index] = true, true
		if len(seen) > maxGraphDepth {
			return errors.New("graph result layer chain too deep")
		}
		index = layers[index].ParentIndex
	}
	return nil
}

// Bound upstream's recursive record parser before calling it. A temporary
// visiting mark detects cycles; completed nodes retain their longest ancestry.
func validateGraphDepth(records []cachetypes.CacheRecord) error {
	depths := make([]int, len(records))
	var visit func(int, int) (int, error)
	visit = func(index, ancestors int) (int, error) {
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
		for _, inputs := range records[index].Inputs {
			for _, input := range inputs {
				parentDepth, err := visit(input.LinkIndex, ancestors+1)
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
		}
		depths[index] = depth
		return depth, nil
	}
	for index := range records {
		if _, err := visit(index, 0); err != nil {
			return err
		}
	}
	return nil
}

type graphValidationTarget struct{}
type graphValidationRecord struct{ solver.CacheExporterRecordBase }

func (*graphValidationTarget) Add(_ digest.Digest, _ [][]solver.CacheLink, _ []solver.CacheExportResult) (solver.CacheExporterRecord, bool, error) {
	return &graphValidationRecord{}, true, nil
}
