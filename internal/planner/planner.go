// Package planner separates component publication from invocation without executing
// commands, resolving registries, or reading build-context files.
package planner

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/onbuildparse"
	"github.com/containerd/platforms"
)

type Mode string

const (
	Build   Mode = "build"
	Publish Mode = "publish"
	Invoke  Mode = "invoke"
)

type Options struct {
	// DeferImageSource identifies external inputs produced during execution.
	DeferImageSource func(string) (bool, error) `json:"-"`
	Mode             Mode
	Target           string
	Arguments        map[string]string
	// ContextSourceDateEpoch is the timestamp resolved from a remote primary
	// context. It is used only when SOURCE_DATE_EPOCH has the value "context".
	ContextSourceDateEpoch  *int64
	SourceDateEpochResolver SourceDateEpochResolver `json:"-"`
	// StageBinds supplies base-image state discovered after parsing but before
	// graph resolution. Keys are raw numeric stage IDs, before closure pruning.
	StageBinds map[string]StageBind
	// PublishedArguments represents argument metadata from an already published
	// component. It is distinct from the consumer's invocation overrides.
	PublishedArguments map[string]string
	Platform           string
	// BuildContexts are normalized named inputs. Planning retains only contexts
	// used by the selected closure and records every binding in Plan.Contexts.
	BuildContexts []buildcontext.Spec
	// TransientRunMounts are execution inputs, never published component instructions.
	TransientRunMounts []definition.Instruction
	// BuildUnusedStages includes standalone stages or component package producers
	// up to the selected target.
	BuildUnusedStages bool
}

// StageBind contains inherited instructions and environment from a FROM base.
// Planning clones the complete value before using it.
type StageBind struct {
	Inherited       []definition.Instruction
	BaseEnvironment map[string]string
}

type Operation struct {
	definition.Instruction
	// NetworkExplicit distinguishes an authored network="default" from the
	// planner's canonical default so executors can apply a build-wide fallback.
	NetworkExplicit  bool              `json:"network_explicit,omitempty"`
	ArgumentsInScope map[string]string `json:"arguments_in_scope,omitempty"`
	// DeclaredProxyArguments records predefined proxy arguments explicitly
	// declared in this stage. Their values use normal ARG cache semantics;
	// undeclared proxy values are supplied only by the executor at RUN time.
	DeclaredProxyArguments []string `json:"declared_proxy_arguments,omitempty"`
	// ShadowedProxyArguments records predefined proxy names supplied by ENV.
	// The executor must not let an undeclared CLI proxy override image config.
	ShadowedProxyArguments []string `json:"shadowed_proxy_arguments,omitempty"`
	// InputContext identifies the root for copy/add without from. MountContexts
	// uses the child index for bind mounts without from. Stage-backed inputs keep
	// their from reference and have no context annotation.
	InputContext        string         `json:"input_context,omitempty"`
	MountContexts       map[int]string `json:"mount_contexts,omitempty"`
	TransientMountCount int            `json:"transient_mount_count,omitempty"`
}

func hasProperty(properties map[string]string, name string) bool {
	_, ok := properties[name]
	return ok
}

// ContextBinding identifies one exact use of a named build context. Operation
// and MountIndex are -1 for a FROM binding; MountIndex is -1 for COPY/ADD.
type ContextBinding struct {
	Role       string `json:"role"` // from, copy, or run-mount
	StageID    string `json:"stage_id"`
	Operation  int    `json:"operation"`
	MountIndex int    `json:"mount_index"`
}

// ContextInput is a used named context together with its already-classified
// graph bindings. Executors consume these bindings instead of re-inferring
// context precedence from raw instruction strings.
type ContextInput struct {
	buildcontext.Spec
	Bindings []ContextBinding `json:"bindings"`
}

type Stage struct {
	AfterStage          string `json:"after_stage,omitempty"`
	DeferredImageSource bool   `json:"deferred_image_source,omitempty"`
	ID                  string `json:"id"`
	Name                string `json:"name,omitempty"`
	Kind                string `json:"kind"`
	Source              string `json:"source,omitempty"`
	// SourceContext names the context supplying this stage's root filesystem.
	SourceContext string `json:"source_context,omitempty"`
	Platform      string `json:"platform"`
	// InheritedOnBuildPlanned marks a FROM whose base triggers were injected
	// during planning. The executor clears those consumed triggers from the
	// child image config instead of parsing or executing them a second time.
	InheritedOnBuildPlanned bool `json:"inherited_onbuild_planned,omitempty"`
	// DynamicBaseStage identifies a local base whose configuration is only
	// known after execution because it contains a component invocation. The
	// executor must bind that completed image configuration and replan before
	// executing this stage.
	DynamicBaseStage string              `json:"dynamic_base_stage,omitempty"`
	Requirements     map[string][]string `json:"requirements,omitempty"`
	Dependencies     []string            `json:"dependencies,omitempty"`
	Operations       []Operation         `json:"operations,omitempty"`
	// History retains non-executable instructions at their source position
	// without changing operation indexes used by graph dependencies.
	History []HistoryOperation `json:"history,omitempty"`
}

type HistoryOperation struct {
	Before    int       `json:"before"`
	Operation Operation `json:"operation"`
}

type Argument struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
}

type Plan struct {
	Mode           Mode   `json:"mode"`
	Target         string `json:"target,omitempty"`
	DefinitionType string `json:"definition_type"`
	Platform       string `json:"platform"`
	// SourceDateEpoch is the canonical numeric SOURCE_DATE_EPOCH selected for
	// image metadata. It remains separate from stage ARG visibility.
	SourceDateEpoch *int64  `json:"source_date_epoch,omitempty"`
	Stages          []Stage `json:"stages"`
	// CompatibilityRoots constrain the caller without scheduling dormant bodies.
	CompatibilityRoots []Stage    `json:"compatibility_roots,omitempty"`
	Outputs            []string   `json:"outputs"`
	Arguments          []Argument `json:"arguments,omitempty"`
	// PackageArguments must accompany the published component, independently of
	// its filesystem snapshots, so invocation cannot silently reinterpret them.
	PackageArguments map[string]string `json:"package_arguments,omitempty"`
	// Inputs are symbolic references, NOT registry-resolved digests.
	Inputs    []Input             `json:"unresolved_inputs,omitempty"`
	Contexts  []ContextInput      `json:"contexts,omitempty"`
	Component *PublishedComponent `json:"component,omitempty"`
	// replan is intentionally process-local. Raw-definition workers and nested
	// component resolution construct plans in the same process that executes
	// them, so the closure can retain the verified resolver and frozen
	// publication metadata without putting either into the public plan format.
	replan dynamicStageReplanner `json:"-"`
}

type dynamicStageReplanner func(string, StageBind) (*Plan, error)

// ReplanDynamicStage binds the completed configuration of a deferred local
// or filesystem base and reruns normal demand-driven planning. Any inherited ONBUILD
// dependencies are therefore selected and verified before the child executes.
func (plan *Plan) ReplanDynamicStage(stageID string, bind StageBind) (*Plan, error) {
	if plan == nil || plan.replan == nil {
		return nil, fmt.Errorf("plan has no deferred dynamic stage replanner")
	}
	deferred := false
	for _, stage := range plan.Stages {
		if stage.ID == stageID && (stage.DynamicBaseStage != "" || stage.DeferredImageSource) {
			deferred = true
			break
		}
	}
	if !deferred {
		return nil, fmt.Errorf("stage %s is not awaiting a dynamic local base", stageID)
	}
	return plan.replan(stageID, bind)
}

type Input struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
}

// PublishedComponent is symbolic publication metadata, not an OCI artifact.
// It binds a pruned definition to one output and the package argument snapshot.
// Registry resolution will eventually supply this metadata from a verified artifact.
type PublishedComponent struct {
	Definition         *definition.Definition  `json:"definition"`
	Output             string                  `json:"output"`
	DormantRoots       []string                `json:"dormant_roots,omitempty"`
	Platform           string                  `json:"platform"`
	PackageArguments   map[string]string       `json:"package_arguments,omitempty"`
	FromBindings       map[string]FromBinding  `json:"from_bindings,omitempty"`
	ReservedStageNames []string                `json:"reserved_stage_names,omitempty"`
	StageReferences    []StageReferenceBinding `json:"stage_references,omitempty"`
}

// FromBinding freezes the graph role of a FROM source at publication. An image
// binding intentionally does not pin the reference here; OCI resolution does so
// later, while invocation arguments may select another external image.
type FromBinding struct {
	AfterStage  string `json:"after_stage,omitempty"`
	AfterIndex  string `json:"after_index,omitempty"`
	Kind        string `json:"kind"` // stage, image, or scratch
	Stage       string `json:"stage,omitempty"`
	SourceIndex string `json:"source_index,omitempty"`
}

// StageReferenceBinding freezes each COPY/ADD and mount dependency to the
// stage it used at publication. Origin distinguishes an authored operation
// from one inherited from a package image's ONBUILD configuration.
// MountIndex is -1 for COPY/ADD itself.
type StageReferenceBinding struct {
	Stage      string `json:"stage"`
	Origin     string `json:"origin"` // authored or onbuild
	Operation  int    `json:"operation"`
	MountIndex int    `json:"mount_index"`
	// Kind is "stage" when omitted for compatibility with existing artifacts.
	// An image binding freezes the external-image role while allowing its
	// parameterized reference to change at invocation.
	Kind   string `json:"kind,omitempty"`
	Target string `json:"target,omitempty"`
	// SourceIndex records an authored numeric stage selector before publication
	// pruned and reindexed the graph. Invocation maps it to Target.
	SourceIndex string `json:"source_index,omitempty"`
}

// NormalizeParameters returns a canonical, reversible encoding for cache keys.
// JSON map keys are sorted by encoding/json; a missing key and an empty value
// have different encodings. Nil and empty maps both mean no parameters. Invalid
// UTF-8 is rejected because JSON would replace it and collapse distinct bytes.
func NormalizeParameters(parameters map[string]string) ([]byte, error) {
	for _, key := range sortedKeys(parameters) {
		if !utf8.ValidString(key) {
			return nil, fmt.Errorf("parameter name %q is not valid UTF-8", key)
		}
		if !utf8.ValidString(parameters[key]) {
			return nil, fmt.Errorf("parameter %q value is not valid UTF-8", key)
		}
	}
	if parameters == nil {
		parameters = map[string]string{}
	}
	return json.Marshal(parameters)
}

