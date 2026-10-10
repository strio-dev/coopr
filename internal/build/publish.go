package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/buildcontext"
	"coopr/internal/componentstore"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"coopr/internal/storeactivity"
	"coopr/internal/transfer"
	"github.com/containerd/platforms"
	orasoci "oras.land/oras-go/v2/content/oci"
)

type ComponentOptions struct {
	Lifecycle                      buildah.LifecycleControls
	Tags                           []string
	MetadataFile                   string
	IgnoreFile                     string
	File, Context, Tag, From       string
	Files                          []string
	DefinitionsInContext           []bool
	SourcePolicyFile               string
	TransientRunMounts             []buildah.RunMount
	DefinitionInContext            bool
	Platform, Target               string
	Platforms                      []string
	Args                           map[string]string
	Push, Pull, NoCache            bool
	PullPolicy                     string
	Network                        string
	AddHosts                       []string
	RunControls                    buildah.RunControls
	Jobs                           int
	RewriteTimestamp               bool
	Timestamp, SourceDateEpoch     *int64
	CacheTTL                       *time.Duration
	AuthFile, CertDir              string
	TLSVerify                      *bool
	Credentials                    string
	Retry                          uint
	RetrySet                       bool
	RetryDelay                     time.Duration
	DecryptionKeys                 []string
	SignaturePolicyPath            string
	StoreDir                       string
	Stdout, Stderr                 io.Writer
	Stdin                          io.Reader
	RunStdin                       io.Reader
	Quiet                          bool
	LogFile                        string
	LogSplit                       bool
	LogRusage                      bool
	RusageLogFile                  string
	BuildStore                     buildah.StoreOptions
	Secrets, SSH                   []string
	Allow                          []string
	BuildContexts                  []buildcontext.Spec
	CacheLocalDir, CacheRepository string
	CacheFrom, CacheTo             []buildah.CacheSpec
}

