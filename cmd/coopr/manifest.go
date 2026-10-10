package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"
	"coopr/internal/transfer"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
	"go.podman.io/common/libimage"
	"go.podman.io/common/pkg/retry"
	imagereference "go.podman.io/image/v5/docker/reference"
	imageapi "go.podman.io/image/v5/image"
	"go.podman.io/image/v5/manifest"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/storage"
)

func newManifestCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "manifest", Short: "Manage native container image manifest lists", RunE: helpOnNoArgs}
	cmd.AddCommand(newManifestCreateCommand(), newManifestAddCommand(), newManifestAnnotateCommand(), newManifestInspectCommand(), newManifestPushCommand(), newManifestRemoveCommand(), newManifestRMCommand(), newManifestExistsCommand())
	return cmd
}

func withManifestStore(cmd *cobra.Command, registry oci.Options, exclusive bool, use func(storage.Store, *libimage.Runtime) error) (retErr error) {
	options, err := commandStorage(cmd)
	if err != nil {
		return err
	}
	resolver, err := oci.NewResolver(registry)
	if err != nil {
		return err
	}
	var lease *storeactivity.Lease
	if exclusive {
		lease, err = storeactivity.AcquireExclusive(cmd.Context(), buildah.ActivityRoots(options, "")...)
	} else {
		lease, err = storeactivity.AcquireShared(cmd.Context(), buildah.ActivityRoots(options, "")...)
	}
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, lease.Close()) }()
	return buildah.WithStore(options, func(backend storage.Store) error {
		native, err := libimage.RuntimeFromStore(backend, &libimage.RuntimeOptions{SystemContext: resolver.SystemContext()})
		if err != nil {
			return err
		}
		return use(backend, native)
	})
}

// Native instance references pin a Coopr local selection before libimage Add.
// Remote names remain native registry references and need not be pulled first.
func manifestMemberReference(ctx context.Context, backend storage.Store, name string) (string, error) {
	if strings.Contains(name, "://") || strings.HasPrefix(name, "containers-storage:") || strings.HasPrefix(name, "oci:") || strings.HasPrefix(name, "oci-archive:") {
		return name, nil
	}
	matched, err := oci.StoredImageName(backend, name)
	if errors.Is(err, storage.ErrImageUnknown) {
		return name, nil
	}
	if err != nil {
		return "", err
	}
	root, _, indexed, err := oci.StoredImageIndex(ctx, backend, name)
	var imageID string
	if err != nil {
		return "", err
	}
	if !indexed {
		available, err := oci.StoredImagePlatforms(ctx, backend, name)
		if err != nil {
			return "", err
		}
		platform := platforms.DefaultSpec()
		if len(available) == 1 {
			platform = available[0]
		}
		selected, err := oci.ResolveStoredImage(ctx, backend, name, platform)
		if err != nil {
			return "", err
		}
		root = selected.Selected
		imageID = selected.StorageImageID
	}
	if indexed {
		images, err := backend.ImagesByDigest(root.Digest)
		if err != nil {
			return "", err
		}
		if len(images) == 0 {
			return "", storage.ErrImageUnknown
		}
		imageID = images[0].ID
	}
	repository := matched
	if !strings.Contains(repository, "/") || strings.HasPrefix(repository, "sha256:") {
		image, err := backend.Image(imageID)
		if err != nil {
			return "", err
		}
		if len(image.Names) > 0 {
			repository = image.Names[0]
		} else {
			// Native digest references require a repository name even with an ID.
			// Retain an internal alias for the list's native instance reference, as
			// Coopr's native index writer does for unnamed resident children.
			repository = "coopr.internal/manifest/" + imageID + ":member"
			if err := backend.AddNames(imageID, []string{repository}); err != nil {
				return "", err
			}
		}
	}
	repository, _, _ = strings.Cut(repository, "@")
	named, err := imagereference.ParseNormalizedNamed(repository)
	if err != nil {
		return "", err
	}
	pinned, err := imagereference.WithDigest(imagereference.TrimNamed(named), root.Digest)
	if err != nil {
		return "", err
	}
	reference, err := imagestorage.Transport.NewStoreReference(backend, pinned, imageID)
	if err != nil {
		return "", err
	}
	return transports.ImageName(reference), nil
}

