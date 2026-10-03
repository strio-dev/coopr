package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"syscall"
	"time"

	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage/pkg/reexec"
)

const (
	stageWorkerName  = "coopr-buildah-stage-worker-v1"
	stageWorkerGrace = time.Second
)

type stageWorkerRequest struct {
	ProgressReference string              `json:"progress_reference,omitempty"`
	ProgressPrefix    string              `json:"progress_prefix,omitempty"`
	Lifecycle         LifecycleControls   `json:"lifecycle,omitempty"`
	Mode              planner.Mode        `json:"mode"`
	Stage             planner.Stage       `json:"stage"`
	Base              string              `json:"base"`
	BaseManifest      digest.Digest       `json:"base_manifest,omitempty"`
	Logical           json.RawMessage     `json:"logical"`
	Operations        []planner.Operation `json:"operations,omitempty"`
	Aliases           map[string]string   `json:"aliases,omitempty"`
	Images            map[string]string   `json:"images,omitempty"`
	ResolvedBases     []stageWorkerBase   `json:"resolved_bases,omitempty"`
	ComponentPins     map[string]string   `json:"component_pins,omitempty"`
	CacheScope        *CacheMountScope    `json:"cache_scope,omitempty"`
	Jobs              int                 `json:"jobs,omitempty"`
	LogRusage         bool                `json:"log_rusage,omitempty"`
	RusageLogFile     string              `json:"rusage_log_file,omitempty"`
	Output            bool                `json:"output,omitempty"`
	CaptureRoot       bool                `json:"capture_root,omitempty"`
	Store             StoreOptions        `json:"store"`
	ContextDir        string              `json:"context_dir,omitempty"`
	IgnoreFile        string              `json:"ignore_file,omitempty"`
	ContextArtifacts  []string            `json:"context_artifacts,omitempty"`
	NoCache           bool                `json:"no_cache,omitempty"`
	CacheLocalDir     string              `json:"cache_local_dir,omitempty"`
	CacheRepository   string              `json:"cache_repository,omitempty"`
	CacheFrom         []CacheSpec         `json:"cache_from,omitempty"`
	CacheTo           []CacheSpec         `json:"cache_to,omitempty"`
	ComponentRelay    string              `json:"component_relay,omitempty"`
	InstructionRelay  string              `json:"instruction_relay,omitempty"`
	Network           string              `json:"network,omitempty"`
	AddHosts          []string            `json:"add_hosts,omitempty"`
	ProxyArgs         map[string]string   `json:"proxy_args,omitempty"`
	RunControls       RunControls         `json:"run_controls,omitempty"`
	ImageControls     ImageControls       `json:"image_controls,omitempty"`
	Timestamp         *int64              `json:"timestamp,omitempty"`
	CacheTTL          *time.Duration      `json:"cache_ttl,omitempty"`
	SourceDateEpoch   *int64              `json:"source_date_epoch,omitempty"`
	RewriteTimestamp  bool                `json:"rewrite_timestamp,omitempty"`
	Allow             []string            `json:"allow,omitempty"`
	Secrets           []string            `json:"secrets,omitempty"`
	SSH               []string            `json:"ssh,omitempty"`
	Isolation         string              `json:"isolation,omitempty"`
	Runtime           string              `json:"runtime,omitempty"`
	ImageOutput       Output              `json:"image_output"`
	JobID             string              `json:"job_id,omitempty"`
	SignaturePolicy   string              `json:"signature_policy_path,omitempty"`
	BigFilesTempDir   string              `json:"big_files_temporary_dir,omitempty"`
	AuthFile          string              `json:"auth_file,omitempty"`
	CertDir           string              `json:"cert_dir,omitempty"`
	SkipTLSVerify     bool                `json:"skip_tls_verify,omitempty"`
	RegistryOptions   oci.Options         `json:"registry_options,omitempty"`
	ResolverEnabled   bool                `json:"resolver_enabled,omitempty"`
	ComponentStore    string              `json:"component_store,omitempty"`
	ImageStore        string              `json:"image_store,omitempty"`
	PlainHTTP         bool                `json:"plain_http,omitempty"`
	PlainHTTPHosts    []string            `json:"plain_http_hosts,omitempty"`
	Pull              bool                `json:"pull,omitempty"`
	PullPolicy        string              `json:"pull_policy,omitempty"`
	ResultPath        string              `json:"result_path"`
}

type stageWorkerBase struct {
	Key    ResolvedBaseKey     `json:"key"`
	Source ResolvedImageSource `json:"source"`
}

