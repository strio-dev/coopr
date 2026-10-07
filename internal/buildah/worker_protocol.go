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

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage/pkg/reexec"
	"go.podman.io/storage/pkg/unshare"
	"golang.org/x/sys/unix"
)

const (
	planWorkerName = "coopr-buildah-plan-worker-v1"
	workerGrace    = 3 * time.Second
	cleanupTimeout = 20 * time.Second
	workerMaxJSON  = 64 << 20
)

func init() {
	reexec.Register("git", runGitWrapper)
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
	ProgressPrefix      string
	ProgressReference   string
	Lifecycle           LifecycleControls
	Store               StoreOptions
	ImageID             string
	ManifestDigest      digest.Digest
	ManifestDescriptor  v1.Descriptor
	ContextDir          string
	IgnoreFile          string
	ContextArtifacts    []string
	Isolation           string
	Runtime             string
	Output              Output
	ComponentStoreDir   string
	CacheLocalDir       string
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
	Pull                bool
	PullPolicy          string
	SignaturePolicyPath string
	Stdout              io.Writer
	Stderr              io.Writer
	Stdin               io.Reader
}

type planWorkerRequest struct {
	ProgressReference   string                 `json:"progress_reference,omitempty"`
	ProgressPrefix      string                 `json:"progress_prefix,omitempty"`
	Lifecycle           LifecycleControls      `json:"lifecycle,omitempty"`
	Mode                string                 `json:"mode"`
	ImageID             string                 `json:"image_id,omitempty"`
	ManifestDigest      digest.Digest          `json:"manifest_digest,omitempty"`
	ManifestDescriptor  v1.Descriptor          `json:"manifest_descriptor,omitempty"`
	JobID               string                 `json:"job_id"`
	Plan                *planner.Plan          `json:"plan,omitempty"`
	Definition          *definition.Definition `json:"definition,omitempty"`
	PlannerOptions      *planner.Options       `json:"planner_options,omitempty"`
	Store               StoreOptions           `json:"store"`
	ContextDir          string                 `json:"context_dir,omitempty"`
	IgnoreFile          string                 `json:"ignore_file,omitempty"`
	ContextArtifacts    []string               `json:"context_artifacts,omitempty"`
	Isolation           string                 `json:"isolation,omitempty"`
	Runtime             string                 `json:"runtime,omitempty"`
	Output              Output                 `json:"output"`
	ComponentStoreDir   string                 `json:"component_store_dir,omitempty"`
	CacheLocalDir       string                 `json:"cache_local_dir,omitempty"`
	CacheRepository     string                 `json:"cache_repository,omitempty"`
	CacheFrom           []CacheSpec            `json:"cache_from,omitempty"`
	CacheTo             []CacheSpec            `json:"cache_to,omitempty"`
	NoCache             bool                   `json:"no_cache,omitempty"`
	Network             string                 `json:"network,omitempty"`
	AddHosts            []string               `json:"add_hosts,omitempty"`
	RunControls         RunControls            `json:"run_controls,omitempty"`
	ImageControls       ImageControls          `json:"image_controls,omitempty"`
	Timestamp           *int64                 `json:"timestamp,omitempty"`
	SourceDateEpoch     *int64                 `json:"source_date_epoch,omitempty"`
	CacheTTL            *time.Duration         `json:"cache_ttl,omitempty"`
	Jobs                int                    `json:"jobs,omitempty"`
	LogRusage           bool                   `json:"log_rusage,omitempty"`
	RusageLogFile       string                 `json:"rusage_log_file,omitempty"`
	RewriteTimestamp    bool                   `json:"rewrite_timestamp,omitempty"`
	Allow               []string               `json:"allow,omitempty"`
	BuildContexts       []buildcontext.Spec    `json:"build_contexts,omitempty"`
	Secrets             []string               `json:"secrets,omitempty"`
	SSH                 []string               `json:"ssh,omitempty"`
	ProxyArgs           map[string]string      `json:"proxy_args,omitempty"`
	AuthFile            string                 `json:"auth_file,omitempty"`
	CertDir             string                 `json:"cert_dir,omitempty"`
	TLSVerify           *bool                  `json:"tls_verify,omitempty"`
	Credentials         string                 `json:"credentials,omitempty"`
	Retry               uint                   `json:"retry,omitempty"`
	RetrySet            bool                   `json:"retry_set,omitempty"`
	RetryDelay          time.Duration          `json:"retry_delay,omitempty"`
	DecryptionKeys      []string               `json:"decryption_keys,omitempty"`
	Pull                bool                   `json:"pull,omitempty"`
	PullPolicy          string                 `json:"pull_policy,omitempty"`
	SignaturePolicyPath string                 `json:"signature_policy_path,omitempty"`
	ResultPath          string                 `json:"result_path,omitempty"`
	PackagePaths        map[string]string      `json:"package_paths,omitempty"`
}