func addManifestMember(cmd *cobra.Command, backend storage.Store, list *libimage.ManifestList, name string, all bool, registry oci.Options) (digest.Digest, error) {
	reference, err := manifestMemberReference(cmd.Context(), backend, name)
	if err != nil {
		return "", err
	}
	var added digest.Digest
	options := &retry.Options{MaxRetry: int(registry.Retry), Delay: registry.RetryDelay}
	if !registry.RetrySet {
		options.MaxRetry = 3
	}
	err = retry.IfNecessary(cmd.Context(), func() error {
		var err error
		added, err = list.Add(cmd.Context(), reference, &libimage.ManifestListAddOptions{All: all})
		return err
	}, options)
	return added, err
}

func newManifestCreateCommand() *cobra.Command {
	var registry registryFlags
	var all, amend bool
	var annotations []string
	cmd := imageIOCommand("create <list> [image...]", "Create a local manifest list", cobra.MinimumNArgs(1), func(cmd *cobra.Command, args []string) error {
		parsed, err := parseManifestAnnotations(annotations)
		if err != nil {
			return err
		}
		return withManifestStore(cmd, registry.resolverOptions(cmd), true, func(backend storage.Store, native *libimage.Runtime) error {
			list, err := native.LookupManifestList(args[0])
			if err != nil && !errors.Is(err, storage.ErrImageUnknown) {
				return err
			}
			if list != nil && !amend {
				return fmt.Errorf("manifest list %q already exists; use --amend", args[0])
			}
			if list == nil {
				list, err = native.CreateManifestList(args[0])
				if err != nil {
					return err
				}
			}
			for _, name := range args[1:] {
				if _, err := addManifestMember(cmd, backend, list, name, all, registry.resolverOptions(cmd)); err != nil {
					return err
				}
			}
			if len(parsed) > 0 {
				if err := list.AnnotateInstance("", &libimage.ManifestListAnnotateOptions{IndexAnnotations: parsed}); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), list.ID())
			return err
		})
	})
	registry.addTo(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "add every instance from source manifest lists")
	cmd.Flags().BoolVarP(&amend, "amend", "a", false, "add to an existing local list")
	cmd.Flags().StringArrayVar(&annotations, "annotation", nil, "set index annotation KEY=VALUE (repeatable)")
	return cmd
}

func newManifestAddCommand() *cobra.Command {
	var registry registryFlags
	var all bool
	var annotations []string
	var annotationOptions libimage.ManifestListAnnotateOptions
	cmd := imageIOCommand("add <list> <image>...", "Add images to a local manifest list", cobra.MinimumNArgs(2), func(cmd *cobra.Command, args []string) error {
		parsed, err := parseManifestAnnotations(annotations)
		if err != nil {
			return err
		}
		annotationOptions.Annotations = parsed
		return withManifestStore(cmd, registry.resolverOptions(cmd), true, func(backend storage.Store, native *libimage.Runtime) error {
			list, err := native.LookupManifestList(args[0])
			if err != nil {
				return err
			}
			for _, name := range args[1:] {
				added, err := addManifestMember(cmd, backend, list, name, all, registry.resolverOptions(cmd))
				if err != nil {
					return err
				}
				if err := list.AnnotateInstance(added, &annotationOptions); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), list.ID())
			return err
		})
	})
	registry.addTo(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "add every instance from source manifest lists")
	addManifestAnnotationFlags(cmd, &annotationOptions, &annotations)
	return cmd
}

func parseManifestAnnotations(values []string) (map[string]string, error) {
	parsed := make(map[string]string, len(values))
	for _, value := range values {
		key, text, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid annotation %q (expected KEY=VALUE)", value)
		}
		parsed[key] = text
	}
	return parsed, nil
}

func addManifestAnnotationFlags(cmd *cobra.Command, options *libimage.ManifestListAnnotateOptions, annotations *[]string) {
	flags := cmd.Flags()
	flags.StringVar(&options.OS, "os", "", "set instance operating system")
	flags.StringVar(&options.Architecture, "arch", "", "set instance architecture")
	flags.StringVar(&options.Variant, "variant", "", "set instance variant")
	flags.StringVar(&options.OSVersion, "os-version", "", "set instance OS version")
	flags.StringSliceVar(&options.OSFeatures, "os-features", nil, "set instance OS features")
	flags.StringSliceVar(&options.Features, "features", nil, "set instance CPU features")
	flags.StringArrayVar(annotations, "annotation", nil, "set instance annotation KEY=VALUE (repeatable)")
}