type stageWorkerResponse struct {
	ImageID               string                          `json:"image_id,omitempty"`
	ManifestDigest        digest.Digest                   `json:"manifest_digest,omitempty"`
	Config                json.RawMessage                 `json:"config,omitempty"`
	Result                Result                          `json:"result"`
	Root                  *PackageRootMetadata            `json:"root,omitempty"`
	ComponentRelays       []componentCacheRelay           `json:"component_relays,omitempty"`
	InstructionRelays     []portableInstructionCacheRelay `json:"instruction_relays,omitempty"`
	ComponentCacheStats   CacheStats                      `json:"component_cache_stats"`
	InstructionCacheStats CacheStats                      `json:"instruction_cache_stats"`
	Error                 string                          `json:"error,omitempty"`
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
		ProgressPrefix:    prepared.progressPrefix,
		ProgressReference: executor.options.ProgressReference,
		Mode:              plan.Mode, Stage: prepared.stage, Base: prepared.base, BaseManifest: prepared.baseManifest, Logical: logical,
		Operations: prepared.operations, Aliases: aliases, Images: encodedImages,
		CacheScope: prepared.cacheScope, Jobs: 1, LogRusage: executor.options.LogRusage, RusageLogFile: executor.options.RusageLogFile,
		Output: stageWorkerOutput(prepared.stage.ID, imageOutputID), CaptureRoot: observed[prepared.stage.ID],
		Store: executor.options.Store, ContextDir: executor.options.ContextDir, IgnoreFile: executor.options.IgnoreFile,
		ContextArtifacts: append(append([]string(nil), executor.options.ContextArtifacts...), jobDir),
		NoCache:          executor.options.NoCache,
		CacheLocalDir:    executor.options.CacheLocalDir,
		CacheRepository:  executor.options.CacheRepository,
		CacheFrom:        slices.Clone(executor.options.CacheFrom),
		CacheTo:          slices.Clone(executor.options.CacheTo),
		Network:          executor.options.Network,
		AddHosts:         append([]string(nil), executor.options.AddHosts...),
		ProxyArgs:        cloneStringMap(executor.options.ProxyArgs),
		RunControls:      executor.options.RunControls, Lifecycle: executor.options.Lifecycle,
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
		SkipTLSVerify: executor.system.DockerInsecureSkipTLSVerify == types.OptionalBoolTrue,
		ResultPath:    filepath.Join(jobDir, "result.json"),
	}
	request.ComponentPins = cloneStringMap(prepared.componentPins)
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
		request.ImageStore = executor.options.Resolver.ImageStoreDir()
		request.PlainHTTP = executor.options.Resolver.PlainHTTP()
		request.PlainHTTPHosts = executor.options.Resolver.PlainHTTPRegistries()
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
	requestPath := filepath.Join(jobDir, "request.json")
	if err := writeWorkerJSON(requestPath, request); err != nil {
		return executedGraphStage{}, err
	}
	command := reexec.Command(stageWorkerName, requestPath)
	command.Env = withoutSigningPassword(gitWorkerEnvironment(os.Environ()))
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	processErr := runWorkerProcess(ctx, command, stageWorkerGrace)
	if !stageWorkerExitConfirmed(command) {
		removeJobDir = false
		return executedGraphStage{}, errors.Join(processErr, fmt.Errorf("stage worker files retained at %s because process exit was not confirmed", jobDir))
	}
	response, responseErr := readStageWorkerResponse(request.ResultPath)
	var cleanupErr error
	failed := processErr != nil || responseErr != nil || response.Error != ""
	if ownedJobID && (responseErr != nil || executor.options.Lifecycle.removeBuilder(failed, ctx.Err() != nil)) {
		if err := cleanupNamedBuilders(executor.options.Store, jobID); err != nil {
			cleanupErr = fmt.Errorf("clean isolated stage worker: %w", err)
		}
	}
	if responseErr == nil && response.Error != "" {
		if ctx.Err() != nil {
			return executedGraphStage{}, errors.Join(ctx.Err(), errors.New(response.Error), processErr, cleanupErr)
		}
		return executedGraphStage{}, errors.Join(errors.New(response.Error), cleanupErr)
	}
	if processErr != nil {
		if ctx.Err() != nil {
			return executedGraphStage{}, errors.Join(ctx.Err(), processErr, cleanupErr)
		}
		return executedGraphStage{}, errors.Join(processErr, responseErr, cleanupErr)
	}
	if responseErr != nil {
		return executedGraphStage{}, errors.Join(responseErr, cleanupErr)
	}
	if cleanupErr != nil {
		return executedGraphStage{}, cleanupErr
	}
	if err := ctx.Err(); err != nil {
		return executedGraphStage{}, err
	}
	executor.stageRelayMu.Lock()
	defer executor.stageRelayMu.Unlock()
	if executor.componentCache != nil {
		if err := executor.componentCache.acceptRelayedCandidates(response.ComponentRelays); err != nil {
			return executedGraphStage{}, fmt.Errorf("accept stage %s component cache: %w", prepared.stage.ID, err)
		}
		executor.componentCache.stats = addCacheStats(executor.componentCache.stats, response.ComponentCacheStats)
	}
	if executor.instructionPortableCache != nil {
		if err := executor.instructionPortableCache.acceptRelayedCandidates(response.InstructionRelays); err != nil {
			return executedGraphStage{}, fmt.Errorf("accept stage %s instruction cache: %w", prepared.stage.ID, err)
		}
		executor.instructionPortableCache.stats = addCacheStats(executor.instructionPortableCache.stats, response.InstructionCacheStats)
	}
	finalConfig, err := imageconfig.Parse(response.Config)
	if err != nil {
		return executedGraphStage{}, fmt.Errorf("decode stage %s result config: %w", prepared.stage.ID, err)
	}
	return executedGraphStage{
		state:  stageState{storageImageID: response.ImageID, manifestDigest: response.ManifestDigest, config: finalConfig},
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
		_, _ = fmt.Fprintln(os.Stderr, err)
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
	response, stageErr := executeStageWorkerRequest(workerCtx, request)
	if stageErr != nil {
		response.Error = stageErr.Error()
	}
	if err := writeWorkerJSON(request.ResultPath, response); err != nil {
		return err
	}
	return stageErr
}

