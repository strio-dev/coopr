package main

import (
	"fmt"
	"runtime"

	"coopr/internal/oci"
	"coopr/internal/transfer"

	"github.com/containerd/platforms"
	"github.com/spf13/cobra"
)

func newCopyCommand(kind oci.Kind) *cobra.Command {
	var registry registryFlags
	var signing signingFlags
	platformValue := "linux/" + runtime.GOARCH
	name := "image"
	long := "Copy a stored image by local tag or bare sha256 digest to a new local tag, OCI archive, registry, or image engine. Complete multi-platform images built by Coopr copy as a whole by default; partial imported indexes select the native Linux platform, and single-platform tags select their sole platform. Use --platform OS/ARCH to copy one stored platform. Exact image-manifest digests retain their stored platform when --platform is omitted. Destinations use local:, oci-archive:, registry:, podman:, or docker: prefixes."
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
			options := registry.transferOptions(cmd)
			options.Platform, options.PlatformExplicit = platforms.Normalize(platform), cmd.Flags().Changed("platform")
			options.Signing = signing.options()
			if kind == oci.Image {
				options.BuildStore, err = commandStorage(cmd)
				if err != nil {
					return err
				}
			}
			result, err := transfer.Copy(cmd.Context(), kind, args[0], destination, options)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), result)
			return err
		},
	}
	if kind == oci.Image {
		cmd.Annotations = map[string]string{nativeStorageAnnotation: "true"}
	}
	flags := cmd.Flags()
	registry.addTo(cmd)
	if kind == oci.Image {
		addSignaturePolicyFlag(flags)
		flags.StringVar(&platformValue, "platform", platformValue, "select the stored image platform")
		signing.addTo(cmd)
	}
	return cmd
}
