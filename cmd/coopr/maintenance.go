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
	"coopr/internal/imagecatalog"
	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"
	"github.com/containerd/platforms"
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

func newImageListCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: "List locally named container images", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, dir, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			var nativeNames map[string]bool
			if store.Shared {
				if err := refreshSharedNativeCatalog(cmd.Context(), store, dir, ""); err != nil {
					return err
				}
				nativeNames, err = sharedNativeNames(store)
				if err != nil {
					return err
				}
			}
			entries, err := imagecatalog.List(cmd.Context(), dir)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "NAME\tDIGEST\tPLATFORMS")
			for _, entry := range entries {
				if store.Shared && !nativeNames[entry.Reference] {
					continue
				}
				platforms := make([]string, 0, len(entry.Platforms))
				for platform := range entry.Platforms {
					platforms = append(platforms, platform)
				}
				slices.Sort(platforms)
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", entry.Reference, entry.Root.Digest, strings.Join(platforms, ","))
			}
			return nil
		},
	}
}

func newImageInspectCommand() *cobra.Command {
	return &cobra.Command{
		Use: "inspect <name>", Short: "Inspect a locally named container image", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := localstore.NormalizeImageTag(args[0])
			if err != nil {
				return err
			}
			store, dir, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			if store.Shared {
				if err := refreshSharedNativeCatalog(cmd.Context(), store, dir, args[0]); err != nil && !errors.Is(err, storage.ErrImageUnknown) {
					return err
				}
			}
			entries, err := imagecatalog.List(cmd.Context(), dir)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if entry.Reference == name {
					return writeJSON(cmd, entry)
				}
			}
			return fmt.Errorf("local image %q not found", name)
		},
	}
}

func newImageRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use: "rm <name>...", Short: "Remove local container image names", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			storeOptions, dir, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			return withExclusiveStoreActivity(cmd.Context(), []string{dir}, func() error {
				for _, arg := range args {
					name, err := localstore.NormalizeImageTag(arg)
					if err != nil {
						return err
					}
					entries, err := imagecatalog.List(cmd.Context(), dir)
					if err != nil {
						return err
					}
					cataloged := slices.ContainsFunc(entries, func(entry imagecatalog.Entry) bool { return entry.Reference == name })
					namesToRemove := catalogNamesForNative(name)
					nativeRemoved := false
					if storeOptions.Shared {
						store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(storeOptions))
						if err != nil {
							return err
						}
						matched, matchErr := store.MatchedName(name)
						if matchErr == nil {
							for _, alias := range catalogNamesForNative(matched) {
								if !slices.Contains(namesToRemove, alias) {
									namesToRemove = append(namesToRemove, alias)
								}
							}
						} else if !errors.Is(matchErr, storage.ErrImageUnknown) {
							_ = store.Close()
							return matchErr
						}
						var removeErr error
						removeName := name
						if matched != "" {
							removeName = matched
						}
						nativeRemoved, removeErr = store.RemoveTag(removeName)
						closeErr := store.Close()
						if err := errors.Join(removeErr, closeErr); err != nil {
							return fmt.Errorf("remove shared-store image name %q: %w", name, err)
						}
					}
					if !cataloged && !nativeRemoved {
						return fmt.Errorf("local image %q not found", name)
					}
					removed := false
					for _, alias := range namesToRemove {
						aliasRemoved, err := imagecatalog.RemoveReference(cmd.Context(), dir, alias)
						if err != nil {
							return err
						}
						removed = removed || aliasRemoved
					}
					if !removed && !nativeRemoved {
						return fmt.Errorf("local image %q not found", name)
					}
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
				}
				return nil
			})
		},
	}
}

