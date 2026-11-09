package buildah

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/buildcontext"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/planner"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage/pkg/reexec"
	"go.podman.io/storage/pkg/unshare"
	"golang.org/x/sys/unix"
)

const (
	planWorkerName = "coopr-buildah-plan-worker-v1"
	// Allow an isolated stage its grace and hard-wait budget, then drain
	// adopted plan children before the outer supervisor escalates.
	workerGrace    = stageWorkerGrace + workerProcessKillWait + workerChildDrainTimeout + time.Second
	cleanupTimeout = 20 * time.Second
	// Cleanup mode has no nested stage: reserve its own child drain, not the
	// full plan hierarchy. A canceled plan (12+5s) plus cleanup (3+3+5s)
	// fits within the default public client's 30s cancellation wait.
	cleanupWorkerGrace     = workerChildDrainTimeout + time.Second
	cleanupCanceledTimeout = 3 * time.Second
	workerMaxJSON          = 64 << 20
)

func init() {
	// unshare uses a modified argv[0] for the second, user-namespaced reexec.
	for _, name := range []string{planWorkerName, planWorkerName + "-in-a-user-namespace"} {
		reexec.Register(name, runPlanWorker)
	}
	for _, name := range []string{stageWorkerName, stageWorkerName + "-in-a-user-namespace"} {
		reexec.Register(name, runStageWorker)
	}
}

// SupervisedPlanOptions contains the serializable execution settings for one
// same-binary Buildah worker. The resolver is reconstructed in that worker.
type SupervisedPlanOptions struct {
	Progress            OperationProgressFunc
	ProgressPrefix      string
	ProgressReference   string
	Lifecycle           LifecycleControls
	Store               StoreOptions
	ImageID             string
	ManifestDescriptor  v1.Descriptor
	TransientRunMounts  []RunMount
	ContextDir          string
	SourcePolicyFile    string
	IgnoreFile          string
	ContextArtifacts    []string
	Isolation           string
	Runtime             string
	Output              Output
	ComponentStoreDir   string
	CacheLocalDir       string
	CacheMountDir       string
	CacheRepository     string
	CacheFrom           []CacheSpec
	CacheTo             []CacheSpec
	NoCache             bool
	Network             string
	AddHosts            []string
	RunControls         RunControls
	ImageControls       ImageControls
	Timestamp           *int64
	SourceDateEpoch     *int64
	CacheTTL            *time.Duration
	Jobs                int
	LogRusage           bool
	RusageLogFile       string
	RewriteTimestamp    bool
	Allow               []string
	BuildContexts       []buildcontext.Spec
	Secrets             []string
	SSH                 []string
	ProxyArgs           map[string]string
	AuthFile            string
	CertDir             string
	TLSVerify           *bool
	Credentials         string
	Retry               uint
	RetrySet            bool
	RetryDelay          time.Duration
	DecryptionKeys      []string
	PullPolicy          string
	SignaturePolicyPath string
	Stdout              io.Writer
	Stderr              io.Writer
	Stdin               io.Reader
}

type planWorkerRequest struct {
	workerExecutionOptions
	ExecutionRecipe     bool                   `json:"execution_recipe,omitempty"`
	Mode                string                 `json:"mode"`
	ImageID             string                 `json:"image_id,omitempty"`
	ManifestDescriptor  v1.Descriptor          `json:"manifest_descriptor,omitempty"`
	JobID               string                 `json:"job_id"`
	Plan                *planner.Plan          `json:"plan,omitempty"`
	Definition          *definition.Definition `json:"definition,omitempty"`
	PlannerOptions      *planner.Options       `json:"planner_options,omitempty"`
	Output              Output                 `json:"output"`
	ComponentStoreDir   string                 `json:"component_store_dir,omitempty"`
	Credentials         string                 `json:"credentials,omitempty"`
	Retry               uint                   `json:"retry,omitempty"`
	RetrySet            bool                   `json:"retry_set,omitempty"`
	RetryDelay          time.Duration          `json:"retry_delay,omitempty"`
	DecryptionKeys      []string               `json:"decryption_keys,omitempty"`
	SignaturePolicyPath string                 `json:"signature_policy_path,omitempty"`
	ResultPath          string                 `json:"result_path,omitempty"`
	PackagePaths        map[string]string      `json:"package_paths,omitempty"`
}