type planWorkerResponse struct {
	Result      Result            `json:"result"`
	Publication PublicationResult `json:"publication"`
	Error       string            `json:"error,omitempty"`
}

// PublicationResult identifies a complete component OCI layout produced by a
// supervised Buildah worker.
type PublicationResult struct {
	Layout string        `json:"layout"`
	Root   v1.Descriptor `json:"root"`
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
	if err := validateRequest(Request{Store: options.Store, ContextDir: options.ContextDir, Output: options.Output}); err != nil {
		return Result{}, err
	}
	response, err := runPlanSupervised(ctx, plan, options, "build")
	if err != nil {
		return Result{}, err
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
	if err := validateRequest(Request{Store: options.Store, ContextDir: options.ContextDir, Output: options.Output}); err != nil {
		return Result{}, err
	}
	response, err := runDefinitionSupervised(ctx, def, planning, options, "build")
	if err != nil {
		return Result{}, err
	}
	response.Result.Layout = options.Output.Path
	return response.Result, nil
}

// ExportStoredImageSupervised exports one committed image in the same rootless
// storage namespace used by builds. The layout at output is caller-owned.
func ExportStoredImageSupervised(ctx context.Context, store StoreOptions, imageID, output string) (Result, error) {
	return ExportStoredImageSelectedSupervised(ctx, store, imageID, "", output)
}

// ExportStoredImageSelectedSupervised exports the exact stored manifest named
// by manifestDigest. An empty digest retains the legacy default-manifest path.
func ExportStoredImageSelectedSupervised(ctx context.Context, store StoreOptions, imageID string, manifestDigest digest.Digest, output string) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("export context is nil")
	}
	if imageID == "" {
		return Result{}, errors.New("storage image ID is required")
	}
	if err := validateRequest(Request{Store: store, Output: Output{Path: output}}); err != nil {
		return Result{}, err
	}
	response, err := runPlanSupervised(ctx, nil, SupervisedPlanOptions{
		Store: store, ImageID: imageID, ManifestDigest: manifestDigest, Output: Output{Path: output},
	}, "export")
	if err != nil {
		return Result{}, err
	}
	response.Result.Layout = output
	return response.Result, nil
}

