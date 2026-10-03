package buildah

import (
	"fmt"
	"net"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"coopr/internal/planner"
	buildkitinstructions "github.com/moby/buildkit/frontend/dockerfile/instructions"
	"go.podman.io/common/pkg/signal"
	"go.podman.io/image/v5/types"
)

// PlanOptions supplies execution-only values which are intentionally absent
// from planner.Plan.
type PlanOptions struct {
	// ProgressPrefix attributes concurrent platform builds without affecting execution.
	ProgressPrefix string
	// ProgressReference is a display label, never an image annotation.
	ProgressReference  string
	progressStageTotal int
	// componentParent shares selection and cycle state with nested local publication.
	// Execution options and caches remain owned by each executor.
	componentParent *graphExecutor

	Lifecycle        LifecycleControls
	Store            StoreOptions
	ContextDir       string
	IgnoreFile       string
	ContextArtifacts []string
	Isolation        string
	Runtime          string
	Output           Output
	Resolver         *oci.Resolver
	// ResolvedBases pins image inputs already selected while binding inherited
	// base metadata. Execution must use those exact storage images rather than
	// resolving a mutable tag a second time.
	ResolvedBases map[ResolvedBaseKey]ResolvedImageSource
	// ReplannedBaseDelta returns immutable image and named-context selections
	// activated by a dynamic local-base replan. It is process-local because the
	// planner's deferred replan closure is also process-local.
	ReplannedBaseDelta      func() map[ResolvedBaseKey]ResolvedImageSource
	CacheLocalDir           string // Optional OCI layout for portable component states.
	CacheRepository         string // Optional caller-selected OCI registry repository.
	CacheFrom               []CacheSpec
	CacheTo                 []CacheSpec
	NoCache                 bool   // Bypass existing cache entries while saving fresh results.
	Network                 string // Default RUN network mode: default, none, or host.
	AddHosts                []string
	RunControls             RunControls
	ImageControls           ImageControls
	Jobs                    int
	LogRusage               bool
	RusageLogFile           string
	Timestamp               *int64
	SourceDateEpoch         *int64
	SourceDateEpochOverride *int64
	RewriteTimestamp        bool
	CacheTTL                *time.Duration
	Allow                   []string // Explicit request-scoped entitlements, such as network.host.
	Secrets                 []string // Buildah --secret source specifications, never secret bytes.
	SSH                     []string // Buildah --ssh source specifications.
	// ProxyArgs are Dockerfile predefined proxy build arguments. They are
	// applied to RUN by the executor and deliberately kept outside planner and
	// instruction-cache identities unless the definition explicitly declares ARG.
	ProxyArgs     map[string]string
	SystemContext *types.SystemContext
	JobID         string // Set only by the supervised worker for deterministic cleanup.
	// CacheMountID resolves an omitted cache mount ID from executor-owned,
	// immutable scope. Callers must include definition/component identity,
	// platform, normalized parameters, and stage identity in that scope.
	CacheMountID CacheMountIDResolver
}

// ResolvedBaseKey includes the stage platform because the same mutable image
// reference can select different immutable manifests in one build graph.
type ResolvedBaseKey struct {
	Reference string
	Platform  string
}

type CacheMountIDResolver func(operation planner.Operation, mountIndex int) (string, error)

