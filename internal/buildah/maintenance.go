package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"coopr/internal/storeactivity"
	"go.podman.io/buildah/pkg/volumes"
	"go.podman.io/common/libimage"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"
	"go.podman.io/storage/pkg/unshare"
)

const maintenanceWorkerName = "coopr-buildah-maintenance-worker-v1"

type StoreMaintenanceMode string

const (
	StoreMaintenanceDF    StoreMaintenanceMode = "df"
	StoreMaintenancePrune StoreMaintenanceMode = "prune"
)

type StoreMaintenanceRequest struct {
	Store             StoreOptions         `json:"store"`
	Mode              StoreMaintenanceMode `json:"mode"`
	All               bool                 `json:"all,omitempty"`
	BuildCache        bool                 `json:"build_cache,omitempty"`
	DryRun            bool                 `json:"dry_run,omitempty"`
	ActivityLeaseHeld bool                 `json:"activity_lease_held,omitempty"`
	Filters           []string             `json:"filters,omitempty"`
	ResultPath        string               `json:"result_path"`
}

type StoreMaintenanceResult struct {
	ImageUsage          []libimage.ImageDiskUsage     `json:"image_usage,omitempty"`
	Images              int                           `json:"images"`
	Containers          int                           `json:"containers"`
	Layers              int                           `json:"layers"`
	Bytes               int64                         `json:"bytes"`
	CacheImages         int                           `json:"cache_images"`
	CacheBytes          int64                         `json:"cache_bytes"`
	CacheBytesKnown     bool                          `json:"cache_bytes_known"`
	RemovedImages       int                           `json:"removed_images"`
	PrunableImages      int                           `json:"prunable_images"`
	ActiveImages        int                           `json:"active_images"`
	ReclaimableBytes    int64                         `json:"reclaimable_bytes"`
	ReclaimedBytes      int64                         `json:"reclaimed_bytes"`
	RemovalReports      []*libimage.RemoveImageReport `json:"removal_reports,omitempty"`
	ReclaimedBytesKnown bool                          `json:"reclaimed_bytes_known"`
	UsageError          string                        `json:"usage_error,omitempty"`
	Error               string                        `json:"error,omitempty"`
}

func init() {
	for _, name := range []string{maintenanceWorkerName, maintenanceWorkerName + "-in-a-user-namespace"} {
		reexec.Register(name, runMaintenanceWorker)
	}
}

