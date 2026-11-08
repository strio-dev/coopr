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

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/planner"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"
)

const (
	stageWorkerName  = "coopr-buildah-stage-worker-v1"
	stageWorkerGrace = time.Second + workerChildDrainTimeout + time.Second
)

type stageWorkerRequest struct {
	workerExecutionOptions
	RetainsCaller         bool                              `json:"retains_caller,omitempty"`
	BaseRoot              *PackageRootMetadata              `json:"base_root,omitempty"`
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
	Output                bool                              `json:"output,omitempty"`
	CaptureRoot           bool                              `json:"capture_root,omitempty"`
	ContextPrepared       bool                              `json:"context_prepared,omitempty"`
	ComponentRelay        string                            `json:"component_relay,omitempty"`
	GraphParent           *instructionVertexRelay           `json:"graph_parent,omitempty"`
	GraphReadBindings     []digest.Digest                   `json:"graph_read_bindings,omitempty"`
	ImageOutput           Output                            `json:"image_output"`
	JobID                 string                            `json:"job_id,omitempty"`
	SignaturePolicy       string                            `json:"signature_policy_path,omitempty"`
	BigFilesTempDir       string                            `json:"big_files_temporary_dir,omitempty"`
	RegistryOptions       oci.Options                       `json:"registry_options,omitempty"`
	ResolverEnabled       bool                              `json:"resolver_enabled,omitempty"`
	ComponentStore        string                            `json:"component_store,omitempty"`
	ResultPath            string                            `json:"result_path"`
	Control               bool                              `json:"control,omitempty"`
}

type stageWorkerBase struct {
	Key    ResolvedBaseKey     `json:"key"`
	Source ResolvedImageSource `json:"source"`
}