func manifestMemberDigest(cmd *cobra.Command, backend storage.Store, name string) (result digest.Digest, retErr error) {
	if parsed, err := digest.Parse(name); err == nil {
		return parsed, nil
	}
	name, err := manifestMemberReference(cmd.Context(), backend, name)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(name, "containers-storage:") && !strings.Contains(name, "://") && !strings.HasPrefix(name, "oci:") && !strings.HasPrefix(name, "oci-archive:") {
		resolver, err := oci.NewResolver(oci.Options{})
		if err != nil {
			return "", err
		}
		raw, _, err := resolver.RemoteImageManifest(cmd.Context(), name)
		if err != nil {
			return "", err
		}
		return manifest.Digest(raw)
	}
	ref, err := alltransports.ParseImageName(name)
	if err != nil {
		return "", err
	}
	resolver, err := oci.NewResolver(oci.Options{})
	if err != nil {
		return "", err
	}
	source, err := ref.NewImageSource(cmd.Context(), resolver.SystemContext())
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	raw, _, err := imageapi.UnparsedInstance(source, nil).Manifest(cmd.Context())
	if err != nil {
		return "", err
	}
	return manifest.Digest(raw)
}

func newManifestAnnotateCommand() *cobra.Command {
	var options libimage.ManifestListAnnotateOptions
	var annotations []string
	var index bool
	cmd := imageIOCommand("annotate <list> [image-or-digest]", "Annotate a local manifest list or an instance", cobra.RangeArgs(1, 2), func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 && !index && options.Subject == "" {
			return errors.New("annotate requires an instance digest, --index, or --subject")
		}
		if len(args) == 1 && (options.OS != "" || options.Architecture != "" || options.Variant != "" || options.OSVersion != "" || len(options.OSFeatures) > 0 || len(options.Features) > 0) {
			return errors.New("platform annotations require an instance digest")
		}

		var err error
		options.Annotations, err = parseManifestAnnotations(annotations)
		if err != nil {
			return err
		}
		if index && len(args) == 2 {
			return errors.New("--index requires no instance selector")
		}
		if options.Subject != "" && len(args) == 2 {
			return errors.New("--subject requires no instance selector")
		}
		if index {
			options.IndexAnnotations = options.Annotations
			options.Annotations = nil
		}
		return withManifestStore(cmd, oci.Options{}, true, func(backend storage.Store, native *libimage.Runtime) error {
			list, err := native.LookupManifestList(args[0])
			if err != nil {
				return err
			}
			var instance digest.Digest
			if len(args) == 2 {
				instance, err = manifestMemberDigest(cmd, backend, args[1])
				if err != nil {
					return err
				}
			}
			if options.Subject != "" {
				subject, err := manifestMemberReference(cmd.Context(), backend, options.Subject)
				if err != nil {
					return err
				}
				options.Subject = subject
			}
			if err := list.AnnotateInstance(instance, &options); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), list.ID())
			return err
		})
	})
	flags := cmd.Flags()
	flags.BoolVar(&index, "index", false, "apply annotations to the index itself")
	flags.StringVar(&options.Subject, "subject", "", "set the image index subject")
	addManifestAnnotationFlags(cmd, &options, &annotations)
	return cmd
}

func newManifestInspectCommand() *cobra.Command {
	var registry registryFlags
	cmd := imageIOCommand("inspect <list>", "Inspect a local or registry manifest list", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return withManifestStore(cmd, registry.resolverOptions(cmd), false, func(_ storage.Store, native *libimage.Runtime) error {
			list, err := native.LookupManifestList(args[0])
			if errors.Is(err, storage.ErrImageUnknown) || errors.Is(err, libimage.ErrNotAManifestList) {
				resolver, err := oci.NewResolver(registry.resolverOptions(cmd))
				if err != nil {
					return err
				}
				raw, mediaType, err := resolver.RemoteImageManifest(cmd.Context(), strings.TrimPrefix(args[0], "docker://"))
				if err != nil {
					return err
				}
				if mediaType == manifest.DockerV2Schema2MediaType {
					if _, err := manifest.Schema2FromManifest(raw); err != nil {
						return err
					}
					if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Warning: The manifest type %s is not a manifest list but a single image.\n", mediaType); err != nil {
						return err
					}
				} else if _, err := manifest.ListFromBlob(raw, mediaType); err != nil {
					return err
				}
				return writeJSON(cmd, json.RawMessage(raw))
			}
			if err != nil {
				return err
			}
			data, err := list.Inspect()
			if err != nil {
				return err
			}
			return writeJSON(cmd, data)
		})
	})
	registry.addTo(cmd)
	return cmd
}

