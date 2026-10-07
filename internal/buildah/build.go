// Package buildah executes Coopr build operations directly through Buildah's
// library API. It does not render or parse a Containerfile.
package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/pkg/parse"
	buildahutil "go.podman.io/buildah/util"
	nettypes "go.podman.io/common/libnetwork/types"
	commonconfig "go.podman.io/common/pkg/config"
	imagecopy "go.podman.io/image/v5/copy"
	ocilayout "go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// StoreOptions identifies the effective native containers/storage settings.
// Explicit selections supply both roots; defaults come from storage.conf.
type StoreOptions struct {
	RunRoot            string
	GraphRoot          string
	ImageStore         string
	GraphDriverName    string
	GraphDriverOptions []string
	TransientStore     bool
	// Native preserves effective containers/storage settings which do not have
	// dedicated Coopr CLI overrides.
	Native storage.StoreOptions
}

// Request is one ordered, single-stage build.
type Request struct {
	Lifecycle        LifecycleControls
	Store            StoreOptions
	Base             string
	ContextDir       string
	ContextArtifacts []string
	Isolation        string
	Runtime          string
	Operations       []Operation
	Secrets          []string
	SSH              []string
	Allow            []string
	AddHosts         []string
	RunControls      RunControls
	ImageControls    ImageControls
	Output           Output
	Timestamp        *int64
	SourceDateEpoch  *int64
	RewriteTimestamp bool
	CacheTTL         *time.Duration
}

// Output selects an image-layout destination. Format is "oci" by default or
// "docker" for Docker schema-2 manifest and config media types. Reference is
// the optional org.opencontainers.image.ref.name annotation in the index.
type Output struct {
	Path                 string
	Reference            string
	Format               string
	Squash               bool
	SquashAll            bool
	DisableCompression   bool
	ConfidentialWorkload define.ConfidentialWorkloadOptions
	SBOM                 []define.SBOMScanOptions
	Filesystem           FilesystemOutput
	Filesystems          []FilesystemOutput
}

const (
	outputFormatOCI    = "oci"
	outputFormatDocker = "docker"
)

func normalizedOutputFormat(format string) (string, error) {
	switch format {
	case "", outputFormatOCI:
		return outputFormatOCI, nil
	case outputFormatDocker:
		return outputFormatDocker, nil
	default:
		return "", fmt.Errorf("unsupported image format %q (want oci or docker)", format)
	}
}

func outputManifestType(format string) (string, error) {
	normalized, err := normalizedOutputFormat(format)
	if err != nil {
		return "", err
	}
	if normalized == outputFormatDocker {
		return define.Dockerv2ImageManifest, nil
	}
	return define.OCIv1ImageManifest, nil
}

func keepEmptyFilesystemLayer(manifestType string) bool {
	return manifestType != define.Dockerv2ImageManifest
}

// Result identifies the committed image.
type Result struct {
	ImageID        string
	ManifestDigest string
	Layout         string
	Reference      string
	Platform       string
	CacheStats     CacheStats
}