type planWorkerResponse struct {
	CleanupComplete bool                   `json:"cleanup_complete"`
	ProgressStream  *workerProgressSummary `json:"progress_stream,omitempty"`
	ProgressDropped uint64                 `json:"progress_dropped,omitempty"`
	Result          Result                 `json:"result"`
	Publication     PublicationResult      `json:"publication"`
	Failure         *workerError           `json:"failure,omitempty"`
}

func (response planWorkerResponse) failure() error {
	if response.Failure != nil {
		return response.Failure
	}
	return nil
}

// PublicationResult identifies a complete component OCI layout produced by a
// supervised Buildah worker.
type PublicationResult struct {
	Layout          string        `json:"layout"`
	Root            v1.Descriptor `json:"root"`
	ProgressDropped uint64        `json:"progress_dropped,omitempty"`
}

// BuildPlanSupervised runs a standalone plan inside a disposable Coopr
// subprocess. It promotes a complete OCI layout only after the worker exits
// successfully, and removes leftover named Buildah containers after failure.
func BuildPlanSupervised(ctx context.Context, plan *planner.Plan, options SupervisedPlanOptions) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("build context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if _, _, err := validateStandaloneGraph(plan); err != nil {
		return Result{}, err
	}
	if err := validateBuildInputs(options.Store, options.ContextDir, options.Output); err != nil {
		return Result{}, err
	}
	response, err := runPlanSupervised(ctx, plan, options, "build")
	if err != nil {
		return Result{ProgressDropped: response.ProgressDropped}, err
	}
	response.Result.Layout = options.Output.Path
	return response.Result, nil
}

// BuildDefinitionSupervised plans and builds a raw container definition inside
// the supervised worker. Planning in the worker allows base-image metadata to
// participate in planning without adding an unsupervised registry/store phase.
func BuildDefinitionSupervised(ctx context.Context, def *definition.Definition, planning planner.Options, options SupervisedPlanOptions) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("build context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := validateDefinitionPlanning(def, planning, planner.Build); err != nil {
		return Result{}, err
	}
	if err := validateBuildInputs(options.Store, options.ContextDir, options.Output); err != nil {
		return Result{}, err
	}
	response, err := runDefinitionSupervised(ctx, def, planning, options, "build")
	if err != nil {
		return Result{ProgressDropped: response.ProgressDropped}, err
	}
	response.Result.Layout = options.Output.Path
	return response.Result, nil
}

// VerifyStoredImageSelectedSupervised verifies that the exact selected
// manifest remains available for an image-config ID with multiple alternates.
func VerifyStoredImageSelectedSupervised(ctx context.Context, store StoreOptions, imageID string, manifest v1.Descriptor) error {
	if ctx == nil {
		return errors.New("image verification context is nil")
	}
	if imageID == "" {
		return errors.New("storage image ID is required")
	}
	if err := validateStoreOptions(store); err != nil {
		return err
	}
	if err := validateOptionalManifestDescriptor(manifest); err != nil {
		return err
	}
	_, err := runPlanSupervised(ctx, nil, SupervisedPlanOptions{Store: store, ImageID: imageID, ManifestDescriptor: manifest}, "verify")
	return err
}

// PublishDefinitionSupervised plans and publishes a raw component definition
// inside the supervised worker. Package paths are derived from the resulting
// worker-side plan before component assembly.
func PublishDefinitionSupervised(ctx context.Context, def *definition.Definition, planning planner.Options, options SupervisedPlanOptions) (PublicationResult, error) {
	if ctx == nil {
		return PublicationResult{}, errors.New("build context is nil")
	}
	if err := ctx.Err(); err != nil {
		return PublicationResult{}, err
	}
	if err := validateDefinitionPlanning(def, planning, planner.Publish); err != nil {
		return PublicationResult{}, err
	}
	if err := validateBuildInputs(options.Store, options.ContextDir, options.Output); err != nil {
		return PublicationResult{}, err
	}
	response, err := runDefinitionSupervised(ctx, def, planning, options, "publish")
	if err != nil {
		return PublicationResult{ProgressDropped: response.ProgressDropped}, err
	}
	response.Publication.Layout = options.Output.Path
	response.Publication.ProgressDropped = response.ProgressDropped
	return response.Publication, nil
}