// RequestFromPlan lowers the currently supported standalone-plan subset into
// direct Buildah operations. It accepts exactly one scratch stage and never
// renders a Containerfile.
func RequestFromPlan(plan *planner.Plan, options PlanOptions) (Request, error) {
	var err error
	options, err = normalizePlanOptions(options)
	if err != nil {
		return Request{}, err
	}
	sourceDateEpoch := plan.SourceDateEpoch
	if options.SourceDateEpochOverride != nil {
		sourceDateEpoch = options.SourceDateEpochOverride
	}
	if err := validateTimestampOptions(options.Timestamp, sourceDateEpoch, options.RewriteTimestamp); err != nil {
		return Request{}, err
	}
	stage, err := standaloneScratchStage(plan)
	if err != nil {
		return Request{}, err
	}
	resolved, err := resolveAndValidateBuildNetwork([]planner.Stage{stage}, options.Network, options.RunControls)
	if err != nil {
		return Request{}, err
	}
	stage = resolved[0]
	operations, err := lowerOperationsWithCacheMountIDs(stage.Operations, options.CacheMountID)
	if err != nil {
		return Request{}, fmt.Errorf("stage %s: %w", stage.ID, err)
	}
	type runProxyScope struct{ declared, shadowed []string }
	runScopes := make([]runProxyScope, 0)
	for _, planned := range stage.Operations {
		if planned.Name == "run" {
			runScopes = append(runScopes, runProxyScope{planned.DeclaredProxyArguments, planned.ShadowedProxyArguments})
		}
	}
	runIndex := 0
	for index, operation := range operations {
		if run, ok := operation.(Run); ok {
			run.ContextIgnoreFile = options.IgnoreFile
			run.SecretSpecs = slices.Clone(options.Secrets)
			run.SSHSpecs = slices.Clone(options.SSH)
			if runIndex < len(runScopes) {
				run.Env = mergePredefinedProxyEnvironment(run.Env, options.ProxyArgs, runScopes[runIndex].declared, runScopes[runIndex].shadowed)
			}
			runIndex++
			operations[index] = run
		} else if copied, ok := operation.(Copy); ok {
			copied.IgnoreFile = options.IgnoreFile
			operations[index] = copied
		} else if added, ok := operation.(Add); ok {
			added.IgnoreFile = options.IgnoreFile
			operations[index] = added
		}
	}
	return Request{
		Store: options.Store, Base: "scratch", ContextDir: options.ContextDir,
		Isolation: options.Isolation, Runtime: options.Runtime,
		Operations: operations, Secrets: slices.Clone(options.Secrets), SSH: slices.Clone(options.SSH), Allow: slices.Clone(options.Allow), AddHosts: slices.Clone(options.AddHosts), RunControls: options.RunControls, Lifecycle: options.Lifecycle, ImageControls: options.ImageControls, Output: options.Output,
		Timestamp: options.Timestamp, SourceDateEpoch: sourceDateEpoch, RewriteTimestamp: options.RewriteTimestamp, CacheTTL: options.CacheTTL,
	}, nil
}