// Build applies Operations in order and commits the result to an image layout.
func Build(ctx context.Context, request Request) (result Result, retErr error) {
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	if err := validateRunControls(request.RunControls); err != nil {
		return Result{}, err
	}
	if err := validateRunControlHost(request.RunControls); err != nil {
		return Result{}, err
	}
	if err := ConfigureRuntimeConfig(request.RunControls); err != nil {
		return Result{}, err
	}
	if err := authorizeOperations(request.Operations, request.Allow); err != nil {
		return Result{}, err
	}
	requestedIsolation := request.Isolation
	if request.RunControls.Isolation != "" {
		requestedIsolation = request.RunControls.Isolation
	}
	isolation, err := parse.IsolationOption(requestedIsolation)
	if err != nil {
		return Result{}, fmt.Errorf("select Buildah isolation: %w", err)
	}
	for index, operation := range request.Operations {
		var network string
		switch run := operation.(type) {
		case Run:
			network = run.Network
		case *Run:
			network = run.Network
		default:
			continue
		}
		if err := validateRunNetworkIsolation(network, isolation); err != nil {
			return Result{}, fmt.Errorf("operation %d: %w", index+1, err)
		}
	}
	capabilities := rootCapabilities()
	storeLease, err := acquireStore(request.Store)
	if err != nil {
		return Result{}, fmt.Errorf("open isolated containers/storage: %w", err)
	}
	defer func() {
		if closeErr := storeLease.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close containers/storage: %w", closeErr))
		}
	}()
	store := storeLease.store
	policy := timestampPolicy{timestamp: request.Timestamp, sourceDateEpoch: request.SourceDateEpoch, rewriteTimestamp: request.RewriteTimestamp}

	network, err := newNetworkInterfaceWithControls(store, request.RunControls)
	if err != nil {
		return Result{}, err
	}
	base := request.Base
	if base == "" {
		base = "scratch"
	}
	addHosts, err := normalizeAddHosts(request.AddHosts)
	if err != nil {
		return Result{}, err
	}
	builderOptions, err := newBuilderOptionsWithHosts(base, isolation, capabilities, network, os.TempDir(), request.Output.Format, addHosts, request.RunControls)
	if err != nil {
		return Result{}, err
	}
	builder, err := upstream.NewBuilder(ctx, store, builderOptions)
	if err != nil {
		return Result{}, fmt.Errorf("create Buildah builder from %q: %w", base, err)
	}
	defer func() {
		if builder != nil && request.Lifecycle.removeBuilder(retErr != nil, ctx.Err() != nil) {
			retErr = errors.Join(retErr, builder.Delete())
		}
	}()
	if err := prepareBuilderImageControls(builder, request.ImageControls); err != nil {
		return Result{}, err
	}
	for index, operation := range request.Operations {
		if run, ok := operation.(Run); ok {
			run.cacheLockRoot = store.GraphRoot()
			request.Operations[index] = run
		}
	}

	artifacts := append(slices.Clone(request.ContextArtifacts), request.Store.RunRoot, request.Store.GraphRoot, request.Store.ImageStore, request.Output.Path)
	linked := false
	for _, operation := range request.Operations {
		if linkedCopyOrAdd(operation) {
			linked = true
			break
		}
	}
	if !linked {
		adapter := nativeBuilder{Builder: builder, isolation: builderOptions.Isolation, runtime: effectiveRuntime(request.Runtime, request.RunControls), runtimeArgs: slices.Clone(request.RunControls.RuntimeFlags), secretSpecs: request.Secrets, sshSpecs: request.SSH, cgroupManager: request.RunControls.CgroupManager, cgroupManagerSet: request.RunControls.CgroupManagerSet, compatVolumes: request.Lifecycle.CompatVolumes, runControls: request.RunControls}
		if err := applyOperationsWithArtifacts(ctx, adapter, request.ContextDir, artifacts, request.Operations); err != nil {
			return Result{}, err
		}
		if err := applyBuilderOutputControls(builder, request.ImageControls, policy); err != nil {
			return Result{}, err
		}
		return commitOutput(ctx, store, builder, request.Output, builderOptions.SystemContext, false, policy, request.ImageControls)
	}
	uncommitted := false
	for index, operation := range request.Operations {
		if linkedCopyOrAdd(operation) && uncommitted {
			if err := applyBuilderCheckpointControls(builder, request.ImageControls, policy); err != nil {
				return Result{}, err
			}
			replacement, _, checkpointErr := checkpoint(ctx, store, builder, builderOptions, false, policy)
			if checkpointErr != nil {
				return Result{}, fmt.Errorf("checkpoint before linked operation %d: %w", index+1, checkpointErr)
			}
			builder = replacement
		}
		adapter := nativeBuilder{Builder: builder, isolation: builderOptions.Isolation, runtime: effectiveRuntime(request.Runtime, request.RunControls), runtimeArgs: slices.Clone(request.RunControls.RuntimeFlags), secretSpecs: request.Secrets, sshSpecs: request.SSH, cgroupManager: request.RunControls.CgroupManager, cgroupManagerSet: request.RunControls.CgroupManagerSet, compatVolumes: request.Lifecycle.CompatVolumes, runControls: request.RunControls}
		if err := applyOperationsWithArtifacts(ctx, adapter, request.ContextDir, artifacts, []Operation{operation}); err != nil {
			return Result{}, fmt.Errorf("operation %d: %w", index+1, err)
		}
		uncommitted = true
		if linkedCopyOrAdd(operation) && index < len(request.Operations)-1 {
			if err := applyBuilderCheckpointControls(builder, request.ImageControls, policy); err != nil {
				return Result{}, err
			}
			replacement, _, checkpointErr := checkpoint(ctx, store, builder, builderOptions, true, policy)
			if checkpointErr != nil {
				return Result{}, fmt.Errorf("checkpoint linked operation %d: %w", index+1, checkpointErr)
			}
			builder = replacement
			uncommitted = false
		}
	}
	if err := applyBuilderOutputControls(builder, request.ImageControls, policy); err != nil {
		return Result{}, err
	}
	return commitOutput(ctx, store, builder, request.Output, builderOptions.SystemContext, linkedCopyOrAdd(request.Operations[len(request.Operations)-1]), policy, request.ImageControls)
}

