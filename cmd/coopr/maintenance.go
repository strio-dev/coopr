package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"coopr/internal/buildah"
	"coopr/internal/componentstore"
	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"
	"github.com/containerd/platforms"
	"github.com/docker/go-units"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"go.podman.io/common/libimage"
	"go.podman.io/common/pkg/report"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func newImageCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "image", Short: "Manage locally stored container images", RunE: helpOnNoArgs}
	cmd.AddCommand(newImageListCommand("ls"), newImageInspectCommand(), newImageRemoveCommand(), newPruneCommand(false))
	cmd.AddCommand(newImagePullCommand(), newImagePushCommand(), newImageTagCommand(), newImageSaveCommand(), newImageLoadCommand(), newImageExistsCommand(), newImageHistoryCommand())
	return cmd
}

func newImagesCommand() *cobra.Command { return newImageListCommand("images") }

type imageListOptions struct {
	quiet, all, digests, noTrunc, noheading bool
	format, sort                            string
	filters                                 []string
}

type imageListRow struct {
	Repository  string
	Tag         string
	Digest      string
	Names       []string
	ReadOnly    bool
	Labels      map[string]string
	Containers  int
	ParentId    string
	RepoDigests []string
	Dangling    bool
	history     []string
	fullID      string
	created     time.Time
	size        int64
	noTrunc     bool
}

func (i imageListRow) ID() string {
	if i.noTrunc {
		return "sha256:" + i.fullID
	}
	return i.fullID[:min(12, len(i.fullID))]
}
func (i imageListRow) History() string      { return strings.Join(i.history, ", ") }
func (i imageListRow) Created() string      { return units.HumanDuration(time.Since(i.created)) + " ago" }
func (i imageListRow) CreatedSince() string { return i.Created() }
func (i imageListRow) CreatedAt() string    { return i.created.UTC().String() }
func (i imageListRow) CreatedTime() string  { return i.CreatedAt() }
func (i imageListRow) Size() string {
	s := units.HumanSizeWithPrecision(float64(i.size), 3)
	n := strings.LastIndexFunc(s, unicode.IsNumber)
	return s[:n+1] + " " + s[n+1:]
}
func (i imageListRow) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID          string `json:"Id"`
		ParentId    string
		Repository  string
		Tag         string
		RepoDigests []string
		Names       []string
		Digest      string
		History     []string `json:",omitempty"`
		Dangling    bool     `json:",omitempty"`
		Created     int64
		CreatedAt   string
		Size        int64
		SharedSize  int
		VirtualSize int64
		Labels      map[string]string
		Containers  int
		ReadOnly    bool `json:",omitempty"`
	}{ID: i.fullID, ParentId: i.ParentId, Repository: i.Repository, Tag: i.Tag, RepoDigests: i.RepoDigests, Names: i.Names, Digest: i.Digest, History: i.history, Dangling: i.Dangling, Created: i.created.Unix(), CreatedAt: i.created.UTC().Format(time.RFC3339Nano), Size: i.size, VirtualSize: i.size, Labels: i.Labels, Containers: i.Containers, ReadOnly: i.ReadOnly})
}

