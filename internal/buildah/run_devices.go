package buildah

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/moby/buildkit/solver/llbsolver/cdidevices"
	"github.com/moby/buildkit/solver/pb"

	commonconfig "go.podman.io/common/pkg/config"
	"tags.cncf.io/container-device-interface/pkg/cdi"
)

const buildKitDeviceAutoAllowAnnotation = "org.mobyproject.buildkit.device.autoallow"

// runDeviceRequest is the executor-neutral form of a Dockerfile RUN --device
// request. Dockerfile devices are optional unless the required option is set.
type runDeviceRequest struct {
	Name     string
	Required bool
}

func newRunDeviceCacheWithDirs(builderConfigDir string, configuredDirs []string) (*cdi.Cache, error) {
	config, err := commonconfig.Default()
	if err != nil {
		return nil, fmt.Errorf("load containers configuration: %w", err)
	}
	dirs := slices.Clone(configuredDirs)
	if dirs == nil {
		dirs = slices.Clone(config.Engine.CdiSpecDirs.Get())
	}
	if builderConfigDir != "" {
		dirs = append(dirs, builderConfigDir)
	}
	cache, err := cdi.NewCache(cdi.WithAutoRefresh(false), cdi.WithSpecDirs(dirs...))
	if err != nil {
		return nil, err
	}
	if err := cache.Refresh(); err != nil {
		return nil, err
	}
	return cache, nil
}

// resolveRunDeviceSpecs expands Dockerfile RUN --device selectors to the exact
// qualified CDI device names accepted by Buildah's RunOptions.DeviceSpecs.
//
// BuildKit owns selector expansion, required/optional handling and deduplication.
// The manager uses this invocation's configured CDI cache; no installer is run.
func resolveRunDeviceSpecs(cache *cdi.Cache, requests []runDeviceRequest) ([]string, error) {
	devices := make([]*pb.CDIDevice, 0, len(requests))
	for _, request := range requests {
		devices = append(devices, &pb.CDIDevice{Name: request.Name, Optional: !request.Required})
	}
	names, err := cdidevices.NewManager(cache, nil).FindDevices(devices...)
	if err == nil && names == nil {
		names = []string{}
	}
	return names, err
}

func runDeviceAutoAllowed(device *cdi.Device) bool {
	if device == nil {
		return false
	}
	value, ok := device.Annotations[buildKitDeviceAutoAllowAnnotation]
	if !ok {
		value = device.GetSpec().Annotations[buildKitDeviceAutoAllowAnnotation]
	}
	allowed, err := strconv.ParseBool(value)
	return err == nil && allowed
}
