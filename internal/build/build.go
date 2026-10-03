// Package build runs local Coopr definitions through the embedded Buildah backend.
package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/buildcontext"
	"coopr/internal/componentstore"
	"coopr/internal/imagecatalog"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"coopr/internal/storeactivity"
	"coopr/internal/transfer"
	"github.com/containerd/platforms"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/define"
)

type Options struct {
	Lifecycle            buildah.LifecycleControls
	Output               buildah.FilesystemOutput
	Outputs              []buildah.FilesystemOutput
	Squash, SquashAll    bool
	SBOM                 []define.SBOMScanOptions
	Signing              transfer.SigningOptions
	ImageControls        buildah.ImageControls
	Timestamp            *int64
	SourceDateEpoch      *int64
	CacheTTL             *time.Duration
	AllPlatforms         bool
	Manifest             string
	IgnoreFile           string
	File, Context, From  string
	DefinitionInContext  bool
	Platform, Target     string
	Platforms            []string
	Format               string
	DisableCompression   bool
	ConfidentialWorkload define.ConfidentialWorkloadOptions
	Tag                  string
	Tags                 []string
	MetadataFile         string
	IIDFile              string
	Push                 bool
	Pull                 bool
	PullPolicy           string
	NoCache              bool
	Network              string
	AddHosts             []string
	RunControls          buildah.RunControls
	Jobs                 int
	RewriteTimestamp     bool
	Args                 map[string]string
	PlainHTTP            bool
	PlainHTTPRegistries  []string
	AuthFile             string
	CertDir              string
	SkipTLSVerify        bool
	Credentials          string
	Retry                uint
	RetrySet             bool
	RetryDelay           time.Duration
	DecryptionKeys       []string
	SignaturePolicyPath  string
	StoreDir             string
	Stdout, Stderr       io.Writer
	Stdin                io.Reader
	RunStdin             io.Reader
	Quiet                bool
	LogFile              string
	LogSplit             bool
	LogRusage            bool
	RusageLogFile        string
	BuildStore           buildah.StoreOptions
	CacheLocalDir        string
	CacheRepository      string
	CacheFrom            []buildah.CacheSpec
	CacheTo              []buildah.CacheSpec
	Secrets              []string
	SSH                  []string
	Allow                []string
	BuildContexts        []buildcontext.Spec
}

