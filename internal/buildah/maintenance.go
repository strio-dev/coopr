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
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"
	"go.podman.io/storage/pkg/unshare"
)

const maintenanceWorkerName = "coopr-buildah-maintenance-worker-v1"

type StoreMaintenanceMode string

const (
	StoreMaintenanceDF         StoreMaintenanceMode = "df"
	StoreMaintenancePrune      StoreMaintenanceMode = "prune"
	StoreMaintenanceCachePrune StoreMaintenanceMode = "cache-prune"
)

type StoreMaintenanceRequest struct {
	Store             StoreOptions         `json:"store"`
	Mode              StoreMaintenanceMode `json:"mode"`
	DryRun            bool                 `json:"dry_run,omitempty"`
	ActivityLeaseHeld bool                 `json:"activity_lease_held,omitempty"`
	ResultPath        string               `json:"result_path"`
}

type StoreMaintenanceResult struct {
	Images              int      `json:"images"`
	Containers          int      `json:"containers"`
	Layers              int      `json:"layers"`
	Bytes               int64    `json:"bytes"`
	CacheImages         int      `json:"cache_images"`
	CacheBytes          int64    `json:"cache_bytes"`
	RemovedImages       int      `json:"removed_images"`
	RemovedCacheAliases int      `json:"removed_cache_aliases"`
	SkippedInUse        int      `json:"skipped_in_use"`
	RetainedImageIDs    []string `json:"retained_image_ids,omitempty"`
	Error               string   `json:"error,omitempty"`
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
	if request.Mode != StoreMaintenanceDF && request.Mode != StoreMaintenancePrune && request.Mode != StoreMaintenanceCachePrune {
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
	if err := runWorkerProcess(ctx, command, workerGrace); err != nil {
		return StoreMaintenanceResult{}, err
	}
	var result StoreMaintenanceResult
	if err := readWorkerJSON(request.ResultPath, &result); err != nil {
		return StoreMaintenanceResult{}, err
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
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
	result := storeUsage(images, layers, containers)
	if request.Mode == StoreMaintenanceDF {
		return result, ctx.Err()
	}
	inUse := make(map[string]bool, len(containers))
	retained := make(map[string]bool)
	for _, container := range containers {
		inUse[container.ImageID] = true
	}
	for _, image := range images {
		cacheNames := instructionCacheNames(image.Names)
		if request.Mode == StoreMaintenanceCachePrune && len(cacheNames) == 0 {
			continue
		}
		if request.Mode == StoreMaintenancePrune {
			// Native graphs can contain unnamed images retained by other tools or
			// manifest lists. A storage path is not proof of Coopr ownership.
			retained[image.ID] = true
			continue
		}
		if inUse[image.ID] {
			result.SkippedInUse++
			retained[image.ID] = true
			continue
		}
		result.RemovedCacheAliases += len(cacheNames)
		if !request.DryRun {
			if err := backend.RemoveNames(image.ID, cacheNames); err != nil {
				return result, err
			}
		}
		retained[image.ID] = true
	}
	for id := range retained {
		result.RetainedImageIDs = append(result.RetainedImageIDs, id)
	}
	slices.Sort(result.RetainedImageIDs)
	return result, ctx.Err()
}

func storeUsage(images []storage.Image, layers []storage.Layer, containers []storage.Container) StoreMaintenanceResult {
	result := StoreMaintenanceResult{Images: len(images), Layers: len(layers), Containers: len(containers)}
	layerByID := make(map[string]storage.Layer, len(layers))
	for _, layer := range layers {
		layerByID[layer.ID] = layer
		size := layer.CompressedSize
		if size < 0 || layer.CompressedDigest == "" {
			size = layer.UncompressedSize
		}
		if size > 0 {
			result.Bytes += size
		}
	}
	cacheLayers := make(map[string]bool)
	for _, image := range images {
		if len(instructionCacheNames(image.Names)) == 0 {
			continue
		}
		result.CacheImages++
		for layerID := image.TopLayer; layerID != ""; layerID = layerByID[layerID].Parent {
			if cacheLayers[layerID] {
				break
			}
			cacheLayers[layerID] = true
			layer, ok := layerByID[layerID]
			if !ok {
				break
			}
			size := layer.CompressedSize
			if size < 0 || layer.CompressedDigest == "" {
				size = layer.UncompressedSize
			}
			if size > 0 {
				result.CacheBytes += size
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