func newImageListCommand(use string) *cobra.Command {
	options := imageListOptions{sort: "created"}
	cmd := &cobra.Command{Use: use + " [IMAGE]", Annotations: map[string]string{nativeStorageAnnotation: "true"}, Short: "List locally stored container images", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 && len(options.filters) > 0 {
			return errors.New("cannot specify an image and filters")
		}
		if !slices.Contains([]string{"created", "id", "repository", "size", "tag"}, options.sort) {
			return fmt.Errorf("invalid sort value %q", options.sort)
		}
		if options.format != "" && !report.IsJSON(options.format) {
			if _, err := report.New(io.Discard, "images").Parse(report.OriginUser, options.format); err != nil {
				return err
			}
		}
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		activity, err := storeactivity.AcquireShared(cmd.Context(), buildah.ActivityRoots(store, "")...)
		if err != nil {
			return err
		}
		listErr := buildah.WithStore(store, func(backend storage.Store) error {
			runtime, err := libimage.RuntimeFromStore(backend, nil)
			if err != nil {
				return err
			}
			filters := slices.Clone(options.filters)
			if !options.all && !slices.Contains(filters, "intermediate=true") {
				filters = append(filters, "intermediate=false")
			}
			if len(args) > 0 {
				filters = append(filters, "reference="+args[0])
			}
			images, err := runtime.ListImages(cmd.Context(), &libimage.ListImagesOptions{Filters: filters, SetListData: true})
			if err != nil {
				return err
			}
			rows := make([]imageListRow, 0)
			for _, image := range images {
				names := slices.DeleteFunc(slices.Clone(image.Names()), func(name string) bool { return strings.HasPrefix(name, "coopr.internal/") })
				if len(names) == 0 && (len(image.Names()) > 0 || image.StorageImage().Flags["coopr.selected-base"] != nil) {
					continue
				}
				refs := make([]reference.Named, 0, len(names))
				for _, name := range names {
					parsed, err := reference.ParseNormalizedNamed(name)
					if err != nil {
						return err
					}
					refs = append(refs, parsed)
				}
				pairs, err := libimage.ToNameTagPairs(refs)
				if err != nil {
					return err
				}
				size, err := image.Size()
				if err != nil {
					return err
				}
				labels, err := image.Labels(cmd.Context())
				if err != nil {
					return err
				}
				containers, err := image.Containers()
				if err != nil {
					return err
				}
				repoDigests, err := image.RepoDigests()
				if err != nil {
					return err
				}
				repoDigests = slices.DeleteFunc(repoDigests, func(name string) bool { return strings.HasPrefix(name, "coopr.internal/") })
				history := slices.DeleteFunc(slices.Clone(image.NamesHistory()), func(name string) bool { return strings.HasPrefix(name, "coopr.internal/") })
				parentID := ""
				if image.ListData.Parent != nil {
					parentID = image.ListData.Parent.ID()
				}
				dangling := image.ListData.IsDangling != nil && *image.ListData.IsDangling
				for _, pair := range pairs {
					rows = append(rows, imageListRow{ParentId: parentID, RepoDigests: repoDigests, Dangling: dangling, history: history, Repository: pair.Name, Tag: pair.Tag, Digest: image.Digest().String(), Names: names, ReadOnly: image.IsReadOnly(), Labels: labels, Containers: len(containers), fullID: image.ID(), created: image.Created(), size: size, noTrunc: options.noTrunc})
				}

			}
			slices.SortStableFunc(rows, func(a, b imageListRow) int {
				switch options.sort {
				case "id":
					return strings.Compare(a.ID(), b.ID())
				case "repository":
					if result := strings.Compare(a.Repository, b.Repository); result != 0 {
						return result
					}
					return strings.Compare(a.Tag, b.Tag)
				case "size":
					if a.size < b.size {
						return -1
					}
					if a.size > b.size {
						return 1
					}
					return 0
				case "tag":
					return strings.Compare(a.Tag, b.Tag)
				default:
					return b.created.Compare(a.created)
				}
			})
			if report.IsJSON(options.format) {
				return writeJSON(cmd, rows)
			}
			format := "{{range .}}{{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{.CreatedSince}}\t{{.Size}}\n{{end -}}"
			if options.digests {
				format = "{{range .}}{{.Repository}}\t{{.Tag}}\t{{.Digest}}\t{{.ID}}\t{{.CreatedSince}}\t{{.Size}}\n{{end -}}"
			}
			origin := report.OriginPodman
			if options.format != "" {
				format = options.format
				origin = report.OriginUser
			}
			if options.quiet {
				format = "{{range .}}{{.ID}}\n{{end -}}"
				origin = report.OriginUser
				seen := map[string]bool{}
				rows = slices.DeleteFunc(rows, func(row imageListRow) bool {
					if seen[row.fullID] {
						return true
					}
					seen[row.fullID] = true
					return false
				})
			}
			rpt, err := report.New(cmd.OutOrStdout(), "images").Parse(origin, format)
			if err != nil {
				return err
			}
			if rpt.RenderHeaders && !options.noheading {
				err = rpt.Execute(report.Headers(imageListRow{}, map[string]string{"ID": "IMAGE ID", "CreatedSince": "CREATED", "Created": "CREATED", "CreatedAt": "CREATED AT", "CreatedTime": "CREATED TIME", "Size": "SIZE", "History": "HISTORY"}))
			}
			if err == nil {
				err = rpt.Execute(rows)
			}
			return errors.Join(err, rpt.Flush())
		})
		return errors.Join(listErr, activity.Close())
	}}
	if use == "ls" {
		cmd.Aliases = []string{"list"}
	}
	flags := cmd.Flags()
	flags.BoolVarP(&options.quiet, "quiet", "q", false, "display only image IDs")
	flags.BoolVarP(&options.all, "all", "a", false, "show intermediate images")
	flags.BoolVar(&options.digests, "digests", false, "show image digests")
	flags.BoolVar(&options.noTrunc, "no-trunc", false, "display full image IDs")
	flags.BoolVarP(&options.noheading, "noheading", "n", false, "omit table headings")
	flags.StringVar(&options.format, "format", "", "format output using a Go template or JSON")
	flags.StringArrayVarP(&options.filters, "filter", "f", nil, "filter images by native image properties")
	flags.StringVar(&options.sort, "sort", "created", "sort by created, id, repository, size, or tag")
	return cmd
}

