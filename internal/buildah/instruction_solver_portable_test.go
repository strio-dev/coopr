package buildah

import (
	"context"
	"encoding/json"
	"reflect"
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

func TestInstructionGraphConfigRetainsNativeLinksAndEveryVariant(t *testing.T) {
	metadata, results := instructionGraphFixture(t)
	config, err := instructionGraphConfig(metadata, results)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Layers) != 2 || len(config.Records) != 2 || len(config.Records[1].Results) != 2 {
		t.Fatalf("lost results: %+v", config)
	}
	if config.Records[0].Digest.String() != metadata.Nodes[0].ID {
		t.Fatal("source digest normalized twice")
	}
	wantDigest := solver.NewCacheKey(metadata.Nodes[1].Inputs[0].Link.Digest, "", 0).ID
	if config.Records[1].Digest.String() != wantDigest {
		t.Fatal("operation output digest differs from BuildKit normalization")
	}
	input := config.Records[1].Inputs[0][0]
	if input.LinkIndex != 0 || input.Selector != metadata.Nodes[1].Inputs[0].Link.Selector.String() {
		t.Fatal("lost input selector or dependency")
	}
	for i, result := range results {
		desc, err := result.Descriptor()
		if err != nil {
			t.Fatal(err)
		}
		if config.Layers[i].Blob != desc.Digest || config.Layers[i].ParentIndex != -1 || config.Records[1].Results[i].LayerIndex != i || !config.Records[1].Results[i].CreatedAt.Equal(metadata.Nodes[1].Results[i].CreatedAt) {
			t.Fatal("result binding differs from native graph")
		}
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	record := cache.GraphRecord{Version: cache.GraphRecordVersion, CreatedAt: time.Now().UTC(), Root: digest.FromString("portable graph root"), Config: config, Results: results, Metadata: raw}
	if err := record.Validate(record.Root); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded cache.GraphRecord
	if err := decodeInstructionGraphJSON(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	var recovered instructionGraphMetadata
	if err := decodeInstructionGraphJSON(decoded.Metadata, &recovered); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := instructionGraphConfig(recovered, decoded.Results)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Config, rebuilt) {
		t.Fatal("native graph differs after portable JSON round trip")
	}
}

func TestInstructionGraphConfigRejectsInvalidNativeAssociations(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*instructionGraphMetadata, *[]cache.GraphResult)
	}{
		{"duplicate node", func(m *instructionGraphMetadata, _ *[]cache.GraphResult) { m.Nodes[1].ID = m.Nodes[0].ID }},
		{"missing source", func(m *instructionGraphMetadata, _ *[]cache.GraphResult) { m.Nodes[1].Inputs[0].Source = "absent" }},
		{"invalid source digest", func(m *instructionGraphMetadata, _ *[]cache.GraphResult) { m.Nodes[0].ID = "invalid" }},
		{"negative input", func(m *instructionGraphMetadata, _ *[]cache.GraphResult) { m.Nodes[1].Inputs[0].Link.Input = -1 }},
		{"unsupported output", func(m *instructionGraphMetadata, _ *[]cache.GraphResult) { m.Nodes[1].Inputs[0].Link.Output = 1 }},
		{"invalid operation digest", func(m *instructionGraphMetadata, _ *[]cache.GraphResult) {
			m.Nodes[1].Inputs[0].Link.Digest = "invalid"
		}},
		{"invalid selector", func(m *instructionGraphMetadata, _ *[]cache.GraphResult) {
			m.Nodes[1].Inputs[0].Link.Selector = "invalid"
		}},
		{"conflicting operations", func(m *instructionGraphMetadata, _ *[]cache.GraphResult) {
			input := m.Nodes[1].Inputs[0]
			input.Link.Digest = digest.FromString("other")
			m.Nodes[1].Inputs = append(m.Nodes[1].Inputs, input)
		}},
		{"missing result", func(m *instructionGraphMetadata, _ *[]cache.GraphResult) { m.Nodes[1].Results[0].ID = "absent" }},
		{"missing root", func(m *instructionGraphMetadata, _ *[]cache.GraphResult) { m.Roots = []string{"absent"} }},
		{"no roots", func(m *instructionGraphMetadata, _ *[]cache.GraphResult) { m.Roots = nil }},
		{"invalid result descriptor", func(_ *instructionGraphMetadata, r *[]cache.GraphResult) {
			(*r)[0].Image.URLs = []string{"https://example.invalid/image"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata, results := instructionGraphFixture(t)
			test.change(&metadata, &results)
			if _, err := instructionGraphConfig(metadata, results); err == nil {
				t.Fatal("accepted invalid native graph")
			}
		})
	}
}

func TestDecodeInstructionGraphJSONRejectsAmbiguousPayloads(t *testing.T) {
	for _, raw := range []string{"", `{"unexpected":true}`, `{"roots":[]} {"roots":[]}`, `{"roots":[`, strings.Repeat(" ", 8<<20+1)} {
		var metadata instructionGraphMetadata
		if err := decodeInstructionGraphJSON([]byte(raw), &metadata); err == nil {
			t.Fatal("accepted invalid native graph JSON")
		}
	}
}

func TestInstructionGraphRestoreRejectsMismatchedPortableStructure(t *testing.T) {
	metadata, results := instructionGraphFixture(t)
	config, err := instructionGraphConfig(metadata, results)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	record := cache.GraphRecord{Version: cache.GraphRecordVersion, CreatedAt: time.Now().UTC(), Root: digest.FromString("root"), Config: config, Results: results, Metadata: raw}
	record.Config.Records[1].Results = record.Config.Records[1].Results[:1]
	portable := &instructionGraphPortable{}
	if err := portable.restore(context.Background(), &record, "unused-layout"); err == nil {
		t.Fatal("accepted native/portable structure mismatch before importing")
	}
}