type stageWorkerResponse struct {
	CleanupComplete       bool                        `json:"cleanup_complete"`
	ProgressStream        *workerProgressSummary      `json:"progress_stream,omitempty"`
	ProgressDropped       uint64                      `json:"progress_dropped,omitempty"`
	RetainsCaller         bool                        `json:"retains_caller,omitempty"`
	BaseImageID           string                      `json:"base_image_id,omitempty"`
	ImageID               string                      `json:"image_id,omitempty"`
	ManifestDigest        digest.Digest               `json:"manifest_digest,omitempty"`
	CommandSet            bool                        `json:"command_set,omitempty"`
	Config                json.RawMessage             `json:"config,omitempty"`
	Result                Result                      `json:"result"`
	Root                  *PackageRootMetadata        `json:"root,omitempty"`
	ComponentRelays       []componentCacheRelay       `json:"component_relays,omitempty"`
	GraphEndpoint         *instructionVertexRelay     `json:"graph_endpoint,omitempty"`
	GraphRoots            []instructionGraphRootRelay `json:"graph_roots,omitempty"`
	GraphSources          *instructionGraphSourceFile `json:"graph_sources,omitempty"`
	graphSources          []instructionGraphSourceRelay
	ComponentCacheStats   CacheStats   `json:"component_cache_stats"`
	InstructionCacheStats CacheStats   `json:"instruction_cache_stats"`
	Failure               *workerError `json:"failure,omitempty"`
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
			retErr = RemoveWorkerDirectory(jobDir, retErr)
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
		workerExecutionOptions: workerExecutionOptions{
			StreamProgress:    executor.options.Progress != nil,
			ExecutableOrigin:  executor.options.executableOrigin,
			ProgressPrefix:    prepared.progressPrefix,
			ProgressReference: executor.options.ProgressReference,
			Jobs:              1, LogRusage: executor.options.LogRusage, RusageLogFile: executor.options.RusageLogFile,
			Store: executor.options.Store, ContextDir: executor.options.ContextDir, SourcePolicyFile: executor.options.SourcePolicyFile, SourcePolicy: executor.sourcePolicy, IgnoreFile: executor.options.IgnoreFile,
			ContextArtifacts:   append(append([]string(nil), executor.options.ContextArtifacts...), jobDir),
			TransientRunMounts: executor.options.TransientRunMounts,
			BuildContexts:      executor.options.BuildContexts,
			NoCache:            executor.options.NoCache,
			CacheLocalDir:      executor.options.CacheLocalDir,
			CacheMountDir:      executor.options.CacheMountDir,
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
			Isolation:        executor.options.Isolation, Runtime: executor.options.Runtime,
			AuthFile: executor.system.AuthFilePath, CertDir: executor.system.DockerCertPath,
		},
		Mode: plan.Mode, Stage: prepared.stage, Base: prepared.base, BaseName: prepared.baseName, BaseRoot: prepared.baseRoot, RetainsCaller: prepared.retainsCaller, PreserveBaseImageAnns: prepared.preserveBaseImageAnnotations, BaseManifest: prepared.baseManifest, Logical: logical, BaseLogical: baseLogical, CommandSet: prepared.logical.CommandSet(), BaseCommandSet: prepared.baseLogical.CommandSet(),
		Operations: prepared.operations, Aliases: aliases, Images: encodedImages,
		CacheScope: prepared.cacheScope,
		Output:     stageWorkerOutput(prepared.stage.ID, imageOutputID), CaptureRoot: graphStageNeedsRootMetadata(plan, prepared.stage.ID, observed),
		ContextPrepared: executor.options.ContextPrepared,
		RegistryOptions: executor.options.registrySettings(),
		ImageOutput:     executor.options.Output,
		JobID:           jobID, SignaturePolicy: executor.system.SignaturePolicyPath,
		BigFilesTempDir: executor.system.BigFilesTemporaryDir,
		ResultPath:      filepath.Join(jobDir, "result.json"),
	}
	if executor.system.DockerInsecureSkipTLSVerify != types.OptionalBoolUndefined {
		request.TLSVerify = new(executor.system.DockerInsecureSkipTLSVerify == types.OptionalBoolFalse)
	}
	request.ComponentPins = cloneStringMap(prepared.componentPins)
	request.GraphParent, err = encodeInstructionVertexRelay(prepared.graphEndpoint)
	if err != nil {
		return executedGraphStage{}, fmt.Errorf("encode stage %s graph parent: %w", prepared.stage.ID, err)
	}
	request.ComponentSources = cloneStringMap(prepared.componentSources)
	request.ComponentSelections = cloneComponentSelections(prepared.componentSelections)
	request.Delivery = prepared.delivery
	request.ActiveComponents = maps.Clone(prepared.activeComponents)
	request.ActiveLocalComponents = maps.Clone(prepared.activeLocalComponents)
	if executor.componentCache != nil {
		request.ComponentRelay = executor.componentCache.stagingDir
	}
	if executor.instructionPortableCache != nil {
		request.GraphReadBindings = slices.Clone(executor.instructionPortableCache.readBindingIDs)
	}
	if executor.options.Resolver != nil {
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
	progressReceiver, err := newWorkerProgressReceiver(request.ResultPath, executor.options.Progress)
	if err != nil {
		return executedGraphStage{}, err
	}
	defer func() { _ = progressReceiver.close() }()
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
	if !stageWorkerExitConfirmed(command, processErr) {
		removeJobDir = false
		return executedGraphStage{}, errors.Join(ErrWorkerWaitIncomplete, processErr, fmt.Errorf("stage worker files retained at %s because process exit was not confirmed", jobDir))
	}
	response, responseErr := readStageWorkerResponse(request.ResultPath)
	progressErr := progressReceiver.finish(response.ProgressStream)
	executor.options.progressBuffer.addDropped(response.ProgressDropped)
	if proofErr := requireWorkerCleanup(response.CleanupComplete, responseErr); proofErr != nil {
		removeJobDir = false
		var failure error
		if response.Failure != nil {
			failure = response.Failure
			if responseErr == nil {
				processErr = WorkerSupervisionError(processErr)
			}
		}
		return executedGraphStage{}, errors.Join(proofErr, failure, processErr, ctx.Err(), progressErr)
	}
	if errors.Is(response.Failure, ErrWorkerWaitIncomplete) {
		removeJobDir = false
		return executedGraphStage{}, errors.Join(response.Failure, WorkerSupervisionError(processErr), progressErr)
	}
	response.graphSources, responseErr = readInstructionGraphSources(request.ResultPath, response.GraphSources)
	if responseErr != nil {
		responseErr = fmt.Errorf("read stage worker graph sources: %w", responseErr)
	}
	return executor.completeStageWorkerResponse(ctx, prepared.stage.ID, response, errors.Join(responseErr, progressErr), processErr, ownedJobID, jobID)
}

