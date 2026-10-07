package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"
	"text/template"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/transfer"

	"github.com/containerd/platforms"
	"github.com/spf13/cobra"
	"go.podman.io/storage"
)

func imageIOCommand(use, short string, args cobra.PositionalArgs, run func(*cobra.Command, []string) error) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: args, Annotations: map[string]string{nativeStorageAnnotation: "true"}, RunE: run}
}

func imagePlatformFlag(cmd *cobra.Command, value *string) {
	cmd.Flags().StringVar(value, "platform", "linux/"+runtime.GOARCH, "select image platform as OS/ARCH[/VARIANT]")
}

func newImagePullCommand() *cobra.Command {
	var registry registryFlags
	var platformValue, policy string
	var decrypt []string
	cmd := imageIOCommand("pull <image>...", "Pull images into the local container store", cobra.MinimumNArgs(1), func(cmd *cobra.Command, args []string) error {
		platform, err := platforms.Parse(platformValue)
		if err != nil {
			return fmt.Errorf("invalid pull platform %q: %w", platformValue, err)
		}
		if _, err := oci.NormalizePullPolicy(policy, false); err != nil {
			return err
		}
		for _, arg := range args {
			if _, err := oci.ParseReference(arg); err != nil {
				return err
			}
		}
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		options := registry.resolverOptions(cmd)
		options.PullPolicy = policy
		options.DecryptionKeys = decrypt
		for _, arg := range args {
			id, err := buildah.PullImage(cmd.Context(), store, options, arg, platforms.Normalize(platform))
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), id); err != nil {
				return err
			}
		}
		return nil
	})
	registry.addTo(cmd)
	addSignaturePolicyFlag(cmd.Flags())
	imagePlatformFlag(cmd, &platformValue)
	cmd.Flags().StringVar(&policy, "policy", "always", "pull policy: always, missing, never, or newer")
	cmd.Flags().StringArrayVar(&decrypt, "decryption-key", nil, "key for decrypting image layers (repeatable)")
	return cmd
}

func newImagePushCommand() *cobra.Command {
	var registry registryFlags
	var signing signingFlags
	var platformValue, digestFile string
	var quiet bool
	cmd := imageIOCommand("push <image> [destination]", "Push a local image to a registry", cobra.RangeArgs(1, 2), func(cmd *cobra.Command, args []string) error {
		target := args[0]
		if len(args) == 2 {
			target = args[1]
		}
		target = strings.TrimPrefix(target, "docker://")
		destination, err := transfer.ParsePushDestination(target, oci.Image)
		if err != nil {
			return err
		}
		platform, err := platforms.Parse(platformValue)
		if err != nil {
			return err
		}
		options := registry.transferOptions(cmd)
		options.Platform = platforms.Normalize(platform)
		options.PlatformExplicit = cmd.Flags().Changed("platform")
		options.Signing = signing.options()
		if err := transfer.ValidateSigningDestination(oci.Image, destination, options.Signing); err != nil {
			return err
		}
		options.BuildStore, err = commandStorage(cmd)
		if err != nil {
			return err
		}
		result, err := transfer.Copy(cmd.Context(), oci.Image, args[0], destination, options)
		if err != nil {
			return err
		}
		if digestFile != "" {
			_, digest, found := strings.Cut(result, "@")
			if !found {
				return fmt.Errorf("push result has no digest: %s", result)
			}
			if err := os.WriteFile(digestFile, []byte(digest), 0o644); err != nil {
				return err
			}
		}
		if !quiet {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), result)
		}
		return err
	})
	registry.addTo(cmd)
	signing.addTo(cmd)
	addSignaturePolicyFlag(cmd.Flags())
	imagePlatformFlag(cmd, &platformValue)
	cmd.Flags().StringVar(&digestFile, "digestfile", "", "write pushed manifest digest to a file")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress push output")
	return cmd
}

func newImageTagCommand() *cobra.Command {
	return imageIOCommand("tag <image> <name>...", "Add local names to a stored image", cobra.MinimumNArgs(2), func(cmd *cobra.Command, args []string) error {
		destinations := make([]transfer.Destination, 0, len(args)-1)
		for _, name := range args[1:] {
			destination, err := transfer.ParseDestination("local:"+name, oci.Image)
			if err != nil {
				return err
			}
			destinations = append(destinations, destination)
		}
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		options := transfer.Options{BuildStore: store, Platform: platforms.Normalize(platforms.DefaultSpec())}
		for _, destination := range destinations {
			if _, err := transfer.Copy(cmd.Context(), oci.Image, args[0], destination, options); err != nil {
				return err
			}
		}
		return nil
	})
}

