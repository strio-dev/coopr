//go:build linux

package buildah

import (
	"fmt"

	"go.podman.io/common/pkg/cgroups"
	commonconfig "go.podman.io/common/pkg/config"
	"go.podman.io/storage/pkg/unshare"
)

func runCgroupManager() (manager string, unavailableSystemdSession bool, err error) {
	containerConfig, err := commonconfig.Default()
	if err != nil {
		return "", false, err
	}
	return adjustedRunCgroupManager(containerConfig)
}

func adjustedRunCgroupManager(containerConfig *commonconfig.Config) (manager string, unavailableSystemdSession bool, err error) {
	configured := *containerConfig
	requestedManager := configured.Engine.CgroupManager
	configured.CheckCgroupsAndAdjustConfig()
	return configured.Engine.CgroupManager,
		requestedManager == commonconfig.SystemdCgroupsManager && configured.Engine.CgroupManager != commonconfig.SystemdCgroupsManager,
		nil
}

func runControlCgroupManager(controls RunControls) (manager string, unavailableSystemdSession bool, err error) {
	containerConfig, err := loadRunContainerConfig(controls.ConfigModules)
	if err != nil {
		return "", false, err
	}
	if controls.CgroupManager != "" {
		containerConfig.Engine.CgroupManager = controls.CgroupManager
	}
	if controls.CgroupManagerSet {
		return containerConfig.Engine.CgroupManager, false, nil
	}
	return adjustedRunCgroupManager(containerConfig)
}

func validateRunControlHost(controls RunControls) error {
	unified, err := cgroups.IsCgroup2UnifiedMode()
	if err != nil {
		return validateResourceControlEnvironment(controls, unshare.IsRootless(), false, nil, err)
	}
	var controllers []string
	if unified {
		controllers, err = cgroups.AvailableControllers()
	}
	rootless := unshare.IsRootless()
	if err := validateResourceControlEnvironment(controls, rootless, unified, controllers, err); err != nil {
		return err
	}
	needsCgroups := controls.Memory != 0 || controls.MemorySwap != 0 || controls.CPUPeriod != 0 || controls.CPUQuota != 0 || controls.CPUShares != 0 || controls.CPUSetCPUs != "" || controls.CPUSetMems != "" || controls.CgroupParent != ""
	if needsCgroups && rootless && unified {
		manager, unavailableSystemdSession, err := runControlCgroupManager(controls)
		if err != nil {
			return fmt.Errorf("select cgroup manager for RUN resource limits: %w", err)
		}
		if unavailableSystemdSession {
			return fmt.Errorf("RUN resource limits require an available systemd user session for rootless builds")
		}
		if manager == commonconfig.CgroupfsCgroupsManager {
			owned, err := cgroups.UserOwnsCurrentSystemdCgroup()
			if err != nil {
				return fmt.Errorf("inspect rootless cgroupfs delegation for RUN resource limits: %w", err)
			}
			if !owned {
				return fmt.Errorf("RUN resource limits require the current cgroup to be delegated to Coopr when using the rootless cgroupfs manager")
			}
		}
	}
	return nil
}