func (executor *graphExecutor) completeStageWorkerResponse(ctx context.Context, stageID string, response stageWorkerResponse, responseErr, processErr error, ownedJobID bool, jobID string) (executedGraphStage, error) {
	finished, resultErr := stageWorkerResult(response, stageID, executor.store)
	responseErr = errors.Join(responseErr, resultErr)
	var cleanupErr error
	failed := processErr != nil || responseErr != nil || response.Failure != nil
	if ownedJobID && (responseErr != nil || executor.options.Lifecycle.removeBuilder(failed, ctx.Err() != nil)) {
		if err := cleanupNamedBuilders(executor.options.Store, jobID); err != nil {
			cleanupErr = fmt.Errorf("clean isolated stage worker: %w", err)
		}
	}
	if response.Failure != nil {
		responseErr = errors.Join(responseErr, response.Failure)
		processErr = WorkerSupervisionError(processErr)
	}
	if err := errors.Join(responseErr, processErr, ctx.Err(), cleanupErr); err != nil {
		return finished, err
	}
	executor.stageRelayMu.Lock()
	defer executor.stageRelayMu.Unlock()
	if err := executor.instructionSolver.acceptInstructionSources(ctx, response.graphSources); err != nil {
		return finished, fmt.Errorf("accept stage %s graph sources: %w", stageID, err)
	}
	endpoint, err := executor.instructionSolver.restoreInstructionVertexRelay(response.GraphEndpoint)
	if err != nil {
		return finished, fmt.Errorf("accept stage %s graph endpoint: %w", stageID, err)
	}
	if err := executor.instructionSolver.acceptInstructionGraphs(response.GraphRoots); err != nil {
		return finished, fmt.Errorf("accept stage %s graph cache: %w", stageID, err)
	}
	finished.state.graphEndpoint = endpoint
	if executor.componentCache != nil {
		if err := executor.componentCache.acceptRelayedCandidates(response.ComponentRelays); err != nil {
			return finished, fmt.Errorf("accept stage %s component cache: %w", stageID, err)
		}
		executor.componentCache.stats = addCacheStats(executor.componentCache.stats, response.ComponentCacheStats)
	}
	if executor.instructionPortableCache != nil {
		executor.instructionPortableCache.stats = addCacheStats(executor.instructionPortableCache.stats, response.InstructionCacheStats)
	}
	return finished, nil
}