// VerifyStoredImageSupervised checks image metadata inside the same rootless
// namespace as builds without exporting layer data.
func VerifyStoredImageSupervised(ctx context.Context, store StoreOptions, imageID string) error {
	return VerifyStoredImageSelectedSupervised(ctx, store, imageID, v1.Descriptor{})
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

// PublishPlanSupervised executes component package stages in a disposable
// Coopr subprocess and atomically promotes the complete component OCI layout.
func PublishPlanSupervised(ctx context.Context, plan *planner.Plan, options SupervisedPlanOptions) (PublicationResult, error) {
	if ctx == nil {
		return PublicationResult{}, errors.New("build context is nil")
	}
	if err := ctx.Err(); err != nil {
		return PublicationResult{}, err
	}
	if err := validateRequest(Request{Store: options.Store, ContextDir: options.ContextDir, Output: options.Output}); err != nil {
		return PublicationResult{}, err
	}
	validationPaths := publicationPackagePaths(plan, filepath.Join(filepath.Dir(options.Output.Path), ".coopr-package-validation"))
	if _, _, err := validatePublicationGraph(plan, validationPaths); err != nil {
		return PublicationResult{}, err
	}
	response, err := runPlanSupervised(ctx, plan, options, "publish")
	if err != nil {
		return PublicationResult{}, err
	}
	response.Publication.Layout = options.Output.Path
	return response.Publication, nil
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
	if err := validateRequest(Request{Store: options.Store, ContextDir: options.ContextDir, Output: options.Output}); err != nil {
		return PublicationResult{}, err
	}
	response, err := runDefinitionSupervised(ctx, def, planning, options, "publish")
	if err != nil {
		return PublicationResult{}, err
	}
	response.Publication.Layout = options.Output.Path
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
	return runBuildSupervised(ctx, plan, nil, nil, options, mode)
}

func runDefinitionSupervised(ctx context.Context, def *definition.Definition, planning planner.Options, options SupervisedPlanOptions, mode string) (planWorkerResponse, error) {
	if len(planning.BuildContexts) == 0 {
		planning.BuildContexts = options.BuildContexts
	} else if len(options.BuildContexts) == 0 {
		options.BuildContexts = planning.BuildContexts
	} else if !slices.Equal(planning.BuildContexts, options.BuildContexts) {
		return planWorkerResponse{}, errors.New("planner and worker build contexts differ")
	}
	return runBuildSupervised(ctx, nil, def, &planning, options, mode)
}

func runBuildSupervised(ctx context.Context, plan *planner.Plan, def *definition.Definition, planning *planner.Options, options SupervisedPlanOptions, mode string) (_ planWorkerResponse, retErr error) {
	var err error
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
	options.AuthFile, options.CertDir, err = oci.NormalizeRegistryPaths(options.AuthFile, options.CertDir)
	if err != nil {
		return planWorkerResponse{}, err
	}
	jobParent := os.TempDir()
	if mode != "verify" && mode != "import" {
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
			_ = os.RemoveAll(jobDir)
		}
	}()
	// Buildah keeps RUN cache mounts below TMPDIR. Give them a stable location
	// scoped to this execution store while keeping the request and result files
	// in the disposable job directory.
	workerTemp, err := prepareWorkerTemp(options.Store.GraphRoot)
	if err != nil {
		return planWorkerResponse{}, err
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
	request := planWorkerRequest{
		Mode: mode, ImageID: options.ImageID, ManifestDigest: options.ManifestDigest, ManifestDescriptor: options.ManifestDescriptor, JobID: jobID, Plan: plan, Definition: def, PlannerOptions: planning,
		Store: options.Store, ContextDir: options.ContextDir, IgnoreFile: options.IgnoreFile,
		ContextArtifacts: append(append([]string(nil), options.ContextArtifacts...), jobDir, options.Store.ImageStore, options.ComponentStoreDir, options.CacheLocalDir, workerTemp, options.AuthFile, options.CertDir, options.RusageLogFile),
		Isolation:        options.Isolation, Runtime: options.Runtime,
		Output:            workerOutput,
		ComponentStoreDir: options.ComponentStoreDir, CacheLocalDir: options.CacheLocalDir,
		CacheRepository: options.CacheRepository,
		CacheFrom:       slices.Clone(options.CacheFrom),
		CacheTo:         slices.Clone(options.CacheTo),
		NoCache:         options.NoCache,
		Network:         options.Network,
		AddHosts:        append([]string(nil), options.AddHosts...),
		RunControls:     options.RunControls, Lifecycle: options.Lifecycle,
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
		AuthFile:          options.AuthFile, CertDir: options.CertDir, TLSVerify: options.TLSVerify, Pull: options.Pull, PullPolicy: options.PullPolicy,
		Credentials: options.Credentials, Retry: options.Retry, RetrySet: options.RetrySet, RetryDelay: options.RetryDelay, DecryptionKeys: options.DecryptionKeys,
		SignaturePolicyPath: options.SignaturePolicyPath,
		ResultPath:          filepath.Join(jobDir, "result.json"),
	}
	if mode == "import" {
		request.Output = options.Output
	}
	if mode == "publish" {
		if plan != nil {
			request.PackagePaths = publicationPackagePaths(plan, filepath.Join(jobDir, "packages"))
		}
	}
	requestPath := filepath.Join(jobDir, "request.json")
	if err := writeWorkerJSON(requestPath, request); err != nil {
		return planWorkerResponse{}, err
	}
	command := reexec.Command(planWorkerName, requestPath)
	command.Stdin = options.Stdin
	command.Stdout = options.Stdout
	command.Stderr = options.Stderr
	gitEnvironment, err := prepareGitWorkerEnvironment(os.Environ(), jobDir)
	if err != nil {
		return planWorkerResponse{}, err
	}
	command.Env = append(withoutSigningPassword(gitEnvironment), "TMPDIR="+workerTempAlias)
	processErr := runWorkerProcess(ctx, command, workerGrace)
	if processErr != nil {
		cleanupErr := error(nil)
		response, readErr := readWorkerResponse(request.ResultPath)
		if options.Lifecycle.removeBuilder(true, ctx.Err() != nil) || readErr != nil || response.Error == "" {
			cleanupErr = cleanupWorkerJob(request, jobDir, options.Stderr)
		}
		if cleanupErr != nil {
			removeJobDir = false
		}
		if readErr == nil && response.Error != "" {
			if ctx.Err() != nil {
				return planWorkerResponse{}, errors.Join(ctx.Err(), fmt.Errorf("build worker exited after cleanup: %s", response.Error), cleanupErr)
			}
			return planWorkerResponse{}, errors.Join(errors.New(response.Error), cleanupErr)
		}
		return planWorkerResponse{}, errors.Join(processErr, cleanupErr)
	}
	response, err := readWorkerResponse(request.ResultPath)
	if err != nil {
		cleanupErr := cleanupWorkerJob(request, jobDir, options.Stderr)
		if cleanupErr != nil {
			removeJobDir = false
		}
		return planWorkerResponse{}, errors.Join(err, cleanupErr)
	}
	if response.Error != "" {
		return planWorkerResponse{}, errors.New(response.Error)
	}
	if err := ctx.Err(); err != nil {
		return planWorkerResponse{}, err
	}
	if mode != "verify" && mode != "import" {
		if err := promoteCompleteOutput(request.Output.Path, options.Output.Path); err != nil {
			return planWorkerResponse{}, fmt.Errorf("promote complete OCI output: %w", err)
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
	if err := executePlanWorker(os.Args[1]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func executePlanWorker(requestPath string) error {
	var request planWorkerRequest
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
	if err := ConfigureRuntimeConfig(request.RunControls); err != nil {
		return err
	}
	if request.Mode == "import" {
		lease, err := acquireStore(request.Store)
		if err != nil {
			return err
		}
		system := &types.SystemContext{
			SignaturePolicyPath:  request.SignaturePolicyPath,
			BigFilesTemporaryDir: filepath.Dir(request.ResultPath),
		}
		imageID, importErr := ImportSelectedImage(context.Background(), lease.store, system, request.Output.Path, request.ManifestDescriptor)
		closeErr := lease.Close()
		if importErr == nil {
			importErr = closeErr
		}
		response := planWorkerResponse{Result: Result{ImageID: imageID, ManifestDigest: request.ManifestDescriptor.Digest.String()}}
		if importErr != nil {
			response.Error = importErr.Error()
		}
		if err := writeWorkerJSON(request.ResultPath, response); err != nil {
			return err
		}
		return importErr
	}
	if request.Mode == "export" || request.Mode == "verify" {
		if request.ImageID == "" {
			return errors.New("image worker request lacks image ID")
		}
		lease, err := acquireStore(request.Store)
		if err != nil {
			return err
		}
		result := Result{}
		var exportErr error
		if request.Mode == "verify" {
			image, inspectErr := lease.store.Image(request.ImageID)
			exportErr = inspectErr
			if exportErr == nil && image.ID != request.ImageID {
				exportErr = fmt.Errorf("storage image %s resolved to %s", request.ImageID, image.ID)
			}
			if exportErr == nil && request.ManifestDescriptor.Digest != "" {
				reference, referenceErr := imagestorage.Transport.NewStoreReference(lease.store, nil, request.ImageID)
				if referenceErr != nil {
					exportErr = referenceErr
				} else {
					source, sourceErr := reference.NewImageSource(context.Background(), &types.SystemContext{SignaturePolicyPath: request.SignaturePolicyPath})
					if sourceErr != nil {
						exportErr = sourceErr
					} else {
						manifest, mediaType, manifestErr := source.GetManifest(context.Background(), &request.ManifestDescriptor.Digest)
						exportErr = errors.Join(manifestErr, source.Close())
						if exportErr == nil && (digest.FromBytes(manifest) != request.ManifestDescriptor.Digest || int64(len(manifest)) != request.ManifestDescriptor.Size || mediaType != request.ManifestDescriptor.MediaType) {
							exportErr = fmt.Errorf("stored manifest differs from selected descriptor %s", request.ManifestDescriptor.Digest)
						}
					}
				}
			}
		} else {
			result, exportErr = exportStoredImageVariantRaw(context.Background(), lease.store, request.ImageID, request.Output, &types.SystemContext{
				SignaturePolicyPath:  request.SignaturePolicyPath,
				BigFilesTemporaryDir: filepath.Dir(request.ResultPath),
			}, optionalDigest(request.ManifestDigest))
		}
		closeErr := lease.Close()
		if exportErr == nil {
			exportErr = closeErr
		}
		response := planWorkerResponse{Result: result}
		if exportErr != nil {
			response.Error = exportErr.Error()
		}
		if err := writeWorkerJSON(request.ResultPath, response); err != nil {
			return err
		}
		return exportErr
	}
	// Resolution and planning can read a registry or import an image. Keep both
	// under the same cancellation scope as filesystem execution.
	workerCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	response := planWorkerResponse{}
	var resolver *oci.Resolver
	var resolvedBases map[ResolvedBaseKey]ResolvedImageSource
	var replannedBaseDelta func() map[ResolvedBaseKey]ResolvedImageSource
	if request.Definition != nil {
		plan, preparedResolver, selected, err := planDefinitionInWorker(workerCtx, request)
		if err != nil {
			buildErr := fmt.Errorf("plan raw definition: %w", err)
			response.Error = buildErr.Error()
			if writeErr := writeWorkerJSON(request.ResultPath, response); writeErr != nil {
				return errors.Join(buildErr, writeErr)
			}
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
	for _, input := range request.Plan.Inputs {
		if resolver != nil {
			break
		}
		if input.Kind == "image" || input.Kind == "component" {
			var err error
			resolver, err = oci.NewResolver(oci.Options{
				AuthFile: request.AuthFile, CertDir: request.CertDir, TLSVerify: request.TLSVerify,
				Credentials: request.Credentials, Retry: request.Retry, RetrySet: request.RetrySet, RetryDelay: request.RetryDelay, DecryptionKeys: request.DecryptionKeys, SignaturePolicyPath: request.SignaturePolicyPath,
				Pull: request.Pull, PullPolicy: request.PullPolicy, ComponentStoreDir: request.ComponentStoreDir, NativeStore: NativeStoreOptions(request.Store),
			})
			if err != nil {
				return fmt.Errorf("create build worker resolver: %w", err)
			}
			break
		}
	}
	if resolver == nil && len(request.Output.SBOM) > 0 {
		var err error
		resolver, err = oci.NewResolver(oci.Options{
			AuthFile: request.AuthFile, CertDir: request.CertDir, TLSVerify: request.TLSVerify,
			Credentials: request.Credentials, Retry: request.Retry, RetrySet: request.RetrySet, RetryDelay: request.RetryDelay, DecryptionKeys: request.DecryptionKeys, SignaturePolicyPath: request.SignaturePolicyPath,
			Pull: request.Pull, PullPolicy: request.PullPolicy, ComponentStoreDir: request.ComponentStoreDir, NativeStore: NativeStoreOptions(request.Store),
		})
		if err != nil {
			return fmt.Errorf("create SBOM image resolver: %w", err)
		}
	}
	if resolver == nil {
		for _, input := range request.Plan.Contexts {
			if input.Kind != buildcontext.DockerImage {
				continue
			}
			var err error
			resolver, err = oci.NewResolver(oci.Options{
				AuthFile: request.AuthFile, CertDir: request.CertDir, TLSVerify: request.TLSVerify,
				Credentials: request.Credentials, Retry: request.Retry, RetrySet: request.RetrySet, RetryDelay: request.RetryDelay, DecryptionKeys: request.DecryptionKeys, SignaturePolicyPath: request.SignaturePolicyPath,
				Pull: request.Pull, PullPolicy: request.PullPolicy, ComponentStoreDir: request.ComponentStoreDir, NativeStore: NativeStoreOptions(request.Store),
			})
			if err != nil {
				return fmt.Errorf("create build worker resolver: %w", err)
			}
			break
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
	planOptions := PlanOptions{
		Store: request.Store, ContextDir: request.ContextDir, IgnoreFile: request.IgnoreFile, ContextArtifacts: request.ContextArtifacts, Isolation: request.Isolation,
		Runtime: request.Runtime, Output: request.Output, Resolver: resolver, ResolvedBases: resolvedBases, ReplannedBaseDelta: replannedBaseDelta,
		CacheLocalDir:   request.CacheLocalDir,
		CacheRepository: request.CacheRepository,
		CacheFrom:       request.CacheFrom,
		CacheTo:         request.CacheTo,
		NoCache:         request.NoCache,
		Network:         request.Network,
		AddHosts:        request.AddHosts,
		RunControls:     request.RunControls, Lifecycle: request.Lifecycle,
		ImageControls:           request.ImageControls,
		Timestamp:               request.Timestamp,
		SourceDateEpochOverride: request.SourceDateEpoch,
		CacheTTL:                request.CacheTTL,
		ProgressPrefix:          request.ProgressPrefix,
		ProgressReference:       request.ProgressReference,
		progressStageTotal:      definitionStageCount(request.Definition),
		Jobs:                    request.Jobs,
		LogRusage:               request.LogRusage,
		RusageLogFile:           request.RusageLogFile,
		RewriteTimestamp:        request.RewriteTimestamp,
		Allow:                   request.Allow,
		Secrets:                 request.Secrets,
		SSH:                     request.SSH,
		ProxyArgs:               request.ProxyArgs,
		SystemContext: &types.SystemContext{
			SignaturePolicyPath:  request.SignaturePolicyPath,
			BigFilesTemporaryDir: filepath.Dir(request.ResultPath),
			AuthFilePath:         request.AuthFile,
			DockerCertPath:       request.CertDir,
		},
		JobID: request.JobID,
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
		response.Error = buildErr.Error()
	}
	if err := writeWorkerJSON(request.ResultPath, response); err != nil {
		return err
	}
	return buildErr
}

func validatePlanWorkerRequest(request planWorkerRequest) error {
	switch request.Mode {
	case "build", "publish", "export", "verify", "import":
	default:
		return fmt.Errorf("unknown build worker mode %q", request.Mode)
	}
	if request.ResultPath == "" {
		return errors.New("build worker request lacks result path")
	}
	if request.Mode == "export" || request.Mode == "verify" {
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
	if request.Mode == "import" {
		if request.Plan != nil || request.Definition != nil || request.PlannerOptions != nil {
			return errors.New("image import worker request cannot contain build input")
		}
		if request.Output.Path == "" {
			return errors.New("image import worker request lacks OCI layout path")
		}
		if request.ManifestDescriptor.Digest == "" {
			return errors.New("image import worker request lacks selected manifest descriptor")
		}
		return validateOptionalManifestDescriptor(request.ManifestDescriptor)
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

func promoteCompleteOutput(source, destination string) error {
	// RENAME_NOREPLACE closes the race between the initial destination check
	// and a completed build. A concurrent creator keeps its output intact.
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
}

func cleanupWorkerJob(request planWorkerRequest, jobDir string, stderr io.Writer) error {
	request.Mode = "cleanup"
	request.Plan = nil
	request.Definition = nil
	request.PlannerOptions = nil
	request.ResultPath = ""
	path := filepath.Join(jobDir, "cleanup.json")
	if err := writeWorkerJSON(path, request); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	command := reexec.Command(planWorkerName, path)
	command.Stderr = stderr
	command.Env = append(withoutSigningPassword(gitWorkerEnvironment(os.Environ())), "TMPDIR="+jobDir)
	if err := runWorkerProcess(ctx, command, workerGrace); err != nil {
		return fmt.Errorf("cleanup interrupted build job %s (files retained at %s): %w", request.JobID, jobDir, err)
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
	return response, nil
}
