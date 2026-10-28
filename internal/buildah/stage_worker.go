package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"syscall"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	"go.podman.io/buildah/pkg/sourcepolicy"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"
)

const (
	stageWorkerName  = "coopr-buildah-stage-worker-v1"
	stageWorkerGrace = time.Second
)

type stageWorkerRequest struct {
	ExecutableOrigin      string                            `json:"executable_origin,omitempty"`
	RetainsCaller         bool                              `json:"retains_caller,omitempty"`
	BaseRoot              *PackageRootMetadata              `json:"base_root,omitempty"`
	ProgressReference     string                            `json:"progress_reference,omitempty"`
	ProgressPrefix        string                            `json:"progress_prefix,omitempty"`
	Lifecycle             LifecycleControls                 `json:"lifecycle,omitempty"`
	Mode                  planner.Mode                      `json:"mode"`
	Stage                 planner.Stage                     `json:"stage"`
	Base                  string                            `json:"base"`
	BaseName              string                            `json:"base_name,omitempty"`
	PreserveBaseImageAnns bool                              `json:"preserve_base_image_annotations,omitempty"`
	BaseManifest          digest.Digest                     `json:"base_manifest,omitempty"`
	BaseCommandSet        bool                              `json:"base_command_set,omitempty"`
	CommandSet            bool                              `json:"command_set,omitempty"`
	BaseLogical           json.RawMessage                   `json:"base_logical"`
	Logical               json.RawMessage                   `json:"logical"`
	Operations            []planner.Operation               `json:"operations,omitempty"`
	Aliases               map[string]string                 `json:"aliases,omitempty"`
	Images                map[string]string                 `json:"images,omitempty"`
	ResolvedBases         []stageWorkerBase                 `json:"resolved_bases,omitempty"`
	Delivery              *oci.ComponentDelivery            `json:"delivery,omitempty"`
	ComponentSelections   map[string]oci.ComponentSelection `json:"component_selections,omitempty"`
	ComponentPins         map[string]string                 `json:"component_pins,omitempty"`
	ComponentSources      map[string]string                 `json:"component_sources,omitempty"`
	ActiveComponents      map[digest.Digest]bool            `json:"active_components,omitempty"`
	ActiveLocalComponents map[string]bool                   `json:"active_local_components,omitempty"`
	CacheScope            *CacheMountScope                  `json:"cache_scope,omitempty"`
	Jobs                  int                               `json:"jobs,omitempty"`
	LogRusage             bool                              `json:"log_rusage,omitempty"`
	RusageLogFile         string                            `json:"rusage_log_file,omitempty"`
	Output                bool                              `json:"output,omitempty"`
	CaptureRoot           bool                              `json:"capture_root,omitempty"`
	Store                 StoreOptions                      `json:"store"`
	ContextPrepared       bool                              `json:"context_prepared,omitempty"`
	BuildContexts         []buildcontext.Spec               `json:"build_contexts,omitempty"`
	TransientRunMounts    []RunMount                        `json:"transient_run_mounts,omitempty"`
	ContextDir            string                            `json:"context_dir,omitempty"`
	SourcePolicyFile      string                            `json:"source_policy_file,omitempty"`
	SourcePolicy          *sourcepolicy.Policy              `json:"source_policy,omitempty"`
	IgnoreFile            string                            `json:"ignore_file,omitempty"`
	ContextArtifacts      []string                          `json:"context_artifacts,omitempty"`
	NoCache               bool                              `json:"no_cache,omitempty"`
	CacheLocalDir         string                            `json:"cache_local_dir,omitempty"`
	CacheRepository       string                            `json:"cache_repository,omitempty"`
	CacheFrom             []CacheSpec                       `json:"cache_from,omitempty"`
	CacheTo               []CacheSpec                       `json:"cache_to,omitempty"`
	ComponentRelay        string                            `json:"component_relay,omitempty"`
	InstructionRelay      string                            `json:"instruction_relay,omitempty"`
	Network               string                            `json:"network,omitempty"`
	AddHosts              []string                          `json:"add_hosts,omitempty"`
	ProxyArgs             map[string]string                 `json:"proxy_args,omitempty"`
	RunControls           RunControls                       `json:"run_controls,omitempty"`
	ImageControls         ImageControls                     `json:"image_controls,omitempty"`
	Timestamp             *int64                            `json:"timestamp,omitempty"`
	CacheTTL              *time.Duration                    `json:"cache_ttl,omitempty"`
	SourceDateEpoch       *int64                            `json:"source_date_epoch,omitempty"`
	RewriteTimestamp      bool                              `json:"rewrite_timestamp,omitempty"`
	Allow                 []string                          `json:"allow,omitempty"`
	Secrets               []string                          `json:"secrets,omitempty"`
	SSH                   []string                          `json:"ssh,omitempty"`
	Isolation             string                            `json:"isolation,omitempty"`
	Runtime               string                            `json:"runtime,omitempty"`
	ImageOutput           Output                            `json:"image_output"`
	JobID                 string                            `json:"job_id,omitempty"`
	SignaturePolicy       string                            `json:"signature_policy_path,omitempty"`
	BigFilesTempDir       string                            `json:"big_files_temporary_dir,omitempty"`
	AuthFile              string                            `json:"auth_file,omitempty"`
	CertDir               string                            `json:"cert_dir,omitempty"`
	TLSVerify             *bool                             `json:"tls_verify,omitempty"`
	RegistryOptions       oci.Options                       `json:"registry_options,omitempty"`
	ResolverEnabled       bool                              `json:"resolver_enabled,omitempty"`
	ComponentStore        string                            `json:"component_store,omitempty"`
	Pull                  bool                              `json:"pull,omitempty"`
	PullPolicy            string                            `json:"pull_policy,omitempty"`
	ResultPath            string                            `json:"result_path"`
	Control               bool                              `json:"control,omitempty"`
}