func validateDefinitionPlanning(def *definition.Definition, planning planner.Options, mode planner.Mode) error {
	if planning.Mode != mode {
		return fmt.Errorf("%s definition build requires planner mode %q, got %q", mode, mode, planning.Mode)
	}
	if err := definition.Validate(def); err != nil {
		return fmt.Errorf("invalid raw definition: %w", err)
	}
	return nil
}

func runPlanSupervised(ctx context.Context, plan *planner.Plan, options SupervisedPlanOptions, mode string) (planWorkerResponse, error) {
	if def, planning, ok := plan.ExecutionRecipe(); ok {
		return runDefinitionSupervised(ctx, def, planning, options, mode, true)
	}
	return runBuildSupervised(ctx, plan, nil, nil, options, mode)
}

func runDefinitionSupervised(ctx context.Context, def *definition.Definition, planning planner.Options, options SupervisedPlanOptions, mode string, executionRecipe ...bool) (planWorkerResponse, error) {
	if len(planning.BuildContexts) == 0 {
		planning.BuildContexts = options.BuildContexts
	} else if len(options.BuildContexts) == 0 {
		options.BuildContexts = planning.BuildContexts
	} else if !slices.Equal(planning.BuildContexts, options.BuildContexts) {
		return planWorkerResponse{}, errors.New("planner and worker build contexts differ")
	}
	return runBuildSupervised(ctx, nil, def, &planning, options, mode, executionRecipe...)
}