func standaloneScratchStage(plan *planner.Plan) (planner.Stage, error) {
	if plan == nil {
		return planner.Stage{}, fmt.Errorf("plan is nil")
	}
	if plan.Mode != planner.Build || plan.DefinitionType != "container" {
		return planner.Stage{}, fmt.Errorf("direct Buildah adapter requires a standalone container build plan")
	}
	if len(plan.Stages) != 1 || len(plan.Outputs) != 1 {
		return planner.Stage{}, fmt.Errorf("direct Buildah adapter requires exactly one stage and one output")
	}
	stage := plan.Stages[0]
	if plan.Outputs[0] != stage.ID {
		return planner.Stage{}, fmt.Errorf("output %q does not select stage %q", plan.Outputs[0], stage.ID)
	}
	if stage.Kind != "from" || !strings.EqualFold(stage.Source, "scratch") {
		return planner.Stage{}, fmt.Errorf("stage %s must be from scratch, got %s %q", stage.ID, stage.Kind, stage.Source)
	}
	if len(stage.Dependencies) != 0 || len(plan.Inputs) != 0 {
		return planner.Stage{}, fmt.Errorf("scratch stage must not have stage or external inputs")
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if plan.Platform != platform || stage.Platform != platform {
		return planner.Stage{}, fmt.Errorf("direct Buildah adapter currently requires native platform %q, got plan %q stage %q", platform, plan.Platform, stage.Platform)
	}
	return stage, nil
}

func lowerOperationsWithCacheMountIDs(planned []planner.Operation, resolve CacheMountIDResolver) ([]Operation, error) {
	result := make([]Operation, 0, len(planned))
	shell := []string{"/bin/sh", "-c"}
	for index, operation := range planned {
		lowered, nextShell, err := lowerOperationWithCacheMountIDs(operation, shell, resolve, nil)
		if err != nil {
			return nil, fmt.Errorf("operation %d %q: %w", index+1, operation.Name, err)
		}
		result = append(result, lowered...)
		if nextShell != nil {
			shell = nextShell
		}
	}
	return result, nil
}

func lowerOperation(operation planner.Operation, shell []string) ([]Operation, []string, error) {
	return lowerOperationWithCacheMountIDs(operation, shell, nil, nil)
}

func lowerOperationWithCacheMountIDs(operation planner.Operation, shell []string, resolve CacheMountIDResolver, boundMountSources map[int]string) ([]Operation, []string, error) {
	instruction := operation.Instruction
	switch instruction.Name {
	case "arg":
		if err := plainInstruction(instruction, -1); err != nil {
			return nil, nil, err
		}
		if len(instruction.Arguments) < 1 || len(instruction.Arguments) > 2 {
			return nil, nil, fmt.Errorf("ARG requires a name and optional value")
		}
		return []Operation{historyOnlyOperation{}}, nil, nil
	case "run":
		if len(instruction.Arguments) == 0 {
			return nil, nil, fmt.Errorf("RUN requires a command")
		}
		if err := allowProperties(instruction, "network", "security"); err != nil {
			return nil, nil, err
		}
		security := instruction.Properties["security"]
		switch security {
		case "", "sandbox":
		case "insecure":
		default:
			return nil, nil, fmt.Errorf("RUN security mode %q is not supported", security)
		}
		command := slices.Clone(instruction.Arguments)
		if instruction.Form != "exec" {
			if instruction.Form != "" && instruction.Form != "shell" {
				return nil, nil, fmt.Errorf("RUN form %q is not supported", instruction.Form)
			}
			command = append(slices.Clone(shell), instruction.Arguments[0])
		}
		mounts, err := lowerRunMounts(operation, resolve, boundMountSources)
		if err != nil {
			return nil, nil, fmt.Errorf("RUN mounts: %w", err)
		}
		devices, err := lowerRunDevices(instruction)
		if err != nil {
			return nil, nil, fmt.Errorf("RUN devices: %w", err)
		}
		return []Operation{Run{Command: command, InlineFiles: slices.Clone(instruction.InlineFiles), Network: instruction.Properties["network"], Security: security, Mounts: mounts, Devices: devices, Env: argumentEnvironment(operation.ArgumentsInScope)}}, nil, nil
	case "copy", "add":
		return lowerCopyOrAdd(instruction, operation.InputContext)
	case "env", "label":
		if len(instruction.Children) != 0 || instruction.Form != "" {
			return nil, nil, fmt.Errorf("%s children or form are not supported", strings.ToUpper(instruction.Name))
		}
		if len(instruction.Arguments) == 2 {
			if len(instruction.Properties) != 0 {
				return nil, nil, fmt.Errorf("%s cannot mix arguments and properties", strings.ToUpper(instruction.Name))
			}
			return []Operation{keyValueOperation(instruction.Name, instruction.Arguments[0], instruction.Arguments[1])}, nil, nil
		}
		if len(instruction.Arguments) != 0 {
			return nil, nil, fmt.Errorf("%s requires properties or a key/value pair", strings.ToUpper(instruction.Name))
		}
		keys := make([]string, 0, len(instruction.Properties))
		for key := range instruction.Properties {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		lowered := make([]Operation, 0, len(keys))
		for _, key := range keys {
			lowered = append(lowered, keyValueOperation(instruction.Name, key, instruction.Properties[key]))
		}
		return lowered, nil, nil
	case "workdir":
		if err := plainInstruction(instruction, 1); err != nil {
			return nil, nil, err
		}
		return []Operation{WorkDir(instruction.Arguments[0])}, nil, nil
	case "user":
		if err := plainInstruction(instruction, 1); err != nil {
			return nil, nil, err
		}
		return []Operation{User(instruction.Arguments[0])}, nil, nil
	case "cmd", "entrypoint":
		if len(instruction.Properties) != 0 || len(instruction.Children) != 0 {
			return nil, nil, fmt.Errorf("%s properties and children are not supported", strings.ToUpper(instruction.Name))
		}
		arguments := slices.Clone(instruction.Arguments)
		if instruction.Form == "shell" {
			if len(instruction.Arguments) != 1 {
				return nil, nil, fmt.Errorf("%s shell form requires one command", strings.ToUpper(instruction.Name))
			}
			arguments = append(slices.Clone(shell), instruction.Arguments[0])
		} else if instruction.Form != "" && instruction.Form != "exec" {
			return nil, nil, fmt.Errorf("%s form %q is not supported", strings.ToUpper(instruction.Name), instruction.Form)
		}
		if instruction.Name == "cmd" {
			return []Operation{Cmd(arguments)}, nil, nil
		}
		return []Operation{Entrypoint(arguments)}, nil, nil
	case "shell":
		if err := plainInstruction(instruction, -1); err != nil {
			return nil, nil, err
		}
		if len(instruction.Arguments) == 0 {
			return nil, nil, fmt.Errorf("SHELL requires at least one argument")
		}
		next := slices.Clone(instruction.Arguments)
		return []Operation{Shell(next)}, next, nil
	case "stopsignal":
		if err := plainInstruction(instruction, 1); err != nil {
			return nil, nil, err
		}
		if _, err := signal.ParseSignal(instruction.Arguments[0]); err != nil {
			return nil, nil, err
		}
		return []Operation{StopSignal(instruction.Arguments[0])}, nil, nil
	case "expose":
		if err := plainInstruction(instruction, -1); err != nil {
			return nil, nil, err
		}
		if len(instruction.Arguments) == 0 {
			return nil, nil, fmt.Errorf("EXPOSE requires at least one port")
		}
		ports := make([]string, 0, len(instruction.Arguments))
		for _, value := range instruction.Arguments {
			ports = append(ports, strings.Fields(value)...)
		}
		normalized, err := normalizeExposedPorts(ports)
		if err != nil {
			return nil, nil, err
		}
		operations := make([]Operation, 0, len(normalized))
		for _, port := range normalized {
			operations = append(operations, Expose(port))
		}
		return operations, nil, nil
	case "volume":
		if err := plainInstruction(instruction, -1); err != nil {
			return nil, nil, err
		}
		if len(instruction.Arguments) == 0 {
			return nil, nil, fmt.Errorf("VOLUME requires at least one path")
		}
		operations := make([]Operation, 0, len(instruction.Arguments))
		for _, volume := range instruction.Arguments {
			volume = strings.TrimSpace(volume)
			if volume == "" || strings.ContainsRune(volume, '\x00') {
				return nil, nil, fmt.Errorf("VOLUME path %q is invalid", volume)
			}
			operations = append(operations, Volume(volume))
		}
		return operations, nil, nil
	case "maintainer":
		if err := plainInstruction(instruction, 1); err != nil {
			return nil, nil, err
		}
		maintainer := instruction.Arguments[0]
		if strings.TrimSpace(maintainer) == "" || strings.ContainsAny(maintainer, "\r\n\x00") {
			return nil, nil, fmt.Errorf("MAINTAINER requires one nonempty single-line value")
		}
		return []Operation{Maintainer(maintainer)}, nil, nil
	case "healthcheck":
		logical := imageconfig.New()
		if err := logical.Apply(instruction); err != nil {
			return nil, nil, err
		}
		healthcheck, err := logical.Healthcheck()
		if err != nil {
			return nil, nil, err
		}
		if healthcheck == nil {
			return nil, nil, fmt.Errorf("HEALTHCHECK did not produce runtime metadata")
		}
		return []Operation{Healthcheck(*healthcheck)}, nil, nil
	case "onbuild":
		logical := imageconfig.New()
		if err := logical.Apply(instruction); err != nil {
			return nil, nil, err
		}
		onBuild, err := logical.OnBuild()
		if err != nil {
			return nil, nil, err
		}
		if len(onBuild) != 1 {
			return nil, nil, fmt.Errorf("ONBUILD did not produce one trigger")
		}
		if _, err := parseOnBuildTrigger(onBuild[0]); err != nil {
			return nil, nil, err
		}
		return []Operation{OnBuild(onBuild[0])}, nil, nil
	default:
		return nil, nil, fmt.Errorf("instruction is not supported by the direct Buildah adapter")
	}
}

// normalizeExposedPorts mirrors BuildKit's Dockerfile EXPOSE normalization:
// bare ports use tcp, ranges expand into individual ports, and protocols are
// case-insensitive. Legacy host/IP mappings remain accepted for compatibility,
// but only their container-side ports are recorded in the image config.
func normalizeExposedPorts(ports []string) ([]string, error) {
	result := make([]string, 0, len(ports))
	for _, rawPort := range ports {
		normalized, err := normalizeExposedPort(rawPort)
		if err != nil {
			return nil, fmt.Errorf("invalid port %q: %w", rawPort, err)
		}
		result = append(result, normalized...)
	}
	return result, nil
}

func normalizeExposedPort(rawPort string) ([]string, error) {
	ip, hostPort, containerPort := splitExposedPortParts(rawPort)
	containerPort, proto, _ := strings.Cut(containerPort, "/")
	if containerPort == "" {
		return nil, fmt.Errorf("no port specified")
	}
	switch strings.ToLower(proto) {
	case "":
		proto = "tcp"
	case "tcp", "udp", "sctp":
		proto = strings.ToLower(proto)
	default:
		return nil, fmt.Errorf("invalid proto: %s", proto)
	}

	if ip != "" && ip[0] == '[' {
		rawIP, _, err := net.SplitHostPort(ip + ":")
		if err != nil {
			return nil, fmt.Errorf("invalid IP address %s: %w", ip, err)
		}
		ip = rawIP
	}
	if ip != "" && net.ParseIP(ip) == nil {
		return nil, fmt.Errorf("invalid IP address: %s", ip)
	}

	startPort, endPort, err := parseExposedPortRange(containerPort)
	if err != nil {
		return nil, fmt.Errorf("invalid containerPort: %s", containerPort)
	}
	if hostPort != "" {
		startHostPort, endHostPort, err := parseExposedPortRange(hostPort)
		if err != nil {
			return nil, fmt.Errorf("invalid hostPort: %s", hostPort)
		}
		if endPort-startPort != endHostPort-startHostPort && endPort != startPort {
			return nil, fmt.Errorf("invalid ranges specified for container and host Ports: %s and %s", containerPort, hostPort)
		}
	}

	result := make([]string, 0, endPort-startPort+1)
	for port := startPort; port <= endPort; port++ {
		result = append(result, strconv.Itoa(port)+"/"+proto)
	}
	return result, nil
}

func parseExposedPortRange(ports string) (int, int, error) {
	if ports == "" {
		return 0, 0, fmt.Errorf("empty string specified for ports")
	}
	start, end, rangeSpecified := strings.Cut(ports, "-")
	startPort, err := parseExposedPortNumber(start)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start port %q: %w", start, err)
	}
	if !rangeSpecified || start == end {
		return startPort, startPort, nil
	}
	endPort, err := parseExposedPortNumber(end)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid end port %q: %w", end, err)
	}
	if endPort < startPort {
		return 0, 0, fmt.Errorf("invalid port range: %s", ports)
	}
	return startPort, endPort, nil
}