type binding struct {
	value    string
	present  bool
	conflict bool
}
type scope map[string]binding

type rawStage struct {
	head definition.Instruction
	body []definition.Instruction
}

type graph struct {
	opts                Options
	globals             scope
	automatic           scope
	globalInstructions  []definition.Instruction
	raw                 []rawStage
	stages              []Stage
	aliases             map[string]int
	states              []int
	finalScopes         []scope
	finalEnvironments   []map[string]string
	finalShadows        []map[string]bool
	finalDeclaredProxy  []map[string]bool
	finalOnBuild        [][]string
	dynamicConfig       []bool
	verifiedConfig      []bool
	packageDerived      []bool
	inheritedOperations []int
	packageResolved     []bool
	packageFixed        []map[string]bool
	resolutionEpoch     []uint64
	scopes              []scope
	deps                [][]int
	bases               []int
	packages            []int
	extended            []int
	compatibilityOnly   map[int]bool
	declared            map[string]bool
	bindResolver        StageBindResolver
	reservedStageNames  map[string]bool
	contexts            map[string]buildcontext.Spec
	contextOverrides    map[int]buildcontext.Spec
	published           *PublishedComponent
	contextBindings     map[string][]ContextBinding
}

var identifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
var stageName = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)

// Create validates the whole definition before choosing a phase. Package nodes
// become immutable input leaves in invocation plans, not executable producers.
func Create(def *definition.Definition, opts Options) (*Plan, error) {
	if opts.Mode == Invoke {
		return nil, fmt.Errorf("invoke mode requires a publication plan; use Instantiate")
	}
	return create(def, opts, nil, nil)
}

// Instantiate plans the output fixed in publication metadata. Invocation options
// may supply application arguments, but cannot retarget the published component.
func Instantiate(component *PublishedComponent, opts Options) (*Plan, error) {
	if len(opts.StageBinds) != 0 {
		return nil, fmt.Errorf("package stage binds must come from verified package configurations")
	}
	return instantiate(component, opts, nil)
}

// InstantiateWithPackageBinds uses base metadata from verified immutable
// package snapshots for direct FROM-package stages in a published component.
func InstantiateWithPackageBinds(component *PublishedComponent, opts Options, binds map[string]StageBind) (*Plan, error) {
	if len(opts.StageBinds) != 0 {
		return nil, fmt.Errorf("invocation options cannot supply stage binds")
	}
	if err := validatePackageStageBinds(component, binds); err != nil {
		return nil, err
	}
	opts.StageBinds = binds
	return instantiate(component, opts, nil)
}

// InstantiateDemandDriven uses verified package snapshot metadata while
// resolving external image metadata only for the selected invocation closure.
// Inherited ONBUILD instructions may activate retained dormant stages.
func InstantiateDemandDriven(component *PublishedComponent, opts Options, packageBinds map[string]StageBind, resolve StageBindResolver) (*Plan, error) {
	if resolve == nil {
		return nil, fmt.Errorf("demand-driven invocation requires a stage bind resolver")
	}
	if len(opts.StageBinds) != 0 {
		return nil, fmt.Errorf("invocation options cannot supply stage binds")
	}
	if err := validatePackageStageBinds(component, packageBinds); err != nil {
		return nil, err
	}
	opts.StageBinds = packageBinds
	return instantiate(component, opts, resolve)
}

func validatePackageStageBinds(component *PublishedComponent, binds map[string]StageBind) error {
	raw, err := ProjectPublished(component)
	if err != nil {
		return fmt.Errorf("invalid published component: %w", err)
	}
	required := make(map[string]bool)
	for _, stage := range raw.Stages {
		if stage.Kind != "from" {
			continue
		}
		binding := raw.FromBindings[stage.ID]
		if binding.Kind != "stage" {
			continue
		}
		base, err := strconv.Atoi(binding.Stage)
		if err != nil || base < 0 || base >= len(raw.Stages) {
			return fmt.Errorf("invalid package base stage %q", binding.Stage)
		}
		if raw.Stages[base].Kind == "package" {
			required[stage.ID] = true
		}
	}
	if len(binds) != len(required) {
		return fmt.Errorf("verified package stage binds do not match direct FROM-package stages")
	}
	for stageID := range binds {
		if !required[stageID] {
			return fmt.Errorf("stage bind %s is not a direct FROM-package stage", stageID)
		}
	}
	return nil
}

func instantiate(component *PublishedComponent, opts Options, resolve StageBindResolver) (*Plan, error) {
	if component == nil || component.Definition == nil || component.Output == "" || component.Platform == "" {
		return nil, fmt.Errorf("incomplete published component metadata")
	}
	if _, err := ValidatePublished(component); err != nil {
		return nil, fmt.Errorf("invalid published component: %w", err)
	}
	for name := range opts.Arguments {
		if _, fixed := component.PackageArguments[name]; fixed {
			return nil, fmt.Errorf("argument %q is fixed at publication and cannot be overridden during invocation", name)
		}
	}
	if opts.Target != "" {
		return nil, fmt.Errorf("target selection is not supported in invoke mode; the published output is fixed")
	}
	if len(opts.PublishedArguments) != 0 {
		return nil, fmt.Errorf("package arguments must come from publication metadata")
	}
	if opts.Mode != "" && opts.Mode != Invoke {
		return nil, fmt.Errorf("Instantiate only supports invoke mode")
	}
	if opts.Platform != "" && !samePlatform(opts.Platform, component.Platform) {
		return nil, fmt.Errorf("invocation platform must match published platform %q", component.Platform)
	}
	opts.Mode = Invoke
	opts.Platform = component.Platform
	opts.PublishedArguments = component.PackageArguments
	return create(component.Definition, opts, component, resolve)
}

func normalizeSourceDateEpochOptions(opts Options) (Options, *int64, error) {
	value, supplied := opts.Arguments["SOURCE_DATE_EPOCH"]
	if !supplied {
		return opts, nil, nil
	}
	epoch, canonical, numeric, err := parseNumericSourceDateEpoch(value)
	if !numeric {
		return opts, nil, nil
	}
	if err != nil {
		return Options{}, nil, err
	}
	opts.Arguments = maps.Clone(opts.Arguments)
	opts.Arguments["SOURCE_DATE_EPOCH"] = canonical
	return opts, epoch, nil
}

func (g *graph) normalizeGlobalSourceDateEpoch(current *int64) (*int64, error) {
	global, declared := g.globals["SOURCE_DATE_EPOCH"]
	if !declared || !global.present {
		return current, nil
	}
	epoch, canonical, err := g.resolveSourceDateEpoch(global.value)
	if err != nil {
		return nil, err
	}
	global.value = canonical
	g.globals["SOURCE_DATE_EPOCH"] = global
	return epoch, nil
}

func (g *graph) normalizeSuppliedSourceDateEpoch(current *int64) (*int64, error) {
	value, supplied := g.opts.Arguments["SOURCE_DATE_EPOCH"]
	if !supplied {
		return current, nil
	}
	epoch, canonical, err := g.resolveSourceDateEpoch(value)
	if err != nil {
		return nil, err
	}
	g.opts.Arguments = maps.Clone(g.opts.Arguments)
	g.opts.Arguments["SOURCE_DATE_EPOCH"] = canonical
	if global, declared := g.globals["SOURCE_DATE_EPOCH"]; declared {
		global.value, global.present = canonical, true
		g.globals["SOURCE_DATE_EPOCH"] = global
	}
	return epoch, nil
}

