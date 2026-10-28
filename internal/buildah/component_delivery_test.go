package buildah

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
)

func backendDeliveryFixture() *oci.ComponentDelivery {
	return &oci.ComponentDelivery{
		Repository: "delivered.example/components/parent",
		Subject:    oci.Descriptor(v1.MediaTypeImageIndex, []byte("parent index")),
		Attachment: oci.Descriptor(v1.MediaTypeImageManifest, []byte("delivery attachment")),
		Scope: oci.DeliveryScope{
			Bundle:  "delivered.example/bundle@" + digest.FromString("bundle").String(),
			Capture: digest.FromString("capture"),
			Policy:  bale.DependencyPolicy{Follow: "auto"},
		},
		Resolution: bale.DestinationResolution{
			Capture:     digest.FromString("capture"),
			Policy:      bale.DependencyPolicy{Follow: "auto"},
			Outcome:     "selected",
			Declaration: &bale.DestinationDeclaration{Digest: digest.FromString("declaration")},
			Dependencies: []bale.DestinationDependency{{
				Ref:         "original.example/child:first",
				Source:      "original.example/child@" + digest.FromString("child").String(),
				Destination: "delivered.example/child@" + digest.FromString("child").String(),
				Policy:      bale.DependencyPolicy{Follow: "auto", Select: &bale.DependencySelector{Context: "first"}},
			}, {
				Ref:         "original.example/child:second",
				Source:      "original.example/child@" + digest.FromString("child").String(),
				Destination: "delivered.example/child@" + digest.FromString("child").String(),
				Policy:      bale.DependencyPolicy{Follow: "auto", Select: &bale.DependencySelector{Context: "second"}},
			}},
		},
	}
}

func TestComponentSelectionPinsRootRatherThanPlatformManifest(t *testing.T) {
	root := oci.Descriptor(v1.MediaTypeImageIndex, []byte("index"))
	selected := oci.Descriptor(v1.MediaTypeImageManifest, []byte("manifest"))
	for _, local := range []bool{false, true} {
		resolved := &ResolvedComponentPlan{Identity: selected.Digest, Artifact: &oci.Resolved{
			Root: root, Selected: selected, Repository: "delivered.example/component", Local: local,
		}}
		pin, err := pinnedComponentReference(resolved)
		if err != nil {
			t.Fatal(err)
		}
		want := "delivered.example/component@" + root.Digest.String()
		if local {
			want = root.Digest.String()
		}
		if pin != want {
			t.Fatalf("local=%t pin=%q want=%q", local, pin, want)
		}
	}
}

func TestComponentSelectionScopeSeparatesCapturesAndPolicies(t *testing.T) {
	target := v1.Platform{OS: "linux", Architecture: "amd64"}
	scope := backendDeliveryFixture().Scope
	first, err := scopedComponentSelectionKey("delivered.example/child:latest", nil, target, &scope)
	if err != nil {
		t.Fatal(err)
	}
	changed := scope
	changed.Capture = digest.FromString("other capture")
	second, err := scopedComponentSelectionKey("delivered.example/child:latest", nil, target, &changed)
	if err != nil {
		t.Fatal(err)
	}
	changed = scope
	changed.Policy.Select = &bale.DependencySelector{Context: "another declaration"}
	third, err := scopedComponentSelectionKey("delivered.example/child:latest", nil, target, &changed)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first == third || second == third {
		t.Fatal("different captures/policies share a selection key")
	}
}

func TestNestedComponentUsesOriginalOperandPolicy(t *testing.T) {
	delivery := backendDeliveryFixture()
	operation := planner.Operation{
		Instruction:                definition.Instruction{Name: "component", Arguments: []string{delivery.Resolution.Dependencies[0].Destination}},
		OriginalComponentReference: "original.example/child:second",
	}
	scope, err := componentOperationDeliveryScope(delivery, operation)
	if err != nil {
		t.Fatal(err)
	}
	if scope == nil || scope.Bundle != delivery.Scope.Bundle || scope.Capture != delivery.Scope.Capture || scope.Policy.Select == nil || scope.Policy.Select.Context != "second" {
		t.Fatalf("nested selection scope = %+v", scope)
	}
	operation.OriginalComponentReference = ""
	if _, err := componentOperationDeliveryScope(delivery, operation); err == nil {
		t.Fatal("selected an arbitrary policy for an ambiguous delivered operand")
	}
}

func TestComponentSelectionsSurviveWorkerAndContinuationMessages(t *testing.T) {
	selection := oci.ComponentSelection{
		Repository: "delivered.example/component",
		Root:       oci.Descriptor(v1.MediaTypeImageIndex, []byte("index")),
		Selected:   oci.Descriptor(v1.MediaTypeImageManifest, []byte("manifest")),
		Delivery:   backendDeliveryFixture(),
	}
	request := stageWorkerRequest{
		ComponentSelections: map[string]oci.ComponentSelection{"selection": selection},
		Delivery:            selection.Delivery,
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded stageWorkerRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.ComponentSelections, request.ComponentSelections) || !reflect.DeepEqual(decoded.Delivery, request.Delivery) {
		t.Fatal("worker encoding lost selected root or delivery context")
	}
	executor := &graphExecutor{}
	reply := componentReturnReply{ComponentSelections: decoded.ComponentSelections}
	if err := executor.adoptComponentContinuation(reply, nil, nil); err != nil {
		t.Fatal(err)
	}
	decoded.ComponentSelections["selection"].Delivery.Scope.Capture = digest.FromString("changed capture")
	if executor.componentSelections["selection"].Delivery.Scope.Capture != selection.Delivery.Scope.Capture {
		t.Fatal("adopted selection aliases incoming mutable metadata")
	}
	if err := executor.adoptComponentContinuation(reply, nil, nil); err == nil || !strings.Contains(err.Error(), "changed frozen selection") {
		t.Fatalf("changed delivery was accepted: %v", err)
	}
}