func parseExposedPortNumber(rawPort string) (int, error) {
	if rawPort == "" {
		return 0, fmt.Errorf("value is empty")
	}
	port, err := strconv.ParseInt(rawPort, 10, 0)
	if err != nil {
		return 0, err
	}
	if port < 0 || port > 65535 {
		return 0, fmt.Errorf("value out of range (0-65535)")
	}
	return int(port), nil
}

func splitExposedPortParts(rawPort string) (hostIP, hostPort, containerPort string) {
	parts := strings.Split(rawPort, ":")
	switch len(parts) {
	case 1:
		return "", "", parts[0]
	case 2:
		return "", parts[0], parts[1]
	case 3:
		return parts[0], parts[1], parts[2]
	default:
		last := len(parts)
		return strings.Join(parts[:last-2], ":"), parts[last-2], parts[last-1]
	}
}

func lowerRunDevices(instruction definition.Instruction) ([]runDeviceRequest, error) {
	var requests []runDeviceRequest
	for index, child := range instruction.Children {
		if child.Name != "device" {
			continue
		}
		if len(child.Arguments) == 0 || child.Form != "" || len(child.Children) != 0 {
			return nil, fmt.Errorf("child %d: device requires a selector", index+1)
		}
		if err := allowProperties(child, "required"); err != nil {
			return nil, err
		}
		for _, selector := range child.Arguments {
			device, err := buildkitinstructions.ParseDevice(selector)
			if err != nil {
				return nil, fmt.Errorf("child %d: %w", index+1, err)
			}
			if strings.TrimSpace(device.Name) == "" {
				return nil, fmt.Errorf("child %d: device name is empty", index+1)
			}
			if value, present := child.Properties["required"]; present {
				required, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("child %d: device required must be true or false", index+1)
				}
				device.Required = required
			}
			requests = append(requests, runDeviceRequest{Name: device.Name, Required: device.Required})
		}
	}
	return requests, nil
}