func newImageInspectCommand() *cobra.Command {
	var format string
	cmd := &cobra.Command{Use: "inspect <name>...", Annotations: map[string]string{nativeStorageAnnotation: "true"}, Short: "Inspect local container images", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if format != "" && !report.IsJSON(format) {
			if _, err := report.New(io.Discard, "inspect").Parse(report.OriginUser, format); err != nil {
				return err
			}
		}
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		activity, err := storeactivity.AcquireShared(cmd.Context(), buildah.ActivityRoots(store, "")...)
		if err != nil {
			return err
		}
		var results []*libimage.ImageData
		var inspectErrors []error
		inspectErr := buildah.WithStore(store, func(backend storage.Store) error {
			for _, arg := range args {
				data, err := inspectStoredImage(cmd.Context(), backend, arg)
				if err == nil {
					results = append(results, data)
				}
				if err != nil {
					inspectErrors = append(inspectErrors, err)
				}
			}
			if len(results) > 0 {
				if format == "" || report.IsJSON(format) {
					inspectErrors = append(inspectErrors, writeJSON(cmd, results))
				} else {
					rpt, err := report.New(cmd.OutOrStdout(), "inspect").Parse(report.OriginUser, format)
					if err == nil {
						err = errors.Join(rpt.Execute(results), rpt.Flush())
					}
					inspectErrors = append(inspectErrors, err)
				}
			}
			return errors.Join(inspectErrors...)
		})
		return errors.Join(inspectErr, activity.Close())
	}}
	cmd.Flags().StringVarP(&format, "format", "f", "", "format output using a Go template or JSON")
	return cmd
}

