package buildah

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	commonconfig "go.podman.io/common/pkg/config"
	"tags.cncf.io/container-device-interface/pkg/cdi"
	"tags.cncf.io/container-device-interface/pkg/parser"
)

const buildKitDeviceClassAnnotation = "org.mobyproject.buildkit.device.class"
const buildKitDeviceAutoAllowAnnotation = "org.mobyproject.buildkit.device.autoallow"

// runDeviceRequest is the executor-neutral form of a Dockerfile RUN --device
// request. Dockerfile devices are optional unless the required option is set.
type runDeviceRequest struct {
	Name     string
	Required bool
}

func newRunDeviceCache(builderConfigDir string) (*cdi.Cache, error) {
	return newRunDeviceCacheWithDirs(builderConfigDir, nil)
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
// BuildKit owns these selector semantics. This follows the pinned manager's
// resolution order while using the CDI cache directly, avoiding the daemon-side
// manager's unrelated locker and on-demand installer dependencies.
func resolveRunDeviceSpecs(cache *cdi.Cache, requests []runDeviceRequest) ([]string, error) {
	available := cache.ListDevices()
	resolved := make([]string, 0, len(requests))
	for _, request := range requests {
		matches := resolveRunDeviceSelector(cache, available, request.Name)
		if len(matches) == 0 && request.Required {
			return nil, fmt.Errorf("required device %q is not registered", request.Name)
		}
		resolved = append(resolved, matches...)
	}
	return deduplicateRunDeviceSpecs(resolved), nil
}

func resolveRunDeviceSelector(cache *cdi.Cache, available []string, selector string) []string {
	kind, name, _ := strings.Cut(selector, "=")
	vendor, _ := parser.ParseQualifier(kind)
	var matches []string
	if vendor != "" {
		switch name {
		case "":
			for _, device := range available {
				if strings.HasPrefix(device, kind+"=") {
					matches = append(matches, device)
					break
				}
			}
		case "*":
			for _, device := range available {
				if strings.HasPrefix(device, kind+"=") {
					matches = append(matches, device)
				}
			}
		default:
			for _, device := range available {
				if device == selector {
					matches = append(matches, device)
					break
				}
			}
		}
	}

	if vendor == "" || len(matches) == 0 {
		for _, device := range available {
			if runDeviceClass(cache.GetDevice(device)) == selector {
				matches = append(matches, device)
			}
		}
	}
	return matches
}

func runDeviceClass(device *cdi.Device) string {
	if device == nil {
		return ""
	}
	if class, ok := device.Annotations[buildKitDeviceClassAnnotation]; ok {
		return class
	}
	return device.GetSpec().Annotations[buildKitDeviceClassAnnotation]
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

func deduplicateRunDeviceSpecs(specs []string) []string {
	result := make([]string, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		if _, exists := seen[spec]; exists {
			continue
		}
		seen[spec] = struct{}{}
		result = append(result, spec)
	}
	return result
}