func lowerCopyOrAdd(instruction definition.Instruction, inputContext string) ([]Operation, []string, error) {
	if len(instruction.Arguments) < 1 || len(instruction.Arguments)-1+len(instruction.InlineFiles) < 1 {
		return nil, nil, fmt.Errorf("%s requires at least one source and a destination", strings.ToUpper(instruction.Name))
	}
	if instruction.Form != "" {
		return nil, nil, fmt.Errorf("%s form %q is not supported", strings.ToUpper(instruction.Name), instruction.Form)
	}
	if inputContext != "build" {
		return nil, nil, fmt.Errorf("%s requires the standalone build context, got %q", strings.ToUpper(instruction.Name), inputContext)
	}
	allowed := []string{"chown", "chmod", "link", "parents"}
	if instruction.Name == "add" {
		allowed = append(allowed, "checksum", "keep-git-dir", "unpack")
	}
	if err := allowProperties(instruction, allowed...); err != nil {
		return nil, nil, err
	}
	excludes := []string{}
	for _, child := range instruction.Children {
		if child.Name != "exclude" || len(child.Arguments) == 0 || len(child.Properties) != 0 || len(child.Children) != 0 {
			return nil, nil, fmt.Errorf("%s only supports exclude children", strings.ToUpper(instruction.Name))
		}
		excludes = append(excludes, child.Arguments...)
	}
	sources := slices.Clone(instruction.Arguments[:len(instruction.Arguments)-1])
	destination := instruction.Arguments[len(instruction.Arguments)-1]
	inlineFiles := slices.Clone(instruction.InlineFiles)
	if instruction.Name == "add" && instruction.Properties["checksum"] != "" && len(sources) != 1 {
		return nil, nil, fmt.Errorf("ADD checksum requires exactly one path source")
	}
	parents := false
	if value, ok := instruction.Properties["parents"]; ok {
		var err error
		parents, err = strconv.ParseBool(value)
		if err != nil {
			return nil, nil, fmt.Errorf("%s parents must be true or false", strings.ToUpper(instruction.Name))
		}
	}
	link := false
	if value, ok := instruction.Properties["link"]; ok {
		switch value {
		case "true":
			link = true
		case "false":
		default:
			return nil, nil, fmt.Errorf("%s link must be true or false", strings.ToUpper(instruction.Name))
		}
	}
	if instruction.Name == "copy" {
		if err := requireLocalCopySources(sources); err != nil {
			return nil, nil, err
		}
		return []Operation{Copy{
			Sources: sources, InlineFiles: inlineFiles, Destination: destination, Chown: instruction.Properties["chown"],
			Chmod: instruction.Properties["chmod"], Link: link, Parents: parents, Excludes: excludes,
		}}, nil, nil
	}
	keepGitDir, err := optionalBoolProperty(instruction, "keep-git-dir")
	if err != nil {
		return nil, nil, err
	}
	unpack, err := optionalBoolProperty(instruction, "unpack")
	if err != nil {
		return nil, nil, err
	}
	return []Operation{Add{
		Sources: sources, InlineFiles: inlineFiles, Destination: destination, Chown: instruction.Properties["chown"],
		Chmod: instruction.Properties["chmod"], Checksum: instruction.Properties["checksum"], KeepGitDir: keepGitDir, Unpack: unpack,
		Link: link, Parents: parents, Excludes: excludes,
	}}, nil, nil
}