type stageWorkerBase struct {
	Key    ResolvedBaseKey     `json:"key"`
	Source ResolvedImageSource `json:"source"`
}

type stageWorkerResponse struct {
	RetainsCaller         bool                            `json:"retains_caller,omitempty"`
	BaseImageID           string                          `json:"base_image_id,omitempty"`
	ImageID               string                          `json:"image_id,omitempty"`
	ManifestDigest        digest.Digest                   `json:"manifest_digest,omitempty"`
	CommandSet            bool                            `json:"command_set,omitempty"`
	Config                json.RawMessage                 `json:"config,omitempty"`
	Result                Result                          `json:"result"`
	Root                  *PackageRootMetadata            `json:"root,omitempty"`
	ComponentRelays       []componentCacheRelay           `json:"component_relays,omitempty"`
	InstructionRelays     []portableInstructionCacheRelay `json:"instruction_relays,omitempty"`
	ComponentCacheStats   CacheStats                      `json:"component_cache_stats"`
	InstructionCacheStats CacheStats                      `json:"instruction_cache_stats"`
	Error                 string                          `json:"error,omitempty"`
	Failure               *workerError                    `json:"failure,omitempty"`
}

// executeGraphStageIsolated runs one parallel stage in a separately supervised
// process group. A failed sibling can therefore stop an in-flight Buildah RUN
// without terminating the top-level plan worker.
func (executor *graphExecutor) executeGraphStageIsolated(ctx context.Context, plan *planner.Plan, prepared preparedGraphStage, aliases map[string]string, images map[string]stageState, resolvedBases map[ResolvedBaseKey]ResolvedImageSource, imageOutputID string, observed map[string]bool) (_ executedGraphStage, retErr error) {
	if ctx == nil {
		return executedGraphStage{}, errors.New("stage worker context is nil")
	}
	if plan == nil {
		return executedGraphStage{}, errors.New("stage worker plan is nil")
	}
	if prepared.bound != nil {
		return executedGraphStage{}, errors.New("bound component stage cannot run in an isolated worker")
	}
	if prepared.logical == nil {
		return executedGraphStage{}, errors.New("isolated stage has no image configuration")
	}
	if err := ctx.Err(); err != nil {
		return executedGraphStage{}, err
	}

	logical, err := prepared.logical.MarshalJSON()
	if err != nil {
		return executedGraphStage{}, fmt.Errorf("marshal stage %s image config: %w", prepared.stage.ID, err)
	}
	if prepared.baseLogical == nil {
		return executedGraphStage{}, errors.New("isolated stage has no base image configuration")
	}
	baseLogical, err := prepared.baseLogical.MarshalJSON()
	if err != nil {
		return executedGraphStage{}, fmt.Errorf("marshal stage %s base image config: %w", prepared.stage.ID, err)
	}
	encodedImages := make(map[string]string, len(images))
	for id, state := range images {
		if state.storageImageID == "" {
			return executedGraphStage{}, fmt.Errorf("stage input %s has no storage image", id)
		}
		encodedImages[id] = state.storageImageID
	}

	parent := executor.system.BigFilesTemporaryDir
	if parent == "" {
		parent = os.TempDir()
	}
	jobDir, err := os.MkdirTemp(parent, ".coopr-stage-worker-*")
	if err != nil {
		return executedGraphStage{}, fmt.Errorf("create stage worker directory: %w", err)
	}
	removeJobDir := true
	defer func() {
		if removeJobDir {
			if err := os.RemoveAll(jobDir); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("remove stage worker directory: %w", err))
			}
		}
	}()

	jobID := executor.options.JobID
	ownedJobID := false
	if jobID == "" {
		jobID, err = newWorkerJobID()
		if err != nil {
			return executedGraphStage{}, err
		}
		ownedJobID = true
	}

	request := stageWorkerRequest{
		ExecutableOrigin:  executor.options.executableOrigin,
		ProgressPrefix:    prepared.progressPrefix,
		ProgressReference: executor.options.ProgressReference,
		Mode:              plan.Mode, Stage: prepared.stage, Base: prepared.base, BaseName: prepared.baseName, BaseRoot: prepared.baseRoot, RetainsCaller: prepared.retainsCaller, PreserveBaseImageAnns: prepared.preserveBaseImageAnnotations, BaseManifest: prepared.baseManifest, Logical: logical, BaseLogical: baseLogical, CommandSet: prepared.logical.CommandSet(), BaseCommandSet: prepared.baseLogical.CommandSet(),
		Operations: prepared.operations, Aliases: aliases, Images: encodedImages,
		CacheScope: prepared.cacheScope, Jobs: 1, LogRusage: executor.options.LogRusage, RusageLogFile: executor.options.RusageLogFile,
		Output: stageWorkerOutput(prepared.stage.ID, imageOutputID), CaptureRoot: graphStageNeedsRootMetadata(plan, prepared.stage.ID, observed),
		Store: executor.options.Store, ContextDir: executor.options.ContextDir, SourcePolicyFile: executor.options.SourcePolicyFile, SourcePolicy: executor.sourcePolicy, IgnoreFile: executor.options.IgnoreFile,
		ContextArtifacts:   append(append([]string(nil), executor.options.ContextArtifacts...), jobDir),
		ContextPrepared:    executor.options.ContextPrepared,
		TransientRunMounts: executor.options.TransientRunMounts,
		BuildContexts:      executor.options.BuildContexts,
		NoCache:            executor.options.NoCache,
		CacheLocalDir:      executor.options.CacheLocalDir,
		CacheRepository:    executor.options.CacheRepository,
		CacheFrom:          slices.Clone(executor.options.CacheFrom),
		CacheTo:            slices.Clone(executor.options.CacheTo),
		Network:            executor.options.Network,
		AddHosts:           append([]string(nil), executor.options.AddHosts...),
		ProxyArgs:          cloneStringMap(executor.options.ProxyArgs),
		RunControls:        executor.options.RunControls, Lifecycle: executor.options.Lifecycle,
		ImageControls:    executor.options.ImageControls,
		Timestamp:        executor.options.Timestamp,
		CacheTTL:         executor.options.CacheTTL,
		SourceDateEpoch:  executor.options.SourceDateEpoch,
		RewriteTimestamp: executor.options.RewriteTimestamp,
		Allow:            append([]string(nil), executor.options.Allow...),
		Secrets:          append([]string(nil), executor.options.Secrets...),
		SSH:              append([]string(nil), executor.options.SSH...),
		Isolation:        executor.options.Isolation, Runtime: executor.options.Runtime, ImageOutput: executor.options.Output,
		JobID: jobID, SignaturePolicy: executor.system.SignaturePolicyPath,
		BigFilesTempDir: executor.system.BigFilesTemporaryDir,
		AuthFile:        executor.system.AuthFilePath, CertDir: executor.system.DockerCertPath,
		ResultPath: filepath.Join(jobDir, "result.json"),
	}
	if executor.system.DockerInsecureSkipTLSVerify != types.OptionalBoolUndefined {
		request.TLSVerify = new(executor.system.DockerInsecureSkipTLSVerify == types.OptionalBoolFalse)
	}
	request.ComponentPins = cloneStringMap(prepared.componentPins)
	request.ComponentSources = cloneStringMap(prepared.componentSources)
	request.ComponentSelections = cloneComponentSelections(prepared.componentSelections)
	request.Delivery = prepared.delivery
	request.ActiveComponents = maps.Clone(prepared.activeComponents)
	request.ActiveLocalComponents = maps.Clone(prepared.activeLocalComponents)
	if executor.componentCache != nil {
		request.ComponentRelay = executor.componentCache.stagingDir
	}
	if executor.instructionPortableCache != nil {
		request.InstructionRelay = executor.instructionPortableCache.stagingDir
	}
	if executor.options.Resolver != nil {
		request.RegistryOptions = executor.options.Resolver.RegistryOptions()
		request.ResolverEnabled = true
		request.ComponentStore = executor.options.Resolver.ComponentStoreDir()
		request.Pull = executor.options.Resolver.PullImages()
		request.PullPolicy = string(executor.options.Resolver.PullPolicy())
	}
	for key, source := range resolvedBases {
		if key.Platform == prepared.stage.Platform {
			request.ResolvedBases = append(request.ResolvedBases, stageWorkerBase{Key: key, Source: source})
		}
	}
	sort.Slice(request.ResolvedBases, func(i, j int) bool {
		return request.ResolvedBases[i].Key.Reference < request.ResolvedBases[j].Key.Reference
	})
	var control *stageWorkerControl
	returns := prepared.componentReturnHandler
	if returns == nil {
		returns = executor.componentReturnHandler
	}
	selections := prepared.componentSelectionHandler
	if selections == nil {
		selections = executor.componentSelectionHandler
	}
	bases := prepared.baseSelectionHandler
	if bases == nil {
		bases = executor.baseSelectionHandler
	}
	if returns != nil || selections != nil || bases != nil {
		control, err = newStageWorkerControl()
		if err != nil {
			return executedGraphStage{}, fmt.Errorf("create stage control pipes: %w", err)
		}
		defer control.close()
		request.Control = true
	}
	requestPath := filepath.Join(jobDir, "request.json")
	if err := writeWorkerJSON(requestPath, request); err != nil {
		return executedGraphStage{}, err
	}
	command := reexec.Command(stageWorkerName, requestPath)
	command.Env = withoutSigningPassword(gitWorkerEnvironment(os.Environ()))
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	var processErr error
	if control == nil {
		processErr = runWorkerProcess(ctx, command, stageWorkerGrace)
	} else {
		processErr = runControlledStageWorker(ctx, command, prepared.stage.ID, control, returns, selections, bases)
	}
	if !stageWorkerExitConfirmed(command) {
		removeJobDir = false
		return executedGraphStage{}, errors.Join(processErr, fmt.Errorf("stage worker files retained at %s because process exit was not confirmed", jobDir))
	}
	response, responseErr := readStageWorkerResponse(request.ResultPath)
	return executor.completeStageWorkerResponse(ctx, prepared.stage.ID, response, responseErr, processErr, ownedJobID, jobID)
}

