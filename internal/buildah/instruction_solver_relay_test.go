package buildah

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
)

func instructionRelayFixture(t *testing.T, forced bool) (*instructionSolver, *instructionVertexRelay) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	engine, err := newInstructionSolver(ctx, solverTestStore{root: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.close(); err != nil {
			t.Error(err)
		}
	})
	source := engine.source(digest.FromString("relay scratch"), instructionSolverResult{}, false)
	pending, err := engine.begin(ctx, source, digest.FromString("relay parent"), "COPY parent", forced)
	if err != nil {
		t.Fatal(err)
	}
	if pending.request == nil {
		t.Fatal("new relay parent did not execute")
	}
	if err := pending.finish(&instructionSolverResult{Entry: instructionCacheEntry{ImageID: "parent", ManifestDigest: digest.FromString("parent")}}); err != nil {
		t.Fatal(err)
	}
	relay, err := encodeInstructionVertexRelay(pending.vertex)
	if err != nil {
		t.Fatal(err)
	}
	return engine, relay
}

func TestInstructionGraphRelayRoundTripAndCompletedParent(t *testing.T) {
	engine, relay := instructionRelayFixture(t, false)
	raw, err := json.Marshal(stageWorkerRequest{GraphParent: relay})
	if err != nil {
		t.Fatal(err)
	}
	var request stageWorkerRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	// This is a separate solver and job, sharing only durable storage.
	child, err := newInstructionSolver(t.Context(), engine.results.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := child.close(); err != nil {
			t.Error(err)
		}
	}()
	parent, err := child.restoreInstructionVertexRelay(request.GraphParent)
	if err != nil {
		t.Fatal(err)
	}
	if parent.request != nil || parent.source == nil || parent.source.Entry.ImageID != "parent" || mustInstructionPortableDigest(t, parent) != relay.Root {
		t.Fatal("relay did not restore an immutable completed parent")
	}
	if !parent.executed.Load() || parent.parent.executed.Load() {
		t.Fatal("relay changed the executed parent or untouched source status")
	}
	result, err := child.job.Build(t.Context(), parent.Inputs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	again, err := encodeInstructionVertexRelay(parent)
	if err != nil || !reflect.DeepEqual(again, relay) {
		t.Fatalf("relay changed during roundtrip: %v", err)
	}
}

func TestInstructionGraphRelayPreservesDAGAndLazyAncestors(t *testing.T) {
	engine, historyRelay := instructionRelayFixture(t, false)
	history, err := engine.restoreInstructionVertexRelay(historyRelay)
	if err != nil {
		t.Fatal(err)
	}
	// A final hit need not execute its secondary input. Both branches share the
	// original source, while the primary parent still defines image history.
	lazy := engine.declare(history.parent, nil, digest.FromString("lazy stage input"), "COPY source", false, nil)
	endpoint := engine.declare(history, []*instructionVertex{lazy, history.parent}, digest.FromString("stage output"), "stage", false, nil)
	endpoint.completed = history.completed
	relay, err := encodeInstructionVertexRelay(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(relay.Nodes) != 4 {
		t.Fatalf("DAG relay has %d nodes, want four with one shared source", len(relay.Nodes))
	}
	child, err := newInstructionSolver(t.Context(), engine.results.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := child.close(); err != nil {
			t.Error(err)
		}
	}()
	restored, err := child.restoreInstructionVertexRelay(relay)
	if err != nil {
		t.Fatal(err)
	}
	if restored.parent == nil || restored.parent.key != history.key || len(restored.dependencies) != 2 {
		t.Fatal("relay lost the primary history parent or secondary input")
	}
	secondary := restored.dependencies[0]
	if secondary.key != lazy.key || secondary.parent != restored.parent.parent || restored.dependencies[1] != restored.parent.parent || secondary.source != nil || secondary.completed != nil {
		t.Fatal("relay materialized a lazy ancestor or lost the shared source")
	}
	if mustInstructionPortableDigest(t, restored) != mustInstructionPortableDigest(t, endpoint) || restored.source.ID() != endpoint.completed.ID() {
		t.Fatal("relay changed the selected output or DAG identity")
	}
	again, err := encodeInstructionVertexRelay(restored)
	if err != nil || !reflect.DeepEqual(again, relay) {
		t.Fatalf("DAG relay changed during roundtrip: %v", err)
	}
	for _, name := range []string{"forward-parent", "forward-input", "negative-input", "executed-lazy", "missing-endpoint", "unreachable-source"} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(relay)
			if err != nil {
				t.Fatal(err)
			}
			var invalid instructionVertexRelay
			if err := json.Unmarshal(raw, &invalid); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "forward-parent":
				invalid.Nodes[1].Parent = 1
			case "forward-input":
				invalid.Nodes[2].Dependencies = []int{3}
			case "negative-input":
				invalid.Nodes[2].Dependencies = []int{-1}
			case "executed-lazy":
				invalid.Nodes[2].Executed = true
			case "missing-endpoint":
				invalid.Nodes[3].ResultID = ""
			case "unreachable-source":
				invalid.Nodes[2].Parent = -1
				invalid.Nodes[2].Digest = invalid.Nodes[2].Key
				invalid.Nodes[1].Parent = -1
				invalid.Nodes[1].Digest = invalid.Nodes[1].Key
				invalid.Nodes[3].Dependencies = []int{2}
				invalid.Nodes[3].Digest = digest.FromString(instructionSolverVersion + "\n" + invalid.Nodes[3].Key.String() + "\n" + invalid.Nodes[1].Digest.String() + "\n" + invalid.Nodes[2].Digest.String())
				invalid.Root = invalid.Nodes[3].Digest
			}
			if _, err := child.restoreInstructionVertexRelay(&invalid); err == nil {
				t.Fatal("invalid DAG relay accepted")
			}
		})
	}
	lazy.dependencies = []*instructionVertex{endpoint}
	if _, err := encodeInstructionVertexRelay(endpoint); err == nil {
		t.Fatal("secondary-input cycle accepted")
	}
}