func newManifestPushCommand() *cobra.Command {
	var registry registryFlags
	var signing signingFlags
	var pushOptions pushTransferFlags
	var all, quiet bool
	var remove bool
	var digestFile string
	cmd := imageIOCommand("push <list> [destination]", "Push a manifest list and its images to a registry", cobra.RangeArgs(1, 2), func(cmd *cobra.Command, args []string) error {
		target := args[0]
		if len(args) == 2 {
			target = args[1]
		}
		target = strings.TrimPrefix(target, "docker://")
		destination, err := transfer.ParsePushDestination(target, oci.Image)
		if err != nil {
			return err
		}
		options := registry.transferOptions(cmd)
		if err := pushOptions.apply(cmd, &options); err != nil {
			return err
		}
		options.Signing = signing.options()
		if err := transfer.ValidateSigningDestination(oci.Image, destination, options.Signing); err != nil {
			return err
		}
		options.BuildStore, err = commandStorage(cmd)
		if err != nil {
			return err
		}
		var listID string
		if err := withManifestStore(cmd, registry.resolverOptions(cmd), false, func(_ storage.Store, native *libimage.Runtime) error {
			list, err := native.LookupManifestList(args[0])
			if err == nil {
				listID = list.ID()
			}
			return err
		}); err != nil {
			return err
		}
		progress := cmd.ErrOrStderr()
		if quiet {
			progress = nil
		}
		pushed, err := transfer.PushManifestList(cmd.Context(), listID, destination, options, all, progress)
		if err != nil {
			return err
		}
		if digestFile != "" {
			if err := os.WriteFile(digestFile, []byte(pushed.String()), 0o644); err != nil {
				return err
			}
		}
		if remove {
			return withManifestStore(cmd, registry.resolverOptions(cmd), true, func(_ storage.Store, native *libimage.Runtime) error {
				_, errs := native.RemoveImages(cmd.Context(), []string{listID}, &libimage.RemoveImagesOptions{LookupManifest: true})
				return errors.Join(errs...)
			})
		}
		return nil
	})
	registry.addTo(cmd)
	signing.addTo(cmd)
	pushOptions.addTo(cmd, true)
	cmd.Flags().StringSliceVar(&pushOptions.options.AddCompression, "add-compression", nil, "add variants using the requested compression formats")
	addSignaturePolicyFlag(cmd.Flags())
	cmd.Flags().BoolVar(&all, "all", true, "push every image in the manifest list")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress push progress and output")
	cmd.Flags().StringVar(&digestFile, "digestfile", "", "write pushed manifest digest to a file")
	cmd.Flags().BoolVar(&remove, "rm", false, "remove the local list after a successful push")
	return cmd
}

func newManifestRemoveCommand() *cobra.Command {
	return imageIOCommand("remove <list> <instance-digest>", "Remove an instance from a local manifest list", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		instance, err := digest.Parse(args[1])
		if err != nil {
			return err
		}
		return withManifestStore(cmd, oci.Options{}, true, func(_ storage.Store, native *libimage.Runtime) error {
			list, err := native.LookupManifestList(args[0])
			if err != nil {
				return err
			}
			if err := list.RemoveInstance(instance); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), list.ID())
			return err
		})
	})
}

func newManifestRMCommand() *cobra.Command {
	var ignore bool
	cmd := imageIOCommand("rm <list>...", "Remove local manifest lists", cobra.MinimumNArgs(1), func(cmd *cobra.Command, args []string) error {
		return withManifestStore(cmd, oci.Options{}, true, func(_ storage.Store, native *libimage.Runtime) error {
			reports, errs := native.RemoveImages(cmd.Context(), args, &libimage.RemoveImagesOptions{LookupManifest: true, Ignore: ignore})
			return imageRemovalError(errors.Join(writeImageRemovalReports(cmd.OutOrStdout(), reports), errors.Join(errs...)))
		})
	})
	cmd.Flags().BoolVarP(&ignore, "ignore", "i", false, "ignore missing manifest lists")
	return cmd
}

func newManifestExistsCommand() *cobra.Command {
	return imageIOCommand("exists <list>", "Check whether a manifest list exists in the local store", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		absent := false
		err := withManifestStore(cmd, oci.Options{}, false, func(_ storage.Store, native *libimage.Runtime) error {
			_, err := native.LookupManifestList(args[0])
			if errors.Is(err, storage.ErrImageUnknown) || errors.Is(err, libimage.ErrNotAManifestList) {
				absent = true
				return nil
			}
			return err
		})
		if err != nil {
			return commandExitError{Code: 125, Err: err}
		}
		if absent {
			return commandExitError{Code: 1}
		}
		return nil
	})
}