func (executor *graphExecutor) completeStageWorkerResponse(ctx context.Context, stageID string, response stageWorkerResponse, responseErr, processErr error, ownedJobID bool, jobID string) (executedGraphStage, error) {
	finished, resultErr := stageWorkerResult(response, stageID, executor.store)
	responseErr = errors.Join(responseErr, resultErr)
	var cleanupErr error
	failed := processErr != nil || responseErr != nil || response.Error != ""
	if ownedJobID && (responseErr != nil || executor.options.Lifecycle.removeBuilder(failed, ctx.Err() != nil)) {
		if err := cleanupNamedBuilders(executor.options.Store, jobID); err != nil {
			cleanupErr = fmt.Errorf("clean isolated stage worker: %w", err)
		}
	}
	if response.Error != "" {
		failure := error(response.Failure)
		if response.Failure == nil {
			failure = errors.New(response.Error)
		}
		responseErr = errors.Join(responseErr, failure)
		// The response describes the RUN failure; the worker exit is only its transport.
		processErr = nil
	}
	if err := errors.Join(processErr, ctx.Err(), responseErr, cleanupErr); err != nil {
		return finished, err
	}
	executor.stageRelayMu.Lock()
	defer executor.stageRelayMu.Unlock()
	if executor.componentCache != nil {
		if err := executor.componentCache.acceptRelayedCandidates(response.ComponentRelays); err != nil {
			return finished, fmt.Errorf("accept stage %s component cache: %w", stageID, err)
		}
		executor.componentCache.stats = addCacheStats(executor.componentCache.stats, response.ComponentCacheStats)
	}
	if executor.instructionPortableCache != nil {
		if err := executor.instructionPortableCache.acceptRelayedCandidates(response.InstructionRelays); err != nil {
			return finished, fmt.Errorf("accept stage %s instruction cache: %w", stageID, err)
		}
		executor.instructionPortableCache.stats = addCacheStats(executor.instructionPortableCache.stats, response.InstructionCacheStats)
	}
	return finished, nil
}

