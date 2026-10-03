package buildah

import (
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/planner"
)

func TestHostNetworkRequiresExplicitRequestEntitlement(t *testing.T) {
	operation := planner.Operation{Instruction: definition.Instruction{Name: "run", Properties: map[string]string{"network": "host"}}}
	if err := authorizePlannedOperation(operation, nil, nil); err == nil || !strings.Contains(err.Error(), "--allow network.host") {
		t.Fatalf("missing entitlement error = %v", err)
	}
	allowed, err := allowedEntitlements([]string{"network.host"})
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizePlannedOperation(operation, allowed, nil); err != nil {
		t.Fatalf("authorized host network: %v", err)
	}
}

func TestUnknownEntitlementIsRejected(t *testing.T) {
	if _, err := allowedEntitlements([]string{"security.unknown"}); err == nil || !strings.Contains(err.Error(), "unsupported build entitlement") {
		t.Fatalf("unknown entitlement error = %v", err)
	}
}

func TestInsecureSecurityRequiresExplicitRequestEntitlement(t *testing.T) {
	operation := planner.Operation{Instruction: definition.Instruction{Name: "run", Properties: map[string]string{"security": "insecure"}}}
	if err := authorizePlannedOperation(operation, nil, nil); err == nil || !strings.Contains(err.Error(), "--allow security.insecure") {
		t.Fatalf("missing entitlement error = %v", err)
	}
	allowed, err := allowedEntitlements([]string{"security.insecure"})
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizePlannedOperation(operation, allowed, nil); err != nil {
		t.Fatalf("authorized insecure security: %v", err)
	}
	if err := authorizeOperations([]Operation{Run{Command: []string{"true"}, Security: "insecure"}}, nil); err == nil || !strings.Contains(err.Error(), "--allow security.insecure") {
		t.Fatalf("direct build missing entitlement error = %v", err)
	}
	if err := authorizeOperations([]Operation{Run{Command: []string{"true"}, Security: "insecure"}}, []string{"security.insecure"}); err != nil {
		t.Fatalf("authorized direct insecure security: %v", err)
	}
}

func TestRunDevicesRequireMatchingEntitlementBeforeExecution(t *testing.T) {
	cache := testCDICache(t)
	request := []runDeviceRequest{{Name: "vendor.example/device=alpha", Required: true}}
	if err := authorizeRunDevices(request, nil, cache); err == nil || !strings.Contains(err.Error(), "--allow device=vendor.example/device=alpha") {
		t.Fatalf("missing device entitlement error = %v", err)
	}
	allowed, err := allowedEntitlements([]string{"device=vendor.example/device=*"})
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizeRunDevices(request, allowed, cache); err != nil {
		t.Fatalf("matching selector entitlement: %v", err)
	}
}

func TestRunDeviceAliasEntitlementAuthorizesUnderlyingDevice(t *testing.T) {
	cache := testCDICache(t)
	allowed, err := allowedEntitlements([]string{"network.host", "device=vendor.example/device=alpha,alias=accelerator"})
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizeRunDevices([]runDeviceRequest{{Name: "accelerator", Required: true}}, allowed, cache); err != nil {
		t.Fatalf("aliased device entitlement: %v", err)
	}
	mapped, aliased := applyRunDeviceEntitlementAliases([]runDeviceRequest{{Name: "accelerator", Required: true}}, allowed)
	if len(mapped) != 1 || mapped[0].Name != "vendor.example/device=alpha" || !aliased[0] {
		t.Fatalf("mapped device request = %#v, aliased = %#v", mapped, aliased)
	}
	if err := authorizeRunDevices([]runDeviceRequest{{Name: "vendor.example/device=alpha", Required: true}}, allowed, cache); err == nil {
		t.Fatal("aliased entitlement authorized the underlying selector directly")
	}
}

func TestRunDeviceAutoAllowAnnotation(t *testing.T) {
	cache := testCDICache(t)
	request := []runDeviceRequest{{Name: "vendor.example/device=beta", Required: true}}
	if err := authorizeRunDevices(request, nil, cache); err != nil {
		t.Fatalf("autoallowed device: %v", err)
	}
}

func TestRunDeviceRequiredResolutionPrecedesBroadAuthorization(t *testing.T) {
	cache := testCDICache(t)
	allowed, err := allowedEntitlements([]string{"device"})
	if err != nil {
		t.Fatal(err)
	}
	err = authorizeRunDevices([]runDeviceRequest{{Name: "missing", Required: true}}, allowed, cache)
	if err == nil || !strings.Contains(err.Error(), "required device") {
		t.Fatalf("missing required device error = %v", err)
	}
}

func TestInvalidDeviceEntitlementIsRejected(t *testing.T) {
	for _, value := range []string{
		"device=",
		"device=vendor.example/device=one,alias=",
		"device=vendor.example/device=one,unknown=two",
		"device=vendor.example/device=one,alias=two,alias=three",
	} {
		if _, err := allowedEntitlements([]string{value}); err == nil {
			t.Fatalf("invalid entitlement %q accepted", value)
		}
	}
}

func TestDirectBuildAuthorization(t *testing.T) {
	operations := []Operation{Run{Command: []string{"true"}, Network: "host"}}
	if err := authorizeOperations(operations, nil); err == nil || !strings.Contains(err.Error(), "--allow network.host") {
		t.Fatalf("missing direct-build entitlement error = %v", err)
	}
	if err := authorizeOperations(operations, []string{"network.host"}); err != nil {
		t.Fatalf("authorized direct build: %v", err)
	}
}
