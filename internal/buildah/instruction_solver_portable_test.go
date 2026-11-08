package buildah

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
)

func instructionGraphFixture(t *testing.T) (instructionGraphMetadata, []cache.GraphResult) {
	t.Helper()
	created := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	source := solver.NewCacheKey(digest.FromString("source"), "", 0).ID
	metadata := instructionGraphMetadata{
		Roots: []string{"native-operation-key"},
		Nodes: []instructionGraphNode{
			{ID: source},
			{ID: "native-operation-key", Inputs: []instructionGraphInput{{Source: source, Link: solver.CacheInfoLink{Digest: digest.FromString("operation"), Selector: digest.FromString("input selection")}}}, Results: []solver.CacheResult{{ID: "old physical result", CreatedAt: created.Add(-time.Hour)}, {ID: "refreshed physical result", CreatedAt: created}}},
		},
	}
	results := []cache.GraphResult{
		{ID: "old physical result", Image: v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("old image"), Size: 20}, Metadata: json.RawMessage(`{"root_mode":493}`)},
		{ID: "refreshed physical result", Image: v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("refreshed image"), Size: 21}, Metadata: json.RawMessage(`{"root_mode":448}`)},
	}
	return metadata, results
}

func TestDecodeInstructionGraphJSONRejectsAmbiguousPayloads(t *testing.T) {
	for _, raw := range []string{"", `{"unexpected":true}`, `{"roots":[]} {"roots":[]}`, `{"roots":[`, strings.Repeat(" ", 8<<20+1)} {
		var metadata instructionGraphMetadata
		if err := decodeInstructionGraphJSON([]byte(raw), &metadata); err == nil {
			t.Fatal("accepted invalid native graph JSON")
		}
	}
}

func TestInstructionGraphRestoreRejectsInvalidLinksBeforeImport(t *testing.T) {
	metadata, results := instructionGraphFixture(t)
	metadata.Nodes[1].Inputs[0].Source = "absent"
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	record := cache.GraphRecord{Version: cache.GraphRecordVersion, CreatedAt: time.Now().UTC(), Root: digest.FromString("root"), Results: results, Metadata: raw}
	portable := &instructionGraphPortable{}
	if err := portable.restoreRecord(context.Background(), &record, "unused-layout", nil, false); err == nil || !strings.Contains(err.Error(), "invalid native graph dependency") {
		t.Fatal("invalid native link was not rejected before result decoding or importing", err)
	}
}
