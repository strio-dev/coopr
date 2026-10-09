package main

import (
	"context"
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
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports"
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
	cmd := imageIOCommand("create <list> [image...]", "Create a local manifest list", cobra.MinimumNArgs(1), func(cmd *cobra.Command, args []string) error {
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
			_, err = fmt.Fprintln(cmd.OutOrStdout(), list.ID())
			return err
		})
	})
	registry.addTo(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "add every instance from source manifest lists")
	cmd.Flags().BoolVarP(&amend, "amend", "a", false, "add to an existing local list")
	return cmd
}

func newManifestAddCommand() *cobra.Command {
	var registry registryFlags
	var all bool
	cmd := imageIOCommand("add <list> <image>...", "Add images to a local manifest list", cobra.MinimumNArgs(2), func(cmd *cobra.Command, args []string) error {
		return withManifestStore(cmd, registry.resolverOptions(cmd), true, func(backend storage.Store, native *libimage.Runtime) error {
			list, err := native.LookupManifestList(args[0])
			if err != nil {
				return err
			}
			for _, name := range args[1:] {
				if _, err := addManifestMember(cmd, backend, list, name, all, registry.resolverOptions(cmd)); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), list.ID())
			return err
		})
	})
	registry.addTo(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "add every instance from source manifest lists")
	return cmd
}

func newManifestAnnotateCommand() *cobra.Command {
	var options libimage.ManifestListAnnotateOptions
	var annotations []string
	var index bool
	cmd := imageIOCommand("annotate <list> [instance-digest]", "Annotate a local manifest list or an instance", cobra.RangeArgs(1, 2), func(cmd *cobra.Command, args []string) error {
		var instance digest.Digest
		if len(args) == 2 {
			var err error
			instance, err = digest.Parse(args[1])
			if err != nil {
				return err
			}
		}
		if len(args) == 1 && !index && options.Subject == "" {
			return errors.New("annotate requires an instance digest, --index, or --subject")
		}
		if instance == "" && (options.OS != "" || options.Architecture != "" || options.Variant != "" || options.OSVersion != "" || len(options.OSFeatures) > 0 || len(options.Features) > 0) {
			return errors.New("platform annotations require an instance digest")
		}

		options.Annotations = map[string]string{}
		for _, annotation := range annotations {
			key, value, ok := strings.Cut(annotation, "=")
			if !ok || key == "" {
				return fmt.Errorf("invalid annotation %q (expected KEY=VALUE)", annotation)
			}
			options.Annotations[key] = value
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
	flags.StringVar(&options.OS, "os", "", "set instance operating system")
	flags.StringVar(&options.Architecture, "arch", "", "set instance architecture")
	flags.StringVar(&options.Variant, "variant", "", "set instance variant")
	flags.StringVar(&options.OSVersion, "os-version", "", "set instance OS version")
	flags.StringSliceVar(&options.OSFeatures, "os-features", nil, "set instance OS features")
	flags.StringSliceVar(&options.Features, "features", nil, "set instance CPU features")
	flags.StringArrayVar(&annotations, "annotation", nil, "set instance annotation KEY=VALUE (repeatable)")
	return cmd
}

func newManifestInspectCommand() *cobra.Command {
	return imageIOCommand("inspect <list>", "Inspect a local manifest list", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return withManifestStore(cmd, oci.Options{}, false, func(_ storage.Store, native *libimage.Runtime) error {
			list, err := native.LookupManifestList(args[0])
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
}

func newManifestPushCommand() *cobra.Command {
	var registry registryFlags
	var signing signingFlags
	var all, quiet bool
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
		options.Signing = signing.options()
		if err := transfer.ValidateSigningDestination(oci.Image, destination, options.Signing); err != nil {
			return err
		}
		options.BuildStore, err = commandStorage(cmd)
		if err != nil {
			return err
		}
		progress := cmd.ErrOrStderr()
		if quiet {
			progress = nil
		}
		pushed, err := transfer.PushManifestList(cmd.Context(), args[0], destination, options, all, progress)
		if err != nil {
			return err
		}
		if digestFile != "" {
			if err := os.WriteFile(digestFile, []byte(pushed.String()), 0o644); err != nil {
				return err
			}
		}
		if !quiet {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), pushed)
		}
		return err
	})
	registry.addTo(cmd)
	signing.addTo(cmd)
	addSignaturePolicyFlag(cmd.Flags())
	cmd.Flags().BoolVar(&all, "all", true, "push every image in the manifest list")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress push progress and output")
	cmd.Flags().StringVar(&digestFile, "digestfile", "", "write pushed manifest digest to a file")
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
	return imageIOCommand("rm <list>...", "Remove local manifest lists", cobra.MinimumNArgs(1), func(cmd *cobra.Command, args []string) error {
		return withManifestStore(cmd, oci.Options{}, true, func(_ storage.Store, native *libimage.Runtime) error {
			for _, name := range args {
				if _, err := native.LookupManifestList(name); err != nil {
					return err
				}
			}
			reports, errs := native.RemoveImages(cmd.Context(), args, &libimage.RemoveImagesOptions{LookupManifest: true})
			for _, report := range reports {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), report.ID); err != nil {
					return err
				}
			}
			return errors.Join(errs...)
		})
	})
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
