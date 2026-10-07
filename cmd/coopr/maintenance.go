package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"coopr/internal/buildah"
	"coopr/internal/componentstore"
	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"go.podman.io/storage"
)

func newImageCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "image", Short: "Manage locally stored container images", RunE: helpOnNoArgs}
	cmd.AddCommand(newImageListCommand("ls"), newImageInspectCommand(), newImageRemoveCommand())
	return cmd
}

func newImagesCommand() *cobra.Command { return newImageListCommand("images") }

type nativeImageEntry struct {
	Reference string                         `json:"reference"`
	Root      v1.Descriptor                  `json:"root"`
	Platforms map[string]oci.StoredSelection `json:"platforms"`
}

func nativeImageEntries(ctx context.Context, options buildah.StoreOptions, requested string) (entries []nativeImageEntry, retErr error) {
	activity, err := storeactivity.AcquireShared(ctx, buildah.ActivityRoots(options, "")...)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	entries = []nativeImageEntry{}
	retErr = buildah.WithStore(options, func(backend storage.Store) error {
		names := []string{requested}
		if requested == "" {
			images, err := backend.Images()
			if err != nil {
				return err
			}
			names = nil
			for _, image := range images {
				for _, name := range image.Names {
					if !strings.HasPrefix(name, "coopr.internal/") && !strings.Contains(name, "@") {
						names = append(names, name)
					}
				}
			}
			slices.Sort(names)
		}
		for _, name := range names {
			root, _, selections, err := oci.StoredImageSelections(ctx, backend, name)
			if requested == "" && errors.Is(err, oci.ErrStoredPlatformUnavailable) {
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
		entries, err := nativeImageEntries(cmd.Context(), store, "")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "NAME\tDIGEST\tPLATFORMS")
		for _, entry := range entries {
			keys := make([]string, 0, len(entry.Platforms))
			for key := range entry.Platforms {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", entry.Reference, entry.Root.Digest, strings.Join(keys, ","))
		}
		return nil
	}}
}

func newImageInspectCommand() *cobra.Command {
	return &cobra.Command{Use: "inspect <name>", Annotations: map[string]string{nativeStorageAnnotation: "true"}, Short: "Inspect a local container image", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		entries, err := nativeImageEntries(cmd.Context(), store, args[0])
		if err != nil {
			return err
		}
		return writeJSON(cmd, entries[0])
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
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
				}
				return nil
			})
		})
	}}
}

func newComponentListCommand() *cobra.Command {
	return &cobra.Command{
		Use: "ls", Short: "List locally named components", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := componentstore.DefaultDir()
			if err != nil {
				return err
			}
			entries, err := localstore.List(cmd.Context(), dir)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "NAME\tDIGEST")
			for _, entry := range entries {
				if entry.Reference != "" {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", entry.Reference, entry.Descriptor.Digest)
				}
			}
			return nil
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
			descriptor, found, err := localstore.Inspect(cmd.Context(), dir, args[0])
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
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
				}
				return nil
			})
		},
	}
}

func newSystemCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "system", Short: "Manage Coopr local storage", RunE: helpOnNoArgs}
	cmd.AddCommand(newSystemDFCommand(), newSystemPruneCommand())
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
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "TYPE\tCOUNT\tBYTES")
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "images\t%d\t%d\ncomponents\t%d\t%d\nbuild-cache\t%d\t%d\n", usage.Images, usage.Bytes, components.Roots, components.Bytes, usage.CacheImages, usage.CacheBytes)
			return nil
		},
	}
}

func newSystemPruneCommand() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use: "prune", Short: "Prune unnamed component content; retain shared native images", Args: cobra.NoArgs,
		Annotations: map[string]string{nativeStorageAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			componentDir, err := componentstore.DefaultDir()
			if err != nil {
				return err
			}
			store, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			return withExclusiveStoreActivity(cmd.Context(), buildah.ActivityRoots(store, componentDir), func() error {
				result, err := buildah.MaintainStoreSupervised(cmd.Context(), buildah.StoreMaintenanceRequest{Store: store, Mode: buildah.StoreMaintenancePrune, DryRun: dryRun, ActivityLeaseHeld: true})
				if err != nil {
					return err
				}
				componentRoots, err := localstore.PruneUnderActivityLease(cmd.Context(), componentDir, dryRun)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "images=%d component-roots=%d skipped-in-use=%d dry-run=%t\n", result.RemovedImages, componentRoots, result.SkippedInUse, dryRun)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be removed")
	return cmd
}

func newCacheCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "cache", Short: "Manage the local instruction cache", RunE: helpOnNoArgs}
	cmd.AddCommand(newCacheDFCommand(), newCachePruneCommand())
	return cmd
}

func newCacheDFCommand() *cobra.Command {
	return &cobra.Command{
		Use: "df", Short: "Show local instruction cache usage", Args: cobra.NoArgs,
		Annotations: map[string]string{nativeStorageAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			result, err := buildah.MaintainStoreSupervised(cmd.Context(), buildah.StoreMaintenanceRequest{Store: store, Mode: buildah.StoreMaintenanceDF})
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "IMAGES\tBYTES\n%d\t%d\n", result.CacheImages, result.CacheBytes)
			return nil
		},
	}
}

func newCachePruneCommand() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use: "prune", Short: "Remove local instruction cache aliases; retain native image bytes", Args: cobra.NoArgs,
		Annotations: map[string]string{nativeStorageAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			return withExclusiveStoreActivity(cmd.Context(), buildah.ActivityRoots(store, ""), func() error {
				result, err := buildah.MaintainStoreSupervised(cmd.Context(), buildah.StoreMaintenanceRequest{Store: store, Mode: buildah.StoreMaintenanceCachePrune, DryRun: dryRun, ActivityLeaseHeld: true})
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "aliases=%d images=%d skipped-in-use=%d dry-run=%t\n", result.RemovedCacheAliases, result.RemovedImages, result.SkippedInUse, dryRun)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be removed")
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