func inspectStoredImage(ctx context.Context, backend storage.Store, name string) (*libimage.ImageData, error) {
	indexRoot, indexData, indexed, err := oci.StoredImageIndex(ctx, backend, name)
	if err != nil {
		return nil, err
	}
	runtime, err := libimage.RuntimeFromStore(backend, nil)
	if err != nil {
		return nil, err
	}
	selectedName := name
	if indexed {
		_, digestErr := digest.Parse(name)
		_, namedDigest, _ := strings.Cut(name, "@")
		if digestErr != nil && namedDigest == "" {
			// A pulled single image can retain its remote index as provenance.
			// Mutable names still inspect the native image, including foreign ones.
			native, _, err := runtime.LookupImage(name, &libimage.LookupImageOptions{ManifestList: true})
			if err != nil {
				return nil, err
			}
			nativeList, err := native.IsManifestList(ctx)
			if err != nil {
				return nil, err
			}
			if !nativeList {
				selectedName = native.Digest().String()
				indexed = false
			}
		}
	}
	var instance manifest.ListUpdate
	if indexed {
		// Choose from the exact retained index, using the same platform
		// selection as libimage rather than the build planner's unique match.
		list, err := manifest.ListFromBlob(indexData, indexRoot.MediaType)
		if err != nil {
			return nil, err
		}
		digest, err := list.ChooseInstance(&types.SystemContext{})
		if err != nil {
			return nil, err
		}
		instance, err = list.Instance(digest)
		if err != nil {
			return nil, err
		}
		selectedName = digest.String()
	}
	available, err := oci.StoredImagePlatforms(ctx, backend, selectedName)
	if err != nil {
		return nil, err
	}
	platform := platforms.DefaultSpec()
	if len(available) == 1 {
		platform = available[0]
	}
	resolved, err := oci.ResolveStoredImage(ctx, backend, selectedName, platform)
	if err != nil {
		return nil, err
	}
	indexedManifest := resolved.Selected
	if resolved.SourceManifest != nil && resolved.SourceManifest.Digest == instance.Digest {
		indexedManifest = *resolved.SourceManifest
	}
	if indexed && (indexedManifest.Size != instance.Size || indexedManifest.MediaType != instance.MediaType) {
		return nil, fmt.Errorf("stored image %q selected manifest differs from its index", name)
	}
	root := resolved.Selected
	image, _, err := runtime.LookupImage(resolved.StorageImageID, nil)
	if err != nil {
		return nil, err
	}
	data, err := image.Inspect(ctx, &libimage.InspectOptions{WithSize: true})
	if err != nil {
		return nil, err
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
		ref, err := imagestorage.Transport.NewStoreReference(backend, nil, resolved.StorageImageID)
		if err != nil {
			return nil, err
		}
		source, err := ref.NewImageSource(ctx, nil)
		if err != nil {
			return nil, err
		}
		raw, _, readErr := source.GetManifest(ctx, &root.Digest)
		if err := errors.Join(readErr, source.Close()); err != nil {
			return nil, err
		}
		var selectedManifest v1.Manifest
		if err := json.Unmarshal(raw, &selectedManifest); err != nil {
			return nil, err
		}
		if selectedManifest.Annotations != nil {
			data.Annotations = selectedManifest.Annotations
		}
		if len(data.History) > 0 {
			data.Comment = data.History[0].Comment
		}
	case manifest.DockerV2Schema2MediaType:
		var config manifest.Schema2V1Image
		if err := json.Unmarshal(resolved.ConfigData, &config); err != nil {
			return nil, err
		}
		data.Comment = config.Comment
		data.HealthCheck = config.ContainerConfig.Healthcheck
		if data.HealthCheck == nil && config.Config != nil {
			data.HealthCheck = config.Config.Healthcheck
		}
	}
	return data, nil
}

