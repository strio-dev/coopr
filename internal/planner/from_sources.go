package planner

import (
	"fmt"
	"strconv"
	"strings"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
)

// FromSourceKind describes how an expanded FROM source is resolved by the raw
// definition. Image sources remain symbolic; this API does not fetch images.
type FromSourceKind string

const (
	FromSourceStage   FromSourceKind = "stage"
	FromSourceScratch FromSourceKind = "scratch"
	FromSourceImage   FromSourceKind = "image"
	// FromSourceContext selects one normalized named context. Context contains
	// the concrete source so the resolver never reclassifies the source name.
	FromSourceContext FromSourceKind = "context"
	// FromSourceCopyImage selects an external instruction input (COPY/ADD or a
	// RUN bind/cache mount) without applying its image configuration or
	// inherited ONBUILD instructions to the stage.
	FromSourceCopyImage FromSourceKind = "copy-image"
)

// FromSource is one FROM head from the unpruned definition. StageID and
// BaseStageID are raw numeric IDs, so later planning can bind discovered image
// metadata without depending on closure pruning or execution order.
type FromSource struct {
	StageID     string             `json:"stage_id"`
	Source      string             `json:"source"`
	Platform    string             `json:"platform"`
	Kind        FromSourceKind     `json:"kind"`
	BaseStageID string             `json:"base_stage_id,omitempty"`
	Context     *buildcontext.Spec `json:"context,omitempty"`
}

// StageBindResolver supplies immutable base-image metadata for one reachable
// external FROM. It is called only after the source and platform have been
// expanded, and before that stage's inherited and authored body is normalized.
type StageBindResolver func(FromSource) (StageBind, error)

// CreateDemandDriven plans only the selected build or publication closure.
// External FROM metadata is requested lazily, so inherited instructions can
// introduce dependencies without fetching unrelated target branches.
func CreateDemandDriven(def *definition.Definition, opts Options, resolve StageBindResolver) (*Plan, error) {
	if resolve == nil {
		return nil, fmt.Errorf("demand-driven planning requires a stage bind resolver")
	}
	if opts.Mode != "" && opts.Mode != Build && opts.Mode != Publish {
		return nil, fmt.Errorf("demand-driven planning only supports build and publish modes")
	}
	if opts.Mode == "" {
		opts.Mode = Build
	}
	return create(def, opts, nil, resolve)
}

// ResolveFromSources expands FROM heads without resolving stage bodies. It uses
// the same global ARG declarations, argument overrides, and automatic platform
// values as Create, but deliberately leaves body references for later planning
// after inherited base-image instructions have been discovered.
func ResolveFromSources(def *definition.Definition, opts Options) ([]FromSource, error) {
	var err error
	opts, _, err = normalizeSourceDateEpochOptions(opts)
	if err != nil {
		return nil, err
	}
	if _, err := NormalizeParameters(opts.Arguments); err != nil {
		return nil, err
	}
	if _, err := NormalizeParameters(opts.PublishedArguments); err != nil {
		return nil, err
	}
	if err := definition.Validate(def); err != nil {
		return nil, err
	}
	if opts.Platform == "" {
		opts.Platform = "linux/amd64"
	}
	if err := validatePlatform(opts.Platform); err != nil {
		return nil, err
	}
	contexts, err := normalizeBuildContexts(opts.BuildContexts)
	if err != nil {
		return nil, err
	}

	g := &graph{
		opts: opts, globals: scope{}, automatic: automaticPlatformScope(opts),
		aliases: map[string]int{}, declared: map[string]bool{}, contexts: contexts,
		contextOverrides: map[int]buildcontext.Spec{}, contextBindings: map[string][]ContextBinding{},
	}
	if err := g.split(def); err != nil {
		return nil, err
	}
	sourceDateEpoch, err := g.normalizeSuppliedSourceDateEpoch(nil)
	if err != nil {
		return nil, err
	}
	opts = g.opts
	if _, err := g.normalizeGlobalSourceDateEpoch(sourceDateEpoch); err != nil {
		return nil, err
	}

	states := make([]int, len(g.raw))
	platforms := make([]string, len(g.raw))
	resolved := make([]FromSource, len(g.raw))
	var resolveHead func(int) error
	resolveHead = func(index int) error {
		if states[index] == 2 {
			return nil
		}
		if states[index] == 1 {
			return fmt.Errorf("cycle in from references at stage %d", index)
		}
		states[index] = 1

		raw := g.raw[index]
		platform := opts.Platform
		if raw.head.Name == "from" {
			source, err := expand(raw.head.Arguments[0], g.expansionScope(g.globals))
			if err != nil {
				return fmt.Errorf("stage %d from: %w", index, err)
			}
			if source == "" || strings.ContainsAny(source, " \t\r\n$") {
				return fmt.Errorf("invalid from source %q", source)
			}

			item := FromSource{
				StageID:  strconv.Itoa(index),
				Source:   source,
				Platform: platform,
				Kind:     FromSourceImage,
			}
			if context, ok := g.contextOverrides[index]; ok && opts.Mode != Publish {
				item.Source = context.Name
				item.Kind = FromSourceContext
				item.Context = cloneContextSpec(context)
			} else if context, ok := g.contextForSource(source, opts.Mode != Publish); ok {
				item.Kind = FromSourceContext
				item.Context = cloneContextSpec(context)
			} else if base, ok := g.aliases[canonicalStageName(source)]; ok {
				if err := resolveHead(base); err != nil {
					return err
				}
				platform = platforms[base]
				item.Kind = FromSourceStage
				item.BaseStageID = strconv.Itoa(base)
			} else if strings.EqualFold(source, "scratch") {
				item.Kind = FromSourceScratch
			}

			if authored, ok := raw.head.Properties["platform"]; ok {
				expanded, err := expand(authored, g.expansionScope(g.globals))
				if err != nil {
					return fmt.Errorf("stage %d platform: %w", index, err)
				}
				if err := validatePlatform(expanded); err != nil {
					return err
				}
				if item.Kind == FromSourceStage && expanded != platform {
					return fmt.Errorf("stage %d cannot change the platform of its base stage", index)
				}
				platform = expanded
			}
			item.Platform = platform
			resolved[index] = item
		}

		platforms[index] = platform
		states[index] = 2
		return nil
	}

	result := make([]FromSource, 0, len(g.raw))
	for index, raw := range g.raw {
		if raw.head.Name != "from" {
			continue
		}
		if err := resolveHead(index); err != nil {
			return nil, err
		}
		result = append(result, resolved[index])
	}
	return result, nil
}