func TestInstructionGraphRelayForcedParentForcesNewConsumer(t *testing.T) {
	engine, relay := instructionRelayFixture(t, true)
	// Prime the same consumer's ordinary cache. The relayed forced parent must
	// still make that consumer execute in the next solver job.
	ordinary, err := engine.restoreInstructionVertexRelay(relay)
	if err != nil {
		t.Fatal(err)
	}
	ordinary.ignore = false
	ordinary.dgst = digest.FromString(instructionSolverVersion + "\n" + ordinary.key.String() + "\n" + ordinary.parent.Digest().String())
	warm, err := engine.begin(t.Context(), ordinary, digest.FromString("consumer"), "RUN consumer", false)
	if err != nil {
		t.Fatal(err)
	}
	if warm.request == nil {
		t.Fatal("new consumer did not execute")
	}
	if err := warm.finish(&instructionSolverResult{Entry: instructionCacheEntry{ImageID: "consumer", ManifestDigest: digest.FromString("consumer")}}); err != nil {
		t.Fatal(err)
	}
	child, err := newInstructionSolver(t.Context(), engine.results.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := child.close(); err != nil {
			t.Error(err)
		}
	}()
	parent, err := child.restoreInstructionVertexRelay(relay)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	pending, err := child.begin(ctx, parent, digest.FromString("consumer"), "RUN consumer", false)
	if err != nil {
		t.Fatal(err)
	}
	if pending.request == nil || pending.request.parent.Entry.ImageID != "parent" {
		t.Fatal("forced completed parent was bypassed or failed to materialize")
	}
	pending.abort()
}