// stageWorkerResult retains a validated committed output even when the worker
// subsequently reports cancellation or fails while closing its resources.
func stageWorkerResult(response stageWorkerResponse, stageID string, store storage.Store) (executedGraphStage, error) {
	if response.ImageID == "" {
		if response.Error != "" {
			return executedGraphStage{}, nil
		}
		return executedGraphStage{}, fmt.Errorf("stage %s result lacks an image ID", stageID)
	}
	if err := digest.NewDigestFromEncoded(digest.SHA256, response.ImageID).Validate(); err != nil {
		return executedGraphStage{}, fmt.Errorf("stage %s result image ID: %w", stageID, err)
	}
	image, err := store.Image(response.ImageID)
	if err != nil {
		return executedGraphStage{}, fmt.Errorf("stage %s result image unavailable: %w", stageID, err)
	}
	if image.ID != response.ImageID {
		return executedGraphStage{}, fmt.Errorf("stage %s result image ID is not canonical", stageID)
	}
	finalConfig, err := imageconfig.Restore(response.Config, response.CommandSet)
	if err != nil {
		return executedGraphStage{}, fmt.Errorf("decode stage %s result config: %w", stageID, err)
	}
	return executedGraphStage{
		state:  stageState{storageImageID: response.ImageID, manifestDigest: response.ManifestDigest, config: finalConfig, root: response.Root, baseImageID: response.BaseImageID, retainsCaller: response.RetainsCaller},
		result: response.Result,
		root:   response.Root,
	}, nil
}