// BuildComponent builds the selected package graph once and installs the
// exact resulting OCI artifact in the local component store, an OCI archive,
// or a registry.
func BuildComponent(ctx context.Context, opts ComponentOptions) (_ string, retErr error) {
	if opts.Jobs < 0 {
		return "", errors.New("jobs must be nonnegative")
	}
	if opts.Timestamp != nil && opts.SourceDateEpoch != nil {
		return "", errors.New("timestamp and source-date-epoch are mutually exclusive")
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
	if len(opts.Files) != 0 {
		opts.File = opts.Files[0]
	}
	if opts.File == "" {
		return "", errors.New("component definition is required")
	}
	files, contained := definitionFiles(opts.File, opts.Files, opts.DefinitionInContext, opts.DefinitionsInContext)
	stdinFiles := 0
	for _, file := range files {
		if file == "-" {
			stdinFiles++
		}
	}
	if stdinFiles > 1 {
		return "", errors.New("definition stdin can only be read once")
	}
	if stdinFiles != 0 && opts.Context == "-" {
		return "", errors.New("definition and context cannot both use stdin")
	}
	opts.Network, opts.AddHosts, err = buildah.NormalizeBuildNetworkOptions(opts.Network, opts.AddHosts)
	if err != nil {
		return "", err
	}
	destinations, err := outputDestinations(oci.Component, opts.Tag, opts.Tags, opts.Push)
	if err != nil {
		return "", err
	}
	outputArtifacts, err := validateOutputArtifacts(destinations, opts.File, opts.MetadataFile, "")
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
	if err := validateArtifactOverlaps(outputArtifacts, []string{opts.File}); err != nil {
		return "", err
	}
	if opts.StoreDir == "" {
		var err error
		opts.StoreDir, err = componentstore.DefaultDir()
		if err != nil {
			return "", err
		}
	}
	def, primary, definitionFile, cleanupPrimary, err := prepareDefinitionsContext(ctx, files, opts.Context, contained, opts.Secrets, opts.SSH, opts.Stdin)
	if err != nil {
		return "", fmt.Errorf("%s: %w", definitionDisplayName(opts.File), err)
	}
	defer func() { _ = cleanupPrimary() }()
	opts.File, opts.Context = definitionFile, primary.Path
	resolvedFiles := make([]string, 0, len(files))
	for i, file := range files {
		if i < len(contained) && contained[i] {
			file, err = contextDefinitionPath(primary.Path, file)
			if err != nil {
				return "", err
			}
		}
		resolvedFiles = append(resolvedFiles, file)
	}
	if err := validateArtifactOverlaps(outputArtifacts, resolvedFiles); err != nil {
		return "", err
	}
	applyFromOverride(def, opts.From)
	opts.IgnoreFile, err = selectIgnoreFile(opts.Context, opts.IgnoreFile)
	if err != nil {
		return "", err
	}
	planning := planner.Options{
		Mode: planner.Publish, Target: opts.Target, Arguments: opts.Args, Platform: opts.Platform,
		BuildContexts: opts.BuildContexts, BuildUnusedStages: opts.Lifecycle.BuildUnusedStages, ContextSourceDateEpoch: primary.SourceDateEpoch,
	}
	stagingAnchor := opts.File
	if isHTTPDefinition(stagingAnchor) {
		stagingAnchor = filepath.Join(opts.Context, ".coopr-remote-definition")
	}
	layout, cleanup, err := stageLayout(stagingAnchor)
	if err != nil {
		return "", err
	}
	defer cleanup()
	artifacts := append([]string{filepath.Dir(layout)}, outputArtifacts...)
	if opts.StoreDir != "" {
		artifacts = append(artifacts, opts.StoreDir)
	}
	buildStore := opts.BuildStore
	if buildStore.RunRoot == "" && buildStore.GraphRoot == "" {
		buildStore, err = buildah.DefaultStoreOptions()
		if err != nil {
			return "", err
		}
	}
	buildStore, err = buildah.NormalizeStoreOptions(buildStore)
	if err != nil {
		return "", err
	}
	storeRoots := buildah.ActivityRoots(buildStore, opts.StoreDir)
	if err := preflightOutputArtifacts(outputArtifacts, storeRoots...); err != nil {
		return "", err
	}
	activity, err := storeactivity.AcquireShared(ctx, storeRoots...)
	if err != nil {
		return "", fmt.Errorf("acquire component build store activity lease: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	artifacts = append(artifacts, storeRoots...)
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
			output = filepath.Join(filepath.Dir(layout), fmt.Sprintf("component-%d", i))
		}
		result, buildErr := buildah.PublishDefinitionSupervised(buildCtx, def, platformPlanning, buildah.SupervisedPlanOptions{
			Store: buildStore, ContextDir: opts.Context, IgnoreFile: opts.IgnoreFile, ContextArtifacts: artifacts,
			Output: buildah.Output{Path: output}, ComponentStoreDir: opts.StoreDir,
			Pull: opts.Pull, PullPolicy: opts.PullPolicy, SourcePolicyFile: opts.SourcePolicyFile, TransientRunMounts: opts.TransientRunMounts,
			NoCache:     opts.NoCache,
			Network:     opts.Network,
			AddHosts:    opts.AddHosts,
			RunControls: opts.RunControls, Lifecycle: opts.Lifecycle,
			Jobs:             stageJobs,
			LogRusage:        opts.LogRusage,
			RusageLogFile:    opts.RusageLogFile,
			RewriteTimestamp: opts.RewriteTimestamp,
			Timestamp:        opts.Timestamp, SourceDateEpoch: opts.SourceDateEpoch, CacheTTL: opts.CacheTTL,
			Allow:         opts.Allow,
			BuildContexts: opts.BuildContexts,
			Secrets:       opts.Secrets, SSH: opts.SSH,
			AuthFile: opts.AuthFile, CertDir: opts.CertDir, TLSVerify: opts.TLSVerify,
			Credentials: opts.Credentials, Retry: opts.Retry, RetrySet: opts.RetrySet, RetryDelay: opts.RetryDelay, DecryptionKeys: opts.DecryptionKeys, SignaturePolicyPath: opts.SignaturePolicyPath,
			CacheLocalDir: opts.CacheLocalDir, CacheRepository: opts.CacheRepository,
			CacheFrom: opts.CacheFrom, CacheTo: opts.CacheTo,
			Stdin: opts.RunStdin, Stdout: platformStdout, Stderr: platformStderr,
		})
		if buildErr != nil {
			return platformBuild{}, fmt.Errorf("component definition %s for %s: %w", definitionDisplayName(opts.File), targetPlatform, buildErr)
		}
		platform, parseErr := platforms.Parse(targetPlatform)
		if parseErr != nil {
			return platformBuild{}, parseErr
		}
		return platformBuild{variant: oci.IndexVariant{Layout: result.Layout, Manifest: result.Root, Platform: platform}}, nil
	})
	if err != nil {
		return "", err
	}
	root := builds[0].variant.Manifest
	outputLayout := builds[0].variant.Layout
	if len(builds) > 1 {
		variants := make([]oci.IndexVariant, len(builds))
		for i, built := range builds {
			variants[i] = built.variant
		}
		outputLayout = filepath.Join(filepath.Dir(layout), "component-index")
		root, _, err = oci.AssembleComponentIndex(ctx, outputLayout, variants)
		if err != nil {
			return "", fmt.Errorf("assemble multi-platform component: %w", err)
		}
	}
	source, err := orasoci.NewWithContext(ctx, outputLayout)
	if err != nil {
		return "", err
	}
	if err := componentstore.Put(ctx, opts.StoreDir, source, root, ""); err != nil {
		return "", fmt.Errorf("store local component: %w", err)
	}
	report, publicationErr := applyOutputDestinations(ctx, oci.Component, opts.StoreDir, root, destinations, transfer.Options{
		ComponentStoreDir: opts.StoreDir, AuthFile: opts.AuthFile, CertDir: opts.CertDir, TLSVerify: opts.TLSVerify,
		Credentials: opts.Credentials, Retry: opts.Retry, RetrySet: opts.RetrySet, RetryDelay: opts.RetryDelay, DecryptionKeys: opts.DecryptionKeys, SignaturePolicyPath: opts.SignaturePolicyPath,
	})
	variants := make([]oci.IndexVariant, len(builds))
	for i := range builds {
		variants[i] = builds[i].variant
	}
	if err := finishOutputs(opts.MetadataFile, "", root, variants, nil, report, publicationErr, "", ""); err != nil {
		return "", err
	}
	if opts.LogSplit && !opts.Quiet {
		for _, platform := range targets {
			log, err := os.OpenFile(platformLogPath(opts.LogFile, platform), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return "", err
			}
			warnUnusedBuildArguments(log, def, opts.Args)
			if err := log.Close(); err != nil {
				return "", err
			}
		}
	} else {
		warnUnusedBuildArguments(stdout, def, opts.Args)
	}
	ref := strings.Join(report.References, "\n")
	if err := logBuildResult(opts.LogFile, opts.LogSplit, targets, ref); err != nil {
		return "", err
	}
	return ref, nil
}

type PublishOptions struct {
	File, Context, Reference string
	Platform, Target         string
	Args                     map[string]string
	TLSVerify                *bool
	Pull                     bool
	NoCache                  bool
	BuildContexts            []buildcontext.Spec
}

// PublishComponent is kept for internal callers while the command surface
// migrates to BuildComponent.
func PublishComponent(ctx context.Context, opts PublishOptions) (string, error) {
	return BuildComponent(ctx, ComponentOptions{
		File: opts.File, Context: opts.Context, Tag: opts.Reference, Push: true, Pull: opts.Pull, NoCache: opts.NoCache,
		Platform: opts.Platform, Target: opts.Target,
		Args: opts.Args, TLSVerify: opts.TLSVerify, BuildContexts: opts.BuildContexts,
	})
}