func TestInstructionGraphRelayRejectsInvalidOrIncompleteNodes(t *testing.T) {
	engine, relay := instructionRelayFixture(t, true)
	for _, name := range []string{"key", "root", "result", "force-cleared", "force-invented", "serial", "source", "empty"} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(relay)
			var invalid instructionVertexRelay
			if err := json.Unmarshal(raw, &invalid); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "key":
				invalid.Nodes[1].Key = "bad"
			case "root":
				invalid.Root = digest.FromString("other")
			case "result":
				invalid.Nodes[1].ResultID = digest.FromString("missing result").String()
			case "force-cleared":
				invalid.Nodes[1].Ignore = false
			case "force-invented":
				invalid.Nodes[0].Ignore = true
			case "serial":
				invalid.Serial = 0
			case "source":
				invalid.Nodes[0].Key = digest.FromString("different source")
				invalid.Nodes[0].Digest = invalid.Nodes[0].Key
			case "empty":
				invalid.Nodes = nil
			}
			if _, err := engine.restoreInstructionVertexRelay(&invalid); err == nil {
				t.Fatal("invalid graph relay accepted")
			}
		})
	}
	incomplete := &instructionVertex{key: digest.FromString("pending"), dgst: digest.FromString("pending")}
	if _, err := encodeInstructionVertexRelay(incomplete); err == nil {
		t.Fatal("incomplete vertex accepted")
	}
	incomplete.parent = incomplete
	incomplete.source = &instructionSolverResult{}
	if _, err := encodeInstructionVertexRelay(incomplete); err == nil {
		t.Fatal("cyclic relay accepted")
	}
}

func TestInstructionGraphRelayPreservesCompletedResultDespiteTTL(t *testing.T) {
	engine, relay := instructionRelayFixture(t, false)
	zero := time.Duration(0)
	engine.results.ttl = &zero
	if _, err := engine.restoreInstructionVertexRelay(relay); err != nil {
		t.Fatalf("TTL prevented same-invocation handoff: %v", err)
	}
}

func TestInstructionGraphRelayPersistsUnmaterializedSource(t *testing.T) {
	engine, relay := instructionRelayFixture(t, false)
	vertex, err := engine.restoreInstructionVertexRelay(relay)
	if err != nil {
		t.Fatal(err)
	}
	vertex.parent.completed = nil
	path, err := engine.results.path(vertex.parent.source.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeInstructionVertexRelay(vertex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.restoreInstructionVertexRelay(encoded); err != nil {
		t.Fatalf("source skipped by a downstream hit was not made durable: %v", err)
	}
}

func TestInstructionGraphRootRelayPublishesOnlyValidatedRoots(t *testing.T) {
	engine, relay := instructionRelayFixture(t, false)
	vertex, err := engine.restoreInstructionVertexRelay(relay)
	if err != nil {
		t.Fatal(err)
	}
	engine.portable = &instructionGraphPortable{roots: map[digest.Digest]instructionGraphRoot{
		relay.Root: {vertex: vertex, result: relay.Nodes[len(relay.Nodes)-1].ResultID},
	}}
	relays, err := engine.relayInstructionGraphs()
	if err != nil || len(relays) != 1 {
		t.Fatalf("root relays=%v error=%v", relays, err)
	}
	target := &instructionSolver{results: &instructionResultStorage{ctx: t.Context(), store: engine.results.store, dir: engine.results.dir}, portable: &instructionGraphPortable{roots: map[digest.Digest]instructionGraphRoot{}}}
	invalid := append(slices.Clone(relays), instructionGraphRootRelay{Root: digest.FromString("invalid")})
	if err := target.acceptInstructionGraphs(invalid); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if len(target.portable.roots) != 0 {
		t.Fatal("partially validated root batch was published")
	}
	if err := target.acceptInstructionGraphs(relays); err != nil {
		t.Fatal(err)
	}
	if len(target.portable.roots) != 1 {
		t.Fatal("completed root was not relayed")
	}
}
