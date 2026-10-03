//go:build !linux

package buildah

import (
	"errors"

	commonconfig "go.podman.io/common/pkg/config"
)

func runCgroupManager() (string, bool, error) {
	containerConfig, err := commonconfig.Default()
	if err != nil {
		return "", false, err
	}
	return containerConfig.Engine.CgroupManager, false, nil
}
func validateRunControlHost(controls RunControls) error {
	return validateResourceControlEnvironment(controls, false, false, nil, errors.New("cgroups are unavailable on this operating system"))
}
