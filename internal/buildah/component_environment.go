package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// A return request is made while the builder and any open layer group remain
// alive. The coordinator owns canonical replay and immutable input selection;
// the worker owns the native builder and resumes only after dependencies exist.
type componentReturnRequest struct {
	Boundary    planner.ComponentBoundary `json:"boundary"`
	Environment map[string]string         `json:"environment"`
	Executed    []planner.Operation       `json:"executed"`
	Revision    uint64                    `json:"revision"`
}

type componentReturnReply struct {
	// Operations is the complete rebound stage, including ARG history. Its
	// already-executed prefix must match Executed before the suffix is resumed.
	Operations          []planner.Operation               `json:"operations"`
	Aliases             map[string]string                 `json:"aliases,omitempty"`
	Images              map[string]string                 `json:"images,omitempty"`
	ResolvedBases       []stageWorkerBase                 `json:"resolved_bases,omitempty"`
	ComponentSelections map[string]oci.ComponentSelection `json:"component_selections,omitempty"`
	ComponentPins       map[string]string                 `json:"component_pins,omitempty"`
	ComponentSources    map[string]string                 `json:"component_sources,omitempty"`
	Revision            uint64                            `json:"revision"`
}

type componentReturnHandler func(context.Context, componentReturnRequest) (componentReturnReply, error)

// Artifact selection remains coordinator-owned even when a nested invocation
// discovers a new reference after a return. Caller ENV affects instantiation,
// while Reference/Parameters/Platform select the immutable artifact.
type componentSelectionRequest struct {
	DeliveryScope         *oci.DeliveryScope     `json:"delivery_scope,omitempty"`
	Reference             string                 `json:"reference"`
	Parameters            map[string]string      `json:"parameters,omitempty"`
	Platform              v1.Platform            `json:"platform"`
	ProgressPrefix        string                 `json:"progress_prefix,omitempty"`
	CallerEnvironment     map[string]string      `json:"caller_environment,omitempty"`
	ActiveComponents      map[digest.Digest]bool `json:"active_components,omitempty"`
	ActiveLocalComponents map[string]bool        `json:"active_local_components,omitempty"`
}

type componentSelectionReply struct {
	ComponentSelections map[string]oci.ComponentSelection `json:"component_selections,omitempty"`
	ComponentPins       map[string]string                 `json:"component_pins,omitempty"`
	ComponentSources    map[string]string                 `json:"component_sources,omitempty"`
	ResolvedBases       []stageWorkerBase                 `json:"resolved_bases,omitempty"`
}

type componentSelectionHandler func(context.Context, componentSelectionRequest) (componentSelectionReply, error)

func validateComponentReturnReply(request componentReturnRequest, reply componentReturnReply) error {
	if reply.Revision != request.Revision+1 {
		return fmt.Errorf("component return revision %d, want %d", reply.Revision, request.Revision+1)
	}
	if len(reply.Operations) < len(request.Executed) {
		return fmt.Errorf("component return removed executed instructions")
	}
	want, err := json.Marshal(request.Executed)
	if err != nil {
		return fmt.Errorf("encode executed component prefix: %w", err)
	}
	got, err := json.Marshal(reply.Operations[:len(request.Executed)])
	if err != nil {
		return fmt.Errorf("encode rebound component prefix: %w", err)
	}
	if !bytes.Equal(want, got) {
		return fmt.Errorf("component return changed the executed instruction prefix")
	}
	return nil
}

func (executor *graphExecutor) adoptComponentContinuation(reply componentReturnReply, aliases map[string]string, images map[string]stageState) error {
	for name, id := range reply.Aliases {
		if previous, exists := aliases[name]; exists && previous != id {
			return fmt.Errorf("component return changed stage alias %q", name)
		}
		aliases[name] = id
	}
	for id, image := range reply.Images {
		if previous, exists := images[id]; exists && previous.storageImageID != image {
			return fmt.Errorf("component return changed selected stage %s", id)
		}
		state := images[id]
		state.storageImageID = image
		images[id] = state
	}
	for _, base := range reply.ResolvedBases {
		if previous, exists := executor.resolvedBases[base.Key]; exists && (previous.ImageID != base.Source.ImageID || previous.Selected.Digest != base.Source.Selected.Digest || !bytes.Equal(previous.ConfigData, base.Source.ConfigData)) {
			return fmt.Errorf("component return changed selected base %q", base.Key.Reference)
		}
		if executor.resolvedBases == nil {
			executor.resolvedBases = make(map[ResolvedBaseKey]ResolvedImageSource)
		}
		executor.resolvedBases[base.Key] = base.Source
	}
	for key, pin := range reply.ComponentPins {
		if previous, exists := executor.componentPins[key]; exists && previous != pin {
			return fmt.Errorf("component return changed pinned component %q", key)
		}
		if executor.componentPins == nil {
			executor.componentPins = make(map[string]string)
		}
		executor.componentPins[key] = pin
	}
	for key, selection := range reply.ComponentSelections {
		if previous, exists := executor.componentSelections[key]; exists && previous.Identity() != selection.Identity() {
			return fmt.Errorf("component return changed frozen selection %q", key)
		}
		if executor.componentSelections == nil {
			executor.componentSelections = make(map[string]oci.ComponentSelection)
		}
		executor.componentSelections[key] = selection.Clone()
	}
	for key, identity := range reply.ComponentSources {
		if previous, exists := executor.componentSources[key]; exists && previous != identity {
			return fmt.Errorf("component return changed local source identity %q", key)
		}
		if executor.componentSources == nil {
			executor.componentSources = make(map[string]string)
		}
		executor.componentSources[key] = identity
	}

	return nil
}

// Registry/native image selections are owned by the build coordinator. Generated
// filesystem transports remain local to the graph that produced their context.
type baseSelectionRequest struct {
	Reference string      `json:"reference"`
	Platform  v1.Platform `json:"platform"`
}

type baseSelectionHandler func(context.Context, baseSelectionRequest) (ResolvedImageSource, error)