func newImageRemoveCommand() *cobra.Command {
	var force, ignore, all, noPrune bool
	cmd := &cobra.Command{Use: "rm <name>...", Annotations: map[string]string{nativeStorageAnnotation: "true"}, Short: "Remove locally stored container images", Args: func(cmd *cobra.Command, args []string) error {
		if all {
			if len(args) > 0 {
				return errors.New("--all cannot be used with image names")
			}
			return nil
		}
		return cobra.MinimumNArgs(1)(cmd, args)
	}, RunE: func(cmd *cobra.Command, args []string) error {
		options, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		return withExclusiveStoreActivity(cmd.Context(), buildah.ActivityRoots(options, ""), func() error {
			return buildah.WithStore(options, func(backend storage.Store) error {
				runtime, err := libimage.RuntimeFromStore(backend, nil)
				if err != nil {
					return err
				}
				var reports []*libimage.RemoveImageReport
				var removalErrors []error
				var ordinary, indexes []string
				if all {
					reports, errs := runtime.RemoveImages(cmd.Context(), nil, &libimage.RemoveImagesOptions{Filters: []string{"readonly=false"}, Force: force, Ignore: ignore || force, NoPrune: noPrune})
					return imageRemovalError(errors.Join(append(errs, writeImageRemovalReports(cmd.OutOrStdout(), reports))...))
				}

				for _, arg := range args {
					name, err := imagestore.FromStore(backend).MatchedName(arg)
					if err != nil {
						if (ignore || force) && libimage.ErrorIsImageUnknown(err) {
							continue
						}
						removalErrors = append(removalErrors, err)
						continue
					}
					image, _, err := runtime.LookupImage(name, &libimage.LookupImageOptions{ManifestList: true})
					if err != nil {
						if (ignore || force) && libimage.ErrorIsImageUnknown(err) {
							continue
						}
						removalErrors = append(removalErrors, err)
						continue
					}
					isIndex, err := image.IsManifestList(cmd.Context())
					// Native lookup/removal deliberately tolerates a missing
					// manifest so damaged records can still be removed.
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						removalErrors = append(removalErrors, err)
						continue
					}
					if isIndex {
						indexes = append(indexes, name)
					} else {
						ordinary = append(ordinary, name)
					}
				}
				for _, group := range []struct {
					names []string
					index bool
				}{{ordinary, false}, {indexes, true}} {
					if len(group.names) == 0 {
						continue
					}
					removed, errs := runtime.RemoveImages(cmd.Context(), group.names, &libimage.RemoveImagesOptions{LookupManifest: group.index, Force: force, Ignore: ignore || force, NoPrune: noPrune})
					reports = append(reports, removed...)
					removalErrors = append(removalErrors, errs...)
				}

				return imageRemovalError(errors.Join(append(removalErrors, writeImageRemovalReports(cmd.OutOrStdout(), reports))...))

			})
		})
	}}
	flags := cmd.Flags()
	flags.BoolVarP(&force, "force", "f", false, "force removal of images and their containers")
	flags.BoolVarP(&ignore, "ignore", "i", false, "ignore images that do not exist")
	flags.BoolVarP(&all, "all", "a", false, "remove all images")
	flags.BoolVar(&noPrune, "no-prune", false, "do not remove dangling parent images")
	return cmd
}

func imageRemovalError(err error) error {
	if err == nil {
		return nil
	}
	missing, inUse, other := false, false, false
	var classify func(error)
	classify = func(err error) {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, part := range joined.Unwrap() {
				classify(part)
			}
			return
		}
		switch {
		case errors.Is(err, storage.ErrImageUnknown) || errors.Is(err, storage.ErrLayerUnknown):
			missing = true
		case errors.Is(err, storage.ErrImageUsedByContainer):
			inUse = true
		default:
			other = true
		}
	}
	classify(err)
	code := 125
	if inUse {
		code = 2
	} else if missing && !other {
		code = 1
	}
	return commandExitError{Code: code, Err: err}
}

