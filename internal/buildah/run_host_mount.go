package buildah

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// validateLiveHostMount admits only an explicit directory bind. It never
// snapshots or chowns the caller-owned directory. Relabeling is an explicit
// native mount option; its default is absent.
func validateLiveHostMount(mount RunMount) (map[string]string, error) {
	if mount.Type != "bind" || mount.BoundFrom || mount.Artifact != nil || !filepath.IsAbs(mount.HostPath) {
		return nil, errors.New("live host mount requires an absolute bind directory without another source")
	}
	properties, err := normalizeRunMountProperties(mount.Type, mount.Properties)
	if err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(properties)) {
		if name != "target" && name != "readonly" && name != "relabel" {
			return nil, fmt.Errorf("live host mount property %q is not supported", name)
		}
	}
	ordinary := mount
	ordinary.HostPath = ""
	ordinary.Properties = properties
	if _, err := serializeRunMount(ordinary, "/"); err != nil {
		return nil, err
	}
	info, err := os.Stat(mount.HostPath)
	if err != nil {
		return nil, fmt.Errorf("live host mount %q is unavailable: %w", mount.HostPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("live host mount %q is not a directory", mount.HostPath)
	}
	return properties, nil
}

func hasLiveHostMount(mounts []RunMount) bool {
	for _, mount := range mounts {
		if mount.HostPath != "" {
			return true
		}
	}
	return false
}