func newImageSaveCommand() *cobra.Command {
	var output, format, platformValue string
	var quiet bool
	cmd := imageIOCommand("save <image>...", "Save local images to an archive", cobra.MinimumNArgs(1), func(cmd *cobra.Command, args []string) error {
		if format != "docker-archive" && format != "oci-archive" {
			return fmt.Errorf("unsupported save format %q (use docker-archive or oci-archive)", format)
		}
		if format == "oci-archive" && len(args) != 1 {
			return errors.New("oci-archive saves exactly one image or index")
		}
		platform, err := platforms.Parse(platformValue)
		if err != nil {
			return err
		}
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		path := output
		if path == "" || path == "-" {
			temp, err := os.CreateTemp("", "coopr-save-*.tar")
			if err != nil {
				return err
			}
			path = temp.Name()
			defer func() { _ = os.Remove(path) }()
			if err := temp.Close(); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		if format == "oci-archive" {
			archiveName := ""
			if err := buildah.WithImageStore(cmd.Context(), store, func(backend storage.Store) error {
				matched, err := oci.StoredImageName(backend, args[0])
				if err != nil {
					return err
				}
				if strings.Contains(matched, "/") && !strings.Contains(matched, "@") {
					archiveName = matched
				}
				return nil
			}); err != nil {
				return err
			}
			_, err = transfer.Copy(cmd.Context(), oci.Image, args[0], transfer.Destination{Transport: "oci-archive", Name: path}, transfer.Options{BuildStore: store, ArchiveReference: archiveName, Platform: platforms.Normalize(platform), PlatformExplicit: cmd.Flags().Changed("platform"), SignaturePolicyPath: commandSignaturePolicy(cmd)})
		} else {
			progress := cmd.ErrOrStderr()
			if quiet {
				progress = nil
			}
			err = buildah.SaveImages(cmd.Context(), store, args, format, path, oci.Options{SignaturePolicyPath: commandSignaturePolicy(cmd)}, platforms.Normalize(platform), cmd.Flags().Changed("platform"), progress)
		}
		if err != nil {
			return err
		}
		if output == "" || output == "-" {
			archive, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(cmd.OutOrStdout(), archive)
			return errors.Join(copyErr, archive.Close())
		}
		return nil
	})
	cmd.Flags().StringVarP(&output, "output", "o", "", "archive path (default: standard output)")
	cmd.Flags().StringVar(&format, "format", "docker-archive", "archive format: docker-archive or oci-archive")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress save progress")
	imagePlatformFlag(cmd, &platformValue)
	addSignaturePolicyFlag(cmd.Flags())
	return cmd
}

func newImageLoadCommand() *cobra.Command {
	var input string
	var quiet bool
	cmd := imageIOCommand("load", "Load images from an OCI or Docker archive", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		path := input
		if path == "" || path == "-" {
			temp, err := os.CreateTemp("", "coopr-load-*.tar")
			if err != nil {
				return err
			}
			path = temp.Name()
			defer func() { _ = os.Remove(path) }()
			_, copyErr := io.Copy(temp, cmd.InOrStdin())
			if err := errors.Join(copyErr, temp.Close()); err != nil {
				return err
			}
		}
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		progress := cmd.ErrOrStderr()
		if quiet {
			progress = nil
		}
		names, err := buildah.LoadImages(cmd.Context(), store, path, oci.Options{SignaturePolicyPath: commandSignaturePolicy(cmd)}, progress)
		if err != nil {
			return err
		}
		for _, name := range names {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Loaded image:", name); err != nil {
				return err
			}
		}
		return nil
	})
	cmd.Flags().StringVarP(&input, "input", "i", "", "archive or image directory (default: standard input)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress load progress")
	addSignaturePolicyFlag(cmd.Flags())
	return cmd
}

func newImageExistsCommand() *cobra.Command {
	return imageIOCommand("exists <image>", "Check whether an image exists in the local store", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		store, err := commandStorage(cmd)
		if err != nil {
			return commandExitError{Code: 125, Err: err}
		}
		exists, err := buildah.ImageExists(cmd.Context(), store, args[0])
		if err != nil {
			return commandExitError{Code: 125, Err: err}
		}
		if !exists {
			return commandExitError{Code: 1}
		}
		return nil
	})
}

func newImageHistoryCommand() *cobra.Command {
	var format, platformValue string
	var quiet, noTrunc bool
	cmd := imageIOCommand("history <image>", "Show a local image's layer history", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		platform, err := platforms.Parse(platformValue)
		if err != nil {
			return err
		}
		var tmpl *template.Template
		if format != "" && format != "json" {
			tmpl, err = template.New("history").Parse(format)
			if err != nil {
				return err
			}
		}
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		history, err := buildah.ImageHistory(cmd.Context(), store, args[0], platforms.Normalize(platform), cmd.Flags().Changed("platform"))
		if err != nil {
			return err
		}
		if format == "json" {
			return writeJSON(cmd, history)
		}
		output := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		if !quiet && tmpl == nil {
			if _, err := fmt.Fprintln(output, "ID\tCREATED\tCREATED BY\tSIZE\tCOMMENT"); err != nil {
				return err
			}
		}
		for _, entry := range history {
			if tmpl != nil {
				if err := tmpl.Execute(output, entry); err != nil {
					return err
				}
				if _, err := fmt.Fprintln(output); err != nil {
					return err
				}
				continue
			}
			id := entry.ID
			createdBy := entry.CreatedBy
			if !noTrunc {
				if len(id) > 12 {
					id = id[:12]
				}
				if len(createdBy) > 45 {
					createdBy = createdBy[:42] + "..."
				}
			}
			if quiet {
				if _, err := fmt.Fprintln(output, id); err != nil {
					return err
				}
				continue
			}
			created := ""
			if entry.Created != nil {
				created = entry.Created.UTC().Format("2006-01-02T15:04:05Z")
			}
			if _, err := fmt.Fprintf(output, "%s\t%s\t%s\t%d B\t%s\n", id, created, createdBy, entry.Size, entry.Comment); err != nil {
				return err
			}
		}
		return output.Flush()
	})
	cmd.Flags().StringVar(&format, "format", "", "output format: json or a Go template")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "show only image IDs")
	cmd.Flags().BoolVar(&noTrunc, "no-trunc", false, "show full IDs and commands")
	imagePlatformFlag(cmd, &platformValue)
	return cmd
}