func newBuilderOptions(base string, isolation define.Isolation, capabilities []string, network nettypes.ContainerNetwork, temporaryDir, outputFormat string) (upstream.BuilderOptions, error) {
	return newBuilderOptionsWithHosts(base, isolation, capabilities, network, temporaryDir, outputFormat, nil, RunControls{})
}

func newBuilderOptionsWithHosts(base string, isolation define.Isolation, capabilities []string, network nettypes.ContainerNetwork, temporaryDir, outputFormat string, addHosts []string, controls RunControls) (upstream.BuilderOptions, error) {
	if err := validateRunControls(controls); err != nil {
		return upstream.BuilderOptions{}, err
	}
	manifestType, err := outputManifestType(outputFormat)
	if err != nil {
		return upstream.BuilderOptions{}, err
	}
	options := upstream.BuilderOptions{
		FromImage: base,
		// Base resolution is deliberately local-only. The caller imports a
		// verified digest into native storage before executing the graph.
		PullPolicy:       define.PullNever,
		Isolation:        isolation,
		Capabilities:     capabilities,
		Format:           manifestType,
		NetworkInterface: network,
		CommonBuildOpts:  controls.commonBuildOptions(addHosts),
		SystemContext: &types.SystemContext{
			BigFilesTemporaryDir: temporaryDir,
		},
	}
	if err := controls.applyBuilderOptions(&options); err != nil {
		return upstream.BuilderOptions{}, err
	}
	return options, nil
}

func commitOutput(ctx context.Context, store storage.Store, builder *upstream.Builder, output Output, systemContext *types.SystemContext, linked bool, policy timestampPolicy, controls ImageControls) (Result, error) {
	manifestType, err := outputManifestType(output.Format)
	if err != nil {
		return Result{}, err
	}
	if manifestType == define.Dockerv2ImageManifest {
		options := upstream.CommitOptions{
			PreferredManifestType: manifestType,
			EmptyLayerIfEmptyDiff: keepEmptyFilesystemLayer(manifestType),
			OmitLayerHistoryEntry: linked,
			OmitHistory:           controls.OmitHistory,
			SystemContext:         systemContext,
		}
		applyFinalCommitOptions(&options, output)
		policy.apply(&options)
		imageID, _, manifestDigest, err := builder.Commit(ctx, nil, options)
		if err != nil {
			return Result{}, fmt.Errorf("commit Docker image to containers/storage: %w", err)
		}
		var result Result
		if output.DisableCompression {
			result, err = exportStoredImageVariantRaw(ctx, store, imageID, output, systemContext, &manifestDigest)
		} else {
			result, err = copyStoredOutputCompressed(ctx, store, imageID, output, systemContext, nil)
		}
		if err == nil {
			manifestDigest, parseErr := digest.Parse(result.ManifestDigest)
			if parseErr != nil {
				err = parseErr
			} else {
				err = retainStoredLayoutLayers(store, imageID, result.Layout, manifestDigest)
			}
		}
		if err == nil {
			err = oci.ConfigureLayoutCreatedAnnotation(ctx, result.Layout, policy.createdEpoch(), controls.CreatedAnnotation)
		}
		return result, err
	}
	destination, err := ocilayout.NewReference(output.Path, output.Reference)
	if err != nil {
		return Result{}, fmt.Errorf("create OCI layout reference: %w", err)
	}
	options := upstream.CommitOptions{
		PreferredManifestType: manifestType,
		EmptyLayerIfEmptyDiff: keepEmptyFilesystemLayer(manifestType),
		OmitLayerHistoryEntry: linked,
		OmitHistory:           controls.OmitHistory,
		SystemContext:         systemContext,
	}
	applyFinalCommitOptions(&options, output)
	policy.apply(&options)
	imageID, _, manifestDigest, err := builder.Commit(ctx, destination, options)
	if err != nil {
		return Result{}, fmt.Errorf("commit image layout: %w", err)
	}
	if err := retainStoredLayoutLayers(store, imageID, output.Path, manifestDigest); err != nil {
		return Result{}, err
	}
	if err := oci.ConfigureLayoutCreatedAnnotation(ctx, output.Path, policy.createdEpoch(), controls.CreatedAnnotation); err != nil {
		return Result{}, fmt.Errorf("set image created annotation: %w", err)
	}
	return Result{
		ImageID: imageID, ManifestDigest: manifestDigest.String(),
		Layout: output.Path, Reference: output.Reference,
	}, nil
}