type platformBuild struct {
	selection imagecatalog.Selection
	variant   oci.ImageVariant
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// Run builds each requested platform, stores the result locally as a manifest
// or index, then applies the requested destination through coopr copy's path.
func Run(ctx context.Context, opts Options) (_ string, retErr error) {
	if opts.Squash && opts.SquashAll {
		return "", errors.New("--squash and --squash-all are mutually exclusive")
	}
	if opts.Jobs < 0 {
		return "", errors.New("jobs must be nonnegative")
	}
	if opts.AllPlatforms && (len(opts.Platforms) != 0 || opts.Platform != "") {
		return "", errors.New("all-platforms and platform are mutually exclusive")
	}
	var err error
	opts.LogFile, err = normalizeBuildLog(opts.LogFile, opts.LogSplit)
	if err != nil {
		return "", err
	}
	if opts.RusageLogFile != "" {
		opts.RusageLogFile, err = filepath.Abs(opts.RusageLogFile)
		if err != nil {
			return "", err
		}
	}
	if opts.Manifest != "" {
		var err error
		opts.Manifest, err = localstore.NormalizeImageTag(opts.Manifest)
		if err != nil {
			return "", fmt.Errorf("invalid manifest name: %w", err)
		}
	}
	if opts.Timestamp != nil && opts.SourceDateEpoch != nil {
		return "", errors.New("timestamp and source-date-epoch are mutually exclusive")
	}
	if opts.SourceDateEpoch != nil {
		opts.Args = maps.Clone(opts.Args)
		if opts.Args == nil {
			opts.Args = map[string]string{}
		}
		if _, exists := opts.Args["SOURCE_DATE_EPOCH"]; !exists {
			opts.Args["SOURCE_DATE_EPOCH"] = fmt.Sprint(*opts.SourceDateEpoch)
		}
	}
	targets, err := requestedPlatforms(opts.Platform, opts.Platforms)
	if err != nil {
		return "", err
	}
	if opts.Format != "" && opts.Format != "oci" && opts.Format != "docker" {
		return "", fmt.Errorf("unsupported image format %q: expected oci or docker", opts.Format)
	}
	opts.Network, opts.AddHosts, err = buildah.NormalizeBuildNetworkOptions(opts.Network, opts.AddHosts)
	if err != nil {
		return "", err
	}
	if opts.File == "" {
		return "", errors.New("definition is required")
	}
	if opts.File == "-" && opts.Context == "-" {
		return "", errors.New("definition and context cannot both use stdin")
	}
	destinations, err := outputDestinations(oci.Image, opts.Tag, opts.Tags, opts.Push)
	if err != nil {
		return "", err
	}
	if err := validateFinalization(&opts, targets, destinations); err != nil {
		return "", err
	}
	outputArtifacts, err := validateOutputArtifacts(destinations, opts.File, opts.MetadataFile, opts.IIDFile)
	if err != nil {
		return "", err
	}
	if opts.LogFile != "" {
		if opts.LogSplit {
			for _, target := range targets {
				outputArtifacts = append(outputArtifacts, platformLogPath(opts.LogFile, target))
			}
		} else {
			outputArtifacts = append(outputArtifacts, opts.LogFile)
		}
	}
	if opts.RusageLogFile != "" {
		outputArtifacts = append(outputArtifacts, opts.RusageLogFile)
	}
	finalArtifacts, err := finalizationArtifacts(opts, outputArtifacts)
	if err != nil {
		return "", err
	}
	outputArtifacts = append(outputArtifacts, finalArtifacts...)
	def, primary, definitionFile, cleanupPrimary, err := prepareDefinitionContext(ctx, opts.File, opts.Context, opts.DefinitionInContext, opts.Secrets, opts.SSH, opts.Stdin)
	if err != nil {
		return "", fmt.Errorf("%s: %w", definitionDisplayName(opts.File), err)
	}
	defer func() { _ = cleanupPrimary() }()
	opts.File, opts.Context = definitionFile, primary.Path
	applyFromOverride(def, opts.From)
	for _, inst := range def.Instructions {
		if inst.Name == "extend" {
			return "", fmt.Errorf("component definition %s requires coopr component build", definitionDisplayName(opts.File))
		}
	}
	for i, inst := range def.Instructions {
		if inst.Name == "package" {
			return "", fmt.Errorf("instruction %d %q requires a component definition with extend", i+1, inst.Name)
		}
	}
	opts.IgnoreFile, err = selectIgnoreFile(opts.Context, opts.IgnoreFile)
	if err != nil {
		return "", err
	}
	for i := range opts.SBOM {
		for _, named := range opts.BuildContexts {
			if named.Kind == buildcontext.Local && !slices.Contains(opts.SBOM[i].ContextDir, named.Path) {
				opts.SBOM[i].ContextDir = append(opts.SBOM[i].ContextDir, named.Path)
			}
		}
	}
	planning := planner.Options{
		Mode: planner.Build, Target: opts.Target, Arguments: opts.Args, Platform: opts.Platform,
		BuildContexts: opts.BuildContexts, BuildUnusedStages: opts.Lifecycle.BuildUnusedStages, ContextSourceDateEpoch: primary.SourceDateEpoch,
	}
	storeDir := opts.StoreDir
	if storeDir == "" {
		storeDir, err = localstore.DefaultImageDir()
		if err != nil {
			return "", err
		}
	}
	buildStore := opts.BuildStore
	if buildStore.RunRoot == "" && buildStore.GraphRoot == "" {
		buildStore, err = buildah.DefaultStoreOptions()
		if err != nil {
			return "", err
		}
		buildStore.GraphRoot = filepath.Join(storeDir, "graph")
	} else if opts.StoreDir == "" {
		storeDir = filepath.Dir(buildStore.GraphRoot)
	}
	buildStore, err = buildah.NormalizeStoreOptions(buildStore)
	if err != nil {
		return "", err
	}
	opts.BuildStore = buildStore
	componentStoreDir, err := componentstore.DefaultDir()
	if err != nil {
		return "", err
	}
	if opts.AllPlatforms {
		targets, err = discoverBuildPlatforms(ctx, def, planning, opts, storeDir, componentStoreDir)
		if err != nil {
			return "", err
		}
		if err := validateFinalization(&opts, targets, destinations); err != nil {
			return "", err
		}
	}
	fileArtifacts := outputArtifacts
	outputs := filesystemOutputs(opts.Output, opts.Outputs)
	for _, filesystem := range outputs {
		if filesystem.Type == "local" {
			fileArtifacts = slices.DeleteFunc(slices.Clone(fileArtifacts), func(path string) bool { return path == filesystem.Path })
		}
		if err := preflightFilesystemOutput(filesystem, opts.File, storeDir, componentStoreDir); err != nil {
			return "", err
		}
		if len(targets) > 1 {
			for _, target := range targets {
				if err := preflightFilesystemOutput(platformFilesystemOutput(filesystem, target, len(targets)), opts.File, storeDir, componentStoreDir); err != nil {
					return "", err
				}
			}
		}
	}
	if err := preflightOutputArtifacts(fileArtifacts, storeDir, componentStoreDir); err != nil {
		return "", err
	}
	activity, err := storeactivity.AcquireShared(ctx, storeDir, componentStoreDir)
	if err != nil {
		return "", fmt.Errorf("acquire build store activity lease: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	ctx = storeactivity.ContextWithLease(ctx, activity)
	signingArtifacts, err := opts.Signing.ContextArtifacts()
	if err != nil {
		return "", err
	}
	contextArtifacts := append([]string{storeDir}, signingArtifacts...)
	contextArtifacts = append(contextArtifacts, outputArtifacts...)
	stagingAnchor := opts.File
	if isHTTPDefinition(stagingAnchor) {
		stagingAnchor = filepath.Join(opts.Context, ".coopr-remote-definition")
	}
	layout, cleanup, err := stageLayout(stagingAnchor)
	if err != nil {
		return "", err
	}
	defer cleanup()
	contextArtifacts = append(contextArtifacts, filepath.Dir(layout))
	stdout, stderr := opts.Stdout, opts.Stderr
	var sharedLog *os.File
	if opts.LogFile != "" && !opts.LogSplit {
		sharedLog, err = openBuildLog(opts.LogFile)
		if err != nil {
			return "", err
		}
		defer func() { retErr = errors.Join(retErr, sharedLog.Close()) }()
	}
	stdout, stderr = buildWriters(stdout, stderr, opts.Quiet, sharedLog)
	stdoutTar := -1
	for i := range outputs {
		if outputs[i].Path == "-" {
			stdoutTar = i
			outputs[i].Path = filepath.Join(filepath.Dir(layout), "rootfs.tar")
		}
	}
	if stdoutTar >= 0 {
		stdout = stderr
	}
	if len(targets) > 1 {
		var outputMu sync.Mutex
		if stdout != nil {
			stdout = lockedWriter{mu: &outputMu, w: stdout}
		}
		if stderr != nil {
			stderr = lockedWriter{mu: &outputMu, w: stderr}
		}
	}
	platformJobs, stageJobs := buildJobLimits(opts.Jobs, len(targets))
	// Stock build outputs reuse a single path across platforms. Serialize those
	// writers so the last requested platform wins without corrupting shared files.
	for _, output := range outputs {
		if output.Type == "tar" {
			platformJobs, stageJobs = buildJobLimits(opts.Jobs, 1)
			break
		}
	}
	for _, scan := range opts.SBOM {
		if scan.SBOMOutput != "" || scan.PURLOutput != "" {
			platformJobs, stageJobs = buildJobLimits(opts.Jobs, 1)
			break
		}
	}
	builds, err := runPlatformBuildsWithLimit(ctx, targets, platformJobs, func(buildCtx context.Context, i int, targetPlatform string) (_ platformBuild, retErr error) {
		platformStdout, platformStderr := stdout, stderr
		if opts.LogSplit {
			log, openErr := openBuildLog(platformLogPath(opts.LogFile, targetPlatform))
			if openErr != nil {
				return platformBuild{}, openErr
			}
			defer func() { retErr = errors.Join(retErr, log.Close()) }()
			platformStdout, platformStderr = buildWriters(stdout, stderr, opts.Quiet, log)
		}
		platformPlanning := planning
		platformPlanning.Platform = targetPlatform
		output := layout
		if len(targets) > 1 {
			output = filepath.Join(filepath.Dir(layout), fmt.Sprintf("image-%d", i))
		}
		progressReference := ""
		if len(destinations) > 0 && destinations[0].Transport != "oci-archive" {
			progressReference = destinations[0].Name
		}
		progressPrefix := ""
		if len(targets) > 1 {
			progressPrefix = "[" + targetPlatform + "] "
		}
		result, buildErr := buildah.BuildDefinitionSupervised(buildCtx, def, platformPlanning, buildah.SupervisedPlanOptions{
			ProgressPrefix:    progressPrefix,
			ProgressReference: progressReference,
			Store:             buildStore, ContextDir: opts.Context, IgnoreFile: opts.IgnoreFile, ContextArtifacts: contextArtifacts,
			Output: buildah.Output{Path: output, Format: opts.Format, Squash: opts.Squash, SquashAll: opts.SquashAll, DisableCompression: opts.DisableCompression, ConfidentialWorkload: opts.ConfidentialWorkload, SBOM: opts.SBOM, Filesystems: platformFilesystemOutputs(outputs, targetPlatform, len(targets))}, ImageStoreDir: storeDir, ComponentStoreDir: componentStoreDir,
			PlainHTTP: opts.PlainHTTP, PlainHTTPRegistries: opts.PlainHTTPRegistries, Pull: opts.Pull, PullPolicy: opts.PullPolicy,
			NoCache:     opts.NoCache,
			Network:     opts.Network,
			AddHosts:    opts.AddHosts,
			RunControls: opts.RunControls, Lifecycle: opts.Lifecycle,
			ImageControls:    opts.ImageControls,
			Timestamp:        opts.Timestamp,
			SourceDateEpoch:  opts.SourceDateEpoch,
			CacheTTL:         opts.CacheTTL,
			Jobs:             stageJobs,
			LogRusage:        opts.LogRusage,
			RusageLogFile:    opts.RusageLogFile,
			RewriteTimestamp: opts.RewriteTimestamp,
			Allow:            opts.Allow,
			BuildContexts:    opts.BuildContexts,
			CacheLocalDir:    opts.CacheLocalDir,
			CacheRepository:  opts.CacheRepository,
			CacheFrom:        opts.CacheFrom,
			CacheTo:          opts.CacheTo,
			Secrets:          opts.Secrets, SSH: opts.SSH,
			AuthFile: opts.AuthFile, CertDir: opts.CertDir, SkipTLSVerify: opts.SkipTLSVerify,
			Credentials: opts.Credentials, Retry: opts.Retry, RetrySet: opts.RetrySet, RetryDelay: opts.RetryDelay, DecryptionKeys: opts.DecryptionKeys, SignaturePolicyPath: opts.SignaturePolicyPath,
			Stdin: opts.RunStdin, Stdout: platformStdout, Stderr: platformStderr,
		})
		if buildErr != nil {
			return platformBuild{}, fmt.Errorf("build %s for %s: %w", definitionDisplayName(opts.File), targetPlatform, buildErr)
		}
		if err := buildCtx.Err(); err != nil {
			return platformBuild{}, err
		}
		manifest, readErr := oci.LayoutRoot(result.Layout)
		if readErr != nil {
			return platformBuild{}, fmt.Errorf("read built OCI layout for %s: %w", targetPlatform, readErr)
		}
		rawConfig, readErr := oci.ReadImageConfigLayout(buildCtx, result.Layout)
		if readErr != nil {
			return platformBuild{}, fmt.Errorf("read built image configuration for %s: %w", targetPlatform, readErr)
		}
		platform, readErr := platforms.Parse(result.Platform)
		if readErr != nil {
			return platformBuild{}, fmt.Errorf("parse built image platform %q: %w", result.Platform, readErr)
		}
		platform = platforms.Normalize(platform)
		var configured v1.Image
		if err := json.Unmarshal(rawConfig, &configured); err != nil {
			return platformBuild{}, fmt.Errorf("decode built image platform metadata for %s: %w", targetPlatform, err)
		}
		platform.OSVersion = configured.OSVersion
		platform.OSFeatures = slices.Clone(configured.OSFeatures)
		return platformBuild{
			selection: imagecatalog.Selection{Root: manifest, Manifest: manifest, ImageID: result.ImageID, ConfigData: rawConfig},
			variant:   oci.ImageVariant{Layout: result.Layout, Manifest: manifest, Platform: platform},
		}, nil
	})
	if err != nil {
		return "", err
	}
	selections := make(map[string]imagecatalog.Selection, len(targets))
	variants := make([]oci.ImageVariant, 0, len(targets))
	for _, built := range builds {
		key := platforms.Format(built.variant.Platform)
		if _, duplicate := selections[key]; duplicate {
			return "", fmt.Errorf("build targets produced duplicate output platform %s", key)
		}
		selections[key] = built.selection
		variants = append(variants, built.variant)
	}
	root := variants[0].Manifest
	outputLayout := variants[0].Layout
	tag := ""
	if len(variants) == 1 {
		if err := imagecatalog.Commit(ctx, storeDir, tag, variants[0].Platform, selections[platforms.Format(variants[0].Platform)]); err != nil {
			return "", fmt.Errorf("catalog built image: %w", err)
		}
	} else {
		outputLayout = filepath.Join(filepath.Dir(layout), "index")
		indexData := []byte(nil)
		root, indexData, err = oci.AssembleImageIndex(ctx, outputLayout, variants, opts.Format)
		if err != nil {
			return "", fmt.Errorf("assemble multi-platform image: %w", err)
		}
		for key, selection := range selections {
			selection.Root = root
			selections[key] = selection
		}
		if err := imagecatalog.CommitIndex(ctx, storeDir, tag, root, indexData, selections); err != nil {
			return "", fmt.Errorf("catalog multi-platform image: %w", err)
		}
	}
	if opts.Manifest != "" {
		outputLayout = filepath.Join(filepath.Dir(layout), "manifest")
		root, _, selections, err = appendManifest(ctx, storeDir, opts.Manifest, outputLayout, opts.Format, buildStore, variants, selections)
		if err != nil {
			return "", fmt.Errorf("append image to manifest %s: %w", opts.Manifest, err)
		}
		variants = make([]oci.ImageVariant, 0, len(selections))
		for key, selection := range selections {
			platform, err := platforms.Parse(key)
			if err != nil {
				return "", err
			}
			variants = append(variants, oci.ImageVariant{Manifest: selection.Manifest, Platform: platform})
		}
		slices.SortFunc(variants, func(a, b oci.ImageVariant) int {
			return strings.Compare(platforms.Format(a.Platform), platforms.Format(b.Platform))
		})
	}
	if opts.Signing.SignBy != "" && slices.ContainsFunc(destinations, func(destination transfer.Destination) bool {
		return destination.Transport == "local"
	}) {
		// Platform workers take shared leases in separate processes. Upgrade
		// only after every worker has joined, before the signing transaction.
		if err := activity.Close(); err != nil {
			return "", err
		}
		activity, err = storeactivity.AcquireExclusive(ctx, storeDir, componentStoreDir)
		if err != nil {
			return "", fmt.Errorf("acquire signing store activity lease: %w", err)
		}
		ctx = storeactivity.ContextWithLease(ctx, activity)
	}
	report, publicationErr := applyOutputDestinations(ctx, oci.Image, outputLayout, root, destinations, transfer.Options{
		ImageStoreDir: storeDir, BuildStore: buildStore, PlainHTTP: opts.PlainHTTP, PlainHTTPRegistries: opts.PlainHTTPRegistries,
		AuthFile: opts.AuthFile, CertDir: opts.CertDir, SkipTLSVerify: opts.SkipTLSVerify,
		Credentials: opts.Credentials, Retry: opts.Retry, RetrySet: opts.RetrySet, RetryDelay: opts.RetryDelay, DecryptionKeys: opts.DecryptionKeys, SignaturePolicyPath: opts.SignaturePolicyPath, Signing: opts.Signing,
	})
	if opts.LogSplit {
		for _, targetPlatform := range targets {
			log, err := os.OpenFile(platformLogPath(opts.LogFile, targetPlatform), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				publicationErr = errors.Join(publicationErr, err)
				continue
			}
			logCompletedTags(log, destinations, report)
			publicationErr = errors.Join(publicationErr, log.Close())
		}
	} else {
		progressWriter := stderr
		if progressWriter == nil {
			progressWriter = os.Stderr
		}
		logCompletedTags(progressWriter, destinations, report)
	}
	if stdoutTar >= 0 {
		stream, err := os.Open(outputs[stdoutTar].Path)
		if err != nil {
			publicationErr = errors.Join(publicationErr, err)
		} else {
			writer := opts.Stdout
			if writer == nil {
				writer = os.Stdout
			}
			_, copyErr := io.Copy(writer, stream)
			publicationErr = errors.Join(publicationErr, copyErr, stream.Close())
		}
	}
	return finishOutputs(opts.MetadataFile, opts.IIDFile, root, variants, selections, report, publicationErr)
}

// runPlatformBuildsWithLimit bounds platform workers, preserves request order,
// and cancels and joins every started sibling on the first failure.
func runPlatformBuildsWithLimit(ctx context.Context, targets []string, limit int, build func(context.Context, int, string) (platformBuild, error)) ([]platformBuild, error) {
	if len(targets) == 1 {
		result, err := build(ctx, 0, targets[0])
		if err != nil {
			return nil, err
		}
		return []platformBuild{result}, nil
	}
	buildCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]platformBuild, len(targets))
	var wg sync.WaitGroup
	var mu sync.Mutex
	next := 0
	var firstErr error
	workers := min(len(targets), max(1, limit))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if firstErr != nil || next == len(targets) {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				result, err := build(buildCtx, i, targets[i])
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
					return
				}
				results[i] = result
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func requestedPlatforms(defaultPlatform string, values []string) ([]string, error) {
	if len(values) == 0 {
		if defaultPlatform == "" {
			defaultPlatform = runtime.GOOS + "/" + runtime.GOARCH
		}
		values = []string{defaultPlatform}
	}
	var result []string
	seen := make(map[string]bool)
	for _, value := range values {
		for _, entry := range strings.Split(value, ",") {
			entry = strings.TrimSpace(entry)
			platform, err := platforms.Parse(entry)
			if err != nil || platform.OS != "linux" || platform.Architecture == "" {
				return nil, fmt.Errorf("invalid build platform %q: expected linux/arch[/variant]", entry)
			}
			key := platforms.Format(platforms.Normalize(platform))
			if seen[key] {
				return nil, fmt.Errorf("duplicate build platform %s", key)
			}
			seen[key] = true
			result = append(result, key)
		}
	}
	return result, nil
}

func preparePrimaryContext(ctx context.Context, definitionFile, value string, secrets, ssh []string, stdin io.Reader) (buildah.PrimaryContext, func() error, error) {
	if value == "-" {
		if stdin == nil {
			stdin = os.Stdin
		}
		return buildah.MaterializeArchiveContext(ctx, stdin)
	}
	defaultContext := filepath.Dir(definitionFile)
	if isHTTPDefinition(definitionFile) {
		defaultContext = "."
	}
	spec, err := buildcontext.ParsePrimary(value, defaultContext)
	if err != nil {
		return buildah.PrimaryContext{}, nil, fmt.Errorf("build context: %w", err)
	}
	primary, cleanup, err := buildah.MaterializePrimaryContext(ctx, spec, secrets, ssh)
	if err != nil {
		return buildah.PrimaryContext{}, nil, fmt.Errorf("prepare build context: %w", err)
	}
	return primary, cleanup, nil
}

func stageLayout(output string) (string, func(), error) {
	outputAbs, err := canonicalParentPath(output)
	if err != nil {
		return "", nil, err
	}
	stageDir, tempErr := os.MkdirTemp("", ".coopr-stage-*")
	if tempErr != nil {
		stageParent := filepath.Dir(outputAbs)
		if err := os.MkdirAll(stageParent, 0755); err != nil {
			return "", nil, fmt.Errorf("create OCI layout staging directory: temp: %v; output: %w", tempErr, err)
		}
		stageDir, err = os.MkdirTemp(stageParent, ".coopr-stage-*")
		if err != nil {
			return "", nil, fmt.Errorf("create OCI layout staging directory: temp: %v; output: %w", tempErr, err)
		}
	}
	return filepath.Join(stageDir, "image"), func() { _ = os.RemoveAll(stageDir) }, nil
}

func sameDestination(left, right string) (bool, error) {
	leftPath, err := canonicalParentPath(left)
	if err != nil {
		return false, err
	}
	rightPath, err := canonicalParentPath(right)
	if err != nil {
		return false, err
	}
	if leftPath == rightPath {
		return true, nil
	}
	leftInfo, leftErr := os.Stat(left)
	if leftErr != nil && !errors.Is(leftErr, os.ErrNotExist) {
		return false, fmt.Errorf("inspect %q: %w", left, leftErr)
	}
	rightInfo, rightErr := os.Stat(right)
	if rightErr != nil && !errors.Is(rightErr, os.ErrNotExist) {
		return false, fmt.Errorf("inspect %q: %w", right, rightErr)
	}
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo), nil
}

// canonicalParentPath also handles an output that does not exist yet. Only
// existing path components can be symlinks; preserve the remaining suffix.
func canonicalParentPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(abs)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Join(append(parts, filepath.Base(abs))...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if parent == filepath.Dir(parent) {
			return "", err
		}
		suffix = append([]string{filepath.Base(parent)}, suffix...)
		parent = filepath.Dir(parent)
	}
}