func executeStageWorkerRequest(ctx context.Context, request stageWorkerRequest) (response stageWorkerResponse, retErr error) {
	if request.Jobs < 1 {
		request.Jobs = 1
	}
	logical, err := imageconfig.Parse(request.Logical)
	if err != nil {
		return response, fmt.Errorf("decode stage %s image config: %w", request.Stage.ID, err)
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
		registry := request.RegistryOptions
		registry.PlainHTTP, registry.PlainHTTPRegistries = request.PlainHTTP, request.PlainHTTPHosts
		registry.Pull, registry.PullPolicy = request.Pull, request.PullPolicy
		registry.AuthFile, registry.CertDir, registry.SkipTLSVerify = request.AuthFile, request.CertDir, request.SkipTLSVerify
		registry.SignaturePolicyPath = request.SignaturePolicy
		registry.ComponentStoreDir, registry.ImageStoreDir = request.ComponentStore, request.ImageStore
		resolver, err = oci.NewResolver(registry)
		if err != nil {
			return response, fmt.Errorf("create stage resolver: %w", err)
		}
	}
	options := PlanOptions{
		ProgressReference: request.ProgressReference,
		ProgressPrefix:    request.ProgressPrefix,
		Store:             request.Store, ContextDir: request.ContextDir, IgnoreFile: request.IgnoreFile, ContextArtifacts: request.ContextArtifacts,
		Isolation: request.Isolation, Runtime: request.Runtime, Output: request.ImageOutput, JobID: request.JobID,
		NoCache:         request.NoCache,
		CacheLocalDir:   request.CacheLocalDir,
		CacheRepository: request.CacheRepository,
		CacheFrom:       request.CacheFrom,
		CacheTo:         request.CacheTo,
		Network:         request.Network,
		AddHosts:        request.AddHosts,
		ProxyArgs:       request.ProxyArgs,
		RunControls:     request.RunControls, Lifecycle: request.Lifecycle,
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
			DockerInsecureSkipTLSVerify: optionalBool(request.SkipTLSVerify),
		},
	}
	executor, err := newGraphExecutor(ctx, options)
	if err != nil {
		return response, err
	}
	defer func() {
		retErr = errors.Join(retErr, executor.Close())
	}()
	executor.componentPins = cloneStringMap(request.ComponentPins)
	var cacheMountID CacheMountIDResolver
	if request.CacheScope != nil {
		cacheMountID, err = scopedCacheMountIDResolver(*request.CacheScope)
		if err != nil {
			return response, fmt.Errorf("stage %s cache mount scope: %w", request.Stage.ID, err)
		}
	}
	finished, err := executor.executeGraphStage(ctx, &planner.Plan{Mode: request.Mode}, preparedGraphStage{
		stage: request.Stage, base: request.Base, baseManifest: request.BaseManifest, logical: logical, operations: request.Operations,
		cacheMountID: cacheMountID, cacheScope: request.CacheScope, progressPrefix: request.ProgressPrefix,
	}, request.Aliases, images, outputStageID(request), observedStage(request), true)
	if err != nil {
		return response, err
	}
	config, err := finished.state.config.MarshalJSON()
	if err != nil {
		return response, fmt.Errorf("marshal stage %s result config: %w", request.Stage.ID, err)
	}
	response = stageWorkerResponse{
		ImageID: finished.state.storageImageID, ManifestDigest: finished.state.manifestDigest, Config: config, Result: finished.result, Root: finished.root,
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