func runBuildSupervised(ctx context.Context, plan *planner.Plan, def *definition.Definition, planning *planner.Options, options SupervisedPlanOptions, mode string, executionRecipe ...bool) (_ planWorkerResponse, retErr error) {
	sourcePolicy, err := loadSourcePolicy(options.SourcePolicyFile)
	if err != nil {
		return planWorkerResponse{}, err
	}
	options.Network, options.AddHosts, err = NormalizeBuildNetworkOptions(options.Network, options.AddHosts)
	if err != nil {
		return planWorkerResponse{}, err
	}
	if err := validateRunControls(options.RunControls); err != nil {
		return planWorkerResponse{}, err
	}
	if err := validateRunControlHost(options.RunControls); err != nil {
		return planWorkerResponse{}, err
	}
	if options.Jobs < 0 {
		return planWorkerResponse{}, errors.New("jobs must be nonnegative")
	}
	activity, err := acquireSupervisedActivity(ctx, options)
	if err != nil {
		return planWorkerResponse{}, fmt.Errorf("acquire supervised store activity lease: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	authFile, ownedAuthFile, err := oci.MaterializeRegistryAuthFile(options.AuthFile)
	if err != nil {
		return planWorkerResponse{}, err
	}
	if ownedAuthFile {
		defer func() { retErr = RemoveWorkerDirectory(authFile, retErr) }()
	}
	options.AuthFile = authFile
	options.AuthFile, options.CertDir, err = oci.NormalizeRegistryPaths(options.AuthFile, options.CertDir)
	if err != nil {
		return planWorkerResponse{}, err
	}
	jobParent := os.TempDir()
	if mode != "verify" {
		if _, err := os.Lstat(options.Output.Path); err == nil {
			return planWorkerResponse{}, fmt.Errorf("OCI output already exists: %s", options.Output.Path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return planWorkerResponse{}, fmt.Errorf("inspect OCI output: %w", err)
		}
		jobParent = filepath.Dir(options.Output.Path)
		if err := os.MkdirAll(jobParent, 0o755); err != nil {
			return planWorkerResponse{}, fmt.Errorf("create OCI output parent: %w", err)
		}
	}
	jobDir, err := os.MkdirTemp(jobParent, ".coopr-build-worker-*")
	if err != nil {
		return planWorkerResponse{}, fmt.Errorf("create build worker directory: %w", err)
	}
	removeJobDir := true
	defer func() {
		if removeJobDir {
			retErr = RemoveWorkerDirectory(jobDir, retErr)
		}
	}()
	// Buildah keeps RUN cache mounts below TMPDIR. Give them a stable location
	// scoped to this execution store. Only its native cache subtree may point
	// at the caller's persistent mount cache. Keep request and result files
	// in the disposable job directory.
	options.CacheMountDir, err = validateCacheMountParent(options.CacheMountDir, options.ContextDir, options.Store.RunRoot, options.Store.GraphRoot, options.CacheLocalDir, options.ComponentStoreDir, options.Output.Path, options.Output.Filesystem.Path)
	if err != nil {
		return planWorkerResponse{}, err
	}
	workerTemp, err := prepareWorkerTemp(options.Store.GraphRoot)
	if err != nil {
		return planWorkerResponse{}, err
	}
	if options.CacheMountDir != "" {
		if err := preparePersistentCacheMounts(workerTemp, options.CacheMountDir); err != nil {
			return planWorkerResponse{}, err
		}
	}
	workerTempAlias, err := socketSafeWorkerTemp(workerTemp)
	if err != nil {
		return planWorkerResponse{}, err
	}
	jobID, err := newWorkerJobID()
	if err != nil {
		return planWorkerResponse{}, err
	}
	proxyArgs := planner.PredefinedProxyArguments(options.ProxyArgs)
	if planning != nil {
		for name, value := range planner.PredefinedProxyArguments(planning.Arguments) {
			proxyArgs[name] = value
		}
	}
	workerOutput := options.Output
	workerOutput.Path = filepath.Join(jobDir, "layout")
	executableOrigin := currentRunSequenceOrigin()
	request := planWorkerRequest{
		workerExecutionOptions: workerExecutionOptions{
			StreamProgress:   options.Progress != nil,
			ExecutableOrigin: executableOrigin,
			Store:            options.Store, ContextDir: options.ContextDir, SourcePolicyFile: options.SourcePolicyFile, SourcePolicy: sourcePolicy, IgnoreFile: options.IgnoreFile,
			ContextArtifacts: append(append([]string(nil), options.ContextArtifacts...), jobDir, options.Store.ImageStore, options.ComponentStoreDir, options.CacheLocalDir, workerTemp, options.AuthFile, options.CertDir, options.RusageLogFile),
			Isolation:        options.Isolation, Runtime: options.Runtime,
			CacheLocalDir: options.CacheLocalDir, CacheMountDir: options.CacheMountDir,
			CacheRepository:    options.CacheRepository,
			CacheFrom:          slices.Clone(options.CacheFrom),
			CacheTo:            slices.Clone(options.CacheTo),
			NoCache:            options.NoCache,
			Network:            options.Network,
			AddHosts:           append([]string(nil), options.AddHosts...),
			TransientRunMounts: options.TransientRunMounts,
			RunControls:        options.RunControls, Lifecycle: options.Lifecycle,
			ImageControls:     options.ImageControls,
			Timestamp:         options.Timestamp,
			SourceDateEpoch:   options.SourceDateEpoch,
			CacheTTL:          options.CacheTTL,
			ProgressPrefix:    options.ProgressPrefix,
			ProgressReference: options.ProgressReference,
			Jobs:              options.Jobs,
			LogRusage:         options.LogRusage,
			RusageLogFile:     options.RusageLogFile,
			RewriteTimestamp:  options.RewriteTimestamp,
			Allow:             append([]string(nil), options.Allow...),
			BuildContexts:     append([]buildcontext.Spec(nil), options.BuildContexts...),
			Secrets:           append([]string(nil), options.Secrets...),
			SSH:               append([]string(nil), options.SSH...),
			ProxyArgs:         proxyArgs,
			AuthFile:          options.AuthFile, CertDir: options.CertDir, TLSVerify: options.TLSVerify, PullPolicy: options.PullPolicy,
		},
		ExecutionRecipe: len(executionRecipe) != 0 && executionRecipe[0],
		Mode:            mode, ImageID: options.ImageID, ManifestDescriptor: options.ManifestDescriptor, JobID: jobID, Plan: plan, Definition: def, PlannerOptions: planning,
		Output:            workerOutput,
		ComponentStoreDir: options.ComponentStoreDir,
		Credentials:       options.Credentials, Retry: options.Retry, RetrySet: options.RetrySet, RetryDelay: options.RetryDelay, DecryptionKeys: options.DecryptionKeys,
		SignaturePolicyPath: options.SignaturePolicyPath,
		ResultPath:          filepath.Join(jobDir, "result.json"),
	}
	progressReceiver, err := newWorkerProgressReceiver(request.ResultPath, options.Progress)
	if err != nil {
		return planWorkerResponse{}, err
	}
	defer func() { retErr = errors.Join(retErr, progressReceiver.close()) }()
	requestPath := filepath.Join(jobDir, "request.json")
	if err := writeWorkerJSON(requestPath, request); err != nil {
		return planWorkerResponse{}, err
	}
	command := reexec.Command(planWorkerName, requestPath)
	command.Stdin = options.Stdin
	command.Stdout = options.Stdout
	command.Stderr = options.Stderr
	command.Env = append(withoutSigningPassword(gitWorkerEnvironment(os.Environ())), "TMPDIR="+workerTempAlias)
	processErr := runWorkerProcess(ctx, command, workerGrace)
	if errors.Is(processErr, ErrWorkerWaitIncomplete) {
		removeJobDir = false
		return planWorkerResponse{}, processErr
	}
	if processErr != nil {
		cleanupErr := error(nil)
		response, readErr := readWorkerResponse(request.ResultPath)
		progressErr := progressReceiver.finish(response.ProgressStream)
		if readErr == nil && response.Failure != nil {
			processErr = WorkerSupervisionError(processErr)
		}
		if proofErr := requireWorkerCleanup(response.CleanupComplete, readErr); command.Process != nil && proofErr != nil {
			removeJobDir = false
			return response, errors.Join(proofErr, response.failure(), processErr, ctx.Err(), progressErr)
		}
		if errors.Is(response.Failure, ErrWorkerWaitIncomplete) {
			removeJobDir = false
			return response, errors.Join(response.failure(), processErr, ctx.Err(), readErr, progressErr)
		}
		if options.Lifecycle.removeBuilder(true, ctx.Err() != nil) || readErr != nil || response.Failure == nil {
			cleanupErr = cleanupWorkerJob(request, jobDir, options.Stderr, ctx.Err() != nil)
		}
		if cleanupErr != nil {
			removeJobDir = false
		}
		if readErr == nil && response.Failure != nil {
			if ctx.Err() != nil {
				return response, errors.Join(ctx.Err(), fmt.Errorf("build worker exited after cleanup: %w", response.failure()), processErr, cleanupErr, progressErr)
			}
			return response, errors.Join(WorkerFailure(response.failure()), processErr, cleanupErr, progressErr)
		}
		return response, errors.Join(processErr, cleanupErr, readErr, progressErr)
	}
	response, responseErr := readWorkerResponse(request.ResultPath)
	progressErr := progressReceiver.finish(response.ProgressStream)
	if proofErr := requireWorkerCleanup(response.CleanupComplete, responseErr); proofErr != nil {
		removeJobDir = false
		return response, errors.Join(proofErr, progressErr)
	}
	if err := errors.Join(response.failure(), progressErr); err != nil {
		return response, err
	}
	if err := ctx.Err(); err != nil {
		return response, err
	}
	if mode != "verify" {
		if err := promoteCompleteOutput(request.Output.Path, options.Output.Path, response.CleanupComplete); err != nil {
			return response, fmt.Errorf("promote complete OCI output: %w", err)
		}
	}
	return response, nil
}

func publicationPackagePaths(plan *planner.Plan, directory string) map[string]string {
	paths := make(map[string]string)
	if plan == nil {
		return paths
	}
	stages := make(map[string]planner.Stage, len(plan.Stages))
	for _, stage := range plan.Stages {
		stages[stage.ID] = stage
	}
	for index, output := range plan.Outputs {
		stage, ok := stages[output]
		if !ok {
			continue
		}
		key := stage.Name
		if key == "" {
			key = stage.ID
		}
		paths[key] = filepath.Join(directory, fmt.Sprintf("package-%d.tar", index))
	}
	return paths
}

func runPlanWorker() {
	unshare.MaybeReexecUsingUserNamespace(false)
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "invalid Coopr build worker arguments")
		os.Exit(2)
	}
	scope, err := newWorkerChildScope()
	if err == nil {
		err = executePlanWorkerScoped(os.Args[1], scope)
	}
	if err != nil {
		if !workerFailureReported(err) {
			_, _ = fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	os.Exit(0)
}

func executePlanWorkerScoped(requestPath string, scope *workerChildScope) (retErr error) {
	var request planWorkerRequest
	var pending *planWorkerResponse
	var finishProgress func(*planWorkerResponse)
	workerCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	// Registered before operation defers: stores, executors, resolvers and known
	// command waits finish before adopted children and final response publication.
	defer func() {
		// Keep TERM handled until children and the final response are complete.
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
		finishProgress(pending)
		if writeErr := writeWorkerJSON(request.ResultPath, *pending); writeErr != nil {
			retErr = errors.Join(retErr, writeErr)
		} else {
			retErr = reportedWorkerFailure(retErr)
		}
	}()
	if err := readWorkerJSON(requestPath, &request); err != nil {
		return err
	}
	if !validWorkerJobID(request.JobID) {
		return errors.New("invalid build worker job ID")
	}
	if request.Mode == "cleanup" {
		return cleanupNamedBuilders(request.Store, request.JobID)
	}
	if err := validatePlanWorkerRequest(request); err != nil {
		return err
	}
	progressBuffer, progressStream, err := newWorkerProgressCapture(request.StreamProgress, request.ResultPath)
	if err != nil {
		return err
	}
	defer progressStream.finish()
	finishProgress = func(response *planWorkerResponse) {
		response.ProgressDropped, response.ProgressStream = finishWorkerProgress(progressBuffer, progressStream)
		response.Result.ProgressDropped = response.ProgressDropped
		response.Publication.ProgressDropped = response.ProgressDropped
	}
	if err := ConfigureRuntimeConfig(request.RunControls); err != nil {
		return err
	}
	if request.Mode == "verify" {
		if request.ImageID == "" {
			return errors.New("image worker request lacks image ID")
		}
		lease, err := acquireStore(request.Store)
		if err != nil {
			return err
		}
		image, verifyErr := lease.store.Image(request.ImageID)
		if verifyErr == nil && image.ID != request.ImageID {
			verifyErr = fmt.Errorf("storage image %s resolved to %s", request.ImageID, image.ID)
		}
		if verifyErr == nil && request.ManifestDescriptor.Digest != "" {
			reference, referenceErr := imagestorage.Transport.NewStoreReference(lease.store, nil, request.ImageID)
			if referenceErr != nil {
				verifyErr = referenceErr
			} else {
				source, sourceErr := reference.NewImageSource(context.Background(), &types.SystemContext{SignaturePolicyPath: request.SignaturePolicyPath})
				if sourceErr != nil {
					verifyErr = sourceErr
				} else {
					manifest, mediaType, manifestErr := source.GetManifest(context.Background(), &request.ManifestDescriptor.Digest)
					verifyErr = errors.Join(manifestErr, source.Close())
					if verifyErr == nil && (digest.FromBytes(manifest) != request.ManifestDescriptor.Digest || int64(len(manifest)) != request.ManifestDescriptor.Size || mediaType != request.ManifestDescriptor.MediaType) {
						verifyErr = fmt.Errorf("stored manifest differs from selected descriptor %s", request.ManifestDescriptor.Digest)
					}
				}
			}
		}

		closeErr := lease.Close()
		if verifyErr == nil {
			verifyErr = closeErr
		}
		response := planWorkerResponse{}
		if verifyErr != nil {
			response.Failure = WorkerFailure(verifyErr)
		}
		pending = &response
		return verifyErr
	}
	// Resolution and planning can read a registry or import an image. Keep both
	// under the same cancellation scope as filesystem execution.
	response := planWorkerResponse{}
	if progressBuffer != nil {
		workerCtx = withOperationProgress(workerCtx, progressBuffer.record)
	}
	var resolver *oci.Resolver
	var resolvedBases map[ResolvedBaseKey]ResolvedImageSource
	var replannedBaseDelta func() map[ResolvedBaseKey]ResolvedImageSource
	if request.Definition != nil {
		plan, preparedResolver, selected, err := planDefinitionInWorker(workerCtx, request)
		if err != nil {
			buildErr := fmt.Errorf("plan raw definition: %w", err)
			response.Failure = WorkerFailure(buildErr)
			pending = &response
			return buildErr
		}
		request.Plan = plan
		resolver = preparedResolver
		resolvedBases = maps.Clone(selected.all)
		replannedBaseDelta = selected.takeDelta
		if request.Mode == "publish" {
			request.PackagePaths = publicationPackagePaths(plan, filepath.Join(filepath.Dir(request.ResultPath), "packages"))
		}
	}
	if resolver == nil {
		hasRegistryCache := request.CacheRepository != "" || slices.ContainsFunc(slices.Concat(request.CacheFrom, request.CacheTo), func(spec CacheSpec) bool {
			return spec.Transport == "registry"
		})
		needsResolver := len(request.Output.SBOM) > 0 || hasRegistryCache ||
			slices.ContainsFunc(request.Plan.Inputs, func(input planner.Input) bool { return input.Kind == "image" || input.Kind == "component" }) ||
			slices.ContainsFunc(request.Plan.Contexts, func(input planner.ContextInput) bool { return input.Kind == buildcontext.DockerImage })
		if needsResolver {
			resolver, err = request.newResolver()
			if err != nil {
				return err
			}
		}
	}
	// The parent sends TERM to the whole process group. Keep this worker alive
	// long enough for Buildah's RUN child to unwind and for builder/store defers
	// to run; the parent escalates to KILL after a bounded grace period.
	if request.LogRusage && request.RusageLogFile != "" {
		if err := os.MkdirAll(filepath.Dir(request.RusageLogFile), 0o755); err != nil {
			return err
		}
		file, err := os.OpenFile(request.RusageLogFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("create resource usage log: %w", err)
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	planOptions := request.planOptions()
	planOptions.workerDirectory = filepath.Dir(request.ResultPath)
	planOptions.progressBuffer = progressBuffer
	planOptions.registryOptions = oci.Options{Retry: request.Retry, RetrySet: request.RetrySet, RetryDelay: request.RetryDelay}
	planOptions.Output = request.Output
	planOptions.Resolver = resolver
	planOptions.ResolvedBases = resolvedBases
	planOptions.ReplannedBaseDelta = replannedBaseDelta
	planOptions.progressStageTotal = definitionStageCount(request.Definition)
	planOptions.SystemContext = &types.SystemContext{
		SignaturePolicyPath:  request.SignaturePolicyPath,
		BigFilesTemporaryDir: filepath.Dir(request.ResultPath),
		AuthFilePath:         request.AuthFile,
		DockerCertPath:       request.CertDir,
	}
	planOptions.JobID = request.JobID
	if progressBuffer != nil {
		planOptions.Progress = progressBuffer.record
	}
	oci.ApplyTLSVerify(planOptions.SystemContext, request.TLSVerify)
	var buildErr error
	if request.Mode == "build" {
		response.Result, buildErr = BuildPlan(workerCtx, request.Plan, planOptions)
		if buildErr == nil {
			response.Result.Platform = request.Plan.Platform
		}
	} else {
		var packages map[string]oci.Package
		packages, buildErr = PublishPlan(workerCtx, request.Plan, PublicationOptions{
			PlanOptions: planOptions, PackagePaths: request.PackagePaths,
		})
		if buildErr == nil {
			stages := make([]string, 0, len(packages))
			for stage := range packages {
				stages = append(stages, stage)
			}
			slices.Sort(stages)
			ordered := make([]oci.Package, len(stages))
			for index, stage := range stages {
				ordered[index] = packages[stage]
			}
			platform, err := executionPlatform(request.Plan.Platform)
			if err != nil {
				buildErr = fmt.Errorf("component platform: %w", err)
			}
			var root v1.Descriptor
			if buildErr == nil {
				root, err = oci.WriteComponentLayout(workerCtx, request.Output.Path, oci.ComponentMetadata{
					Version:   oci.ComponentVersion,
					Platform:  platform,
					Component: *request.Plan.Component,
					Packages:  ordered,
				}, request.PackagePaths)
			}
			if err != nil {
				buildErr = fmt.Errorf("write component artifact: %w", err)
			} else {
				response.Publication = PublicationResult{Layout: request.Output.Path, Root: root}
			}
		}
	}
	if buildErr != nil {
		response.Failure = WorkerFailure(buildErr)
	}
	pending = &response
	return buildErr
}

func validatePlanWorkerRequest(request planWorkerRequest) error {
	switch request.Mode {
	case "build", "publish", "verify":
	default:
		return fmt.Errorf("unknown build worker mode %q", request.Mode)
	}
	if request.ResultPath == "" {
		return errors.New("build worker request lacks result path")
	}
	if request.Mode == "verify" {
		if request.ImageID == "" {
			return errors.New("image worker request lacks image ID")
		}
		if request.Plan != nil || request.Definition != nil || request.PlannerOptions != nil {
			return errors.New("image worker request cannot contain build input")
		}
		if err := validateOptionalManifestDescriptor(request.ManifestDescriptor); err != nil {
			return err
		}
		return nil
	}
	hasPlan := request.Plan != nil
	hasDefinition := request.Definition != nil
	if hasPlan == hasDefinition {
		return errors.New("build worker request requires exactly one plan or raw definition")
	}
	if hasDefinition {
		if request.PlannerOptions == nil {
			return errors.New("raw definition worker request lacks planner options")
		}
		expected := planner.Build
		if request.Mode == "publish" {
			expected = planner.Publish
		}
		if err := validateDefinitionPlanning(request.Definition, *request.PlannerOptions, expected); err != nil {
			return err
		}
	} else if request.PlannerOptions != nil {
		return errors.New("planned worker request cannot contain planner options")
	}
	return nil
}

func validateOptionalManifestDescriptor(manifest v1.Descriptor) error {
	if manifest.Digest == "" && manifest.MediaType == "" && manifest.Size == 0 {
		return nil
	}
	if manifest.Digest.Validate() != nil || manifest.MediaType == "" || manifest.Size < 0 {
		return errors.New("selected manifest descriptor requires a valid digest, media type, and non-negative size")
	}
	return nil
}

func promoteCompleteOutput(source, destination string, cleanupComplete bool) error {
	if err := requireWorkerCleanup(cleanupComplete, nil); err != nil {
		return err
	}
	// RENAME_NOREPLACE closes the race between the initial destination check
	// and a completed build. A concurrent creator keeps its output intact.
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
}

func cleanupWorkerJob(request planWorkerRequest, jobDir string, stderr io.Writer, canceled bool) error {
	request.Mode = "cleanup"
	request.Plan = nil
	request.Definition = nil
	request.PlannerOptions = nil
	request.ResultPath = ""
	path := filepath.Join(jobDir, "cleanup.json")
	if err := writeWorkerJSON(path, request); err != nil {
		return err
	}
	workTimeout := cleanupTimeout
	if canceled {
		workTimeout = cleanupCanceledTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), workTimeout)
	defer cancel()
	command := reexec.Command(planWorkerName, path)
	command.Stderr = stderr
	command.Env = append(withoutSigningPassword(gitWorkerEnvironment(os.Environ())), "TMPDIR="+jobDir)
	if err := runWorkerProcess(ctx, command, cleanupWorkerGrace); err != nil {
		return errors.Join(ErrWorkerWaitIncomplete, fmt.Errorf("cleanup interrupted build job %s (files retained at %s): %w", request.JobID, jobDir, err))
	}
	return nil
}

func cleanupNamedBuilders(options StoreOptions, jobID string) (retErr error) {
	lease, err := acquireStore(options)
	if err != nil {
		return fmt.Errorf("open interrupted build store: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, lease.Close()) }()
	containers, err := lease.store.Containers()
	if err != nil {
		return fmt.Errorf("list interrupted build containers: %w", err)
	}
	prefix := "coopr-" + jobID + "-"
	for _, container := range containers {
		owned := false
		for _, name := range container.Names {
			if strings.HasPrefix(name, prefix) {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		if _, err := lease.store.Unmount(container.ID, true); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("unmount interrupted container %s: %w", container.ID, err))
			continue
		}
		if err := lease.store.DeleteContainer(container.ID); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("delete interrupted container %s: %w", container.ID, err))
		}
	}
	return retErr
}

func newWorkerJobID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("create build worker ID: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

func validWorkerJobID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func writeWorkerJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode build worker message: %w", err)
	}
	if len(data) > workerMaxJSON {
		return errors.New("build worker message exceeds size limit")
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write build worker message: %w", err)
	}
	return nil
}

func readWorkerJSON(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open build worker message: %w", err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, workerMaxJSON+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode build worker message: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("build worker message contains trailing data")
	}
	return nil
}

func readWorkerResponse(path string) (planWorkerResponse, error) {
	var response planWorkerResponse
	if err := readWorkerJSON(path, &response); err != nil {
		return planWorkerResponse{}, err
	}
	response.Result.ProgressDropped = response.ProgressDropped
	response.Publication.ProgressDropped = response.ProgressDropped
	return response, nil
}