func commitStoredSnapshotSelected(ctx context.Context, builder *upstream.Builder, systemContext *types.SystemContext, manifestType string, linked bool, policy timestampPolicy) (string, digest.Digest, error) {
	options := upstream.CommitOptions{
		PreferredManifestType: manifestType,
		EmptyLayerIfEmptyDiff: keepEmptyFilesystemLayer(manifestType),
		OmitLayerHistoryEntry: linked,
		SystemContext:         systemContext,
	}
	policy.apply(&options)
	imageID, _, manifestDigest, err := builder.Commit(ctx, nil, options)
	if err != nil {
		return "", "", fmt.Errorf("commit cached storage image: %w", err)
	}
	return imageID, manifestDigest, nil
}

// copyStoredOutputSelected exports a committed immutable image directly from
// containers/storage. A cache hit has no pending builder diff, so committing a
// clean replacement builder would synthesize an extra empty layer.
func copyStoredOutputSelected(ctx context.Context, store storage.Store, imageID string, output Output, systemContext *types.SystemContext, selected *digest.Digest) (Result, error) {
	if !output.DisableCompression {
		return copyStoredOutputCompressed(ctx, store, imageID, output, systemContext, selected)
	}
	source, err := imagestorage.Transport.NewStoreReference(store, nil, imageID)
	if err != nil {
		return Result{}, fmt.Errorf("create cached storage reference: %w", err)
	}
	imageSource, err := source.NewImageSource(ctx, systemContext)
	if err != nil {
		return Result{}, fmt.Errorf("open cached storage image: %w", err)
	}
	_, mediaType, manifestErr := imageSource.GetManifest(ctx, selected)
	closeErr := imageSource.Close()
	if err := errors.Join(manifestErr, closeErr); err != nil {
		return Result{}, fmt.Errorf("inspect cached storage image: %w", err)
	}
	if mediaType != define.Dockerv2ImageManifest && mediaType != v1.MediaTypeImageManifest {
		return Result{}, fmt.Errorf("cached storage image has unsupported manifest type %q", mediaType)
	}
	return exportStoredImageVariantRaw(ctx, store, imageID, output, systemContext, selected)
}

func copyStoredOutputCompressed(ctx context.Context, store storage.Store, imageID string, output Output, systemContext *types.SystemContext, selected *digest.Digest) (_ Result, retErr error) {
	baseSource, err := imagestorage.Transport.NewStoreReference(store, nil, imageID)
	if err != nil {
		return Result{}, fmt.Errorf("create compressed-output source: %w", err)
	}
	var source types.ImageReference = baseSource
	if selected != nil {
		source = selectedStorageReference{ImageReference: baseSource, manifest: *selected}
	}
	destination, err := ocilayout.NewReference(output.Path, output.Reference)
	if err != nil {
		return Result{}, fmt.Errorf("create compressed-output destination: %w", err)
	}
	policyContext, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}, Transports: map[string]signature.PolicyTransportScopes{}})
	if err != nil {
		return Result{}, err
	}
	defer func() { retErr = errors.Join(retErr, policyContext.Destroy()) }()
	system := &types.SystemContext{}
	if systemContext != nil {
		*system = *systemContext
	}
	system.OCIAcceptUncompressedLayers = false
	manifest, err := imagecopy.Image(ctx, policyContext, destination, source, &imagecopy.Options{
		SourceCtx: system, DestinationCtx: system, ImageListSelection: imagecopy.CopySystemImage,
	})
	if err != nil {
		return Result{}, fmt.Errorf("write compressed OCI output: %w", err)
	}
	root, err := oci.LayoutRoot(output.Path)
	if err != nil {
		return Result{}, err
	}
	if digest.FromBytes(manifest) != root.Digest {
		return Result{}, fmt.Errorf("compressed output manifest digest differs from layout root %s", root.Digest)
	}
	format, err := normalizedOutputFormat(output.Format)
	if err != nil {
		return Result{}, err
	}
	if format == outputFormatDocker {
		root, err = convertLayoutToDockerSchema2(ctx, output.Path)
		if err != nil {
			return Result{}, err
		}
	}
	return Result{ImageID: imageID, ManifestDigest: root.Digest.String(), Layout: output.Path, Reference: output.Reference}, nil
}

