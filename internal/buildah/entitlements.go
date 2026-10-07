package buildah

import (
	"fmt"

	"coopr/internal/planner"
	"github.com/moby/buildkit/util/entitlements"
	"tags.cncf.io/container-device-interface/pkg/cdi"
)

const networkHostEntitlement = "network.host"
const securityInsecureEntitlement = "security.insecure"

func allowedEntitlements(values []string) (entitlements.Set, error) {
	parsed := make([]entitlements.Entitlement, 0, len(values))
	for _, value := range values {
		if value != "" {
			parsed = append(parsed, entitlements.Entitlement(value))
		}
	}
	allowed, err := entitlements.WhiteList(parsed, nil)
	if err != nil {
		return nil, fmt.Errorf("unsupported build entitlement: %w", err)
	}
	return allowed, nil
}

func authorizePlannedOperation(operation planner.Operation, allowed entitlements.Set, deviceCache *cdi.Cache) error {
	if operation.Name == "run" && operation.Properties["network"] == "host" && !allowed.Allowed(networkHostEntitlement) {
		return fmt.Errorf("RUN network=host requires --allow %s", networkHostEntitlement)
	}
	if operation.Name == "run" && operation.Properties["security"] == "insecure" && !allowed.Allowed(securityInsecureEntitlement) {
		return fmt.Errorf("RUN security=insecure requires --allow %s", securityInsecureEntitlement)
	}
	if operation.Name != "run" {
		return nil
	}
	requests, err := lowerRunDevices(operation.Instruction)
	if err != nil {
		return err
	}
	return authorizeRunDevices(requests, allowed, deviceCache)
}

func hasRunDeviceOptions(operation planner.Operation) bool {
	if operation.Name != "run" {
		return false
	}
	for _, child := range operation.Children {
		if child.Name == "device" && len(child.Arguments) > 0 {
			return true
		}
	}
	return false
}

func runDeviceCacheForPlannedOperation(operation planner.Operation) (*cdi.Cache, error) {
	if !hasRunDeviceOptions(operation) {
		return nil, nil
	}
	return newRunDeviceCache("")
}

func authorizeOperations(operations []Operation, allow []string) error {
	allowed, err := allowedEntitlements(allow)
	if err != nil {
		return err
	}
	for index, operation := range operations {
		run, ok := operation.(Run)
		if !ok {
			continue
		}
		if run.Network == "host" && !allowed.Allowed(networkHostEntitlement) {
			return fmt.Errorf("operation %d: RUN network=host requires --allow %s", index+1, networkHostEntitlement)
		}
		if run.Security == "insecure" && !allowed.Allowed(securityInsecureEntitlement) {
			return fmt.Errorf("operation %d: RUN security=insecure requires --allow %s", index+1, securityInsecureEntitlement)
		}
		if len(run.Devices) == 0 {
			continue
		}
		cache, err := newRunDeviceCache("")
		if err != nil {
			return fmt.Errorf("operation %d: load CDI devices: %w", index+1, err)
		}
		if err := authorizeRunDevices(run.Devices, allowed, cache); err != nil {
			return fmt.Errorf("operation %d: %w", index+1, err)
		}
		run.Devices, _ = applyRunDeviceEntitlementAliases(run.Devices, allowed)
		operations[index] = run
	}
	return nil
}

func authorizeRunDevices(requests []runDeviceRequest, allowed entitlements.Set, cache *cdi.Cache) error {
	if len(requests) == 0 {
		return nil
	}
	if cache == nil {
		return fmt.Errorf("CDI device cache is unavailable")
	}
	resolvedRequests, aliased := applyRunDeviceEntitlementAliases(requests, allowed)
	aliasGranted := make(map[string]bool)
	for index := range resolvedRequests {
		if !aliased[index] {
			continue
		}
		resolved, err := resolveRunDeviceSpecs(cache, []runDeviceRequest{resolvedRequests[index]})
		if err != nil {
			return err
		}
		for _, name := range resolved {
			aliasGranted[name] = true
		}
	}
	requested, err := resolveRunDeviceSpecs(cache, resolvedRequests)
	if err != nil {
		return err
	}
	config, _ := allowed[entitlements.EntitlementDevice].(*entitlements.DevicesConfig)
	if config != nil && config.All {
		return nil
	}
	granted := make(map[string]bool)
	if config != nil {
		for selector, aliasTarget := range config.Devices {
			if aliasTarget != "" {
				continue
			}
			resolved, err := resolveRunDeviceSpecs(cache, []runDeviceRequest{{Name: selector}})
			if err != nil {
				return err
			}
			for _, name := range resolved {
				granted[name] = true
			}
		}
	}
	for _, name := range requested {
		if granted[name] || aliasGranted[name] || runDeviceAutoAllowed(cache.GetDevice(name)) {
			continue
		}
		return fmt.Errorf("RUN device %q requires --allow device or --allow device=%s", name, name)
	}
	return nil
}

func applyRunDeviceEntitlementAliases(requests []runDeviceRequest, allowed entitlements.Set) ([]runDeviceRequest, []bool) {
	result := make([]runDeviceRequest, len(requests))
	copy(result, requests)
	aliased := make([]bool, len(requests))
	config, _ := allowed[entitlements.EntitlementDevice].(*entitlements.DevicesConfig)
	if config != nil {
		for index := range result {
			if selector := config.Devices[result[index].Name]; selector != "" {
				result[index].Name = selector
				aliased[index] = true
			}
		}
	}
	return result, aliased
}