func create(def *definition.Definition, opts Options, published *PublishedComponent, bindResolver StageBindResolver) (*Plan, error) {
	opts, sourceDateEpoch, err := normalizeSourceDateEpochOptions(opts)
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
	if opts.Mode != "" && opts.Mode != Build && opts.Mode != Publish && opts.Mode != Invoke {
		return nil, fmt.Errorf("unknown mode %q", opts.Mode)
	}
	if opts.Mode != Invoke && len(opts.PublishedArguments) > 0 {
		return nil, fmt.Errorf("published arguments are only accepted in invoke mode")
	}
	contexts, err := normalizeBuildContexts(opts.BuildContexts)
	if err != nil {
		return nil, err
	}
	g := &graph{
		opts: opts, globals: scope{}, automatic: automaticPlatformScope(opts), published: published,
		aliases: map[string]int{}, declared: map[string]bool{}, bindResolver: bindResolver,
		reservedStageNames: map[string]bool{}, contexts: contexts,
		contextOverrides: map[int]buildcontext.Spec{}, contextBindings: map[string][]ContextBinding{},
	}
	if published != nil {
		for _, name := range published.ReservedStageNames {
			g.reservedStageNames[canonicalStageName(name)] = true
		}
	}
	if err := g.split(def); err != nil {
		return nil, err
	}
	sourceDateEpoch, err = g.normalizeSuppliedSourceDateEpoch(sourceDateEpoch)
	if err != nil {
		return nil, err
	}
	opts = g.opts
	sourceDateEpoch, err = g.normalizeGlobalSourceDateEpoch(sourceDateEpoch)
	if err != nil {
		return nil, err
	}
	stageBinds, err := validateAndCloneStageBinds(g.raw, opts.StageBinds)
	if err != nil {
		return nil, err
	}
	component := len(g.extended) > 0
	if opts.Mode == "" {
		if component {
			opts.Mode = Publish
		} else {
			opts.Mode = Build
		}
	}
	if (opts.Mode == Build && component) || (opts.Mode != Build && !component) {
		return nil, fmt.Errorf("%s mode does not match definition: components require at least one extend", opts.Mode)
	}
	allowStageBinds := opts.Mode == Build || opts.Mode == Publish && bindResolver != nil || opts.Mode == Invoke && published != nil
	if len(stageBinds) != 0 && !allowStageBinds {
		return nil, fmt.Errorf("stage binds for components require frozen publication metadata")
	}
	opts.StageBinds = stageBinds
	g.opts = opts
	if !component && len(g.packages) > 0 {
		return nil, fmt.Errorf("package stages require a component definition with extend")
	}
	if opts.Mode == Invoke && opts.Target != "" {
		return nil, fmt.Errorf("target selection is not supported in invoke mode; the published output is fixed")
	}
	n := len(g.raw)
	if n == 0 {
		return nil, fmt.Errorf("no stages: expected from or extend")
	}
	g.stages = make([]Stage, n)
	g.states = make([]int, n)
	g.finalScopes = make([]scope, n)
	g.finalEnvironments = make([]map[string]string, n)
	g.finalShadows = make([]map[string]bool, n)
	g.finalDeclaredProxy = make([]map[string]bool, n)
	g.finalOnBuild = make([][]string, n)
	g.dynamicConfig = make([]bool, n)
	g.verifiedConfig = make([]bool, n)
	g.packageDerived = make([]bool, n)
	g.inheritedOperations = make([]int, n)
	g.packageResolved = make([]bool, n)
	g.packageFixed = make([]map[string]bool, n)
	g.resolutionEpoch = make([]uint64, n)
	g.scopes = make([]scope, n)
	g.deps = make([][]int, n)
	g.bases = make([]int, n)
	for i := range g.bases {
		g.bases[i] = -1
	}
	output := n - 1
	if opts.Mode == Invoke {
		var err error
		output, err = strconv.Atoi(published.Output)
		if err != nil || output < 0 || output >= n {
			return nil, fmt.Errorf("invalid published output %q", published.Output)
		}
	}
	if bindResolver != nil && opts.Target != "" {
		var ok bool
		output, ok = g.aliases[canonicalStageName(opts.Target)]
		if !ok {
			return nil, fmt.Errorf("unknown target %q", opts.Target)
		}
	}
	if bindResolver != nil {
		if (component && opts.Mode != Publish && opts.Mode != Invoke) || (!component && opts.Mode != Build) {
			return nil, fmt.Errorf("demand-driven planning mode %q does not match the definition", opts.Mode)
		}
		if err := g.resolve(output); err != nil {
			return nil, err
		}
		if opts.BuildUnusedStages && opts.Mode == Build {
			for index := 0; index <= output; index++ {
				if err := g.resolve(index); err != nil {
					return nil, err
				}
			}
		} else if opts.BuildUnusedStages && opts.Mode == Publish {
			for index := 0; index <= output; index++ {
				if g.raw[index].head.Name != "package" {
					continue
				}
				if err := g.resolvePhase(index, true); err != nil {
					return nil, err
				}
			}
		}
	} else {
		for i := range g.raw {
			if err := g.resolve(i); err != nil {
				return nil, err
			}
		}
	}
	if opts.Mode == Invoke {
		resolvedOnly := bindResolver != nil
		if err := g.checkFromBindings(published.FromBindings, published.ReservedStageNames, resolvedOnly); err != nil {
			return nil, err
		}
		expectedReferences := published.StageReferences
		if resolvedOnly {
			expectedReferences = slices.DeleteFunc(slices.Clone(expectedReferences), func(ref StageReferenceBinding) bool {
				id, _ := strconv.Atoi(ref.Stage) // ValidatePublished checked every stage ID.
				// Deferred bodies have no operations until their executed base is
				// rebound. Their immutable references are checked during that replan.
				return g.states[id] != 2 || g.stages[id].DynamicBaseStage != "" || g.stages[id].DeferredImageSource
			})
		}
		if !slices.Equal(g.stageReferences(nil), expectedReferences) {
			return nil, fmt.Errorf("stage copy or mount references changed from publication")
		}
	}
	for key := range opts.PublishedArguments {
		if !g.declared[key] {
			return nil, fmt.Errorf("unknown published argument %q", key)
		}
	}
	// Full graph cycle checks include copy and mount edges, not just FROM chains.
	if bindResolver == nil {
		if _, err := g.walk(allIndices(n), false); err != nil {
			return nil, err
		}
		if opts.Target != "" {
			var ok bool
			output, ok = g.aliases[canonicalStageName(opts.Target)]
			if !ok {
				return nil, fmt.Errorf("unknown target %q", opts.Target)
			}
		}
	}
	if component && g.raw[output].head.Name == "package" {
		return nil, fmt.Errorf("selected output cannot be a package stage")
	}
	componentStart := -1
	if component {
		for _, id := range g.extended {
			if id <= output {
				componentStart = id
			}
		}
		if componentStart < 0 {
			return nil, fmt.Errorf("selected component output must follow extend; preceding FROM stages are producers")
		}
	}
	reachable, err := g.walk([]int{output}, false)
	if err != nil {
		return nil, err
	}
	retentionRoots := []int{output}
	if opts.Mode == Publish {
		selectedClosure := make(map[int]bool, len(reachable))
		for _, id := range reachable {
			selectedClosure[id] = true
		}
		invocationClosure, err := g.walk([]int{output}, true)
		if err != nil {
			return nil, err
		}
		lastDeferredBase := -1
		for _, id := range invocationClosure {
			if g.raw[id].head.Name == "from" && g.bases[id] == -1 && !strings.EqualFold(g.stages[id].Source, "scratch") && id > lastDeferredBase {
				lastDeferredBase = id
			}
		}
		if lastDeferredBase >= 0 {
			for id, raw := range g.raw {
				// External ONBUILD is discovered only when the invocation base is
				// resolved. It may reference a later named package or an unaliased
				// stage by its original numeric index, so retain every stage and its
				// source order. This also keeps projected numeric IDs unchanged.
				if selectedClosure[id] {
					continue
				}
				if err := g.resolvePhase(id, raw.head.Name == "package"); err != nil {
					return nil, err
				}
				retentionRoots = append(retentionRoots, id)
			}
			reachable, err = g.walk(retentionRoots, false)
			if err != nil {
				return nil, err
			}
		}
	}
	if opts.Mode == Publish {
		hasExtend := false
		for _, id := range reachable {
			hasExtend = hasExtend || g.raw[id].head.Name == "extend"
		}
		if !hasExtend {
			// The selected image may be independent of the caller. Preserve its
			// mandatory compatibility contract without retaining unused body edges.
			g.compatibilityOnly = map[int]bool{}
			contract, err := g.extendContract(componentStart)
			if err != nil {
				return nil, err
			}
			g.stages[componentStart] = contract
			g.deps[componentStart] = nil
			g.compatibilityOnly[componentStart] = true
			retentionRoots = append(retentionRoots, componentStart)
		}
		reachable, err = g.walk(retentionRoots, false)
		if err != nil {
			return nil, err
		}
	}

	requiredPackages := []int{}
	for _, id := range reachable {
		if g.stages[id].Kind == "package" {
			requiredPackages = append(requiredPackages, id)
		}
	}
	publication, err := g.walk(requiredPackages, false)
	if err != nil {
		return nil, err
	}
	fixed := map[string]bool{}
	for _, id := range publication {
		if g.stages[id].Kind == "extend" {
			return nil, fmt.Errorf("package production depends on extend (stage %s)", g.stages[id].ID)
		}
		for name := range g.packageFixed[id] {
			fixed[name] = true
		}
	}
	if opts.Mode == Invoke {
		for name := range opts.PublishedArguments {
			fixed[name] = true
		}
	}
	for name := range opts.PublishedArguments {
		if !fixed[name] {
			return nil, fmt.Errorf("argument %q is not a package argument", name)
		}
	}
	if opts.Mode == Invoke {
		for name := range opts.Arguments {
			if fixed[name] {
				return nil, fmt.Errorf("argument %q is fixed at publication and cannot be overridden during invocation", name)
			}
		}
		// Defaults are source text, not proof of what was used to publish a package.
		for name := range fixed {
			if _, ok := opts.PublishedArguments[name]; !ok {
				return nil, fmt.Errorf("invocation requires published value for package argument %q", name)
			}
		}
	}
	roots := []int{output}
	if opts.Mode == Publish {
		roots = requiredPackages
	}
	executionRoots := roots
	if opts.BuildUnusedStages {
		switch opts.Mode {
		case Build:
			executionRoots = allIndices(output + 1)
		case Publish:
			executionRoots = slices.Clone(requiredPackages)
			for index := 0; index <= output; index++ {
				if g.stages[index].Kind == "package" && !slices.Contains(executionRoots, index) {
					executionRoots = append(executionRoots, index)
				}
			}
			slices.Sort(executionRoots)
		}
	}
	order, err := g.walk(executionRoots, opts.Mode == Invoke)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Mode: opts.Mode, Target: g.stages[output].Name, DefinitionType: "container", Platform: g.stages[output].Platform, SourceDateEpoch: sourceDateEpoch, Stages: []Stage{}, Outputs: []string{}}
	if component {
		plan.DefinitionType = "component"
	}
	for _, i := range roots {
		plan.Outputs = append(plan.Outputs, g.stages[i].ID)
	}
	for _, name := range sortedKeys(g.declared) {
		phase := "build"
		if component {
			phase = "invocation"
			if fixed[name] {
				phase = "package"
			}
		}
		plan.Arguments = append(plan.Arguments, Argument{Name: name, Phase: phase})
	}
	seenInputs := map[Input]bool{}
	addInput := func(kind, ref string) {
		input := Input{Kind: kind, Reference: ref}
		if !seenInputs[input] {
			plan.Inputs = append(plan.Inputs, input)
			seenInputs[input] = true
		}
	}
	selectedStages := make(map[string]bool, len(order))
	for _, id := range order {
		selectedStages[strconv.Itoa(id)] = true
	}
	usedContexts := map[string]bool{}
	for _, i := range order {
		stage := g.stages[i]
		if opts.Mode == Invoke && stage.Kind == "package" {
			stage.Kind = "package-input"
			stage.Operations = nil
			stage.Dependencies = nil
			addInput("package", stage.Name)
		} else {
			for _, op := range stage.Operations {
				if err := definition.ValidateResolved(op.Instruction); err != nil {
					return nil, fmt.Errorf("stage %s %s: %w", stage.ID, op.Name, err)
				}
			}
			if stage.SourceContext != "" {
				addInput("context", stage.SourceContext)
				usedContexts[stage.SourceContext] = true
			} else if stage.Kind == "from" && g.bases[i] == -1 && !strings.EqualFold(stage.Source, "scratch") {
				addInput("image", stage.Source)
			}
			for _, op := range stage.Operations {
				if op.Name == "component" {
					addInput("component", op.Arguments[0])
				}
				if (op.Name == "copy" || op.Name == "add") && op.Properties["from"] != "" {
					if _, context := g.contexts[op.InputContext]; context {
						addInput("context", op.InputContext)
						usedContexts[op.InputContext] = true
					} else if _, local := g.resolveStageReference(op.Properties["from"]); !local {
						addInput("image", op.Properties["from"])
					}
				}
				for childIndex, mount := range op.Children {
					if mount.Name != "mount" || len(mount.Arguments) != 1 ||
						(mount.Arguments[0] != "bind" && mount.Arguments[0] != "cache") || mount.Properties["from"] == "" {
						continue
					}
					if contextName := op.MountContexts[childIndex]; contextName != "" {
						addInput("context", contextName)
						usedContexts[contextName] = true
					} else if _, local := g.resolveStageReference(mount.Properties["from"]); !local {
						addInput("image", mount.Properties["from"])
					}
				}
			}
		}
		plan.Stages = append(plan.Stages, stage)
	}
	hasSelectedExtend := false
	for _, id := range order {
		hasSelectedExtend = hasSelectedExtend || g.raw[id].head.Name == "extend"
	}
	if component && opts.Mode == Invoke && !hasSelectedExtend {
		// Independent targets must not acquire unrelated dormant target contracts.
		// Only an output with no selected caller root needs a separate contract.
		contract, err := g.extendContract(componentStart)
		if err != nil {
			return nil, err
		}
		plan.CompatibilityRoots = append(plan.CompatibilityRoots, contract)
	}

	for _, name := range sortedKeys(usedContexts) {
		bindings := slices.DeleteFunc(slices.Clone(g.contextBindings[name]), func(binding ContextBinding) bool {
			return !selectedStages[binding.StageID]
		})
		plan.Contexts = append(plan.Contexts, ContextInput{Spec: g.contexts[name], Bindings: bindings})
	}
	switch opts.Mode {
	case Invoke:
		plan.PackageArguments = maps.Clone(opts.PublishedArguments)
	case Publish:
		plan.PackageArguments = map[string]string{}
		for name := range fixed {
			// Declarations with distinct stage-local defaults cannot be represented by
			// a single name/value in artifact metadata. Fail rather than lose scope.
			var value string
			found := false
			for _, i := range publication {
				if b, ok := g.scopes[i][name]; ok {
					if b.conflict {
						return nil, fmt.Errorf("package argument %q changes value within a stage; use distinct names", name)
					}
					if !b.present {
						return nil, fmt.Errorf("package argument %q requires a value", name)
					}
					if found && value != b.value {
						return nil, fmt.Errorf("package argument %q changes value across stages; use distinct names", name)
					}
					value = b.value
					found = true
				}
			}
			if !found {
				if global, ok := g.globals[name]; ok {
					if !global.present {
						return nil, fmt.Errorf("package argument %q requires a value", name)
					}
					value = global.value
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("package argument %q requires a value", name)
			}
			plan.PackageArguments[name] = value
		}
	}
	if opts.Mode == Publish {
		invocation, err := g.walk(retentionRoots, true)
		if err != nil {
			return nil, err
		}
		plan.Component = g.component(output, invocation, retentionRoots[1:], plan.PackageArguments)
	}
	for _, stage := range plan.Stages {
		if stage.DynamicBaseStage == "" && !stage.DeferredImageSource {
			continue
		}
		replanOptions := g.opts
		replanOptions.Arguments = maps.Clone(g.opts.Arguments)
		replanOptions.PublishedArguments = maps.Clone(g.opts.PublishedArguments)
		replanOptions.StageBinds = maps.Clone(g.opts.StageBinds)
		replanOptions.BuildContexts = slices.Clone(g.opts.BuildContexts)
		plan.replan = func(stageID string, bind StageBind) (*Plan, error) {
			if _, exists := replanOptions.StageBinds[stageID]; exists {
				return nil, fmt.Errorf("stage %s dynamic local base is already bound", stageID)
			}
			next := replanOptions
			next.StageBinds = maps.Clone(replanOptions.StageBinds)
			if next.StageBinds == nil {
				next.StageBinds = make(map[string]StageBind)
			}
			next.StageBinds[stageID] = bind
			return create(def, next, published, bindResolver)
		}
		break
	}
	return plan, nil
}