// A signaled process is still reaped by Cmd.Wait; ProcessState.Exited reports
// false for that case on Unix, while a nil ProcessState means Wait did not
// finish and the request files must be retained.
func stageWorkerExitConfirmed(command *exec.Cmd) bool {
	return command.Process == nil || command.ProcessState != nil
}

func stageWorkerOutput(stageID, imageOutputID string) bool { return stageID == imageOutputID }

func runStageWorker() {
	// The graph executor has already opened the execution store in its final
	// rootless namespace. A stage worker must inherit that namespace: trying to
	// create a nested user namespace breaks network helpers such as pasta.
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "invalid Coopr stage worker arguments")
		os.Exit(2)
	}
	if err := executeStageWorker(os.Args[1]); err != nil {
		if !workerFailureReported(err) {
			_, _ = fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	os.Exit(0)
}

func executeStageWorker(requestPath string) error {
	var request stageWorkerRequest
	if err := readWorkerJSON(requestPath, &request); err != nil {
		return err
	}
	if request.ResultPath == "" {
		return errors.New("stage worker request lacks result path")
	}
	if request.JobID != "" && !validWorkerJobID(request.JobID) {
		return errors.New("invalid stage worker job ID")
	}
	workerCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	var control *stageWorkerControlClient
	if request.Control {
		read := os.NewFile(stageWorkerControlReadFD, "stage-control-read")
		write := os.NewFile(stageWorkerControlWriteFD, "stage-control-write")
		var err error
		control, err = stageWorkerControlFromFiles(read, write)
		if err != nil {
			_ = read.Close()
			_ = write.Close()
			return fmt.Errorf("initialize stage control pipes: %w", err)
		}
		defer control.close()
		if _, err := read.Stat(); err != nil {
			return fmt.Errorf("open stage control read pipe: %w", err)
		}
		if _, err := write.Stat(); err != nil {
			return fmt.Errorf("open stage control write pipe: %w", err)
		}
	}
	response, stageErr := executeStageWorkerRequestControlled(workerCtx, request, control)
	if stageErr != nil {
		response.Error = stageErr.Error()
		response.Failure = workerFailure(stageErr)
	}
	if err := writeWorkerJSON(request.ResultPath, response); err != nil {
		return err
	}
	return reportedWorkerFailure(stageErr)
}

func executeStageWorkerRequestControlled(ctx context.Context, request stageWorkerRequest, control *stageWorkerControlClient) (response stageWorkerResponse, retErr error) {
	if request.Jobs < 1 {
		request.Jobs = 1
	}
	logical, err := imageconfig.Restore(request.Logical, request.CommandSet)
	if err != nil {
		return response, fmt.Errorf("decode stage %s image config: %w", request.Stage.ID, err)
	}
	baseLogical, err := imageconfig.Restore(request.BaseLogical, request.BaseCommandSet)
	if err != nil {
		return response, fmt.Errorf("decode stage %s base image config: %w", request.Stage.ID, err)
	}
	if request.Aliases == nil {
		request.Aliases = make(map[string]string)
	}
	images := make(map[string]stageState, len(request.Images))
	for id, imageID := range request.Images {
		images[id] = stageState{storageImageID: imageID}
	}
	resolvedBases := make(map[ResolvedBaseKey]ResolvedImageSource, len(request.ResolvedBases))
	for _, selected := range request.ResolvedBases {
		if selected.Key.Reference == "" || selected.Key.Platform != request.Stage.Platform || selected.Source.ImageID == "" {
			return response, fmt.Errorf("invalid selected image input for stage %s", request.Stage.ID)
		}
		if _, duplicate := resolvedBases[selected.Key]; duplicate {
			return response, fmt.Errorf("duplicate selected image input %q for stage %s", selected.Key.Reference, request.Stage.ID)
		}
		resolvedBases[selected.Key] = selected.Source
	}
	var resolver *oci.Resolver
	if request.ResolverEnabled {
		if err := ConfigureRuntimeConfig(request.RunControls); err != nil {
			return response, err
		}
		registry := request.RegistryOptions
		registry.ProgressWriter = os.Stderr
		registry.Pull, registry.PullPolicy = request.Pull, request.PullPolicy
		registry.AuthFile, registry.CertDir, registry.TLSVerify = request.AuthFile, request.CertDir, request.TLSVerify
		registry.SignaturePolicyPath = request.SignaturePolicy
		registry.ComponentStoreDir = request.ComponentStore
		resolver, err = oci.NewResolver(registry)
		if err != nil {
			return response, fmt.Errorf("create stage resolver: %w", err)
		}
	}
	options := PlanOptions{
		executableOrigin:  request.ExecutableOrigin,
		ProgressReference: request.ProgressReference,
		ProgressPrefix:    request.ProgressPrefix,
		Store:             request.Store, ContextDir: request.ContextDir, SourcePolicyFile: request.SourcePolicyFile, SourcePolicy: request.SourcePolicy, IgnoreFile: request.IgnoreFile, ContextArtifacts: request.ContextArtifacts,
		Isolation: request.Isolation, Runtime: request.Runtime, Output: request.ImageOutput, JobID: request.JobID,
		ContextPrepared:    request.ContextPrepared,
		TransientRunMounts: request.TransientRunMounts,
		BuildContexts:      request.BuildContexts,
		NoCache:            request.NoCache,
		CacheLocalDir:      request.CacheLocalDir,
		CacheRepository:    request.CacheRepository,
		CacheFrom:          request.CacheFrom,
		CacheTo:            request.CacheTo,
		Network:            request.Network,
		AddHosts:           request.AddHosts,
		ProxyArgs:          request.ProxyArgs,
		RunControls:        request.RunControls, Lifecycle: request.Lifecycle,
		ImageControls:           request.ImageControls,
		Timestamp:               request.Timestamp,
		CacheTTL:                request.CacheTTL,
		SourceDateEpochOverride: request.SourceDateEpoch,
		RewriteTimestamp:        request.RewriteTimestamp,
		Allow:                   request.Allow,
		Secrets:                 request.Secrets,
		SSH:                     request.SSH,
		ResolvedBases:           resolvedBases,
		Resolver:                resolver,
		Jobs:                    request.Jobs,
		LogRusage:               request.LogRusage,
		RusageLogFile:           request.RusageLogFile,
		SystemContext: &types.SystemContext{
			SignaturePolicyPath: request.SignaturePolicy, BigFilesTemporaryDir: request.BigFilesTempDir,
			AuthFilePath: request.AuthFile, DockerCertPath: request.CertDir,
		},
	}
	oci.ApplyTLSVerify(options.SystemContext, request.TLSVerify)
	executor, err := newGraphExecutor(ctx, options)
	if err != nil {
		return response, err
	}
	defer func() {
		retErr = errors.Join(retErr, executor.Close())
	}()
	executor.componentPins = cloneStringMap(request.ComponentPins)
	executor.componentSources = cloneStringMap(request.ComponentSources)
	executor.componentSelections = cloneComponentSelections(request.ComponentSelections)
	executor.activeComponents = maps.Clone(request.ActiveComponents)
	executor.activeLocalComponents = maps.Clone(request.ActiveLocalComponents)
	if executor.activeComponents == nil {
		executor.activeComponents = make(map[digest.Digest]bool)
	}
	if control != nil {
		executor.componentReturnHandler = control.componentReturn
		executor.componentSelectionHandler = control.selectComponent
		executor.baseSelectionHandler = control.selectBase
	}
	var cacheMountID CacheMountIDResolver
	if request.CacheScope != nil {
		cacheMountID, err = scopedCacheMountIDResolver(*request.CacheScope)
		if err != nil {
			return response, fmt.Errorf("stage %s cache mount scope: %w", request.Stage.ID, err)
		}
	}
	finished, err := executor.executeGraphStage(ctx, &planner.Plan{Mode: request.Mode}, preparedGraphStage{
		stage: request.Stage, base: request.Base, baseName: request.BaseName, baseRoot: request.BaseRoot, retainsCaller: request.RetainsCaller, preserveBaseImageAnnotations: request.PreserveBaseImageAnns, baseManifest: request.BaseManifest, logical: logical, baseLogical: baseLogical, operations: request.Operations,
		cacheMountID: cacheMountID, cacheScope: request.CacheScope, progressPrefix: request.ProgressPrefix, delivery: request.Delivery,
	}, request.Aliases, images, outputStageID(request), observedStage(request), true)
	if err != nil {
		return response, err
	}
	config, err := finished.state.config.MarshalJSON()
	if err != nil {
		return response, fmt.Errorf("marshal stage %s result config: %w", request.Stage.ID, err)
	}
	response = stageWorkerResponse{
		BaseImageID: finished.state.baseImageID, RetainsCaller: finished.state.retainsCaller, ImageID: finished.state.storageImageID, ManifestDigest: finished.state.manifestDigest, Config: config, CommandSet: finished.state.config.CommandSet(), Result: finished.result, Root: finished.root,
		ComponentCacheStats: cacheStats(executor.componentCache), InstructionCacheStats: portableInstructionCacheStats(executor.instructionPortableCache),
	}
	if executor.componentCache != nil {
		response.ComponentRelays, err = executor.componentCache.relayCandidates(request.ComponentRelay)
		if err != nil {
			return response, fmt.Errorf("relay stage component cache: %w", err)
		}
	}
	if executor.instructionPortableCache != nil {
		response.InstructionRelays, err = executor.instructionPortableCache.relayCandidates(request.InstructionRelay)
		if err != nil {
			return response, fmt.Errorf("relay stage instruction cache: %w", err)
		}
	}
	return response, nil
}

func outputStageID(request stageWorkerRequest) string {
	if request.Output {
		return request.Stage.ID
	}
	return ""
}

func observedStage(request stageWorkerRequest) map[string]bool {
	if !request.CaptureRoot {
		return nil
	}
	return map[string]bool{request.Stage.ID: true}
}

func readStageWorkerResponse(path string) (stageWorkerResponse, error) {
	var response stageWorkerResponse
	if err := readWorkerJSON(path, &response); err != nil {
		return stageWorkerResponse{}, err
	}
	return response, nil
}
