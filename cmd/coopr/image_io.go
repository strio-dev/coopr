package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/transfer"

	"github.com/containerd/platforms"
	"github.com/docker/go-units"
	"github.com/spf13/cobra"
	"go.podman.io/common/libimage"
	"go.podman.io/common/pkg/download"
	"go.podman.io/common/pkg/report"
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
	var quiet, allTags bool
	cmd := imageIOCommand("pull <image>...", "Pull images into the local container store", cobra.MinimumNArgs(1), func(cmd *cobra.Command, args []string) error {
		platform, err := platforms.Parse(platformValue)
		if err != nil {
			return fmt.Errorf("invalid pull platform %q: %w", platformValue, err)
		}
		if _, err := oci.NormalizePullPolicy(policy, false); err != nil {
			return err
		}
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		options := registry.resolverOptions(cmd)
		options.PullPolicy = policy
		options.DecryptionKeys = decrypt
		if !quiet {
			options.ProgressWriter = cmd.ErrOrStderr()
		}
		var failures []error
		for _, arg := range args {
			var ids []string
			var pullErr error
			if allTags {
				ids, pullErr = buildah.PullAllTags(cmd.Context(), store, options, arg, platforms.Normalize(platform))
			} else {
				var id string
				id, pullErr = buildah.PullImage(cmd.Context(), store, options, arg, platforms.Normalize(platform))
				if pullErr == nil {
					ids = []string{id}
				}
			}
			for _, id := range ids {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), id); err != nil {
					return errors.Join(append(failures, err)...)
				}
			}
			if pullErr != nil {
				failures = append(failures, fmt.Errorf("pull %s: %w", arg, pullErr))
			}
		}
		return errors.Join(failures...)
	})
	registry.addTo(cmd)
	addSignaturePolicyFlag(cmd.Flags())
	imagePlatformFlag(cmd, &platformValue)
	cmd.Flags().BoolVarP(&allTags, "all-tags", "a", false, "pull all tags in the repository")
	cmd.Flags().StringVar(&policy, "policy", "always", "pull policy: always, missing, never, or newer")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress pull progress")
	cmd.Flags().StringArrayVar(&decrypt, "decryption-key", nil, "key for decrypting image layers (repeatable)")
	return cmd
}