// Keep the invocation closure in source order. Package stages become opaque
// leaves: their producer bodies and private dependencies ran during publication
// and must not be replayed or validated as invocation operations.
func (g *graph) component(output int, reachable, dormant []int, args map[string]string) *PublishedComponent {
	kept := make(map[int]bool, len(reachable))
	dormantSet := make(map[int]bool, len(dormant))
	newIDs := make(map[int]string, len(reachable))
	for _, id := range reachable {
		kept[id] = true
	}
	for _, id := range dormant {
		dormantSet[id] = true
	}
	def := &definition.Definition{}
	// Preserve global declarations, including their source defaults. Their fixed
	// values are separately stored in package metadata and override those defaults.
	def.Instructions = append(def.Instructions, g.globalInstructions...)
	selected, next := "", 0
	for id := range g.raw {
		if kept[id] {
			newIDs[id] = strconv.Itoa(next)
			next++
		}
	}
	bindings := map[string]FromBinding{}
	for id, raw := range g.raw {
		if !kept[id] {
			continue
		}
		if id == output {
			selected = newIDs[id]
		}
		if raw.head.Name == "from" {
			binding := FromBinding{Kind: "image"}
			switch {
			case g.bases[id] >= 0:
				binding = FromBinding{Kind: "stage", Stage: newIDs[g.bases[id]]}
				if _, err := strconv.Atoi(g.stages[id].Source); err == nil {
					binding.SourceIndex = g.stages[id].Source
				}
			case strings.EqualFold(g.stages[id].Source, "scratch"):
				binding.Kind = "scratch"
			}
			if after := g.stages[id].AfterStage; after != "" {
				target, _ := strconv.Atoi(after)
				binding.AfterStage = newIDs[target]
				expanded, _ := expand(raw.head.Properties["after"], g.expansionScope(g.globals))
				if canonicalSourceIndex(expanded) {
					binding.AfterIndex = expanded
				}
			}
			bindings[newIDs[id]] = binding
		}
		head := raw.head
		if dormantSet[id] && head.Name == "extend" && head.Properties["as"] != "" {
			// Retain dormant extend stages as numeric-index placeholders without
			// making their target names invocation-visible. FROM/package stages
			// keep names because external ONBUILD may consume them with --from.
			head.Properties = maps.Clone(head.Properties)
			delete(head.Properties, "as")
		}
		def.Instructions = append(def.Instructions, head)
		if raw.head.Name == "package" {
			// Preserve the fixed package-argument scope without retaining the
			// producer instructions. Instantiate uses these declarations to
			// validate the immutable values stored in publication metadata.
			for _, name := range sortedKeys(args) {
				def.Instructions = append(def.Instructions, definition.Instruction{
					Name: "arg", Arguments: []string{name, args[name]},
				})
			}
		} else if !g.compatibilityOnly[id] {
			def.Instructions = append(def.Instructions, foldLayerInstructions(raw.body)...)
		}
	}
	dormantRoots := make([]string, 0, len(dormant))
	for _, id := range dormant {
		if mapped, ok := newIDs[id]; ok && mapped != selected {
			dormantRoots = append(dormantRoots, mapped)
		}
	}
	return &PublishedComponent{Definition: def, Output: selected, DormantRoots: dormantRoots, Platform: g.opts.Platform, PackageArguments: maps.Clone(args), FromBindings: bindings, ReservedStageNames: sortedKeys(g.aliases), StageReferences: g.stageReferences(newIDs)}
}

// Bodies originate from validated nested definitions, so their generated
// boundaries are balanced and cannot contain stage declarations.
func foldLayerInstructions(body []definition.Instruction) []definition.Instruction {
	var result []definition.Instruction
	var stack []*[]definition.Instruction
	target := &result
	for _, inst := range body {
		switch inst.LayerBoundary {
		case "begin":
			*target = append(*target, definition.Instruction{Name: "layer"})
			stack = append(stack, target)
			target = &(*target)[len(*target)-1].Children
		case "end":
			target = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
		default:
			*target = append(*target, inst)
		}
	}
	return result
}

func (g *graph) stageReferences(newIDs map[int]string) []StageReferenceBinding {
	refs := []StageReferenceBinding{}
	index := func(id int) string {
		if newIDs == nil {
			return strconv.Itoa(id)
		}
		return newIDs[id]
	}
	for id, stage := range g.stages {
		if g.compatibilityOnly[id] {
			continue
		}
		if newIDs != nil {
			if _, kept := newIDs[id]; !kept {
				continue
			}
			if stage.Kind == "package" {
				continue
			}
		}
		for opIndex, op := range stage.Operations {
			origin, sourceIndex := "authored", opIndex-g.inheritedOperations[id]
			if opIndex < g.inheritedOperations[id] {
				if !g.packageDerived[id] {
					continue
				}
				origin, sourceIndex = "onbuild", opIndex
			}
			if (op.Name == "copy" || op.Name == "add") && op.Properties["from"] != "" {
				ref := StageReferenceBinding{Stage: index(id), Origin: origin, Operation: sourceIndex, MountIndex: -1}
				if target, local := g.resolveStageReference(op.Properties["from"]); local {
					if origin == "onbuild" {
						ref.Kind = "stage"
					}
					ref.Target = index(target)
					if _, numeric := strconv.Atoi(op.Properties["from"]); numeric == nil {
						ref.SourceIndex = g.publishedSourceIndex(index(id), origin, sourceIndex, -1, op.Properties["from"])
					}
				} else {
					ref.Kind = "image"
				}
				refs = append(refs, ref)
			}
			for childIndex, mount := range op.Children {
				if childIndex >= len(op.Children)-op.TransientMountCount {
					continue
				}
				if mount.Properties["from"] != "" {
					ref := StageReferenceBinding{Stage: index(id), Origin: origin, Operation: sourceIndex, MountIndex: childIndex}
					if target, local := g.resolveStageReference(mount.Properties["from"]); local {
						if origin == "onbuild" {
							ref.Kind = "stage"
						}
						ref.Target = index(target)
						if _, numeric := strconv.Atoi(mount.Properties["from"]); numeric == nil {
							ref.SourceIndex = g.publishedSourceIndex(index(id), origin, sourceIndex, childIndex, mount.Properties["from"])
						}
					} else {
						ref.Kind = "image"
					}
					refs = append(refs, ref)
				}
			}
		}
	}
	return refs
}