func refreshSharedNativeCatalog(ctx context.Context, options buildah.StoreOptions, dir, requested string) (retErr error) {
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			if errors.Is(retErr, storage.ErrImageUnknown) {
				retErr = closeErr
			} else {
				retErr = errors.Join(retErr, closeErr)
			}
		}
	}()
	names := []string{requested}
	var requestedAliases []string
	if requested == "" {
		names, err = store.Names()
		if err != nil {
			return err
		}
	} else {
		requestedAliases = catalogNamesForNative(requested)
		matched, matchErr := store.MatchedName(requested)
		if matchErr == nil {
			names = []string{matched}
		} else if !errors.Is(matchErr, storage.ErrImageUnknown) {
			return matchErr
		}
	}
	known := make(map[string]bool, len(names)*2)
	for _, name := range names {
		selectors := catalogNamesForNative(name)
		if requested != "" {
			selectors = append(selectors, requestedAliases...)
			slices.Sort(selectors)
			selectors = slices.Compact(selectors)
		}
		for _, selector := range selectors {
			known[selector] = true
		}
	}
	if requested == "" {
		entries, err := imagecatalog.List(ctx, dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !known[entry.Reference] {
				if _, err := imagecatalog.RemoveReference(ctx, dir, entry.Reference); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range names {
		if strings.HasPrefix(name, "coopr.internal/") {
			continue
		}
		available, err := store.Platforms(ctx, name)
		if err != nil {
			if requested != "" && errors.Is(err, storage.ErrImageUnknown) {
				selectors := append(catalogNamesForNative(name), requestedAliases...)
				slices.Sort(selectors)
				for _, selector := range slices.Compact(selectors) {
					if selector != "" {
						if _, cleanupErr := imagecatalog.RemoveReference(ctx, dir, selector); cleanupErr != nil {
							return cleanupErr
						}
					}
				}
			}
			if requested == "" && errors.Is(err, oci.ErrStoredPlatformUnavailable) {
				continue // A foreign-only manifest list is still valid Podman state.
			}
			return err
		}
		selections := make(map[string]imagecatalog.Selection, len(available))
		var root v1.Descriptor
		for _, platform := range available {
			resolved, err := store.Resolve(ctx, name, platform)
			if err != nil {
				return err
			}
			selection := imagecatalog.Selection{Root: resolved.Root, Manifest: resolved.Selected, ImageID: resolved.StorageImageID, ConfigData: resolved.ConfigData}
			root = resolved.Root
			selections[platforms.Format(platforms.Normalize(resolved.Platform))] = selection
		}
		indexRoot, indexData, isIndex, err := store.Index(ctx, name)
		if err != nil {
			return err
		}
		selectors := append(catalogNamesForNative(name), requestedAliases...)
		slices.Sort(selectors)
		selectors = slices.Compact(selectors)
		for _, selector := range selectors {
			if isIndex {
				if root.Digest != indexRoot.Digest {
					return fmt.Errorf("shared-store image %q index changed while refreshing", name)
				}
				if err := imagecatalog.CommitIndex(ctx, dir, selector, indexRoot, indexData, selections); err != nil {
					return err
				}
				continue
			}
			for key, selection := range selections {
				platform, err := platforms.Parse(key)
				if err != nil {
					return err
				}
				if err := imagecatalog.Commit(ctx, dir, selector, platform, selection); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func sharedNativeNames(options buildah.StoreOptions) (map[string]bool, error) {
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		return nil, err
	}
	names, namesErr := store.Names()
	closeErr := store.Close()
	if err := errors.Join(namesErr, closeErr); err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, "coopr.internal/") {
			result[name] = true
		}
	}
	return result, nil
}

func catalogNamesForNative(name string) []string {
	result := []string{name}
	if normalized, err := localstore.NormalizeImageTag(name); err == nil && normalized != name {
		result = append(result, normalized)
	}
	if short := strings.TrimPrefix(name, "localhost/"); short != name {
		result = append(result, short)
	}
	if native, err := imagestore.NormalizeTag(name); err == nil && native != name {
		result = append(result, native)
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func addComponentMaintenanceCommands(cmd *cobra.Command) {
	cmd.AddCommand(newComponentListCommand("ls"), newComponentInspectCommand(), newComponentRemoveCommand())
}

func newComponentsCommand() *cobra.Command { return newComponentListCommand("components") }

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
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, _, err := commandStorage(cmd)
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
		Use: "prune", Short: "Remove unnamed and unused local image and component content", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			imageDir, err := commandImageDirectory(cmd)
			if err != nil {
				return err
			}
			componentDir, err := componentstore.DefaultDir()
			if err != nil {
				return err
			}
			store, _, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			return withExclusiveStoreActivity(cmd.Context(), []string{imageDir, componentDir}, func() error {
				protected, err := imagecatalog.NamedImageIDs(cmd.Context(), imageDir)
				if err != nil {
					return err
				}
				ids := make([]string, 0, len(protected))
				for id := range protected {
					ids = append(ids, id)
				}
				result, err := buildah.MaintainStoreSupervised(cmd.Context(), buildah.StoreMaintenanceRequest{Store: store, Mode: buildah.StoreMaintenancePrune, ProtectedImageIDs: ids, DryRun: dryRun, ActivityLeaseHeld: true})
				if err != nil {
					return err
				}
				retained := make(map[string]bool, len(result.RetainedImageIDs))
				for _, id := range result.RetainedImageIDs {
					retained[id] = true
				}
				catalogRoots, err := imagecatalog.PruneMissing(cmd.Context(), imageDir, retained, dryRun)
				if err != nil {
					return err
				}
				componentRoots, err := localstore.PruneUnderActivityLease(cmd.Context(), componentDir, dryRun)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "images=%d catalog-roots=%d component-roots=%d skipped-in-use=%d dry-run=%t\n", result.RemovedImages, catalogRoots, componentRoots, result.SkippedInUse, dryRun)
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
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, _, err := commandStorage(cmd)
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
		Use: "prune", Short: "Remove local instruction cache aliases and unused images", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			imageDir, err := commandImageDirectory(cmd)
			if err != nil {
				return err
			}
			store, _, err := commandStorage(cmd)
			if err != nil {
				return err
			}
			return withExclusiveStoreActivity(cmd.Context(), []string{imageDir}, func() error {
				protected, err := imagecatalog.NamedImageIDs(cmd.Context(), imageDir)
				if err != nil {
					return err
				}
				ids := make([]string, 0, len(protected))
				for id := range protected {
					ids = append(ids, id)
				}
				result, err := buildah.MaintainStoreSupervised(cmd.Context(), buildah.StoreMaintenanceRequest{Store: store, Mode: buildah.StoreMaintenanceCachePrune, ProtectedImageIDs: ids, DryRun: dryRun, ActivityLeaseHeld: true})
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