func newImagePushCommand() *cobra.Command {
	var registry registryFlags
	var signing signingFlags
	var pushOptions pushTransferFlags
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
		if err := pushOptions.apply(cmd, &options); err != nil {
			return err
		}
		if !quiet {
			options.ProgressWriter = cmd.ErrOrStderr()
		}
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
		return nil
	})
	registry.addTo(cmd)
	signing.addTo(cmd)
	pushOptions.addTo(cmd, false)
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
	var quiet, multi, compress, uncompressed bool
	cmd := imageIOCommand("save <image>...", "Save local images to an archive", cobra.MinimumNArgs(1), func(cmd *cobra.Command, args []string) error {
		if format != "docker-archive" && format != "oci-archive" && format != "oci-dir" && format != "docker-dir" {
			return fmt.Errorf("unsupported save format %q (use docker-archive, oci-archive, oci-dir or docker-dir)", format)
		}
		directory := strings.HasSuffix(format, "-dir")
		if directory && (output == "" || output == "-") {
			return errors.New("directory save formats require --output")
		}
		if cmd.Flags().Changed("compress") && format != "docker-dir" {
			return errors.New("--compress can only be set when --format is docker-dir")
		}
		if multi && format != "docker-archive" {
			return errors.New("--multi-image-archive requires docker-archive")
		}
		names := args
		var additional []string
		if !multi {
			names = args[:1]
			additional = args[1:]
		}
		if len(additional) > 0 && format != "docker-archive" {
			return errors.New("additional tags require docker-archive")
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
		if format == "oci-archive" || format == "oci-dir" {
			var acceptUncompressed *bool
			if cmd.Flags().Changed("uncompressed") {
				acceptUncompressed = &uncompressed
			}
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
			_, err = transfer.Copy(cmd.Context(), oci.Image, args[0], transfer.Destination{Transport: format, Name: path}, transfer.Options{BuildStore: store, ArchiveReference: archiveName, ArchiveUncompressed: acceptUncompressed, Platform: platforms.Normalize(platform), PlatformExplicit: cmd.Flags().Changed("platform"), SignaturePolicyPath: commandSignaturePolicy(cmd), ProgressWriter: saveProgress(cmd, quiet)})
		} else {
			progress := cmd.ErrOrStderr()
			if quiet {
				progress = nil
			}
			err = buildah.SaveImages(cmd.Context(), store, names, format, path, oci.Options{SignaturePolicyPath: commandSignaturePolicy(cmd)}, platforms.Normalize(platform), cmd.Flags().Changed("platform"), progress, libimage.SaveOptions{AdditionalTags: additional, CopyOptions: libimage.CopyOptions{DirForceCompress: compress, OciAcceptUncompressedLayers: uncompressed}})
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
	cmd.Flags().BoolVarP(&multi, "multi-image-archive", "m", false, "save multiple images instead of additional tags")
	cmd.Flags().BoolVar(&compress, "compress", false, "compress layers in directory output")
	cmd.Flags().BoolVar(&uncompressed, "uncompressed", false, "allow uncompressed layers in directory output")
	cmd.Flags().StringVarP(&output, "output", "o", "", "archive path (default: standard output)")
	cmd.Flags().StringVar(&format, "format", "docker-archive", "format: docker-archive, oci-archive, oci-dir or docker-dir")
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
		if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
			downloaded, err := download.FromURL(cmd.Context(), "", path, download.Options{})
			if err != nil {
				return err
			}
			path = downloaded
			defer func() { _ = os.Remove(downloaded) }()
		}
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
	var quiet, noTrunc, human bool
	cmd := imageIOCommand("history <image>", "Show a local image's layer history", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		platform, err := platforms.Parse(platformValue)
		if err != nil {
			return err
		}
		if format != "" && format != "json" && !quiet {
			if _, err := report.New(io.Discard, "history").Parse(report.OriginUser, format); err != nil {
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
		return writeImageHistory(cmd.OutOrStdout(), history, format, quiet, noTrunc, human)
	})
	cmd.Flags().StringVar(&format, "format", "", "output format: json or a Go template")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "show only image IDs")
	cmd.Flags().BoolVarP(&human, "human", "H", true, "display dates in human readable format")
	cmd.Flags().BoolVar(&noTrunc, "no-trunc", false, "show full IDs and commands")
	imagePlatformFlag(cmd, &platformValue)
	return cmd
}

// historyReporter uses the same display fields as Podman's history report while
// leaving the native history values intact for JSON output.
type historyReporter struct {
	libimage.ImageHistory
	noTrunc bool
	human   bool
}

func (h historyReporter) ID() string {
	if !h.noTrunc && len(h.ImageHistory.ID) > 12 {
		return h.ImageHistory.ID[:12]
	}
	return h.ImageHistory.ID
}
func (h historyReporter) Created() string {
	if h.ImageHistory.Created == nil {
		return ""
	}
	if !h.human {
		return h.ImageHistory.Created.Format(time.RFC3339)
	}
	return units.HumanDuration(time.Since(*h.ImageHistory.Created)) + " ago"
}
func (h historyReporter) CreatedSince() string { return h.Created() }
func (h historyReporter) CreatedAt() string {
	if h.ImageHistory.Created == nil {
		return ""
	}
	return h.ImageHistory.Created.Format(time.RFC3339)
}
func (h historyReporter) Size() string {
	return units.HumanSizeWithPrecision(float64(h.ImageHistory.Size), 3)
}
func (h historyReporter) CreatedBy() string {
	if !h.noTrunc && len(h.ImageHistory.CreatedBy) > 45 {
		return h.ImageHistory.CreatedBy[:42] + "..."
	}
	return h.ImageHistory.CreatedBy
}
func writeImageHistory(out io.Writer, history []libimage.ImageHistory, format string, quiet, noTrunc, human bool) (retErr error) {
	rows := make([]historyReporter, 0, len(history))
	for _, entry := range history {
		rows = append(rows, historyReporter{ImageHistory: entry, noTrunc: noTrunc, human: human})
	}
	rpt := report.New(out, "history")
	defer func() { retErr = errors.Join(retErr, rpt.Flush()) }()
	var err error
	switch {
	case quiet:
		rpt, err = rpt.Parse(report.OriginUser, "{{range .}}{{.ID}}\n{{end -}}")
	case format != "":
		rpt, err = rpt.Parse(report.OriginUser, format)
	default:
		rpt, err = rpt.Parse(report.OriginPodman, "{{range .}}{{.ID}}\t{{.Created}}\t{{.CreatedBy}}\t{{.Size}}\t{{.Comment}}\n{{end -}}")
	}
	if err != nil {
		return err
	}
	if rpt.RenderHeaders {
		if err := rpt.Execute(report.Headers(historyReporter{}, map[string]string{"CreatedBy": "CREATED BY"})); err != nil {
			return err
		}
	}
	return rpt.Execute(rows)
}

func saveProgress(cmd *cobra.Command, quiet bool) io.Writer {
	if quiet {
		return nil
	}
	return cmd.ErrOrStderr()
}