func writeImageRemovalReports(output io.Writer, reports []*libimage.RemoveImageReport) error {
	for _, report := range reports {
		for _, name := range report.Untagged {
			if _, err := fmt.Fprintln(output, "Untagged:", name); err != nil {
				return err
			}
		}
		if report.Removed {
			if _, err := fmt.Fprintln(output, "Deleted:", report.ID); err != nil {
				return err
			}
		}
	}
	return nil
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
					name = strings.TrimPrefix(name, "local:")
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
	var format string
	var verbose bool
	cmd := &cobra.Command{
		Use: "df", Short: "Show local image, component, and build storage usage", Args: cobra.NoArgs,
		Annotations: map[string]string{nativeStorageAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "" && verbose {
				return errors.New("cannot combine --format and --verbose options")
			}
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

			if verbose {
				return writeVerboseImageUsage(cmd.OutOrStdout(), usage.ImageUsage, components.Roots, components.Bytes)
			}
			if format != "" {
				rows := []diskUsageRow{{Type: "Images", Total: usage.Images, Active: usage.ActiveImages, RawSize: usage.Bytes, RawReclaimable: usage.ReclaimableBytes, ReclaimableKnown: true}, {Type: "Components", Total: components.Roots, Active: "N/A", RawSize: components.Bytes}, {Type: "Cached images (included in Images)", Total: usage.CacheImages, Active: "N/A", RawSize: usage.CacheBytes, SizeUnknown: !usage.CacheBytesKnown}}
				if report.IsJSON(format) {
					return writeJSON(cmd, rows)
				}
				rpt, err := report.New(cmd.OutOrStdout(), "df").Parse(report.OriginUser, format)
				if err != nil {
					return err
				}
				if rpt.RenderHeaders {
					err = rpt.Execute(report.Headers(diskUsageRow{}, map[string]string{"Size": "SIZE", "Reclaimable": "RECLAIMABLE"}))
				}
				if err == nil {
					err = rpt.Execute(rows)
				}
				return errors.Join(err, rpt.Flush())
			}
			output := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if _, err := fmt.Fprintln(output, "TYPE\tTOTAL\tACTIVE\tSIZE\tRECLAIMABLE (UNIQUE)"); err != nil {
				return err
			}
			cacheSize := "unavailable"
			if usage.CacheBytesKnown {
				cacheSize = units.HumanSize(float64(usage.CacheBytes))
			}
			if _, err := fmt.Fprintf(output, "Images\t%d\t%d\t%s\t%s\nComponents\t%d\tN/A\t%s\tN/A\nCached images (included in Images)\t%d\tN/A\t%s\tN/A\n", usage.Images, usage.ActiveImages, units.HumanSize(float64(usage.Bytes)), units.HumanSize(float64(usage.ReclaimableBytes)), components.Roots, units.HumanSize(float64(components.Bytes)), usage.CacheImages, cacheSize); err != nil {
				return err
			}
			return output.Flush()

		},
	}
	cmd.Flags().StringVar(&format, "format", "", "format output using a Go template or JSON")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show detailed image storage usage")
	return cmd
}

