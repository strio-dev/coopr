package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/strio-dev/coopr/internal/build"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/buildcontext"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/transfer"
	"go.podman.io/buildah/define"
	buildahcli "go.podman.io/buildah/pkg/cli"
	buildahparse "go.podman.io/buildah/pkg/parse"
)

func newBuildCommand() *cobra.Command {
	return newBuildCommandWithGlobals(true)
}

func newBuildCommandWithGlobals(standalone bool) *cobra.Command {
	var definitionFiles []string
	var network, pullPolicy string
	var osName, arch, variant string
	var blobDirectory string
	platform := runtime.GOOS + "/" + runtime.GOARCH
	var push bool
	var platforms []string
	var allow []string
	var addHosts []string
	var squashAll bool
	var times buildTimeFlags
	var imageControls imageControlFlags
	var signing signingFlags
	var controls buildControlFlags
	var registry registryFlags
	var native buildahcli.BudResults
	nativeFlags := buildahcli.GetBudFlags(&native)
	cmd := &cobra.Command{
		Annotations: map[string]string{nativeStorageAnnotation: "true"},
		Use:         "build [file|context]", Short: "Build an image into native container storage",
		Long: "Build into native container storage with the embedded Buildah backend. Supply a definition file path or --file with a build context; no filename is selected automatically. Use --tag NAME to name the built image in native container storage. --push publishes the name supplied by --tag to a registry. Coopr excludes its stores and temporary files from the local context and honors .cooprignore, .containerignore, and .dockerignore.",
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
			var cw define.ConfidentialWorkloadOptions
			if cmd.Flags().Changed("cw") {
				cw, err = buildahparse.GetConfidentialWorkloadOptions(native.CWOptions)
				if err != nil {
					return fmt.Errorf("parse --cw: %w", err)
				}
			}
			pullPolicy, err = resolveBuildPullPolicy(cmd)
			if err != nil {
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
			filesystems := make([]buildah.FilesystemOutput, 0, len(native.BuildOutputs))
			for _, value := range native.BuildOutputs {
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
					return fmt.Errorf("--tag requires a nonempty image name")
				}
				_, err = transfer.ParseTag(tag, oci.Image)
				if err != nil {
					return err
				}
			}
			requestedPlatform, err := buildPlatform(cmd, platform, osName, arch, variant)
			if err != nil {
				return err
			}
			if native.AllPlatforms {
				if cmd.Flags().Changed("os") || cmd.Flags().Changed("arch") || cmd.Flags().Changed("variant") {
					return fmt.Errorf("--all-platforms cannot be used with --os, --arch, or --variant")
				}
				requestedPlatform = ""
			}
			var executionStdin interface{ Read([]byte) (int, error) }
			if native.Stdin {
				executionStdin = cmd.InOrStdin()
			}
			result, err := build.Run(cmd.Context(), build.Options{
				File: files[0], Files: files, Context: resolvedContext, DefinitionsInContext: definitionsInContext, SourcePolicyFile: native.SourcePolicyFile, TransientRunMounts: transientRunMounts, From: native.From, IgnoreFile: native.IgnoreFile,
				Lifecycle:          controls.lifecycle(),
				DisableCompression: native.DisableCompression, BlobDirectory: blobDirectory, CompressionFormat: native.CompressionFormat, CompressionLevel: compressionLevelValue(cmd, native.CompressionLevel), ForceCompression: forceCompressionValue(cmd, native.ForceCompressionFormat), ConfidentialWorkload: cw,
				Quiet: native.Quiet, LogFile: native.Logfile, LogSplit: native.LogSplitByPlatform, LogRusage: native.LogRusage && !native.Quiet, RusageLogFile: native.RusageLogFile,
				BuildStore: store,
				Outputs:    filesystems, Squash: native.Squash, SquashAll: squashAll, SBOM: scans, Signing: signing.options(),
				ImageControls: imageOptions, Timestamp: forceTimestamp, SourceDateEpoch: epoch, CacheTTL: ttl,
				AllPlatforms: native.AllPlatforms, Manifest: native.Manifest,
				Tags: native.Tag, MetadataFile: native.MetadataFile, IIDFile: native.Iidfile, IIDFileRaw: native.IidfileRaw,
				Push:             push,
				PullPolicy:       pullPolicy,
				NoCache:          native.NoCache,
				Network:          network,
				AddHosts:         addHosts,
				RunControls:      runControls,
				Jobs:             controls.Jobs,
				RewriteTimestamp: native.RewriteTimestamp,
				Platform:         requestedPlatform, Platforms: platforms, Target: native.Target, Format: native.Format, Args: resolvedArgs,
				AuthFile: registry.authFileOption(cmd), CertDir: registry.certDir, TLSVerify: registry.tlsPolicy(cmd),
				Credentials: registry.credentials, Retry: registry.retry, RetrySet: cmd.Flags().Changed("retry"), RetryDelay: registry.retryDelay, DecryptionKeys: registry.decryptionKeys, SignaturePolicyPath: commandSignaturePolicy(cmd),
				CacheFrom: cacheSources, CacheTo: cacheDestinations,
				Secrets: native.Secrets, SSH: native.SSH,
				Allow:         allow,
				BuildContexts: contexts,
				Stdin:         cmd.InOrStdin(), RunStdin: executionStdin, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}
			if native.Iidfile != "" || native.IidfileRaw != "" || native.Logfile != "" {
				return nil
			}
			resultWriter := cmd.OutOrStdout()
			for _, filesystem := range filesystems {
				if filesystem.Path == "-" {
					resultWriter = cmd.ErrOrStderr()
					break
				}
			}
			_, err = fmt.Fprintln(resultWriter, result.ImageID)
			return err
		},
	}
	f := cmd.Flags()
	addNativeFlags(cmd, &nativeFlags, buildahcli.GetBudFlagsCompletions(), "source-policy-file", "metadata-file", "iidfile-raw")
	f.Lookup("source-policy-file").Usage = "BuildKit-format policy file for base image sources"
	f.Lookup("metadata-file").Usage = "write result, index, platform, and image configuration digests as JSON"
	f.Lookup("iidfile-raw").Usage = "write the image ID without its algorithm prefix"
	addNativeFlags(cmd, &nativeFlags, buildahcli.GetBudFlagsCompletions(), "from", "disable-compression", "compression-format", "compression-level", "force-compression", "cw", "quiet", "logfile", "logsplit", "log-rusage", "rusage-logfile", "output", "squash", "all-platforms", "manifest", "iidfile", "ignorefile", "tag", "no-cache", "rewrite-timestamp", "target", "format", "build-arg", "build-arg-file", "stdin", "build-context", "cache-from", "cache-to", "secret", "ssh")
	f.Lookup("from").Usage = "replace the image in the first FROM instruction"
	f.Lookup("output").Usage = "export flattened filesystem (repeatable): PATH, type=local,dest=PATH, or type=tar,dest=FILE (- for stdout)"
	f.Lookup("squash").Usage = "combine new image layers, preserving base layers"
	f.Lookup("ignorefile").Usage = "context ignore file (default: first of .cooprignore, .containerignore, .dockerignore at the context root)"
	f.Lookup("no-cache").Usage = "rebuild without reading cached results; save fresh results to the build cache"
	f.Lookup("target").Usage = "named output stage"
	f.Lookup("build-arg").Usage = "build argument (repeatable NAME[=VALUE]; NAME inherits from the environment when set)"
	f.Lookup("build-arg-file").Usage = "read build arguments from a file (repeatable; --build-arg wins)"
	f.Lookup("build-context").Usage = "additional build context: NAME=PATH|URL|docker-image://REFERENCE|oci-layout://PATH:TAG (repeatable)"
	f.Lookup("cache-from").Usage = "read cached results from a repository or type=local,src=PATH (repeatable)"
	f.Lookup("cache-to").Usage = "write cached results to a repository or type=local,dest=PATH (repeatable)"
	f.StringArrayVarP(&definitionFiles, "file", "f", nil, "definition file path (repeatable, combined in order; required with a build context)")
	f.StringVar(&blobDirectory, "blob-cache", "", "directory for native layer blob caching")
	_ = f.MarkHidden("blob-cache")
	f.BoolVar(&squashAll, "squash-all", false, "combine all image layers, including the base")
	cmd.MarkFlagsMutuallyExclusive("squash", "squash-all")
	imageControls.addTo(cmd)
	addSBOMFlags(cmd)
	signing.addTo(cmd)
	f.StringVar(&native.IidfileRaw, "raw-iidfile", "", "alias for --iidfile-raw")
	f.BoolVar(&push, "push", false, "publish the image to the registry named by --tag")
	addPullFlags(cmd, &pullPolicy)
	f.StringVar(&network, "network", "default", "RUN network mode: default, private, none, host, ns:PATH, pasta[:OPTIONS], or named network")
	f.StringSliceVar(&addHosts, "add-host", nil, "add HOST:IP to build containers (repeatable)")
	f.StringSliceVar(&platforms, "platform", nil, "target platform as os/arch (repeatable or comma-separated; default: host platform)")
	f.StringVar(&osName, "os", "", "target operating system")
	f.StringVar(&arch, "arch", "", "target architecture")
	f.StringVar(&variant, "variant", "", "target architecture variant")
	cmd.MarkFlagsMutuallyExclusive("all-platforms", "platform")
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