func storedImageMatchesOutputFormat(ctx context.Context, store storage.Store, imageID, outputFormat string, systemContext *types.SystemContext, selected *digest.Digest) (bool, error) {
	desired, err := outputManifestType(outputFormat)
	if err != nil {
		return false, err
	}
	reference, err := imagestorage.Transport.NewStoreReference(store, nil, imageID)
	if err != nil {
		return false, err
	}
	source, err := reference.NewImageSource(ctx, systemContext)
	if err != nil {
		return false, err
	}
	_, mediaType, manifestErr := source.GetManifest(ctx, selected)
	closeErr := source.Close()
	if err := errors.Join(manifestErr, closeErr); err != nil {
		return false, err
	}
	return mediaType == desired, nil
}

func storedImageDefaultManifestDigest(store storage.Store, imageID string) (digest.Digest, error) {
	manifest, err := store.ImageBigData(imageID, storage.ImageDigestBigDataKey)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(manifest), nil
}

func optionalDigest(value digest.Digest) *digest.Digest {
	if value == "" {
		return nil
	}
	selected := value
	return &selected
}

func validateRequest(request Request) error {
	if err := validateStoreOptions(request.Store); err != nil {
		return err
	}
	if _, err := normalizedOutputFormat(request.Output.Format); err != nil {
		return err
	}
	if err := validateFinalizationOutput(request.Output); err != nil {
		return err
	}
	if err := request.ImageControls.Validate(); err != nil {
		return err
	}
	if err := validateCacheTTL(request.CacheTTL); err != nil {
		return err
	}
	if err := validateTimestampOptions(request.Timestamp, request.SourceDateEpoch, request.RewriteTimestamp); err != nil {
		return err
	}
	if request.Output.Path == "" {
		return errors.New("OCI layout output is required")
	}
	if !filepath.IsAbs(request.Output.Path) {
		return fmt.Errorf("OCI layout output must be absolute: %q", request.Output.Path)
	}
	if request.ContextDir != "" {
		if !filepath.IsAbs(request.ContextDir) {
			return fmt.Errorf("build context must be absolute: %q", request.ContextDir)
		}
		info, err := os.Stat(request.ContextDir)
		if err != nil {
			return fmt.Errorf("inspect build context: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("build context is not a directory: %s", request.ContextDir)
		}
	}
	return nil
}

func validateStoreOptions(options StoreOptions) error {
	for _, item := range []struct{ name, path string }{
		{"containers/storage run root", options.RunRoot},
		{"containers/storage graph root", options.GraphRoot},
	} {
		name, path := item.name, item.path
		if path == "" {
			return fmt.Errorf("%s is required", name)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be absolute: %q", name, path)
		}
	}
	if options.RunRoot == options.GraphRoot {
		return errors.New("containers/storage run root and graph root must differ")
	}
	if options.ImageStore != "" {
		if !filepath.IsAbs(options.ImageStore) {
			return fmt.Errorf("containers/storage image store must be absolute: %q", options.ImageStore)
		}
		if options.ImageStore == options.GraphRoot {
			return errors.New("containers/storage image store and graph root must differ")
		}
	}
	runRoot, err := canonicalStorePath(options.RunRoot)
	if err != nil {
		return fmt.Errorf("resolve containers/storage run root: %w", err)
	}
	graphRoot, err := canonicalStorePath(options.GraphRoot)
	if err != nil {
		return fmt.Errorf("resolve containers/storage graph root: %w", err)
	}
	if pathsOverlap(runRoot, graphRoot) {
		return fmt.Errorf("containers/storage run root %q and graph root %q overlap", runRoot, graphRoot)
	}
	return nil
}

func rootCapabilities() []string {
	return slices.Clone(commonconfig.DefaultCapabilities)
}

type nativeBuilder struct {
	*upstream.Builder
	isolation        define.Isolation
	runtime          string
	runtimeArgs      []string
	secretSpecs      []string
	sshSpecs         []string
	cgroupManager    string
	cgroupManagerSet bool
	compatVolumes    bool
	runControls      RunControls
}

func (b nativeBuilder) runHostFileControls() (noHostname, noHosts bool) {
	if b.CommonBuildOpts == nil {
		return false, false
	}
	return b.CommonBuildOpts.NoHostname, b.CommonBuildOpts.NoHosts
}

func (b nativeBuilder) addSourceSecret(id string) ([]byte, bool, error) {
	secrets, err := parseOperationSecrets(b.secretSpecs)
	if err != nil {
		return nil, false, err
	}
	secret, found := secrets[id]
	if !found {
		return nil, false, nil
	}
	value, err := secret.ResolveValue()
	if err != nil {
		return nil, false, fmt.Errorf("resolve ADD secret %q: %w", id, err)
	}
	return value, true, nil
}

func (b nativeBuilder) run(command []string, options upstream.RunOptions) error {
	if b.compatVolumes {
		options.CompatBuiltinVolumes = types.OptionalBoolTrue
	}
	cgroupManager := b.cgroupManager
	if !b.cgroupManagerSet {
		var err error
		cgroupManager, _, err = runCgroupManager()
		if err != nil {
			return fmt.Errorf("load container runtime configuration: %w", err)
		}
	}
	options.CgroupManager = cgroupManager
	options.Isolation = b.isolation
	if options.Isolation == define.IsolationDefault {
		options.Isolation = define.IsolationOCI
	}
	options.Terminal = upstream.WithoutTerminal
	if len(options.Args) == 1 && options.Args[0] == insecureRunRequestedMarker {
		if options.Isolation != define.IsolationOCI && options.Isolation != define.IsolationOCIRootless {
			return fmt.Errorf("RUN security=insecure requires OCI or rootless isolation, got %s", options.Isolation)
		}
		realRuntime, err := resolveInsecureRuntime(b.runtime)
		if err != nil {
			return err
		}
		options.Runtime = insecureRuntimeExecutable
		options.Args = nativeRuntimeArgs(options.Args, b.runtimeArgs, realRuntime)
		processLabel, mountLabel := b.ProcessLabel, b.MountLabel
		b.ProcessLabel, b.MountLabel = "", ""
		defer func() {
			b.ProcessLabel, b.MountLabel = processLabel, mountLabel
		}()
	} else {
		options.Runtime = b.runtime
		options.Args = nativeRuntimeArgs(options.Args, b.runtimeArgs, "")
	}
	return b.runWithSlirpNetwork(command, options)
}

func nativeRuntimeArgs(generated, flags []string, insecureRuntime string) []string {
	if insecureRuntime != "" {
		return append([]string{insecureRuntimeMarker, insecureRuntime}, flags...)
	}
	return append(slices.Clone(flags), generated...)
}

func effectiveRuntime(fallback string, controls RunControls) string {
	if controls.Runtime != "" {
		return controls.Runtime
	}
	return fallback
}

func resolveInsecureRuntime(runtime string) (string, error) {
	if runtime == "" {
		runtime = buildahutil.Runtime()
	}
	if local := buildahutil.FindLocalRuntime(runtime); local != "" {
		runtime = local
	}
	resolved, err := exec.LookPath(runtime)
	if err != nil {
		return "", fmt.Errorf("resolve OCI runtime for RUN security=insecure: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve OCI runtime for RUN security=insecure: %w", err)
	}
	if resolved == insecureRuntimeExecutable {
		return "", fmt.Errorf("resolve OCI runtime for RUN security=insecure: invalid runtime %q", resolved)
	}
	return resolved, nil
}

func (b nativeBuilder) add(destination string, extract bool, options upstream.AddAndCopyOptions, sources ...string) error {
	return b.Add(destination, extract, options, sources...)
}

func (b nativeBuilder) ensureContainerPathIsDirectory(containerPath, user string) (retErr error) {
	mountPoint, err := b.Mount(b.MountLabel)
	if err != nil {
		return err
	}
	defer func() {
		if err := b.Unmount(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("unmount after checking destination: %w", err))
		}
	}()
	stats, err := copier.Stat(mountPoint, filepath.Join(mountPoint, b.WorkDir()), copier.StatOptions{}, []string{containerPath})
	if err != nil {
		return fmt.Errorf("check container destination %q: %w", containerPath, err)
	}
	if len(stats) == 1 && len(stats[0].Globbed) == 1 {
		result := stats[0].Results[stats[0].Globbed[0]]
		if result.IsDir {
			return nil
		}
		return fmt.Errorf("container destination %q already exists but is not a directory", containerPath)
	}
	if err := b.EnsureContainerPathAs(containerPath, user, nil); err != nil {
		return fmt.Errorf("create container destination directory %q: %w", containerPath, err)
	}
	return nil
}