func newPruneCommand(pruneComponents bool) *cobra.Command {
	var force bool
	request := buildah.StoreMaintenanceRequest{Mode: buildah.StoreMaintenancePrune, ActivityLeaseHeld: true}
	short := "Prune dangling images and unused instruction snapshots"
	if pruneComponents {
		short += " and component content"
	}
	cmd := &cobra.Command{
		Use: "prune", Short: short, Args: cobra.NoArgs,
		Annotations: map[string]string{nativeStorageAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, filter := range request.Filters {
				key, _, found := strings.Cut(filter, "=")
				if !found || (key != "label" && key != "label!" && key != "until") {
					return fmt.Errorf("unsupported prune filter %q", filter)
				}
			}
			if !force && !request.DryRun {
				if err := cmd.Context().Err(); err != nil {
					return err
				}
				warning := "WARNING! This command removes dangling images and unused instruction snapshots."
				if request.All {
					warning = "WARNING! This command removes all unused images, instruction snapshots, and RUN cache mounts."
				} else if request.BuildCache {
					warning += " It also removes persistent RUN cache mounts."
				}
				if pruneComponents {
					warning += " It also removes unreferenced component content."
				}
				if _, err := fmt.Fprint(cmd.OutOrStdout(), warning+"\nAre you sure you want to continue? [y/N] "); err != nil {
					return err
				}
				answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && (!errors.Is(err, io.EOF) || len(answer) == 0) {
					return err
				}
				if err := cmd.Context().Err(); err != nil {
					return err
				}
				if !strings.HasPrefix(strings.ToLower(answer), "y") {
					return nil
				}
			}
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
				if err := errors.Join(err, writeImageRemovalReports(cmd.OutOrStdout(), result.RemovalReports)); err != nil {
					return err
				}
				if result.UsageError != "" {
					if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Warning: image storage usage unavailable:", result.UsageError); err != nil {
						return err
					}
				}
				componentRoots := 0
				reclaimedBytes := result.ReclaimedBytes
				var componentBytes int64
				if pruneComponents {
					before, err := localstore.DiskUsage(cmd.Context(), componentDir)
					if err != nil {
						return err
					}
					componentBytes = before.Bytes
					componentRoots, err = localstore.PruneUnderActivityLease(cmd.Context(), componentDir, request.DryRun)
					if err != nil {
						return err
					}
				}
				if request.DryRun {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Images eligible for removal: %d\n", result.PrunableImages); err != nil {
						return err
					}
					if pruneComponents {
						_, err := fmt.Fprintf(cmd.OutOrStdout(), "Component roots eligible for removal: %d\n", componentRoots)
						return err
					}
					return nil
				}
				if pruneComponents {
					after, err := localstore.DiskUsage(cmd.Context(), componentDir)
					if err != nil {
						return err
					}
					reclaimedBytes += max(int64(0), componentBytes-after.Bytes)
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Removed component roots: %d\n", componentRoots); err != nil {
						return err
					}
				}
				if !result.ReclaimedBytesKnown {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "Total reclaimed space: unavailable (image storage usage could not be measured)")
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Total reclaimed space: %s\n", units.HumanSize(float64(reclaimedBytes)))
				return err

			})
		},
	}
	cmd.Flags().StringArrayVar(&request.Filters, "filter", nil, "filter images by label or until timestamp")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "do not prompt for confirmation")
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

// Keep unavailable measurements explicit; component content has no container-based
// active/reclaimable interpretation, and cached image sizes overlap Images.
type diskUsageRow struct {
	Type             string
	Total            int
	Active           any
	RawSize          int64
	RawReclaimable   int64
	ReclaimableKnown bool
	SizeUnknown      bool
}

func (d diskUsageRow) Size() string {
	if d.SizeUnknown {
		return "unavailable"
	}
	return units.HumanSize(float64(d.RawSize))
}
func (d diskUsageRow) Reclaimable() string {
	if !d.ReclaimableKnown {
		return "N/A"
	}
	return units.HumanSize(float64(d.RawReclaimable))
}
func (d diskUsageRow) MarshalJSON() ([]byte, error) {
	type raw diskUsageRow
	return json.Marshal(struct {
		raw
		TotalCount  int
		Size        string
		Reclaimable string
	}{raw(d), d.Total, d.Size(), d.Reclaimable()})
}
func writeVerboseImageUsage(out io.Writer, usage []libimage.ImageDiskUsage, componentRoots int, componentBytes int64) error {
	output := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(output, "Images space usage:\n\nREPOSITORY\tTAG\tIMAGE ID\tCREATED\tSIZE\tSHARED SIZE\tUNIQUE SIZE\tCONTAINERS"); err != nil {
		return err
	}
	for _, row := range usage {
		if strings.HasPrefix(row.Repository, "coopr.internal/") {
			continue
		}
		if _, err := fmt.Fprintf(output, "%s\t%s\t%.12s\t%s\t%s\t%s\t%s\t%d\n", row.Repository, row.Tag, row.ID, units.HumanDuration(time.Since(row.Created)), units.HumanSize(float64(row.Size)), units.HumanSize(float64(row.SharedSize)), units.HumanSize(float64(row.UniqueSize)), row.Containers); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(output, "\nComponents space usage:\n\nROOTS\tSIZE\n%d\t%s\n", componentRoots, units.HumanSize(float64(componentBytes))); err != nil {
		return err
	}
	return output.Flush()
}
