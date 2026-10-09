package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"coopr/internal/buildah"
	"coopr/internal/componentstore"
	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"go.podman.io/common/libimage"
	"go.podman.io/image/v5/manifest"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/storage"
)

func newImageCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "image", Short: "Manage locally stored container images", RunE: helpOnNoArgs}
	cmd.AddCommand(newImageListCommand("ls"), newImageInspectCommand(), newImageRemoveCommand(), newPruneCommand(false))
	cmd.AddCommand(newImagePullCommand(), newImagePushCommand(), newImageTagCommand(), newImageSaveCommand(), newImageLoadCommand(), newImageExistsCommand(), newImageHistoryCommand())
	return cmd
}

func newImagesCommand() *cobra.Command { return newImageListCommand("images") }

type nativeImageEntry struct {
	Reference string
	Root      v1.Descriptor
	Platforms map[string]oci.StoredSelection
}

func nativeImageEntries(ctx context.Context, options buildah.StoreOptions) (entries []nativeImageEntry, retErr error) {
	activity, err := storeactivity.AcquireShared(ctx, buildah.ActivityRoots(options, "")...)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	entries = []nativeImageEntry{}
	retErr = buildah.WithStore(options, func(backend storage.Store) error {
		images, err := backend.Images()
		if err != nil {
			return err
		}
		var names []string
		for _, image := range images {
			for _, name := range image.Names {
				if !strings.HasPrefix(name, "coopr.internal/") && !strings.Contains(name, "@") {
					names = append(names, name)
				}
			}
		}
		slices.Sort(names)
		for _, name := range names {
			root, _, selections, err := oci.StoredImageSelections(ctx, backend, name)
			if errors.Is(err, oci.ErrStoredPlatformUnavailable) {
				continue
			}
			if err != nil {
				return err
			}
			matched, err := oci.StoredImageName(backend, name)
			if err != nil {
				return err
			}
			entries = append(entries, nativeImageEntry{Reference: matched, Root: root, Platforms: selections})
		}
		return nil
	})
	return entries, retErr
}

func newImageListCommand(use string) *cobra.Command {
	return &cobra.Command{Use: use, Annotations: map[string]string{nativeStorageAnnotation: "true"}, Short: "List locally named container images", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		entries, err := nativeImageEntries(cmd.Context(), store)
		if err != nil {
			return err
		}
		output := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(output, "NAME\tDIGEST\tPLATFORMS")
		for _, entry := range entries {
			keys := make([]string, 0, len(entry.Platforms))
			for key := range entry.Platforms {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			_, _ = fmt.Fprintf(output, "%s\t%s\t%s\n", entry.Reference, entry.Root.Digest, strings.Join(keys, ","))
		}
		return output.Flush()
	}}
}

func newImageInspectCommand() *cobra.Command {
	return &cobra.Command{Use: "inspect <name>", Annotations: map[string]string{nativeStorageAnnotation: "true"}, Short: "Inspect a local container image", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		activity, err := storeactivity.AcquireShared(cmd.Context(), buildah.ActivityRoots(store, "")...)
		if err != nil {
			return err
		}
		inspectErr := buildah.WithStore(store, func(backend storage.Store) error {
			root, rootData, selections, err := oci.StoredImageSelections(cmd.Context(), backend, args[0])
			if err != nil {
				return err
			}
			if root.MediaType == v1.MediaTypeImageIndex || root.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
				return writeJSON(cmd, json.RawMessage(rootData))
			}
			var selected oci.StoredSelection
			for _, selection := range selections {
				selected = selection
			}
			runtime, err := libimage.RuntimeFromStore(backend, nil)
			if err != nil {
				return err
			}
			image, _, err := runtime.LookupImage(selected.ImageID, nil)
			if err != nil {
				return err
			}
			data, err := image.Inspect(cmd.Context(), &libimage.InspectOptions{WithSize: true})
			if err != nil {
				return err
			}
			// libimage inspects the record's current manifest. Formats sharing
			// a config share its ID; retain the selected manifest's five fields.
			data.Digest = root.Digest
			data.ManifestType = root.MediaType
			data.Annotations = map[string]string{}
			data.Comment = ""
			data.HealthCheck = nil
			switch root.MediaType {
			case v1.MediaTypeImageManifest:
				ref, err := imagestorage.Transport.NewStoreReference(backend, nil, selected.ImageID)
				if err != nil {
					return err
				}
				source, err := ref.NewImageSource(cmd.Context(), nil)
				if err != nil {
					return err
				}
				raw, _, readErr := source.GetManifest(cmd.Context(), &root.Digest)
				if err := errors.Join(readErr, source.Close()); err != nil {
					return err
				}
				var selectedManifest v1.Manifest
				if err := json.Unmarshal(raw, &selectedManifest); err != nil {
					return err
				}
				if selectedManifest.Annotations != nil {
					data.Annotations = selectedManifest.Annotations
				}
				if len(data.History) > 0 {
					data.Comment = data.History[0].Comment
				}
			case manifest.DockerV2Schema2MediaType:
				var config manifest.Schema2V1Image
				if err := json.Unmarshal(selected.ConfigData, &config); err != nil {
					return err
				}
				data.Comment = config.Comment
				data.HealthCheck = config.ContainerConfig.Healthcheck
				if data.HealthCheck == nil && config.Config != nil {
					data.HealthCheck = config.Config.Healthcheck
				}
			}
			return writeJSON(cmd, data)
		})
		return errors.Join(inspectErr, activity.Close())
	}}
}

