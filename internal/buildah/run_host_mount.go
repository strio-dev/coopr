package buildah

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// validateLiveHostMount admits only an explicit directory bind. It never
// snapshots or chowns the caller-owned directory. Relabeling is an explicit
// native mount option; its default is absent.
func validateLiveHostMount(mount RunMount) error {
	if mount.Type != "bind" || mount.BoundFrom || mount.Artifact != nil || !filepath.IsAbs(mount.HostPath) {
		return errors.New("live host mount requires an absolute bind directory without another source")
	}
	if mount.Properties["source"] != "" || mount.Properties["from"] != "" {
		return errors.New("live host mount cannot select another source")
	}
	ordinary := mount
	ordinary.HostPath = ""
	if _, err := serializeRunMount(ordinary, "/"); err != nil {
		return err
	}
	info, err := os.Stat(mount.HostPath)
	if err != nil {
		return fmt.Errorf("live host mount %q is unavailable: %w", mount.HostPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("live host mount %q is not a directory", mount.HostPath)
	}
	return nil
}

func hasLiveHostMount(mounts []RunMount) bool {
	for _, mount := range mounts {
		if mount.HostPath != "" {
			return true
		}
	}
	return false
}
