package buildah

import (
	"fmt"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestRetiredInsecureEntitlementIsRejected(t *testing.T) {
	if _, err := allowedEntitlements([]string{"security.insecure"}); err == nil {
		t.Fatal("retired security.insecure entitlement accepted")
	}
}

func TestAuthoredRunNetworkModesAreCheckedAfterArgumentExpansion(t *testing.T) {
	for _, mode := range []string{"default", "none", "host", "private", "buildnet", "ns:/coopr-test-namespace", "pasta:--map-gw", "slirp4netns:allow_host_loopback=true"} {
		t.Run(mode, func(t *testing.T) {
			valid := mode == "default" || mode == "none" || mode == "host"
			for _, stage := range []string{`from "scratch"`, `extend`} {
				def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`arg "network"
%s {
    arg "network"
    run "true" network="${network}"
}
`, stage)))
				if err != nil {
					t.Fatal(err)
				}
				var plan *planner.Plan
				if stage == `extend` {
					publication, publishErr := planner.Create(def, planner.Options{Mode: planner.Publish})
					if publishErr != nil {
						t.Fatal(publishErr)
					}
					plan, err = planner.Instantiate(publication.Component, planner.Options{Arguments: map[string]string{"network": mode}})
				} else {
					plan, err = planner.Create(def, planner.Options{Arguments: map[string]string{"network": mode}})
				}
				if !valid {
					if err == nil || !strings.Contains(err.Error(), "network property") {
						t.Fatalf("%s network %q: error = %v, want unsupported network property", stage, mode, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				operation := plan.Stages[len(plan.Stages)-1].Operations[0]
				if operation.Properties["network"] != mode || !operation.NetworkExplicit {
					t.Fatalf("%s resolved RUN = %+v", stage, operation)
				}
				if err := authorizePlannedOperation(operation, nil, nil); (mode == "host") != (err != nil) {
					t.Fatalf("%s network %q: authorization = %v", stage, mode, err)
				}
			}
		})
	}
}

func TestInheritedRunRejectsBuildWideOnlyNetworkModes(t *testing.T) {
	for _, mode := range []string{"private", "buildnet", "ns:/coopr-test-namespace", "pasta:--map-gw", "slirp4netns:allow_host_loopback=true"} {
		logical, err := imageconfig.Parse([]byte(`{"config":{"Env":["NETWORK=` + mode + `"],"OnBuild":["RUN --network=$NETWORK true"]}}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := consumeOnBuild(logical); err == nil || !strings.Contains(err.Error(), "network property") {
			t.Fatalf("inherited network %q: error = %v, want unsupported network property", mode, err)
		}
	}
}

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
		"device=vendor.example/device=one,unknown=two",
	} {
		if _, err := allowedEntitlements([]string{value}); err == nil {
			t.Fatalf("invalid entitlement %q accepted", value)
		}
	}
}

func TestDeviceEntitlementUsesUpstreamParsing(t *testing.T) {
	cache := testCDICache(t)
	for _, test := range []struct {
		value string
		name  string
	}{
		{value: "device=", name: "vendor.example/device=alpha"},
		{value: "device=vendor.example/device=alpha,alias=", name: "vendor.example/device=alpha"},
		{value: `device="vendor.example/device=alpha"`, name: "vendor.example/device=alpha"},
		{value: "device=vendor.example/device=alpha,alias=old,alias=accelerator", name: "accelerator"},
		{value: "device=vendor.example/device=alpha,alias=old,alias=", name: "vendor.example/device=alpha"},
		{value: `device=vendor.example/device=alpha,"alias=accel,erator"`, name: "accel,erator"},
	} {
		t.Run(test.value, func(t *testing.T) {
			allowed, err := allowedEntitlements([]string{"", test.value, ""})
			if err != nil {
				t.Fatal(err)
			}
			if err := authorizeRunDevices([]runDeviceRequest{{Name: test.name, Required: true}}, allowed, cache); err != nil {
				t.Fatal(err)
			}
			if err := authorizeRunDevices([]runDeviceRequest{{Name: "missing", Required: true}}, allowed, cache); err == nil {
				t.Fatal("missing required device was accepted")
			}
			if err := authorizeRunDevices([]runDeviceRequest{{Name: "missing"}}, allowed, cache); err != nil {
				t.Fatalf("missing optional device: %v", err)
			}
		})
	}
}

func TestDeviceEntitlementLastGrantWinsAcrossAliasAndDirectSelector(t *testing.T) {
	cache := testCDICache(t)
	alpha := "vendor.example/device=alpha"
	alias := "device=vendor.example/device=beta,alias=" + alpha
	direct := "device=" + alpha
	for _, test := range []struct {
		values []string
		want   string
	}{
		{values: []string{alias, direct}, want: alpha},
		{values: []string{direct, alias}, want: "vendor.example/device=beta"},
	} {
		allowed, err := allowedEntitlements(test.values)
		if err != nil {
			t.Fatal(err)
		}
		requests := []runDeviceRequest{{Name: alpha, Required: true}}
		mapped, _ := applyRunDeviceEntitlementAliases(requests, allowed)
		if mapped[0].Name != test.want {
			t.Fatalf("grants %v mapped device to %q, want %q", test.values, mapped[0].Name, test.want)
		}
		if err := authorizeRunDevices(requests, allowed, cache); err != nil {
			t.Fatal(err)
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