func TestComponentResolutionUsesFrozenDeliveryAcrossVariants(t *testing.T) {
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	artifact := &oci.Resolved{
		Repository: "delivered.example/component",
		Root:       oci.Descriptor(v1.MediaTypeImageIndex, []byte("index")),
		Selected:   oci.Descriptor(v1.MediaTypeImageManifest, []byte("manifest")),
		Delivery:   backendDeliveryFixture(),
	}
	calls := 0
	executor := &graphExecutor{resolveComponent: func(_ context.Context, request ComponentPlanRequest) (*ResolvedComponentPlan, error) {
		calls++
		if calls == 1 {
			if request.Selection != nil {
				t.Fatal("first invocation unexpectedly already frozen")
			}
		} else {
			if request.Selection == nil || request.Selection.Identity() != artifact.ComponentSelection().Identity() {
				t.Fatalf("new parameters lost frozen artifact/delivery: %+v", request.Selection)
			}
			if request.Reference != "delivered.example/component@"+artifact.Root.Digest.String() {
				t.Fatalf("unexpected invocation reference %q", request.Reference)
			}
		}
		return &ResolvedComponentPlan{Identity: artifact.Selected.Digest, Artifact: artifact}, nil
	}}
	for _, value := range []string{"one", "two"} {
		if _, err := executor.resolveComponentInvocation(context.Background(), "delivered.example/component:latest", map[string]string{"value": value}, platform, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("resolver calls=%d", calls)
	}
}

func TestComponentCacheInputsIncludeDeliveryContext(t *testing.T) {
	artifact := &oci.Resolved{
		Root:     oci.Descriptor(v1.MediaTypeImageIndex, []byte("index")),
		Delivery: backendDeliveryFixture(),
	}
	resolved := &ResolvedComponentPlan{Artifact: artifact}
	first := componentCacheInputs(resolved)
	if first["component-root"] != artifact.Root.Digest || first["component-delivery"] != artifact.Delivery.Identity() {
		t.Fatalf("missing root/delivery cache inputs: %v", first)
	}
	artifact.Delivery.Scope.Capture = digest.FromString("new capture")
	second := componentCacheInputs(resolved)
	if first["component-delivery"] == second["component-delivery"] {
		t.Fatal("changed delivery capture did not change cache input")
	}
}

func TestStageWorkerControlFreezesNestedDeliveryAtCoordinator(t *testing.T) {
	parent := backendDeliveryFixture()
	scope, err := parent.DependencyScope("original.example/child:first")
	if err != nil {
		t.Fatal(err)
	}
	artifact := &oci.Resolved{
		Repository: "delivered.example/child",
		Root:       oci.Descriptor(v1.MediaTypeImageIndex, []byte("child index")),
		Selected:   oci.Descriptor(v1.MediaTypeImageManifest, []byte("child manifest")),
		Delivery:   parent.Clone(),
	}
	artifact.Delivery.Scope = *scope.Clone()
	calls := 0
	executor := &graphExecutor{resolveComponent: func(_ context.Context, request ComponentPlanRequest) (*ResolvedComponentPlan, error) {
		calls++
		if !reflect.DeepEqual(request.DeliveryScope, scope) {
			t.Errorf("coordinator lost nested policy: got %+v want %+v", request.DeliveryScope, scope)
		}
		if calls == 2 && (request.Selection == nil || request.Selection.Identity() != artifact.ComponentSelection().Identity()) {
			t.Errorf("second variant did not receive frozen selection: %+v", request.Selection)
		}
		return &ResolvedComponentPlan{Identity: artifact.Selected.Digest, Artifact: artifact}, nil
	}}
	client, ctx := controlPair(t, nil, executor.selectCoordinatedComponent)
	target := v1.Platform{OS: "linux", Architecture: "amd64"}
	key, err := scopedComponentSelectionKey("delivered.example/child:latest", nil, target, scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"one", "two"} {
		reply, err := client.selectComponent(ctx, componentSelectionRequest{
			Reference: "delivered.example/child:latest", Platform: target,
			DeliveryScope: scope.Clone(), CallerEnvironment: map[string]string{"value": value},
		})
		if err != nil {
			t.Fatal(err)
		}
		if reply.ComponentPins[key] != "delivered.example/child@"+artifact.Root.Digest.String() || reply.ComponentSelections[key].Identity() != artifact.ComponentSelection().Identity() {
			t.Fatalf("worker reply lost frozen root/delivery: %+v", reply)
		}
	}
	if calls != 2 {
		t.Fatalf("instantiations=%d want two caller-environment variants", calls)
	}
}
