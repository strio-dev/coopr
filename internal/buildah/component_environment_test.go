package buildah

import (
	"context"
	"coopr/internal/buildcontext"
	"encoding/json"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/planner"
)

func TestComponentReturnValidatesExecutedPrefixAcrossWorkerEncoding(t *testing.T) {
	operation := planner.Operation{Instruction: definition.Instruction{Name: "component", Arguments: []string{"./shared.coopr"}, Properties: map[string]string{}}, ArgumentsInScope: map[string]string{}, ComponentBoundary: &planner.ComponentBoundary{StageID: "0", Instruction: 1}}
	request := componentReturnRequest{Boundary: *operation.ComponentBoundary, Executed: []planner.Operation{operation}, Revision: 3}
	reply := componentReturnReply{Operations: []planner.Operation{operation}, Revision: 4}
	encoded, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var decoded componentReturnReply
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := validateComponentReturnReply(request, decoded); err != nil {
		t.Fatal(err)
	}
	decoded.Operations[0].Arguments[0] = "./different.coopr"
	if err := validateComponentReturnReply(request, decoded); err == nil {
		t.Fatal("accepted rewritten executed component")
	}
	reply.Revision = 3
	if err := validateComponentReturnReply(request, reply); err == nil {
		t.Fatal("accepted stale continuation revision")
	}
	reply.Revision = 4
	reply.Operations = nil
	if err := validateComponentReturnReply(request, reply); err == nil {
		t.Fatal("accepted truncated continuation")
	}
}

func TestNestedBaseSelectionsUseAncestorAndSharedPin(t *testing.T) {
	calls := 0
	target := v1.Platform{OS: "linux", Architecture: "amd64"}
	executor := &graphExecutor{baseSelectionHandler: func(_ context.Context, request baseSelectionRequest) (ResolvedImageSource, error) {
		calls++
		if request.Reference != "registry.invalid/tool:latest" || request.Platform.Architecture != "amd64" {
			t.Fatalf("request=%+v", request)
		}
		return ResolvedImageSource{ImageID: "ancestor-pinned"}, nil
	}}
	first, err := executor.resolveBaseImage(context.Background(), "registry.invalid/tool:latest", target)
	if err != nil {
		t.Fatal(err)
	}
	second, err := executor.materializeNamedContext(context.Background(), buildcontext.Spec{Name: "tools", Kind: buildcontext.DockerImage, Reference: "registry.invalid/tool:latest"}, target)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.ImageID != "ancestor-pinned" || second.ImageID != first.ImageID {
		t.Fatalf("calls=%d first=%+v second=%+v", calls, first, second)
	}
}

func TestComponentContinuationRetainsImmutableLocalSourceIdentity(t *testing.T) {
	executor := &graphExecutor{}
	reply := componentReturnReply{ComponentPins: map[string]string{"selection": "pinned"}, ComponentSources: map[string]string{"selection": "canonical#target"}}
	if err := executor.adoptComponentContinuation(reply, nil, nil); err != nil {
		t.Fatal(err)
	}
	if executor.componentSources["selection"] != "canonical#target" {
		t.Fatal("local source identity lost")
	}
	reply.ComponentSources["selection"] = "different#target"
	if err := executor.adoptComponentContinuation(reply, nil, nil); err == nil {
		t.Fatal("accepted changed local source identity for frozen selection")
	}
}
