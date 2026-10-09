package main

import (
	"fmt"
	"runtime"

	"coopr/internal/build"
	"coopr/internal/buildah"
	"coopr/internal/buildcontext"
	"coopr/internal/oci"
	"github.com/spf13/cobra"
)

func newComponentCommandWithGlobals(standalone bool) *cobra.Command {
	cmd := &cobra.Command{
		Use: "component", Short: "Build and manage reusable OCI components",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newComponentBuildCommandWithGlobals(standalone),
		newCopyCommand(oci.Component),
		newComponentListCommand("ls"),
		newComponentInspectCommand(),
		newComponentRemoveCommand(),
		newComponentPullCommand(), newComponentPushCommand(), newComponentTagCommand(),
		newComponentSaveCommand(), newComponentLoadCommand(), newComponentExistsCommand(),
	)
	return cmd
}

func newComponentBuildCommand() *cobra.Command {
	return newComponentBuildCommandWithGlobals(true)
}

func newComponentBuildCommandWithGlobals(standalone bool) *cobra.Command {
	var ignoreFile string
	var definitionFiles []string
	var sourcePolicyFile string
	var from, target, network, pullPolicy string
	var osName, arch, variant string
	var tags []string
	var metadataFile string
	platform := runtime.GOOS + "/" + runtime.GOARCH
	var args []string
	var push, pull, noCache, rewriteTimestamp bool
	var platforms []string
	var secrets, ssh []string
	var allow []string
	var addHosts []string
	var buildContexts []string
	var buildArgFiles []string
	var cacheFrom, cacheTo []string
	var times buildTimeFlags
	var controls buildControlFlags
	var registry registryFlags
	var runStdin bool
	var quiet, logSplit bool
	var logRusage bool
	var logFile string
	var rusageLogFile string
	cmd := &cobra.Command{
		Annotations: map[string]string{nativeStorageAnnotation: "true"},
		Use:         "build [file|context]", Short: "Build a component into the local OCI store",
		Long: "Build a selected component output into Coopr's local OCI store. Supply a definition file path or --file with a build context; no filename is selected automatically. Use --tag NAME for a local tag or --tag registry:NAME or oci-archive:PATH to copy it. --push publishes the name supplied by --tag to a registry.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, paths []string) error {
			argument := ""
			if len(paths) != 0 {
				argument = paths[0]
			}
			files, resolvedContext, definitionsInContext, err := resolveBuildInputs(argument, definitionFiles)
			if err != nil {
				return err
			}
			store, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			resolvedArgs, err := readBuildArgFiles(buildArgFiles, args)
			if err != nil {
				return err
			}
			pullPolicy, err = resolveBuildPullPolicy(cmd)
			if err != nil {
				return err
			}
			timestamp, epoch, ttl, err := times.values(cmd)
			if err != nil {
				return err
			}
			runControls, err := controls.controls()
			if err != nil {
				return err
			}
			transientRunMounts, err := buildah.ParseTransientRunMounts(controls.mounts)
			if err != nil {
				return err
			}
			contexts, err := buildcontext.Parse(buildContexts)
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
					return fmt.Errorf("--tag requires a nonempty component name")
				}
			}
			requestedPlatform, err := buildPlatform(cmd, platform, osName, arch, variant)
			if err != nil {
				return err
			}
			var executionStdin interface{ Read([]byte) (int, error) }
			if runStdin {
				executionStdin = cmd.InOrStdin()
			}
			ref, err := build.BuildComponent(cmd.Context(), build.ComponentOptions{
				File: files[0], Files: files, Context: resolvedContext, DefinitionsInContext: definitionsInContext, SourcePolicyFile: sourcePolicyFile, TransientRunMounts: transientRunMounts, From: from, IgnoreFile: ignoreFile, Tags: tags, MetadataFile: metadataFile, Push: push, Pull: pull, PullPolicy: pullPolicy, NoCache: noCache, Network: network, AddHosts: addHosts,
				Lifecycle: controls.lifecycle(), Quiet: quiet, LogFile: logFile, LogSplit: logSplit, LogRusage: logRusage && !quiet, RusageLogFile: rusageLogFile,
				BuildStore:  store,
				RunControls: runControls, Jobs: controls.jobs,
				RewriteTimestamp: rewriteTimestamp,
				Timestamp:        timestamp, SourceDateEpoch: epoch, CacheTTL: ttl,
				Platform: requestedPlatform, Platforms: platforms, Target: target, Args: resolvedArgs,
				Secrets: secrets, SSH: ssh,
				Allow:         allow,
				BuildContexts: contexts,
				CacheFrom:     cacheSources, CacheTo: cacheDestinations,
				AuthFile: registry.authFile, CertDir: registry.certDir, TLSVerify: registry.tlsPolicy(cmd),
				Credentials: registry.credentials, Retry: registry.retry, RetrySet: cmd.Flags().Changed("retry"), RetryDelay: registry.retryDelay, DecryptionKeys: registry.decryptionKeys, SignaturePolicyPath: commandSignaturePolicy(cmd),
				Stdin: cmd.InOrStdin(), RunStdin: executionStdin, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), ref)
			return err
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&definitionFiles, "file", "f", nil, "definition file path (repeatable, combined in order; required with a build context)")
	f.StringVar(&sourcePolicyFile, "source-policy-file", "", "BuildKit-format policy file for base image sources")
	f.StringVar(&from, "from", "", "replace the image in the first FROM instruction")
	f.StringVar(&metadataFile, "metadata-file", "", "write component index and per-platform digests as JSON")
	f.StringVar(&ignoreFile, "ignorefile", "", "context ignore file (default: first of .cooprignore, .containerignore, .dockerignore at the context root)")
	f.StringArrayVarP(&tags, "tag", "t", nil, "name or copy the component; unprefixed names use the local store; prefixes: registry:, oci-archive:")
	f.BoolVar(&push, "push", false, "publish the component to a registry (requires --tag)")
	addPullFlags(cmd, &pullPolicy, &pull)
	f.BoolVar(&noCache, "no-cache", false, "rebuild without reading cached results; save fresh results to the build cache")
	f.StringVar(&network, "network", "default", "RUN network mode: default, private, none, host, ns:PATH, pasta[:OPTIONS], or named network")
	f.StringArrayVar(&addHosts, "add-host", nil, "add HOST:IP to build containers (repeatable)")
	f.BoolVar(&rewriteTimestamp, "rewrite-timestamp", false, "clamp layer timestamps newer than SOURCE_DATE_EPOCH")
	f.StringArrayVar(&platforms, "platform", nil, "target platform as os/arch (repeatable or comma-separated; default: host platform)")
	f.StringVar(&osName, "os", "", "target operating system")
	f.StringVar(&arch, "arch", "", "target architecture")
	f.StringVar(&variant, "variant", "", "target architecture variant")
	f.StringVar(&target, "target", "", "named component output to build")
	f.StringArrayVar(&args, "build-arg", nil, "build argument (repeatable NAME[=VALUE]; NAME inherits from the environment when set)")
	f.StringArrayVar(&buildArgFiles, "build-arg-file", nil, "read build arguments from a file (repeatable; --build-arg wins)")
	f.BoolVar(&runStdin, "stdin", false, "pass stdin to RUN instructions")
	f.BoolVarP(&quiet, "quiet", "q", false, "suppress build progress")
	f.StringVar(&logFile, "logfile", "", "write build output to a file")
	f.BoolVar(&logSplit, "logsplit", false, "split logfile output by target platform")
	f.BoolVar(&logRusage, "log-rusage", false, "log resource usage between build instructions")
	f.StringVar(&rusageLogFile, "rusage-logfile", "", "write resource usage logs to a file")
	_ = f.MarkHidden("log-rusage")
	_ = f.MarkHidden("rusage-logfile")
	f.StringArrayVar(&buildContexts, "build-context", nil, "additional build context: NAME=PATH|URL|docker-image://REFERENCE|oci-layout://PATH:TAG (repeatable)")
	f.StringArrayVar(&secrets, "secret", nil, "secret source for RUN mounts: id=ID[,src=PATH|env=NAME] (repeatable)")
	f.StringArrayVar(&ssh, "ssh", nil, "SSH agent or key source for RUN mounts: ID[=PATH] (repeatable)")
	f.StringArrayVar(&allow, "allow", nil, "allow an elevated build entitlement (repeatable: network.host, security.insecure, device, or device=SELECTOR)")
	f.StringArrayVar(&cacheFrom, "cache-from", nil, "read cached results from oci-layout:PATH or registry:HOST/REPOSITORY (repeatable)")
	f.StringArrayVar(&cacheTo, "cache-to", nil, "write cached results to oci-layout:PATH or registry:HOST/REPOSITORY (repeatable)")
	times.addTo(cmd)
	if standalone {
		addGlobalRunFlags(cmd.Flags())
	}
	addSignaturePolicyFlag(cmd.Flags())
	f.StringArrayVar(&registry.decryptionKeys, "decryption-key", nil, "key used to decrypt image inputs (repeatable)")
	controls.addTo(cmd)
	registry.addTo(cmd)
	return cmd
}