func newImageRemoveCommand() *cobra.Command {
	return &cobra.Command{Use: "rm <name>...", Annotations: map[string]string{nativeStorageAnnotation: "true"}, Short: "Remove local container image names", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		options, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		return withExclusiveStoreActivity(cmd.Context(), buildah.ActivityRoots(options, ""), func() error {
			return buildah.WithStore(options, func(backend storage.Store) error {
				store := imagestore.FromStore(backend)
				for _, arg := range args {
					name, err := store.MatchedName(arg)
					if err != nil {
						return err
					}
					removed, err := store.RemoveTag(name)
					if err != nil {
						return err
					}
					if !removed {
						return fmt.Errorf("local image %q not found", arg)
					}
					if _, err := fmt.Fprintln(cmd.OutOrStdout(), name); err != nil {
						return err
					}
				}
				return nil
			})
		})
	}}
}

func newComponentListCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: "List locally named components", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := componentstore.DefaultDir()
			if err != nil {
				return err
			}
			entries, err := localstore.List(cmd.Context(), dir)
			if err != nil {
				return err
			}
			output := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(output, "NAME\tDIGEST")
			for _, entry := range entries {
				if entry.Reference != "" {
					_, _ = fmt.Fprintf(output, "%s\t%s\n", entry.Reference, entry.Descriptor.Digest)
				}
			}
			return output.Flush()
		},
	}
}

func newComponentInspectCommand() *cobra.Command {
	return &cobra.Command{
		Use: "inspect <name-or-digest>", Short: "Inspect a local component", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := componentstore.DefaultDir()
			if err != nil {
				return err
			}
			descriptor, found, err := localstore.Inspect(cmd.Context(), dir, strings.TrimPrefix(args[0], "local:"))
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("local component %q not found", args[0])
			}
			return writeJSON(cmd, descriptor)
		},
	}
}

func newComponentRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use: "rm <name>...", Short: "Remove local component names", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := componentstore.DefaultDir()
			if err != nil {
				return err
			}
			return withExclusiveStoreActivity(cmd.Context(), []string{dir}, func() error {
				for _, name := range args {
					if err := componentstore.ValidateTag(name); err != nil {
						return err
					}
					removed, err := localstore.RemoveTag(cmd.Context(), dir, name)
					if err != nil {
						return err
					}
					if !removed {
						return fmt.Errorf("local component %q not found", name)
					}
					if _, err := fmt.Fprintln(cmd.OutOrStdout(), name); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}

func newSystemCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "system", Short: "Manage Coopr local storage", RunE: helpOnNoArgs}
	cmd.AddCommand(newSystemDFCommand(), newPruneCommand(true))
	return cmd
}

func newSystemDFCommand() *cobra.Command {
	return &cobra.Command{
		Use: "df", Short: "Show local image, component, and build storage usage", Args: cobra.NoArgs,
		Annotations: map[string]string{nativeStorageAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			usage, err := buildah.MaintainStoreSupervised(cmd.Context(), buildah.StoreMaintenanceRequest{Store: store, Mode: buildah.StoreMaintenanceDF})
			if err != nil {
				return err
			}
			componentDir, err := componentstore.DefaultDir()
			if err != nil {
				return err
			}
			components, err := localstore.DiskUsage(cmd.Context(), componentDir)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "TYPE\tCOUNT\tBYTES"); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "images\t%d\t%d\ncomponents\t%d\t%d\nbuild-cache\t%d\t%d\n", usage.Images, usage.Bytes, components.Roots, components.Bytes, usage.CacheImages, usage.CacheBytes)
			return err
		},
	}
}

func newPruneCommand(pruneComponents bool) *cobra.Command {
	request := buildah.StoreMaintenanceRequest{Mode: buildah.StoreMaintenancePrune, ActivityLeaseHeld: true}
	short := "Prune dangling images and unused instruction snapshots"
	if pruneComponents {
		short += " and component content"
	}
	cmd := &cobra.Command{
		Use: "prune", Short: short, Args: cobra.NoArgs,
		Annotations: map[string]string{nativeStorageAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			request.Store = store
			componentDir := ""
			if pruneComponents {
				componentDir, err = componentstore.DefaultDir()
				if err != nil {
					return err
				}
			}
			return withExclusiveStoreActivity(cmd.Context(), buildah.ActivityRoots(store, componentDir), func() error {
				result, err := buildah.MaintainStoreSupervised(cmd.Context(), request)
				if err != nil {
					return err
				}
				componentRoots := 0
				if pruneComponents {
					componentRoots, err = localstore.PruneUnderActivityLease(cmd.Context(), componentDir, request.DryRun)
					if err != nil {
						return err
					}
				}
				label, count := "images", result.RemovedImages
				if request.DryRun {
					label, count = "image-candidates", result.PrunableImages
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s=%d", label, count); err != nil {
					return err
				}
				if pruneComponents {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), " component-roots=%d", componentRoots); err != nil {
						return err
					}
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), " build-cache=%t dry-run=%t\n", request.All || request.BuildCache, request.DryRun)
				return err
			})
		},
	}
	cmd.Flags().BoolVarP(&request.All, "all", "a", false, "remove all unused images, including named images, and RUN cache mounts")
	if !pruneComponents {
		cmd.Flags().BoolVar(&request.BuildCache, "build-cache", false, "also remove persistent RUN cache mounts")
	}
	cmd.Flags().BoolVar(&request.DryRun, "dry-run", false, "preview initial candidates without removing data; recursive pruning may remove additional images")
	return cmd
}

func withExclusiveStoreActivity(ctx context.Context, roots []string, action func() error) (retErr error) {
	activity, err := storeactivity.AcquireExclusive(ctx, roots...)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	return action()
}

func helpOnNoArgs(cmd *cobra.Command, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
	}
	return cmd.Help()
}

func writeJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