func (g *graph) checkFromBindings(bindings map[string]FromBinding, reserved []string, resolvedOnly bool) error {
	if !slices.IsSorted(reserved) || len(reserved) != len(slices.Compact(slices.Clone(reserved))) {
		return fmt.Errorf("invalid published stage name reservations")
	}
	count := 0
	for id, raw := range g.raw {
		if raw.head.Name != "from" {
			continue
		}
		if resolvedOnly && g.states[id] != 2 {
			continue
		}
		count++
		key := strconv.Itoa(id)
		expected, ok := bindings[key]
		if !ok {
			return fmt.Errorf("published FROM binding missing for stage %s", key)
		}
		actual := FromBinding{Kind: "image"}
		switch {
		case g.bases[id] >= 0:
			actual = FromBinding{Kind: "stage", Stage: strconv.Itoa(g.bases[id])}
			actual.SourceIndex = expected.SourceIndex
		case strings.EqualFold(g.stages[id].Source, "scratch"):
			actual.Kind = "scratch"
		}
		contextOverride := g.stages[id].SourceContext != ""
		if contextOverride {
			// Explicit build contexts override both image references and stage aliases.
			// They are caller inputs, not a reinterpretation of the published binding.
			// Keep checking the immutable authored ordering edge below.
			actual.Kind, actual.Stage, actual.SourceIndex = expected.Kind, expected.Stage, expected.SourceIndex
		}
		actual.AfterStage = g.stages[id].AfterStage
		actual.AfterIndex = expected.AfterIndex
		if expected != actual {
			return fmt.Errorf("stage %s FROM binding changed from %+v to %+v at invocation", key, expected, actual)
		}
		if actual.Kind == "image" && !contextOverride && containsStageName(reserved, g.stages[id].Source) {
			return fmt.Errorf("stage %s FROM source %q was a stage name at publication", key, g.stages[id].Source)
		}
	}
	if !resolvedOnly && len(bindings) != count {
		return fmt.Errorf("published FROM bindings do not match retained stages")
	}
	return nil
}

func (g *graph) split(def *definition.Definition) error {
	for pos, inst := range definition.FlattenLayers(def.Instructions) {
		switch inst.Name {
		case "from", "extend", "package":
			i := len(g.raw)
			name := inst.Properties["as"]
			if _, set := inst.Properties["as"]; set {
				canonical := canonicalStageName(name)
				if !stageName.MatchString(canonical) || canonical == "scratch" {
					return fmt.Errorf("instruction %d: invalid stage name %q", pos+1, name)
				}
				if _, ok := g.aliases[canonical]; ok {
					return fmt.Errorf("duplicate stage name %q", name)
				}
				g.aliases[canonical] = i
			}
			if inst.Name == "extend" {
				g.extended = append(g.extended, i)
			}
			if inst.Name == "package" {
				g.packages = append(g.packages, i)
			}
			g.raw = append(g.raw, rawStage{head: inst})
			if context, ok := g.contexts[canonicalStageName(name)]; ok && inst.Name == "from" {
				g.contextOverrides[i] = context
			}
		default:
			if len(g.raw) == 0 {
				if inst.Name != "arg" {
					return fmt.Errorf("instruction %d: %s requires a stage", pos+1, inst.Name)
				}
				g.globalInstructions = append(g.globalInstructions, inst)
				if _, err := g.declare(inst, g.globals, nil); err != nil {
					return err
				}
			} else {
				i := len(g.raw) - 1
				g.raw[i].body = append(g.raw[i].body, inst)
			}
		}
	}
	return nil
}

func (g *graph) declare(inst definition.Instruction, values scope, expansion scope) (bool, error) {
	name := inst.Arguments[0]
	if !identifier.MatchString(name) {
		return false, fmt.Errorf("invalid argument name %q", name)
	}
	g.declared[name] = true
	value := values[name]
	supplied := false
	if v, ok := g.opts.Arguments[name]; ok {
		value = binding{value: v, present: true}
		supplied = true
	} else if v, ok := g.opts.PublishedArguments[name]; ok {
		value = binding{value: v, present: true}
		supplied = true
	} else if len(inst.Arguments) == 2 {
		if expansion == nil {
			expansion = g.expansionScope(values)
		}
		expander := expand
		if inst.ProcessQuotes {
			expander = expandDockerWord
		}
		expanded, err := expander(inst.Arguments[1], expansion)
		if err != nil {
			return false, fmt.Errorf("argument %q: %w", name, err)
		}
		value = binding{value: expanded, present: true}
		supplied = true
	} else if value.present {
		// A valueless redeclaration retains the current value without
		// changing a later ENV instruction's precedence over that ARG.
	} else if global, ok := g.globals[name]; ok && global.present {
		value = global
		supplied = true
	} else if automatic, ok := g.automatic[name]; ok {
		value = automatic
		supplied = true
	}
	values[name] = value
	return supplied, nil
}

func validateAndCloneStageBinds(raw []rawStage, binds map[string]StageBind) (map[string]StageBind, error) {
	if len(binds) == 0 {
		return nil, nil
	}
	cloned := make(map[string]StageBind, len(binds))
	for key, bind := range binds {
		index, err := strconv.Atoi(key)
		if err != nil || index < 0 || strconv.Itoa(index) != key || index >= len(raw) {
			return nil, fmt.Errorf("stage bind %q does not identify a stage", key)
		}
		if raw[index].head.Name != "from" {
			return nil, fmt.Errorf("stage bind %q requires a from stage, got %s", key, raw[index].head.Name)
		}

		copy := StageBind{BaseEnvironment: maps.Clone(bind.BaseEnvironment)}
		for name, value := range copy.BaseEnvironment {
			if name == "" || strings.ContainsAny(name, "=\x00") {
				return nil, fmt.Errorf("stage bind %q has invalid environment name %q", key, name)
			}
			if strings.ContainsRune(value, '\x00') {
				return nil, fmt.Errorf("stage bind %q environment %q contains NUL", key, name)
			}
		}
		copy.Inherited = make([]definition.Instruction, 0, len(bind.Inherited))
		for position, inst := range bind.Inherited {
			if !inheritedInstruction(inst.Name) {
				return nil, fmt.Errorf("stage bind %q inherited instruction %d cannot be %q", key, position+1, inst.Name)
			}
			if err := definition.Validate(&definition.Definition{Instructions: []definition.Instruction{inst}}); err != nil {
				return nil, fmt.Errorf("stage bind %q inherited instruction %d: %w", key, position+1, err)
			}
			copy.Inherited = append(copy.Inherited, cloneInstruction(inst))
		}
		cloned[key] = copy
	}
	return cloned, nil
}

func inheritedInstruction(name string) bool {
	switch name {
	case "add", "arg", "cmd", "copy", "entrypoint", "env", "expose", "healthcheck", "label", "run", "shell", "stopsignal", "user", "volume", "workdir":
		return true
	default:
		return false
	}
}

func (g *graph) resolve(i int) error {
	return g.resolvePhase(i, false)
}

// extendContract resolves compatibility separately from execution. A dormant
// caller root still constrains invocation without replaying its unused body.
func (g *graph) extendContract(i int) (Stage, error) {
	raw := g.raw[i]
	stage := Stage{ID: strconv.Itoa(i), Name: raw.head.Properties["as"], Kind: "extend", Platform: g.opts.Platform}
	for _, key := range []string{"architecture", "distro", "distro-version", "package-manager"} {
		var authored []string
		if scalar, present := raw.head.Properties[key]; present {
			authored = []string{scalar}
		} else {
			for _, requirement := range raw.head.Children {
				if requirement.Name == key {
					authored = requirement.Arguments
					break
				}
			}
		}
		if len(authored) == 0 {
			continue
		}
		if stage.Requirements == nil {
			stage.Requirements = make(map[string][]string)
		}
		for _, expression := range authored {
			value, err := expand(expression, g.expansionScope(g.globals))
			if err != nil {
				return Stage{}, fmt.Errorf("stage %s extend %s: %w", stage.ID, key, err)
			}
			value = strings.TrimSpace(value)
			if value == "" || strings.ContainsRune(value, '\x00') {
				return Stage{}, fmt.Errorf("stage %s extend %s must resolve to a nonempty value without NUL", stage.ID, key)
			}
			if key == "architecture" {
				if strings.Contains(value, "/") {
					return Stage{}, fmt.Errorf("stage %s extend architecture must be a single OCI architecture, got %q", stage.ID, value)
				}
				parsed, err := platforms.Parse("linux/" + value)
				if err != nil {
					return Stage{}, fmt.Errorf("stage %s extend invalid architecture %q: %w", stage.ID, value, err)
				}
				value = platforms.Normalize(parsed).Architecture
			}
			stage.Requirements[key] = append(stage.Requirements[key], value)
		}
		slices.Sort(stage.Requirements[key])
		stage.Requirements[key] = slices.Compact(stage.Requirements[key])
	}
	return stage, nil
}

