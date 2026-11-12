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

func TestInstructionPortableKeysLinkOnlyEquivalentPrefixes(t *testing.T) {
	engine, err := newInstructionSolver(t.Context(), solverTestStore{root: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.close(); err != nil {
			t.Error(err)
		}
	})
	source := engine.source(digest.FromString("source"), instructionSolverResult{}, false)
	prefix := &instructionVertex{key: digest.FromString("WORKDIR"), parent: source}
	child := &instructionVertex{key: digest.FromString("COPY"), parent: prefix}
	sourceID := solver.NewCacheKey(source.key, "", 0).ID
	link := func(source string, operation, selector digest.Digest, target string) {
		t.Helper()
		if err := engine.keys.AddLink(source, solver.CacheInfoLink{Digest: operation, Selector: selector}, target); err != nil {
			t.Fatal(err)
		}
	}
	link(sourceID, prefix.key, "", "old-prefix")
	link(sourceID, prefix.key, "", "new-prefix")
	link(sourceID, digest.FromString("other operation"), "", "other-operation-prefix")
	link(solver.NewCacheKey(digest.FromString("other source"), "", 0).ID, prefix.key, "", "other-source-prefix")
	link(sourceID, prefix.key, digest.FromString("other selector"), "other-selector-prefix")
	link("old-prefix", child.key, "", "cached-child")
	childLink := solver.CacheInfoLink{Digest: child.key}
	if engine.keys.HasLink("new-prefix", childLink, "cached-child") {
		t.Fatal("fixture already associates the independent prefix with the child")
	}
	if err := engine.linkPortableKeys(child); err != nil {
		t.Fatal(err)
	}
	if !engine.keys.HasLink("new-prefix", childLink, "cached-child") {
		t.Fatal("equivalent prefix did not recover the imported child association")
	}
	for _, other := range []string{"other-operation-prefix", "other-source-prefix", "other-selector-prefix"} {
		if engine.keys.HasLink(other, childLink, "cached-child") {
			t.Fatalf("unrelated prefix %q gained a child association", other)
		}
	}
}