func optionalBoolProperty(instruction definition.Instruction, name string) (*bool, error) {
	value, ok := instruction.Properties[name]
	if !ok {
		return nil, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return nil, fmt.Errorf("%s %s must be true or false", strings.ToUpper(instruction.Name), name)
	}
	return &parsed, nil
}

func keyValueOperation(name, key, value string) Operation {
	if name == "env" {
		return Env{Name: key, Value: value}
	}
	return Label{Name: key, Value: value}
}

func plainInstruction(instruction definition.Instruction, argumentCount int) error {
	if instruction.Form != "" || len(instruction.Properties) != 0 || len(instruction.Children) != 0 {
		return fmt.Errorf("%s form, properties, and children are not supported", strings.ToUpper(instruction.Name))
	}
	if argumentCount >= 0 && len(instruction.Arguments) != argumentCount {
		return fmt.Errorf("%s requires %d arguments", strings.ToUpper(instruction.Name), argumentCount)
	}
	return nil
}

func allowProperties(instruction definition.Instruction, allowed ...string) error {
	for property := range instruction.Properties {
		if !slices.Contains(allowed, property) {
			return fmt.Errorf("%s property %q is not supported", strings.ToUpper(instruction.Name), property)
		}
	}
	return nil
}

func argumentEnvironment(arguments map[string]string) []string {
	keys := make([]string, 0, len(arguments))
	for key := range arguments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+arguments[key])
	}
	return environment
}

func mergePredefinedProxyEnvironment(environment []string, proxyArgs map[string]string, blockedGroups ...[]string) []string {
	if len(proxyArgs) == 0 {
		return environment
	}
	blocked := make(map[string]bool)
	for _, group := range blockedGroups {
		for _, name := range group {
			blocked[name] = true
		}
	}
	values := make(map[string]string, len(environment)+len(proxyArgs))
	for _, item := range environment {
		name, value, found := strings.Cut(item, "=")
		if found {
			values[name] = value
		}
	}
	for name, value := range proxyArgs {
		if planner.IsPredefinedProxyArgument(name) && !blocked[name] {
			values[name] = value
		}
	}
	return argumentEnvironment(values)
}