func MaintainStoreSupervised(ctx context.Context, request StoreMaintenanceRequest) (_ StoreMaintenanceResult, retErr error) {
	if ctx == nil {
		return StoreMaintenanceResult{}, errors.New("maintenance context is nil")
	}
	if err := validateStoreOptions(request.Store); err != nil {
		return StoreMaintenanceResult{}, err
	}
	if request.Mode != StoreMaintenanceDF && request.Mode != StoreMaintenancePrune {
		return StoreMaintenanceResult{}, fmt.Errorf("unsupported store maintenance mode %q", request.Mode)
	}
	if !request.ActivityLeaseHeld {
		roots := ActivityRoots(request.Store, "")
		var activity *storeactivity.Lease
		var err error
		if request.Mode == StoreMaintenanceDF {
			activity, err = storeactivity.AcquireShared(ctx, roots...)
		} else {
			activity, err = storeactivity.AcquireExclusive(ctx, roots...)
		}
		if err != nil {
			return StoreMaintenanceResult{}, fmt.Errorf("acquire maintenance store activity lease: %w", err)
		}
		defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	}
	dir, err := os.MkdirTemp("", ".coopr-maintenance-*")
	if err != nil {
		return StoreMaintenanceResult{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	request.ResultPath = filepath.Join(dir, "result.json")
	requestPath := filepath.Join(dir, "request.json")
	if err := writeWorkerJSON(requestPath, request); err != nil {
		return StoreMaintenanceResult{}, err
	}
	command := reexec.Command(maintenanceWorkerName, requestPath)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if !request.DryRun && (request.All || request.BuildCache) {
		// Use the same TMPDIR as supervised builds so the upstream helper
		// cleans Coopr's RUN caches, rather than an unrelated host cache.
		workerTemp, err := prepareWorkerTemp(request.Store.GraphRoot)
		if err != nil {
			return StoreMaintenanceResult{}, err
		}
		command.Env = append(os.Environ(), "TMPDIR="+workerTemp)
	}
	processErr := runWorkerProcess(ctx, command, workerGrace)
	return readMaintenanceResult(ctx, request.ResultPath, processErr)
}

func readMaintenanceResult(ctx context.Context, resultPath string, processErr error) (StoreMaintenanceResult, error) {
	var result StoreMaintenanceResult
	if err := readWorkerJSON(resultPath, &result); err != nil {
		return StoreMaintenanceResult{}, errors.Join(processErr, err)
	}
	if result.Error != "" {
		// A persisted response describes the actual operation failure. Preserve
		// cancellation separately instead of replacing it with an exit status.
		return result, errors.Join(ctx.Err(), errors.New(result.Error))
	}
	return result, errors.Join(processErr, ctx.Err())
}

func runMaintenanceWorker() {
	unshare.MaybeReexecUsingUserNamespace(false)
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "maintenance worker requires one request path")
		os.Exit(2)
	}
	var request StoreMaintenanceRequest
	if err := readWorkerJSON(os.Args[1], &request); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, stop := signalContext()
	defer stop()
	result, err := maintainStore(ctx, request)
	if err != nil {
		result.Error = err.Error()
	}
	if writeErr := writeWorkerJSON(request.ResultPath, result); writeErr != nil {
		_, _ = fmt.Fprintln(os.Stderr, writeErr)
		os.Exit(1)
	}
	if err != nil {
		os.Exit(1)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
}

func maintainStore(ctx context.Context, request StoreMaintenanceRequest) (_ StoreMaintenanceResult, retErr error) {
	lease, err := acquireStore(request.Store)
	if err != nil {
		return StoreMaintenanceResult{}, err
	}
	defer func() { retErr = errors.Join(retErr, lease.Close()) }()
	backend := lease.store
	images, err := backend.Images()
	if err != nil {
		return StoreMaintenanceResult{}, err
	}
	layers, err := backend.Layers()
	if err != nil {
		return StoreMaintenanceResult{}, err
	}
	containers, err := backend.Containers()
	if err != nil {
		return StoreMaintenanceResult{}, err
	}
	runtime, err := libimage.RuntimeFromStore(instructionCachePruneStore{backend}, nil)
	if err != nil {
		return StoreMaintenanceResult{}, err
	}
	usage, totalBytes, usageErr := runtime.DiskUsage(ctx)
	if usageErr != nil && request.Mode == StoreMaintenanceDF {
		return StoreMaintenanceResult{}, usageErr
	}
	result := storeUsage(images, layers, containers)
	if usageErr == nil {
		result.Bytes = totalBytes
	} else {
		result.UsageError = usageErr.Error()
	}
	seen := make(map[string]bool)
	readOnly := make(map[string]bool, len(images))
	for _, image := range images {
		readOnly[image.ID] = image.ReadOnly
	}
	for _, entry := range usage {
		if seen[entry.ID] {
			continue
		}
		seen[entry.ID] = true
		if entry.Containers > 0 {
			result.ActiveImages++
		} else if !readOnly[entry.ID] {
			result.ReclaimableBytes += entry.UniqueSize
		}
	}
	if request.Mode == StoreMaintenanceDF {
		result.ImageUsage = usage
		return result, ctx.Err()
	}

	// Match Podman's image-prune filters, including native manifest-list and
	// parent/child checks. Never force deletion of containers using an image.
	filters := []string{"readonly=false", "containers=false"}
	for _, filter := range request.Filters {
		key, _, found := strings.Cut(filter, "=")
		if !found || (key != "label" && key != "label!" && key != "until") {
			return result, fmt.Errorf("unsupported prune filter %q", filter)
		}
		filters = append(filters, filter)
	}
	if !request.All {
		filters = append(filters, "dangling=true")
	}
	if request.DryRun {
		candidates, err := runtime.ListImages(ctx, &libimage.ListImagesOptions{Filters: filters})
		result.PrunableImages = len(candidates)
		return result, err
	}
	previousReports := 1
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		reports, removalErrors := runtime.RemoveImages(ctx, nil, &libimage.RemoveImagesOptions{Filters: filters})
		removed := 0
		for _, report := range reports {
			if report.Removed {
				removed++
			}
		}
		result.RemovedImages += removed
		result.RemovalReports = append(result.RemovalReports, reports...)
		if err := errors.Join(removalErrors...); err != nil {
			return result, err
		}
		if len(reports)+previousReports == 0 {
			break
		}
		previousReports = len(reports)
	}
	if request.All || request.BuildCache {
		if err := volumes.CleanCacheMount(); err != nil {
			return result, err
		}
	}
	_, remainingBytes, remainingErr := runtime.DiskUsage(ctx)
	if err := errors.Join(usageErr, remainingErr); err != nil {
		result.UsageError = err.Error()
	} else {
		result.ReclaimedBytes = max(int64(0), totalBytes-remainingBytes)
		result.ReclaimedBytesKnown = true
	}

	return result, ctx.Err()
}

// Private instruction-cache names are lookup indexes, not user tags. Present
// them as unnamed to libimage without untagging retained images on disk.
// libimage uses MultiList for both prune selection and recursive parent checks.
type instructionCachePruneStore struct{ storage.Store }

func (store instructionCachePruneStore) MultiList(options storage.MultiListOptions) (storage.MultiListResult, error) {
	snapshot, err := store.Store.MultiList(options)
	if err != nil {
		return snapshot, err
	}
	snapshot.Images = slices.Clone(snapshot.Images)
	for index := range snapshot.Images {
		snapshot.Images[index].Names = slices.DeleteFunc(slices.Clone(snapshot.Images[index].Names), func(name string) bool {
			return strings.HasPrefix(name, instructionCacheNamePrefix)
		})
	}
	return snapshot, nil
}

func storeUsage(images []storage.Image, layers []storage.Layer, containers []storage.Container) StoreMaintenanceResult {
	result := StoreMaintenanceResult{Images: len(images), Layers: len(layers), Containers: len(containers), CacheBytesKnown: true}
	layerByID := make(map[string]storage.Layer, len(layers))
	for _, layer := range layers {
		layerByID[layer.ID] = layer
	}

	cacheLayers := make(map[string]bool)
	for _, image := range images {
		if len(instructionCacheNames(image.Names)) == 0 {
			continue
		}
		result.CacheImages++
		roots := append([]string{image.TopLayer}, image.MappedTopLayers...)
		for _, root := range roots {
			for layerID := root; layerID != ""; layerID = layerByID[layerID].Parent {
				if cacheLayers[layerID] {
					break
				}
				cacheLayers[layerID] = true
				layer, ok := layerByID[layerID]
				if !ok {
					result.CacheBytesKnown = false
					break
				}
				if layer.UncompressedSize < 0 {
					result.CacheBytesKnown = false
				} else {
					result.CacheBytes += layer.UncompressedSize
				}
			}
		}

	}
	return result
}

func instructionCacheNames(names []string) []string {
	var result []string
	for _, name := range names {
		if strings.HasPrefix(name, instructionCacheNamePrefix) {
			result = append(result, name)
		}
	}
	return result
}
