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
	buildahcli "go.podman.io/buildah/pkg/cli"
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
	var definitionFiles []string
	var network, pullPolicy string
	var osName, arch, variant string
	platform := runtime.GOOS + "/" + runtime.GOARCH
	var push bool
	var platforms []string
	var allow []string
	var addHosts []string
	var times buildTimeFlags
	var controls buildControlFlags
	var registry registryFlags
	var native buildahcli.BudResults
	nativeFlags := buildahcli.GetBudFlags(&native)
	cmd := &cobra.Command{
		Annotations: map[string]string{nativeStorageAnnotation: "true"},
		Use:         "build [file|context]", Short: "Build a component into the local OCI store",
		Long: "Build a selected component output into Coopr's local OCI store. Supply a definition file path or --file with a build context; no filename is selected automatically. Use --tag NAME to name the component in the local store. --push publishes the name supplied by --tag to a registry.",
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
			resolvedArgs, err := readBuildArgFiles(native.BuildArgFile, native.BuildArg)
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
			contexts, err := buildcontext.Parse(native.BuildContext)
			if err != nil {
				return err
			}
			cacheSources, err := parseCacheSpecs(native.CacheFrom, "src")
			if err != nil {
				return fmt.Errorf("cache-from: %w", err)
			}
			cacheDestinations, err := parseCacheSpecs(native.CacheTo, "dest")
			if err != nil {
				return fmt.Errorf("cache-to: %w", err)
			}
			if push && len(native.Tag) == 0 {
				return fmt.Errorf("--push requires --tag")
			}
			for _, tag := range native.Tag {
				if tag == "" {
					return fmt.Errorf("--tag requires a nonempty component name")
				}
				if _, err := transfer.ParseTag(tag, oci.Component); err != nil {
					return err
				}
			}
			requestedPlatform, err := buildPlatform(cmd, platform, osName, arch, variant)
			if err != nil {
				return err
			}
			var executionStdin interface{ Read([]byte) (int, error) }
			if native.Stdin {
				executionStdin = cmd.InOrStdin()
			}
			ref, err := build.BuildComponent(cmd.Context(), build.ComponentOptions{
				File: files[0], Files: files, Context: resolvedContext, DefinitionsInContext: definitionsInContext, SourcePolicyFile: native.SourcePolicyFile, TransientRunMounts: transientRunMounts, From: native.From, IgnoreFile: native.IgnoreFile, Tags: native.Tag, MetadataFile: native.MetadataFile, Push: push, PullPolicy: pullPolicy, NoCache: native.NoCache, Network: network, AddHosts: addHosts,
				Lifecycle: controls.lifecycle(), Quiet: native.Quiet, LogFile: native.Logfile, LogSplit: native.LogSplitByPlatform, LogRusage: native.LogRusage && !native.Quiet, RusageLogFile: native.RusageLogFile,
				BuildStore:  store,
				RunControls: runControls, Jobs: controls.Jobs,
				RewriteTimestamp: native.RewriteTimestamp,
				Timestamp:        timestamp, SourceDateEpoch: epoch, CacheTTL: ttl,
				Platform: requestedPlatform, Platforms: platforms, Target: native.Target, Args: resolvedArgs,
				Secrets: native.Secrets, SSH: native.SSH,
				Allow:         allow,
				BuildContexts: contexts,
				CacheFrom:     cacheSources, CacheTo: cacheDestinations,
				AuthFile: registry.authFileOption(cmd), CertDir: registry.certDir, TLSVerify: registry.tlsPolicy(cmd),
				Credentials: registry.credentials, Retry: registry.retry, RetrySet: cmd.Flags().Changed("retry"), RetryDelay: registry.retryDelay, DecryptionKeys: registry.decryptionKeys, SignaturePolicyPath: commandSignaturePolicy(cmd),
				Stdin: cmd.InOrStdin(), RunStdin: executionStdin, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}
			if native.Logfile != "" {
				return nil
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), ref)
			return err
		},
	}
	f := cmd.Flags()
	addNativeFlags(cmd, &nativeFlags, buildahcli.GetBudFlagsCompletions(), "source-policy-file", "metadata-file")
	f.Lookup("source-policy-file").Usage = "BuildKit-format policy file for base image sources"
	f.Lookup("metadata-file").Usage = "write component index and per-platform digests as JSON"
	addNativeFlags(cmd, &nativeFlags, buildahcli.GetBudFlagsCompletions(), "from", "ignorefile", "tag", "no-cache", "rewrite-timestamp", "target", "build-arg", "build-arg-file", "stdin", "quiet", "logfile", "logsplit", "log-rusage", "rusage-logfile", "build-context", "secret", "ssh", "cache-from", "cache-to")
	f.Lookup("from").Usage = "replace the image in the first FROM instruction"
	f.Lookup("ignorefile").Usage = "context ignore file (default: first of .cooprignore, .containerignore, .dockerignore at the context root)"
	f.Lookup("no-cache").Usage = "rebuild without reading cached results; save fresh results to the build cache"
	f.Lookup("target").Usage = "named component output to build"
	f.Lookup("build-arg").Usage = "build argument (repeatable NAME[=VALUE]; NAME inherits from the environment when set)"
	f.Lookup("build-arg-file").Usage = "read build arguments from a file (repeatable; --build-arg wins)"
	f.Lookup("build-context").Usage = "additional build context: NAME=PATH|URL|docker-image://REFERENCE|oci-layout://PATH:TAG (repeatable)"
	f.Lookup("cache-from").Usage = "read cached results from a repository or type=local,src=PATH (repeatable)"
	f.Lookup("cache-to").Usage = "write cached results to a repository or type=local,dest=PATH (repeatable)"
	f.StringArrayVarP(&definitionFiles, "file", "f", nil, "definition file path (repeatable, combined in order; required with a build context)")
	f.BoolVar(&push, "push", false, "publish the component to a registry (requires --tag)")
	addPullFlags(cmd, &pullPolicy)
	f.StringVar(&network, "network", "default", "RUN network mode: default, private, none, host, ns:PATH, pasta[:OPTIONS], or named network")
	f.StringSliceVar(&addHosts, "add-host", nil, "add HOST:IP to build containers (repeatable)")
	f.StringSliceVar(&platforms, "platform", nil, "target platform as os/arch (repeatable or comma-separated; default: host platform)")
	f.StringVar(&osName, "os", "", "target operating system")
	f.StringVar(&arch, "arch", "", "target architecture")
	f.StringVar(&variant, "variant", "", "target architecture variant")
	f.StringArrayVar(&allow, "allow", nil, "allow an elevated build entitlement (repeatable: network.host, security.insecure, device, or device=SELECTOR)")
	times.addTo(cmd)
	if standalone {
		addGlobalRunFlags(cmd.Flags())
	}
	addSignaturePolicyFlag(cmd.Flags())
	f.StringSliceVar(&registry.decryptionKeys, "decryption-key", nil, "key used to decrypt image inputs (repeatable)")
	controls.addTo(cmd)
	registry.addTo(cmd)
	return cmd
}