func (g *graph) resolvePhase(i int, packagePhase bool) error {
	// Some focused planner tests construct graph state directly. Keep this
	// derived scope storage lazy so those graphs follow the same invariant.
	if len(g.finalDeclaredProxy) != len(g.raw) {
		g.finalDeclaredProxy = make([]map[string]bool, len(g.raw))
	}
	if g.bindResolver != nil && g.opts.Mode == Publish && g.raw[i].head.Name == "package" {
		packagePhase = true
	}
	if g.states[i] == 2 {
		if g.bindResolver != nil && g.opts.Mode == Publish && packagePhase && !g.packageResolved[i] {
			descendants := g.resolvedLocalDescendants(i)
			descendantPhases := make(map[int]bool, len(descendants))
			for _, descendant := range descendants {
				descendantPhases[descendant] = g.packageResolved[descendant]
			}
			g.resetResolvedStage(i)
			if err := g.resolvePhase(i, true); err != nil {
				return err
			}
			for _, descendant := range descendants {
				g.resetResolvedStage(descendant)
			}
			for _, descendant := range descendants {
				if err := g.resolvePhase(descendant, descendantPhases[descendant]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if g.states[i] == 1 {
		return fmt.Errorf("cycle in from references at stage %d", i)
	}
	g.states[i] = 1
	raw := g.raw[i]
	stage := Stage{ID: strconv.Itoa(i), Name: raw.head.Properties["as"], Kind: raw.head.Name, Platform: g.opts.Platform}
	g.packageDerived[i] = stage.Kind == "package"
	override, stageOverridden := g.contextOverrides[i]
	stageOverridden = stageOverridden && g.contextsAvailable(packagePhase)
	// Global ARGs are frontend scope for structural expansion. Dockerfile stage
	// scope starts empty and imports a global only through a stage ARG.
	values := scope{}
	if stage.Kind == "package" || stage.Kind == "extend" {
		values = maps.Clone(g.globals)
	}
	var environment map[string]string
	var shadows map[string]bool
	declaredProxy := map[string]bool{}
	var baseEpoch uint64
	if stage.Kind == "extend" {
		contract, err := g.extendContract(i)
		if err != nil {
			return err
		}
		stage.Requirements = contract.Requirements
	}
	if stage.Kind == "from" && stageOverridden {
		stage.Source = override.Name
		stage.SourceContext = override.Name
		g.addContextBinding(override.Name, ContextBinding{Role: "from", StageID: stage.ID, Operation: -1, MountIndex: -1})
	} else if stage.Kind == "from" {
		source, err := expand(raw.head.Arguments[0], g.expansionScope(g.globals))
		if err != nil {
			return fmt.Errorf("stage %s from: %w", stage.ID, err)
		}
		if source == "" || strings.ContainsAny(source, " \t\r\n$") {
			return fmt.Errorf("invalid from source %q", source)
		}
		if g.published != nil {
			binding := g.published.FromBindings[stage.ID]
			if binding.SourceIndex != "" {
				source, err = bindPublishedNumericSource(source, binding.SourceIndex, binding.Stage)
				if err != nil {
					return fmt.Errorf("stage %s FROM: %w", stage.ID, err)
				}
			}
		}
		stage.Source = source
		if context, ok := g.contextForSource(source, g.contextsAvailable(packagePhase)); ok {
			stage.SourceContext = context.Name
			g.addContextBinding(context.Name, ContextBinding{Role: "from", StageID: stage.ID, Operation: -1, MountIndex: -1})
		} else if base, ok := g.resolveStageReference(source); ok {
			if err := g.resolvePhase(base, packagePhase); err != nil {
				return err
			}
			g.bases[i] = base
			g.deps[i] = append(g.deps[i], base)
			g.packageDerived[i] = g.packageDerived[base]
			values = maps.Clone(g.finalScopes[base])
			environment = maps.Clone(g.finalEnvironments[base])
			shadows = maps.Clone(g.finalShadows[base])
			declaredProxy = maps.Clone(g.finalDeclaredProxy[base])
			stage.Platform = g.stages[base].Platform
			baseEpoch = g.resolutionEpoch[base]
		}
	}
	if p, ok := raw.head.Properties["platform"]; ok {
		platform, err := expand(p, g.expansionScope(g.globals))
		if err != nil {
			return fmt.Errorf("stage %s platform: %w", stage.ID, err)
		}
		if err := validatePlatform(platform); err != nil {
			return err
		}
		if g.bases[i] >= 0 && platform != stage.Platform {
			return fmt.Errorf("stage %s cannot change the platform of its base stage", stage.ID)
		}
		stage.Platform = platform
	}
	if authored, ok := raw.head.Properties["after"]; ok {
		after, err := expand(authored, g.expansionScope(g.globals))
		if err != nil {
			return fmt.Errorf("stage %s after: %w", stage.ID, err)
		}
		if g.published != nil {
			binding := g.published.FromBindings[stage.ID]
			if binding.AfterIndex != "" {
				after, err = bindPublishedNumericSource(after, binding.AfterIndex, binding.AfterStage)
				if err != nil {
					return err
				}
			}
		}
		dependency, ok := g.resolveStageReference(after)
		if !ok || dependency >= i {
			return fmt.Errorf("stage %s after requires an earlier stage, got %q", stage.ID, after)
		}
		if err := g.resolvePhase(dependency, packagePhase); err != nil {
			return err
		}
		stage.AfterStage = strconv.Itoa(dependency)
		g.deps[i] = append(g.deps[i], dependency)
	}
	bind, bound := g.opts.StageBinds[stage.ID]
	localBind := g.bindResolver != nil && (g.opts.Mode == Publish && (packagePhase || g.packageDerived[i] || g.bases[i] >= 0 && g.packageResolved[g.bases[i]]) ||
		g.opts.Mode == Build || g.opts.Mode == Invoke && g.bases[i] >= 0 && (bound || g.verifiedConfig[g.bases[i]] || g.dynamicConfig[g.bases[i]]))
	deferredSource := stage.AfterStage != ""
	deferReference := stage.Source
	deferContext := false
	if stage.SourceContext != "" {
		context := g.contexts[stage.SourceContext]
		deferContext = context.Kind == buildcontext.DockerImage
		deferReference = context.Reference
	}
	if g.opts.DeferImageSource != nil && g.bases[i] == -1 && (stage.SourceContext == "" || deferContext) && !strings.EqualFold(stage.Source, "scratch") {
		deferSource, err := g.opts.DeferImageSource(deferReference)
		if err != nil {
			return fmt.Errorf("stage %s source: %w", stage.ID, err)
		}
		deferredSource = deferredSource || deferSource
	}
	switch prefix, _, _ := strings.Cut(stage.Source, ":"); prefix {
	case "oci", "oci-archive", "docker-archive", "dir":
		deferredSource = true
	}
	deferredConfig := deferredSource && g.bases[i] == -1 && (stage.SourceContext == "" || deferContext) && !strings.EqualFold(stage.Source, "scratch") && !bound
	stage.DeferredImageSource = deferredConfig
	if stage.Kind == "from" && g.bases[i] >= 0 && (localBind || g.opts.Mode == Invoke && len(g.opts.StageBinds) != 0 && g.packageDerived[i]) {
		dynamicBase := g.dynamicConfig[g.bases[i]]
		if dynamicBase && !bound {
			deferredConfig = true
			stage.DynamicBaseStage = strconv.Itoa(g.bases[i])
		}
		packageBind := g.opts.Mode == Invoke && g.raw[g.bases[i]].head.Name == "package"
		if bound && !packageBind && !dynamicBase {
			return fmt.Errorf("stage %s has a supplied bind for local base stage %d", stage.ID, g.bases[i])
		}
		if !bound && !deferredConfig {
			if g.opts.Mode == Invoke && g.raw[g.bases[i]].head.Name == "package" {
				return fmt.Errorf("stage %s requires verified package base metadata", stage.ID)
			}
			bind.BaseEnvironment = maps.Clone(g.finalEnvironments[g.bases[i]])
			for index, trigger := range g.finalOnBuild[g.bases[i]] {
				inherited, err := onbuildparse.ParseDeferred(trigger)
				if err != nil {
					return fmt.Errorf("stage %s local base ONBUILD trigger %d: %w", stage.ID, index+1, err)
				}
				bind.Inherited = append(bind.Inherited, inherited...)
			}
			bound = true
		}
	}
	if g.bindResolver != nil && (g.opts.Mode != Publish || packagePhase) && stage.Kind == "from" && g.bases[i] == -1 && !strings.EqualFold(stage.Source, "scratch") && !bound && !deferredConfig {
		kind := FromSourceImage
		var contextSpec *buildcontext.Spec
		if stage.SourceContext != "" {
			kind = FromSourceContext
			contextSpec = cloneContextSpec(g.contexts[stage.SourceContext])
		}
		resolved, err := g.bindResolver(FromSource{
			StageID:  stage.ID,
			Source:   stage.Source,
			Platform: stage.Platform,
			Kind:     kind,
			Context:  contextSpec,
		})
		if err != nil {
			return fmt.Errorf("stage %s resolve base %q: %w", stage.ID, stage.Source, err)
		}
		cloned, err := validateAndCloneStageBinds(g.raw, map[string]StageBind{stage.ID: resolved})
		if err != nil {
			return err
		}
		bind = cloned[stage.ID]
		bound = true
		if g.opts.StageBinds == nil {
			g.opts.StageBinds = map[string]StageBind{}
		}
		g.opts.StageBinds[stage.ID] = bind
	}
	stage.InheritedOnBuildPlanned = bound
	if environment == nil {
		environment = make(map[string]string, len(bind.BaseEnvironment))
		shadows = make(map[string]bool, len(bind.BaseEnvironment))
	}
	for name, value := range bind.BaseEnvironment {
		environment[name] = value
		if _, inherited := shadows[name]; !inherited {
			shadows[name] = true
		}
	}
	used := maps.Clone(values)
	instructions := raw.body
	if stageOverridden {
		instructions = nil
	}
	if deferredConfig {
		// The completed base image is authoritative. Expanding authored
		// instructions against the pre-component environment would freeze wrong
		// values, and inherited ONBUILD may add dependencies not yet selected.
		instructions = nil
	}
	inheritedCount := 0
	if bound {
		body := raw.body
		if stageOverridden {
			body = nil
		}
		instructions = make([]definition.Instruction, 0, len(bind.Inherited)+len(body))
		instructions = append(instructions, bind.Inherited...)
		instructions = append(instructions, body...)
		inheritedCount = len(bind.Inherited)
	}
	var onBuild []string
	dynamicConfig := deferredConfig
	packageFixed := map[string]bool{}
	g.collectArgumentReferences(raw.head, g.globals, packageFixed)
	for instructionIndex, inst := range instructions {
		if inst.Name == "run" && len(g.opts.TransientRunMounts) != 0 && (g.opts.Mode != Publish || packagePhase) {
			inst.Children = append(slices.Clone(inst.Children), g.opts.TransientRunMounts...)
		}
		authoredOnBuild := instructionIndex >= inheritedCount && inst.Name == "onbuild"
		if !authoredOnBuild {
			g.collectArgumentReferences(inst, values, packageFixed)
			if inst.Name == "run" {
				for name, value := range values {
					if value.present && !shadows[name] {
						packageFixed[name] = true
					}
				}
			}
		}
		if inst.Name == "arg" {
			if g.opts.Mode == Invoke && stage.Kind == "package" && len(inst.Arguments) > 0 {
				packageFixed[inst.Arguments[0]] = true
			}
			expansion := g.instructionScope(g.expansionScope(values), environment, shadows)
			supplied, err := g.declare(inst, values, expansion)
			if err != nil {
				return fmt.Errorf("stage %s: %w", stage.ID, err)
			}
			if supplied {
				shadows[inst.Arguments[0]] = false
			}
			if IsPredefinedProxyArgument(inst.Arguments[0]) {
				declaredProxy[inst.Arguments[0]] = true
			}
			for name, value := range values {
				previous, exists := used[name]
				if exists && (previous.value != value.value || previous.present != value.present || previous.conflict) {
					value.conflict = true
				}
				used[name] = value
			}
			argument := values[inst.Arguments[0]]
			history := definition.Instruction{Name: "arg", Arguments: []string{inst.Arguments[0]}}
			if argument.present {
				history.Arguments = append(history.Arguments, argument.value)
			}
			stage.History = append(stage.History, HistoryOperation{
				Before: len(stage.Operations), Operation: Operation{Instruction: history},
			})
			continue
		}
		expansionValues := g.instructionScope(values, environment, shadows)
		normalized, err := normalize(inst, expansionValues)
		if err != nil {
			return fmt.Errorf("stage %s %s: %w", stage.ID, inst.Name, err)
		}
		if err := expandStructuralReferences(inst, &normalized, g.globals, expansionValues); err != nil {
			return fmt.Errorf("stage %s %s: %w", stage.ID, inst.Name, err)
		}
		dynamicInherited := g.opts.Mode == Invoke && instructionIndex < inheritedCount && !g.packageDerived[i]
		operationIndex := len(stage.Operations)
		if g.published != nil {
			origin, ordinal := "authored", operationIndex-g.inheritedOperations[i]
			if instructionIndex < inheritedCount {
				origin, ordinal = "onbuild", operationIndex
			}
			if err := g.bindPublishedNumericReference(stage.ID, origin, ordinal, -1, normalized.Properties); err != nil {
				return err
			}
			for childIndex, mount := range normalized.Children {
				if err := g.bindPublishedNumericReference(stage.ID, origin, ordinal, childIndex, mount.Properties); err != nil {
					return err
				}
			}
		}
		inputContext := ""
		if inst.Name == "copy" || inst.Name == "add" {
			var err error
			inputContext, err = g.reference(i, stage.Platform, normalized.Properties, dynamicInherited, true, packagePhase, operationIndex, -1, "copy")
			if err != nil {
				return err
			}
		}
		mountContexts := map[int]string{}
		for childIndex, mount := range normalized.Children {
			allowImage := mount.Name == "mount" && len(mount.Arguments) == 1 &&
				(mount.Arguments[0] == "bind" || mount.Arguments[0] == "cache")
			contextName, err := g.reference(i, stage.Platform, mount.Properties, dynamicInherited, allowImage, packagePhase, operationIndex, childIndex, "run-mount")
			if err != nil {
				return err
			}
			if contextName != "" {
				mountContexts[childIndex] = contextName
			}
		}
		effective := map[string]string{}
		for _, k := range sortedKeys(values) {
			b := values[k]
			if b.present && !shadows[k] {
				effective[k] = b.value
			}
		}
		shadowedProxy := map[string]bool{}
		for name, shadowed := range shadows {
			if shadowed && IsPredefinedProxyArgument(name) {
				shadowedProxy[name] = true
			}
		}
		op := Operation{
			Instruction: normalized, NetworkExplicit: inst.Name == "run" && hasProperty(inst.Properties, "network"), ArgumentsInScope: effective,
			DeclaredProxyArguments: sortedTrueKeys(declaredProxy), ShadowedProxyArguments: sortedTrueKeys(shadowedProxy),
		}
		if inst.Name == "run" && (g.opts.Mode != Publish || packagePhase) {
			op.TransientMountCount = len(g.opts.TransientRunMounts)
		}
		if inputContext != "" {
			op.InputContext = inputContext
		} else if (inst.Name == "copy" || inst.Name == "add") && normalized.Properties["from"] == "" {
			op.InputContext = localContext(g.opts.Mode)
		}
		if len(mountContexts) != 0 {
			op.MountContexts = mountContexts
		}
		for child, mount := range normalized.Children {
			if mount.Name == "mount" && len(mount.Arguments) == 1 && mount.Arguments[0] == "bind" && mount.Properties["from"] == "" {
				if op.MountContexts == nil {
					op.MountContexts = map[int]string{}
				}
				op.MountContexts[child] = localContext(g.opts.Mode)
			}
		}
		stage.Operations = append(stage.Operations, op)
		if instructionIndex < inheritedCount {
			g.inheritedOperations[i]++
		}
		if normalized.Name == "component" {
			dynamicConfig = true
		}
		if instructionIndex >= inheritedCount && normalized.Name == "onbuild" {
			onBuild = append(onBuild, normalized.Arguments[0])
		}
		if normalized.Name == "env" {
			applyEnvironment(normalized, environment, shadows)
		}
	}
	g.scopes[i] = used
	g.finalScopes[i] = values
	g.finalEnvironments[i] = environment
	g.finalShadows[i] = shadows
	g.finalDeclaredProxy[i] = declaredProxy
	g.finalOnBuild[i] = onBuild
	if len(g.packageFixed) != len(g.raw) {
		g.packageFixed = make([]map[string]bool, len(g.raw))
	}
	g.packageFixed[i] = packageFixed
	g.dynamicConfig[i] = dynamicConfig
	g.verifiedConfig[i] = !dynamicConfig && (bound || g.bases[i] >= 0 && g.verifiedConfig[g.bases[i]])
	for _, d := range g.deps[i] {
		stage.Dependencies = append(stage.Dependencies, strconv.Itoa(d))
	}
	g.stages[i] = stage
	if g.bindResolver != nil {
		if g.opts.Mode == Publish && !packagePhase {
			for _, dependency := range g.deps[i] {
				if g.raw[dependency].head.Name == "package" {
					if err := g.resolvePhase(dependency, true); err != nil {
						return err
					}
				}
			}
		}
		for {
			for _, dependency := range g.deps[i] {
				if err := g.resolvePhase(dependency, packagePhase); err != nil {
					return err
				}
			}
			complete := true
			for _, dependency := range g.deps[i] {
				if g.states[dependency] != 2 {
					complete = false
					break
				}
			}
			if complete {
				break
			}
		}
	}
	if g.bases[i] >= 0 && g.resolutionEpoch[g.bases[i]] != baseEpoch {
		g.resetResolvedStage(i)
		return g.resolvePhase(i, packagePhase)
	}
	g.packageResolved[i] = packagePhase
	g.resolutionEpoch[i]++
	g.states[i] = 2
	return nil
}

// resetResolvedStage promotes a stage first discovered through an invocation
// branch into package production. Invocation-only resolution never fetches an
// external base, so replaying the stage here is what binds package inputs while
// leaving unrelated invocation branches unresolved.
func (g *graph) resetResolvedStage(i int) {
	g.states[i] = 0
	g.stages[i] = Stage{}
	g.finalScopes[i] = nil
	g.finalEnvironments[i] = nil
	g.finalShadows[i] = nil
	g.finalDeclaredProxy[i] = nil
	g.finalOnBuild[i] = nil
	g.dynamicConfig[i] = false
	g.verifiedConfig[i] = false
	g.packageDerived[i] = false
	g.inheritedOperations[i] = 0
	g.packageResolved[i] = false
	if i < len(g.packageFixed) {
		g.packageFixed[i] = nil
	}
	g.scopes[i] = nil
	g.deps[i] = nil
	g.bases[i] = -1
}

func (g *graph) resolvedLocalDescendants(base int) []int {
	selected := map[int]bool{base: true}
	var descendants []int
	for {
		added := false
		for stage := range g.raw {
			if g.states[stage] != 2 || selected[stage] || g.bases[stage] < 0 || !selected[g.bases[stage]] {
				continue
			}
			selected[stage] = true
			descendants = append(descendants, stage)
			added = true
		}
		if !added {
			return descendants
		}
	}
}

func (g *graph) instructionScope(values scope, environment map[string]string, shadows map[string]bool) scope {
	result := maps.Clone(values)
	for name, value := range environment {
		if _, isArgument := values[name]; !isArgument || shadows[name] {
			result[name] = binding{value: value, present: true}
		}
	}
	return result
}

func applyEnvironment(inst definition.Instruction, environment map[string]string, shadows map[string]bool) {
	if len(inst.Arguments) == 2 {
		environment[inst.Arguments[0]] = inst.Arguments[1]
		shadows[inst.Arguments[0]] = true
	}
	for name, value := range inst.Properties {
		environment[name] = value
		shadows[name] = true
	}
}

func validatePlatform(platform string) error {
	parts := strings.Split(platform, "/")
	if len(parts) < 2 || len(parts) > 3 || slices.Contains(parts, "") {
		return fmt.Errorf("invalid platform %q", platform)
	}
	if parts[0] != "linux" {
		return fmt.Errorf("unsupported platform %q: Coopr requires Linux", platform)
	}
	return nil
}

func normalizeBuildContexts(specs []buildcontext.Spec) (map[string]buildcontext.Spec, error) {
	result := make(map[string]buildcontext.Spec, len(specs))
	for _, spec := range specs {
		name := canonicalStageName(spec.Name)
		if spec.Name != name || !stageName.MatchString(name) || name == "scratch" {
			return nil, fmt.Errorf("invalid build context name %q", spec.Name)
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("duplicate build context name %q", name)
		}
		switch spec.Kind {
		case buildcontext.Local:
			if spec.Path == "" || spec.Reference != "" {
				return nil, fmt.Errorf("invalid local build context %q", name)
			}
		case buildcontext.DockerImage:
			if spec.Path != "" || spec.Reference == "" {
				return nil, fmt.Errorf("invalid docker-image build context %q", name)
			}
		case buildcontext.Git, buildcontext.HTTPArchive:
			if spec.Path != "" || spec.Reference == "" {
				return nil, fmt.Errorf("invalid remote build context %q", name)
			}
		case buildcontext.OCILayout:
			if spec.Path == "" || spec.Reference == "" {
				return nil, fmt.Errorf("invalid OCI-layout build context %q", name)
			}
		default:
			return nil, fmt.Errorf("invalid build context kind %q for %q", spec.Kind, name)
		}
		result[name] = spec
	}
	return result, nil
}

func localContext(mode Mode) string {
	switch mode {
	case Publish:
		return "publisher"
	case Invoke:
		return "caller"
	default:
		return "build"
	}
}

func (g *graph) reference(i int, platform string, props map[string]string, sourceVisible, allowImage, packagePhase bool, operation, mount int, role string) (string, error) {
	source, ok := props["from"]
	if !ok {
		return "", nil
	}
	if context, found := g.contextForSource(source, g.contextsAvailable(packagePhase)); found {
		g.addContextBinding(context.Name, ContextBinding{Role: role, StageID: strconv.Itoa(i), Operation: operation, MountIndex: mount})
		if g.bindResolver != nil && (g.opts.Mode != Publish || packagePhase) {
			if _, err := g.bindResolver(FromSource{
				StageID: strconv.Itoa(i), Source: context.Name, Platform: platform, Kind: FromSourceContext, Context: cloneContextSpec(context),
			}); err != nil {
				return "", fmt.Errorf("stage %d resolve build context %q: %w", i, context.Name, err)
			}
		}
		return context.Name, nil
	}
	target, ok := g.resolveStageReference(source)
	if !ok {
		if sourceVisible && g.reservedStageNames[canonicalStageName(source)] {
			return "", fmt.Errorf("stage %d references unknown stage %q", i, source)
		}
		if !allowImage {
			return "", fmt.Errorf("stage %d references unknown stage %q", i, source)
		}
		if source == "" || strings.EqualFold(source, "scratch") || strings.ContainsAny(source, " \t\r\n$") {
			return "", fmt.Errorf("stage %d references invalid image %q", i, source)
		}
		if _, err := strconv.Atoi(source); err == nil {
			return "", fmt.Errorf("stage %d references unknown numeric stage %q", i, source)
		}
		if g.bindResolver != nil && (g.opts.Mode != Publish || packagePhase) {
			if _, err := g.bindResolver(FromSource{
				StageID: strconv.Itoa(i), Source: source, Platform: platform, Kind: FromSourceCopyImage,
			}); err != nil {
				return "", fmt.Errorf("stage %d resolve image source %q: %w", i, source, err)
			}
		}
		return "", nil
	}
	if !slices.Contains(g.deps[i], target) {
		g.deps[i] = append(g.deps[i], target)
	}
	return "", nil
}

func (g *graph) resolveStageReference(source string) (int, bool) {
	if target, ok := g.aliases[canonicalStageName(source)]; ok {
		return target, true
	}
	target, err := strconv.Atoi(source)
	if err != nil || target < 0 || target >= len(g.raw) || strconv.Itoa(target) != source {
		return 0, false
	}
	return target, true
}

func (g *graph) contextsAvailable(packagePhase bool) bool {
	return g.opts.Mode != Publish || packagePhase
}

func (g *graph) contextForSource(source string, available bool) (buildcontext.Spec, bool) {
	if !available {
		return buildcontext.Spec{}, false
	}
	context, ok := g.contexts[canonicalStageName(source)]
	return context, ok
}

func (g *graph) addContextBinding(name string, binding ContextBinding) {
	bindings := g.contextBindings[name]
	if !slices.Contains(bindings, binding) {
		g.contextBindings[name] = append(bindings, binding)
	}
}

func cloneContextSpec(spec buildcontext.Spec) *buildcontext.Spec {
	cloned := spec
	return &cloned
}

func normalize(inst definition.Instruction, values scope) (definition.Instruction, error) {
	if inst.DeferredOnBuild != "" {
		if inst.Name != "run" {
			return definition.Instruction{}, fmt.Errorf("deferred ONBUILD metadata requires a RUN instruction")
		}
		parsed, err := onbuildparse.ParseExpandedRaw(inst.DeferredOnBuild, func(word string) (string, error) {
			return expandDockerWord(word, values)
		}, func(word string) (string, error) {
			return expandHeredoc(word, values)
		})
		if err != nil {
			return definition.Instruction{}, err
		}
		if len(parsed) != 1 || parsed[0].Name != "run" {
			return definition.Instruction{}, fmt.Errorf("deferred ONBUILD metadata must contain exactly one RUN")
		}
		out := parsed[0]
		if _, explicit := out.Properties["network"]; !explicit {
			if out.Properties == nil {
				out.Properties = make(map[string]string)
			}
			out.Properties["network"] = "default"
		}
		return out, nil
	}
	out := definition.Instruction{Name: inst.Name, Form: inst.Form, LayerBoundary: inst.LayerBoundary, Properties: map[string]string{}}
	for _, file := range inst.InlineFiles {
		data := file.Data
		if file.Expand {
			var err error
			data, err = expandHeredoc(data, values)
			if err != nil {
				return out, err
			}
		}
		out.InlineFiles = append(out.InlineFiles, definition.InlineFile{Path: file.Path, Data: data})
	}
	for _, arg := range inst.Arguments {
		value := arg
		// Runtime commands and inherited triggers retain their payload until
		// execution. Expanding them here could turn data into shell code or
		// substitute the parent's values into a future child stage.
		if inst.Name != "run" && inst.Name != "cmd" && inst.Name != "entrypoint" && inst.Name != "healthcheck" && inst.Name != "onbuild" {
			var err error
			if inst.ProcessQuotes {
				value, err = expandDockerWord(arg, values)
			} else {
				value, err = expand(arg, values)
			}
			if err != nil {
				return out, err
			}
		}
		out.Arguments = append(out.Arguments, value)
	}
	for _, name := range sortedKeys(inst.Properties) {
		expander := expand
		if inst.ProcessQuotes {
			expander = expandDockerWord
		}
		value, err := expander(inst.Properties[name], values)
		if err != nil {
			return out, err
		}
		out.Properties[name] = value
	}
	if inst.Name == "run" {
		if _, explicit := out.Properties["network"]; !explicit {
			out.Properties["network"] = "default"
		}
	}
	for _, child := range inst.Children {
		normalized, err := normalize(child, values)
		if err != nil {
			return out, err
		}
		out.Children = append(out.Children, normalized)
	}
	return out, nil
}

func expandStructuralReferences(authored definition.Instruction, normalized *definition.Instruction, globals, stage scope) error {
	if normalized == nil {
		return nil
	}
	if authored.DeferredOnBuild != "" {
		// The upstream Dockerfile decoder already expanded these fields once.
		return nil
	}
	values := maps.Clone(globals)
	maps.Copy(values, stage)
	if authored.Name == "copy" || authored.Name == "add" {
		if from, present := authored.Properties["from"]; present {
			expander := expand
			if authored.ProcessQuotes {
				expander = expandDockerWord
			}
			expanded, err := expander(from, values)
			if err != nil {
				return fmt.Errorf("expand from: %w", err)
			}
			normalized.Properties["from"] = expanded
		}
	}
	for index := range authored.Children {
		if index >= len(normalized.Children) || authored.Children[index].Name != "mount" {
			continue
		}
		if from, present := authored.Children[index].Properties["from"]; present {
			expander := expand
			if authored.Children[index].ProcessQuotes {
				expander = expandDockerWord
			}
			expanded, err := expander(from, values)
			if err != nil {
				return fmt.Errorf("expand mount %d from: %w", index+1, err)
			}
			normalized.Children[index].Properties["from"] = expanded
		}
	}
	return nil
}

func (g *graph) expansionScope(values scope) scope {
	result := maps.Clone(g.automatic)
	maps.Copy(result, values)
	return result
}

func (g *graph) walk(roots []int, stopPackages bool) ([]int, error) {
	states := make([]int, len(g.raw))
	order := []int{}
	var visit func(int) error
	visit = func(i int) error {
		if states[i] == 2 {
			return nil
		}
		if states[i] == 1 {
			return fmt.Errorf("dependency cycle at stage %s", g.stages[i].ID)
		}
		states[i] = 1
		if !stopPackages || g.stages[i].Kind != "package" {
			for _, dep := range g.deps[i] {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		states[i] = 2
		order = append(order, i)
		return nil
	}
	for _, root := range roots {
		if err := visit(root); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func allIndices(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}
func sortedKeys[V any](m map[string]V) []string {
	keys := slices.Collect(maps.Keys(m))
	slices.Sort(keys)
	return keys
}

// Rebind only numeric executable selectors after expansion. Published authored
// definitions retain their original selectors and invocation must agree with them.
func bindPublishedNumericSource(source, original, target string) (string, error) {
	if source != original {
		return "", fmt.Errorf("numeric stage reference %q changed from published index %q", source, original)
	}
	return target, nil
}

func (g *graph) bindPublishedNumericReference(stage, origin string, operation, mount int, props map[string]string) error {
	for _, ref := range g.published.StageReferences {
		if ref.Stage != stage || ref.Origin != origin || ref.Operation != operation || ref.MountIndex != mount || ref.SourceIndex == "" {
			continue
		}
		source, err := bindPublishedNumericSource(props["from"], ref.SourceIndex, ref.Target)
		if err != nil {
			return fmt.Errorf("stage %s %s operation %d: %w", stage, origin, operation, err)
		}
		props["from"] = source
		break
	}
	return nil
}

func (g *graph) publishedSourceIndex(stage, origin string, operation, mount int, source string) string {
	if g.published != nil {
		for _, ref := range g.published.StageReferences {
			if ref.Stage == stage && ref.Origin == origin && ref.Operation == operation && ref.MountIndex == mount && ref.SourceIndex != "" {
				return ref.SourceIndex
			}
		}
	}
	return source
}