// stageWorkerResult retains a validated committed output even when the worker
// subsequently reports cancellation or fails while closing its resources.
func stageWorkerResult(response stageWorkerResponse, stageID string, store storage.Store) (executedGraphStage, error) {
	if response.ImageID == "" {
		if response.Failure != nil {
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
// finish and the request files must be retained. An incomplete-wait result must
// be checked first because its background waiter can still update ProcessState.
func stageWorkerExitConfirmed(command *exec.Cmd, processErr error) bool {
	if errors.Is(processErr, ErrWorkerWaitIncomplete) {
		return false
	}
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
	scope, err := newWorkerChildScope()
	if err == nil {
		err = executeStageWorkerScoped(os.Args[1], scope)
	}
	if err != nil {
		if !workerFailureReported(err) {
			_, _ = fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	os.Exit(0)
}

func executeStageWorkerScoped(requestPath string, scope *workerChildScope) (retErr error) {
	var request stageWorkerRequest
	var pending *stageWorkerResponse
	workerCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer func() {
		defer stopSignals()
		var cleanupErr error
		if scope != nil {
			cleanupErr = scope.finish(retErr)
		}
		retErr = errors.Join(retErr, cleanupErr)
		if pending == nil {
			return
		}
		pending.CleanupComplete = scope != nil && cleanupErr == nil
		if cleanupErr != nil {
			pending.Failure = WorkerFailure(retErr)
		}
		if writeErr := writeWorkerJSON(request.ResultPath, *pending); writeErr != nil {
			retErr = errors.Join(retErr, writeErr)
		} else {
			retErr = reportedWorkerFailure(retErr)
		}
	}()
	if err := readWorkerJSON(requestPath, &request); err != nil {
		return err
	}
	if request.ResultPath == "" {
		return errors.New("stage worker request lacks result path")
	}
	if request.JobID != "" && !validWorkerJobID(request.JobID) {
		return errors.New("invalid stage worker job ID")
	}
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
		response.Failure = WorkerFailure(stageErr)
	}
	pending = &response
	return stageErr
}

func executeStageWorkerRequestControlled(ctx context.Context, request stageWorkerRequest, control *stageWorkerControlClient) (response stageWorkerResponse, retErr error) {
	progressBuffer, progressStream, err := newWorkerProgressCapture(request.StreamProgress, request.ResultPath)
	if err != nil {
		return response, err
	}
	if progressBuffer != nil {
		ctx = withOperationProgress(ctx, progressBuffer.record)
		defer func() {
			response.ProgressDropped, response.ProgressStream = finishWorkerProgress(progressBuffer, progressStream)
			response.Result.ProgressDropped = response.ProgressDropped
		}()
	}
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
		if selected.Key.Reference == "" || selected.Key.Platform != request.Stage.Platform {
			return response, fmt.Errorf("invalid selected image input for stage %s", request.Stage.ID)
		}
		if err := validateSelectedInput(selected.Source, true); err != nil {
			return response, fmt.Errorf("invalid selected input for stage %s: %w", request.Stage.ID, err)
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
	options := request.planOptions()
	options.workerDirectory = filepath.Dir(request.ResultPath)
	options.progressBuffer = progressBuffer
	options.registryOptions = request.RegistryOptions
	options.Output = request.ImageOutput
	options.JobID = request.JobID
	options.ContextPrepared = request.ContextPrepared
	options.ResolvedBases = resolvedBases
	options.Resolver = resolver
	options.SystemContext = &types.SystemContext{
		SignaturePolicyPath: request.SignaturePolicy, BigFilesTemporaryDir: request.BigFilesTempDir,
		AuthFilePath: request.AuthFile, DockerCertPath: request.CertDir,
	}
	if progressBuffer != nil {
		options.Progress = progressBuffer.record
	}
	oci.ApplyTLSVerify(options.SystemContext, request.TLSVerify)
	executor, err := newGraphExecutor(ctx, options)
	if err != nil {
		return response, err
	}
	defer func() {
		retErr = errors.Join(retErr, executor.Close())
	}()
	if executor.instructionSolver != nil && executor.instructionSolver.portable != nil {
		if len(request.GraphReadBindings) > 4096 {
			return response, errors.New("too many parent-approved graph read bindings")
		}
		seen := map[digest.Digest]bool{}
		for _, binding := range request.GraphReadBindings {
			if !validInstructionRelayDigest(binding) || seen[binding] {
				return response, errors.New("invalid parent-approved graph read bindings")
			}
			seen[binding] = true
		}
		executor.instructionSolver.portable.relayBindings = slices.Clone(request.GraphReadBindings)
		executor.instructionSolver.portable.relayBindingsSet = true
	}
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
	graphParent, err := executor.instructionSolver.restoreInstructionVertexRelay(request.GraphParent)
	if err != nil {
		return response, fmt.Errorf("restore stage %s graph parent: %w", request.Stage.ID, err)
	}
	finished, err := executor.executeGraphStage(ctx, &planner.Plan{Mode: request.Mode}, preparedGraphStage{
		stage: request.Stage, base: request.Base, baseName: request.BaseName, baseRoot: request.BaseRoot, retainsCaller: request.RetainsCaller, preserveBaseImageAnnotations: request.PreserveBaseImageAnns, baseManifest: request.BaseManifest, logical: logical, baseLogical: baseLogical, operations: request.Operations,
		cacheMountID: cacheMountID, cacheScope: request.CacheScope, progressPrefix: request.ProgressPrefix, delivery: request.Delivery,
		graphEndpoint: graphParent,
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
	response.GraphEndpoint, err = encodeInstructionVertexRelay(finished.state.graphEndpoint)
	if err != nil {
		return response, fmt.Errorf("relay stage %s graph endpoint: %w", request.Stage.ID, err)
	}
	response.GraphRoots, err = executor.instructionSolver.relayInstructionGraphs()
	if err != nil {
		return response, fmt.Errorf("relay stage %s graph cache: %w", request.Stage.ID, err)
	}
	sources, err := executor.instructionSolver.relayInstructionSources(request.GraphReadBindings)
	if err != nil {
		return response, fmt.Errorf("relay stage %s graph sources: %w", request.Stage.ID, err)
	}
	response.GraphSources, err = writeInstructionGraphSources(request.ResultPath, sources)
	if err != nil {
		return response, fmt.Errorf("write stage %s graph sources: %w", request.Stage.ID, err)
	}
	if executor.componentCache != nil {
		response.ComponentRelays, err = executor.componentCache.relayCandidates(request.ComponentRelay)
		if err != nil {
			return response, fmt.Errorf("relay stage component cache: %w", err)
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
	response.Result.ProgressDropped = response.ProgressDropped
	return response, nil
}
