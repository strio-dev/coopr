package main

import (
	"fmt"
	"runtime"

	"coopr/internal/build"
	"coopr/internal/buildah"
	"coopr/internal/buildcontext"
	"coopr/internal/oci"
	"coopr/internal/transfer"
	"github.com/spf13/cobra"
	"go.podman.io/buildah/define"
	buildahparse "go.podman.io/buildah/pkg/parse"
)

func newBuildCommand() *cobra.Command {
	return newBuildCommandWithGlobals(true)
}

func newBuildCommandWithGlobals(standalone bool) *cobra.Command {
	var ignoreFile string
	var contextDir, definitionFile, from, target, format, network, pullPolicy string
	var osName, arch, variant string
	var tags []string
	var metadataFile, iidFile string
	platform := runtime.GOOS + "/" + runtime.GOARCH
	var args []string
	var push, pull, noCache, plainHTTP, rewriteTimestamp bool
	var plainHTTPRegistries []string
	var platforms []string
	var caches []string
	var cacheFrom, cacheTo []string
	var secrets, ssh []string
	var allow []string
	var addHosts []string
	var buildContexts []string
	var buildArgFiles []string
	var output []string
	var squash, squashAll bool
	var allPlatforms bool
	var manifest string
	var runStdin bool
	var disableCompression bool
	var confidentialWorkload string
	var quiet, logSplit bool
	var logRusage bool
	var logFile string
	var rusageLogFile string
	var times buildTimeFlags
	var imageControls imageControlFlags
	var signing signingFlags
	var controls buildControlFlags
	var registry registryFlags
	cmd := &cobra.Command{
		Use: "build [file|context]", Short: "Build an image into Coopr's local image graph",
		Long: "Build into Coopr's local image graph with the embedded Buildah backend. Supply a definition file path or --file with a build context; no filename is selected automatically. Use --tag NAME for a local tag, or --tag podman:NAME, docker:NAME, registry:NAME, or oci-archive:PATH to copy the built image to that destination. --push publishes the name supplied by --tag to a registry. Coopr excludes its stores and temporary files from the local context and honors .cooprignore, .containerignore, and .dockerignore.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, paths []string) error {
			argument := ""
			if len(paths) != 0 {
				argument = paths[0]
			}
			file, resolvedContext, definitionInContext, err := resolveBuildInput(argument, definitionFile, contextDir)
			if err != nil {
				return err
			}
			store, catalogue, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			resolvedArgs, err := readBuildArgFiles(buildArgFiles, args)
			if err != nil {
				return err
			}
			var cw define.ConfidentialWorkloadOptions
			if cmd.Flags().Changed("cw") {
				cw, err = buildahparse.GetConfidentialWorkloadOptions(confidentialWorkload)
				if err != nil {
					return fmt.Errorf("parse --cw: %w", err)
				}
			}
			if _, err := oci.NormalizePullPolicy(pullPolicy, pull); err != nil {
				return err
			}
			imageOptions, err := imageControls.controls(cmd)
			if err != nil {
				return err
			}
			forceTimestamp, epoch, ttl, err := times.values(cmd)
			if err != nil {
				return err
			}
			filesystems := make([]buildah.FilesystemOutput, 0, len(output))
			for _, value := range output {
				filesystem, err := buildah.ParseFilesystemOutput(value)
				if err != nil {
					return err
				}
				filesystems = append(filesystems, filesystem)
			}
			scans, err := parseSBOMFlags(cmd)
			if err != nil {
				return err
			}
			runControls, err := controls.controls()
			if err != nil {
				return err
			}
			contexts, err := buildcontext.Parse(buildContexts)
			if err != nil {
				return err
			}
			cacheLocalDir, cacheRepository, err := parseBuildCaches(caches)
			if err != nil {
				return err
			}
			cacheSources, err := parseCacheSpecs(cacheFrom)
			if err != nil {
				return fmt.Errorf("cache-from: %w", err)
			}
			cacheDestinations, err := parseCacheSpecs(cacheTo)
			if err != nil {
				return fmt.Errorf("cache-to: %w", err)
			}
			if push && len(tags) == 0 {
				return fmt.Errorf("--push requires --tag")
			}
			for _, tag := range tags {
				if tag == "" {
					return fmt.Errorf("--tag requires a nonempty image name")
				}
				var destination transfer.Destination
				if push {
					destination, err = transfer.ParsePushDestination(tag, oci.Image)
				} else {
					destination, err = transfer.ParseDestination(tag, oci.Image)
				}
				if err != nil {
					return err
				}
				if err := prepareDestination(destination); err != nil {
					return err
				}
			}
			requestedPlatform, err := buildPlatform(cmd, platform, osName, arch, variant)
			if err != nil {
				return err
			}
			if allPlatforms {
				if cmd.Flags().Changed("os") || cmd.Flags().Changed("arch") || cmd.Flags().Changed("variant") {
					return fmt.Errorf("--all-platforms cannot be used with --os, --arch, or --variant")
				}
				requestedPlatform = ""
			}
			var executionStdin interface{ Read([]byte) (int, error) }
			if runStdin {
				executionStdin = cmd.InOrStdin()
			}
			result, err := build.Run(cmd.Context(), build.Options{
				File: file, Context: resolvedContext, DefinitionInContext: definitionInContext, From: from, IgnoreFile: ignoreFile,
				Lifecycle:          controls.lifecycle(),
				DisableCompression: disableCompression, ConfidentialWorkload: cw,
				Quiet: quiet, LogFile: logFile, LogSplit: logSplit, LogRusage: logRusage && !quiet, RusageLogFile: rusageLogFile,
				BuildStore: store, StoreDir: catalogue,
				Outputs: filesystems, Squash: squash, SquashAll: squashAll, SBOM: scans, Signing: signing.options(),
				ImageControls: imageOptions, Timestamp: forceTimestamp, SourceDateEpoch: epoch, CacheTTL: ttl,
				AllPlatforms: allPlatforms, Manifest: manifest,
				Tags: tags, MetadataFile: metadataFile, IIDFile: iidFile,
				Push:             push,
				Pull:             pull,
				PullPolicy:       pullPolicy,
				NoCache:          noCache,
				Network:          network,
				AddHosts:         addHosts,
				RunControls:      runControls,
				Jobs:             controls.jobs,
				RewriteTimestamp: rewriteTimestamp,
				Platform:         requestedPlatform, Platforms: platforms, Target: target, Format: format, Args: resolvedArgs,
				PlainHTTP: plainHTTP, PlainHTTPRegistries: plainHTTPRegistries,
				AuthFile: registry.authFile, CertDir: registry.certDir, SkipTLSVerify: !registry.tlsVerify,
				Credentials: registry.credentials, Retry: registry.retry, RetrySet: cmd.Flags().Changed("retry"), RetryDelay: registry.retryDelay, DecryptionKeys: registry.decryptionKeys, SignaturePolicyPath: commandSignaturePolicy(cmd),
				CacheLocalDir: cacheLocalDir, CacheRepository: cacheRepository,
				CacheFrom: cacheSources, CacheTo: cacheDestinations,
				Secrets: secrets, SSH: ssh,
				Allow:         allow,
				BuildContexts: contexts,
				Stdin:         cmd.InOrStdin(), RunStdin: executionStdin, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}
			resultWriter := cmd.OutOrStdout()
			for _, filesystem := range filesystems {
				if filesystem.Path == "-" {
					resultWriter = cmd.ErrOrStderr()
					break
				}
			}
			_, err = fmt.Fprintln(resultWriter, result)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVarP(&definitionFile, "file", "f", "", "definition file path (required when supplying a build context)")
	f.StringVar(&from, "from", "", "replace the image in the first FROM instruction")
	f.BoolVarP(&disableCompression, "disable-compression", "D", true, "do not compress newly-created image layers")
	f.StringVar(&confidentialWorkload, "cw", "", "confidential workload options")
	f.BoolVarP(&quiet, "quiet", "q", false, "suppress build progress")
	f.StringVar(&logFile, "logfile", "", "write build output to a file")
	f.BoolVar(&logSplit, "logsplit", false, "split logfile output by target platform")
	f.BoolVar(&logRusage, "log-rusage", false, "log resource usage between build instructions")
	f.StringVar(&rusageLogFile, "rusage-logfile", "", "write resource usage logs to a file")
	_ = f.MarkHidden("log-rusage")
	_ = f.MarkHidden("rusage-logfile")
	f.StringArrayVarP(&output, "output", "o", nil, "export flattened filesystem (repeatable): PATH, type=local,dest=PATH, or type=tar,dest=FILE (- for stdout)")
	f.BoolVar(&squash, "squash", false, "combine new image layers, preserving base layers")
	f.BoolVar(&squashAll, "squash-all", false, "combine all image layers, including the base")
	cmd.MarkFlagsMutuallyExclusive("squash", "squash-all")
	f.BoolVar(&allPlatforms, "all-platforms", false, "build all runnable Linux platforms common to the base images")
	f.StringVar(&manifest, "manifest", "", "append built platforms to a local named image index")
	imageControls.addTo(cmd)
	addSBOMFlags(cmd)
	signing.addTo(cmd)
	f.StringVar(&metadataFile, "metadata-file", "", "write result, index, platform, and image configuration digests as JSON")
	f.StringVar(&iidFile, "iidfile", "", "write the image ID (index digest for multiple platforms)")
	f.StringVar(&ignoreFile, "ignorefile", "", "context ignore file (default: first of .cooprignore, .containerignore, .dockerignore at the context root)")
	f.StringVar(&contextDir, "context", "", "build context: local directory, local tar, stdin (-), Git URL, or HTTP(S) tar archive (default: definition directory)")
	f.StringArrayVarP(&tags, "tag", "t", nil, "name or copy the image; unprefixed names use Coopr's local store; prefixes: podman:, docker:, registry:, oci-archive:")
	f.BoolVar(&push, "push", false, "publish the image to the registry named by --tag")
	addPullFlags(cmd, &pullPolicy, &pull)
	f.BoolVar(&noCache, "no-cache", false, "rebuild without reading cached results; save fresh results to the build cache")
	f.StringVar(&network, "network", "default", "RUN network mode: default, private, none, host, ns:PATH, pasta[:OPTIONS], or named network")
	f.StringArrayVar(&addHosts, "add-host", nil, "add HOST:IP to build containers (repeatable)")
	f.BoolVar(&rewriteTimestamp, "rewrite-timestamp", false, "clamp layer timestamps newer than SOURCE_DATE_EPOCH")
	f.StringArrayVar(&platforms, "platform", nil, "target platform as os/arch (repeatable or comma-separated; default: host platform)")
	f.StringVar(&osName, "os", "", "target operating system")
	f.StringVar(&arch, "arch", "", "target architecture")
	f.StringVar(&variant, "variant", "", "target architecture variant")
	cmd.MarkFlagsMutuallyExclusive("all-platforms", "platform")
	f.StringVar(&target, "target", "", "named output stage")
	f.StringVar(&format, "format", defaultBuildFormat(), "image format: oci (default) or docker")
	f.StringArrayVar(&args, "build-arg", nil, "build argument (repeatable NAME[=VALUE]; NAME inherits from the environment when set)")
	f.StringArrayVar(&buildArgFiles, "build-arg-file", nil, "read build arguments from a file (repeatable; --build-arg wins)")
	f.BoolVar(&runStdin, "stdin", false, "pass stdin to RUN instructions")
	f.StringArrayVar(&buildContexts, "build-context", nil, "additional build context: NAME=PATH|URL|docker-image://REFERENCE|oci-layout://PATH:TAG (repeatable)")
	f.BoolVar(&plainHTTP, "plain-http", false, "allow Coopr HTTP transport for loopback OCI registries")
	f.StringArrayVar(&plainHTTPRegistries, "plain-http-registry", nil, "allow Coopr HTTP transport for an exact registry host[:port] (repeatable)")
	f.StringArrayVar(&caches, "cache", nil, "optional portable instruction and component cache: oci-layout:PATH or registry:HOST/REPOSITORY (repeatable)")
	f.StringArrayVar(&cacheFrom, "cache-from", nil, "read cached results from oci-layout:PATH or registry:HOST/REPOSITORY (repeatable)")
	f.StringArrayVar(&cacheTo, "cache-to", nil, "write cached results to oci-layout:PATH or registry:HOST/REPOSITORY (repeatable)")
	f.StringArrayVar(&secrets, "secret", nil, "secret source for RUN mounts: id=ID[,src=PATH|env=NAME] (repeatable)")
	f.StringArrayVar(&ssh, "ssh", nil, "SSH agent or key source for RUN mounts: ID[=PATH] (repeatable)")
	f.StringArrayVar(&allow, "allow", nil, "allow an elevated build entitlement (repeatable: network.host, security.insecure, device, or device=SELECTOR)")
	times.addTo(cmd)
	if standalone {
		addGlobalRunFlags(cmd.Flags())
		addSignaturePolicyFlag(cmd.Flags())
	}
	controls.addTo(cmd)
	registry.addTo(cmd)
	return cmd
}

func parseBuildCaches(values []string) (localDir, repository string, err error) {
	specs, err := parseCacheSpecs(values)
	if err != nil {
		return "", "", err
	}
	for _, spec := range specs {
		switch spec.Transport {
		case "oci-layout":
			if localDir != "" {
				return "", "", fmt.Errorf("only one oci-layout cache is supported")
			}
			localDir = spec.Reference
		case "registry":
			if repository != "" {
				return "", "", fmt.Errorf("only one registry cache is supported")
			}
			repository = spec.Reference
		}
	}
	return localDir, repository, nil
}
