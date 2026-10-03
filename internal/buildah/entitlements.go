package buildah

import (
	"fmt"
	"strings"

	"coopr/internal/planner"
	"tags.cncf.io/container-device-interface/pkg/cdi"
)

const networkHostEntitlement = "network.host"
const deviceEntitlement = "device"
const securityInsecureEntitlement = "security.insecure"

func allowedEntitlements(values []string) (map[string]bool, error) {
	allowed := make(map[string]bool, len(values))
	for _, value := range values {
		switch {
		case value == networkHostEntitlement, value == deviceEntitlement, value == securityInsecureEntitlement:
			allowed[value] = true
		case strings.HasPrefix(value, deviceEntitlement+"="):
			_, alias, ok := parseDeviceEntitlement(value)
			if !ok {
				return nil, fmt.Errorf("unsupported build entitlement %q", value)
			}
			if alias != "" {
				for existing := range allowed {
					_, existingAlias, existingOK := parseDeviceEntitlement(existing)
					if existingOK && existingAlias == alias {
						delete(allowed, existing)
					}
				}
			}
			allowed[value] = true
		default:
			return nil, fmt.Errorf("unsupported build entitlement %q", value)
		}
	}
	return allowed, nil
}

func parseDeviceEntitlement(value string) (selector, alias string, ok bool) {
	if !strings.HasPrefix(value, deviceEntitlement+"=") {
		return "", "", false
	}
	selector, options, hasOptions := strings.Cut(strings.TrimPrefix(value, deviceEntitlement+"="), ",")
	if strings.TrimSpace(selector) == "" {
		return "", "", false
	}
	if !hasOptions {
		return selector, "", true
	}
	if strings.Contains(options, ",") {
		return "", "", false
	}
	name, alias, found := strings.Cut(options, "=")
	if !found || name != "alias" || strings.TrimSpace(alias) == "" {
		return "", "", false
	}
	return selector, alias, true
}

func authorizePlannedOperation(operation planner.Operation, allowed map[string]bool, deviceCache *cdi.Cache) error {
	if operation.Name == "run" && operation.Properties["network"] == "host" && !allowed[networkHostEntitlement] {
		return fmt.Errorf("RUN network=host requires --allow %s", networkHostEntitlement)
	}
	if operation.Name == "run" && operation.Properties["security"] == "insecure" && !allowed[securityInsecureEntitlement] {
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
		if run.Network == "host" && !allowed[networkHostEntitlement] {
			return fmt.Errorf("operation %d: RUN network=host requires --allow %s", index+1, networkHostEntitlement)
		}
		if run.Security == "insecure" && !allowed[securityInsecureEntitlement] {
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

func authorizeRunDevices(requests []runDeviceRequest, allowed map[string]bool, cache *cdi.Cache) error {
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
	if allowed[deviceEntitlement] {
		return nil
	}
	granted := make(map[string]bool)
	for entitlement := range allowed {
		selector, alias, ok := parseDeviceEntitlement(entitlement)
		if !ok || alias != "" {
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
	for _, name := range requested {
		if granted[name] || aliasGranted[name] || runDeviceAutoAllowed(cache.GetDevice(name)) {
			continue
		}
		return fmt.Errorf("RUN device %q requires --allow device or --allow device=%s", name, name)
	}
	return nil
}

func applyRunDeviceEntitlementAliases(requests []runDeviceRequest, allowed map[string]bool) ([]runDeviceRequest, []bool) {
	result := make([]runDeviceRequest, len(requests))
	copy(result, requests)
	aliased := make([]bool, len(requests))
	for index := range result {
		for entitlement := range allowed {
			selector, alias, ok := parseDeviceEntitlement(entitlement)
			if ok && alias == result[index].Name {
				result[index].Name = selector
				aliased[index] = true
				break
			}
		}
	}
	return result, aliased
}
