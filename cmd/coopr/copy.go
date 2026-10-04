package main

import (
	"fmt"
	"runtime"
	"strings"

	"coopr/internal/oci"
	"coopr/internal/transfer"
	"github.com/containerd/platforms"
	"github.com/spf13/cobra"
)

func newCopyCommand(kind oci.Kind) *cobra.Command {
	return newCopyCommandWithGlobals(kind, true)
}

func newCopyCommandWithGlobals(kind oci.Kind, standalone bool) *cobra.Command {
	var plainHTTP bool
	var plainHTTPRegistries []string
	var registry registryFlags
	var signing signingFlags
	platformValue := "linux/" + runtime.GOARCH
	name := "image"
	long := "Copy a stored image by local tag or bare sha256 digest to a new local tag, OCI archive, registry, or image engine. Complete multi-platform images built by Coopr copy as a whole by default; partial imported indexes select the native Linux platform, and single-platform tags select their sole platform. Use --platform OS/ARCH to copy one stored platform. Exact image-manifest digests retain their cataloged platform when --platform is omitted. Destinations use local:, oci-archive:, registry:, podman:, or docker: prefixes."
	if kind == oci.Component {
		name = "component"
		long = "Copy a stored component by local tag or bare sha256 digest to a new local tag, OCI archive, or registry. Destinations use local:, oci-archive:, or registry: prefixes."
	}
	cmd := &cobra.Command{
		Use:   "copy <source> <destination>",
		Short: "Copy a stored " + name + " without rebuilding",
		Long:  long,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := platforms.Parse(platformValue)
			if err != nil {
				return fmt.Errorf("invalid copy platform %q: %w", platformValue, err)
			}
			destination, err := transfer.ParseDestination(args[1], kind)
			if err != nil {
				return err
			}
			if err := transfer.ValidateSigningDestination(kind, destination, signing.options()); err != nil {
				return err
			}
			if err := prepareDestination(destination); err != nil {
				return err
			}
			if kind == oci.Image && strings.HasPrefix(args[0], "podman:") {
				if err := prepareStorageNamespace(); err != nil {
					return err
				}
			}
			store, catalogue, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			if kind == oci.Image && store.Shared && !strings.HasPrefix(args[0], "podman:") && !strings.HasPrefix(args[0], "docker:") && !strings.HasPrefix(args[0], "sha256:") {
				if err := refreshSharedNativeCatalog(cmd.Context(), store, catalogue, args[0]); err != nil {
					return err
				}
			}
			result, err := transfer.Copy(cmd.Context(), kind, args[0], destination, transfer.Options{
				BuildStore: store, ImageStoreDir: catalogue,
				PlainHTTP: plainHTTP, PlainHTTPRegistries: plainHTTPRegistries,
				AuthFile: registry.authFile, CertDir: registry.certDir, SkipTLSVerify: !registry.tlsVerify,
				Credentials: registry.credentials, Retry: registry.retry, RetrySet: cmd.Flags().Changed("retry"), RetryDelay: registry.retryDelay, DecryptionKeys: registry.decryptionKeys, SignaturePolicyPath: commandSignaturePolicy(cmd),
				Platform: platforms.Normalize(platform), PlatformExplicit: cmd.Flags().Changed("platform"),
				Signing: signing.options(),
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), result)
			return err
		},
	}
	flags := cmd.Flags()
	flags.BoolVar(&plainHTTP, "plain-http", false, "allow Coopr HTTP transport for loopback OCI registries")
	flags.StringArrayVar(&plainHTTPRegistries, "plain-http-registry", nil, "allow Coopr HTTP transport for an exact registry host[:port] (repeatable)")
	if standalone {
		addSignaturePolicyFlag(cmd.Flags())
	}
	registry.addTo(cmd)
	if kind == oci.Image {
		flags.StringVar(&platformValue, "platform", platformValue, "select the stored image platform")
		signing.addTo(cmd)
	}
	return cmd
}
