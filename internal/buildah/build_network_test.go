package buildah

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/planner"
	"go.podman.io/buildah/define"
)

func TestNormalizeBuildNetworkOptions(t *testing.T) {
	for _, mode := range []string{"default", "private", "none", "host", "pasta", "pasta:--map-gw", "slirp4netns", "slirp4netns:allow_host_loopback=true", "buildnet"} {
		network, hosts, err := NormalizeBuildNetworkOptions(mode, []string{
			" Example.TEST : 2001:0db8::1 ",
			"Equal.TEST=[2001:db8::2]",
		})
		if err != nil {
			t.Fatalf("normalize %s: %v", mode, err)
		}
		if network != mode || !reflect.DeepEqual(hosts, []string{"example.test:2001:db8::1", "equal.test:2001:db8::2"}) {
			t.Fatalf("normalize %s = %q, %v", mode, network, hosts)
		}
	}
	for _, invalid := range []struct {
		network string
		hosts   []string
	}{
		{network: "bad/network"},
		{hosts: []string{"missing-address"}},
		{hosts: []string{"host:not-an-ip"}},
		{hosts: []string{"host=[not-an-ip]"}},
	} {
		if _, _, err := NormalizeBuildNetworkOptions(invalid.network, invalid.hosts); err == nil {
			t.Fatalf("accepted invalid network/hosts: %+v", invalid)
		}
	}
}

func TestRunNetworkOptionsUseNativeBuildahNamespaceSemantics(t *testing.T) {
	namespaceFile := filepath.Join(t.TempDir(), "netns")
	if err := os.WriteFile(namespaceFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		mode       string
		wantPolicy define.NetworkConfigurationPolicy
		wantHost   bool
		wantPath   string
	}{
		{mode: "private", wantPolicy: define.NetworkEnabled},
		{mode: "none", wantPolicy: define.NetworkDisabled},
		{mode: "host", wantPolicy: define.NetworkEnabled, wantHost: true},
		{mode: "ns:" + namespaceFile, wantPolicy: define.NetworkEnabled, wantPath: namespaceFile},
		{mode: "buildnet", wantPolicy: define.NetworkEnabled, wantPath: "buildnet"},
		{mode: "pasta:--map-gw", wantPolicy: define.NetworkEnabled, wantPath: "pasta:--map-gw"},
		{mode: "slirp4netns:allow_host_loopback=true", wantPolicy: define.NetworkEnabled, wantPath: "slirp4netns:allow_host_loopback=true"},
	} {
		policy, namespaces, err := runNetworkOptions(test.mode)
		if err != nil {
			t.Fatalf("%s: %v", test.mode, err)
		}
		network := namespaces.Find("network")
		if policy != test.wantPolicy || network.Host != test.wantHost || network.Path != test.wantPath {
			t.Fatalf("%s = policy %s namespace %+v", test.mode, policy, network)
		}
	}
}

func TestResolveBuildNetworkAppliesOnlyToImplicitRuns(t *testing.T) {
	stages := []planner.Stage{{Operations: []planner.Operation{
		{Instruction: definition.Instruction{Name: "run", Properties: map[string]string{"network": "default"}}},
		{Instruction: definition.Instruction{Name: "run", Properties: map[string]string{"network": "default"}}, NetworkExplicit: true},
		{Instruction: definition.Instruction{Name: "run", Properties: map[string]string{"network": "host"}}, NetworkExplicit: true},
	}}}
	resolved := resolveBuildNetwork(stages, "none")
	got := []string{
		resolved[0].Operations[0].Properties["network"],
		resolved[0].Operations[1].Properties["network"],
		resolved[0].Operations[2].Properties["network"],
	}
	if !reflect.DeepEqual(got, []string{"none", "default", "host"}) {
		t.Fatalf("effective networks = %v", got)
	}
	if stages[0].Operations[0].Properties["network"] != "default" {
		t.Fatal("network resolution mutated the input plan")
	}
}

func TestDNSControlsValidateEffectivePerRunNetwork(t *testing.T) {
	controls := RunControls{DNSServers: []string{"1.1.1.1"}}
	for _, test := range []struct {
		name        string
		global      string
		authored    string
		explicit    bool
		wantError   bool
		wantNetwork string
	}{
		{name: "implicit global none", global: "none", authored: "default", wantError: true},
		{name: "authored none overrides global default", global: "default", authored: "none", explicit: true, wantError: true},
		{name: "authored default overrides global none", global: "none", authored: "default", explicit: true, wantNetwork: "default"},
		{name: "authored host overrides global none", global: "none", authored: "host", explicit: true, wantNetwork: "host"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stages := []planner.Stage{{ID: "component-or-replanned", Operations: []planner.Operation{{
				Instruction: definition.Instruction{Name: "run", Properties: map[string]string{"network": test.authored}}, NetworkExplicit: test.explicit,
			}}}}
			resolved, err := resolveAndValidateBuildNetwork(stages, test.global, controls)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "stage component-or-replanned operation 1") {
					t.Fatalf("effective network validation = %v", err)
				}
				return
			}
			if err != nil || resolved[0].Operations[0].Properties["network"] != test.wantNetwork {
				t.Fatalf("resolved network = %q, %v", resolved[0].Operations[0].Properties["network"], err)
			}
		})
	}
}

func TestBuildExecutionOptionsDistinguishNetworkAndHosts(t *testing.T) {
	baseStages := []planner.Stage{{ID: "output", Operations: []planner.Operation{{Instruction: definition.Instruction{Name: "run", Properties: map[string]string{"network": "default"}}}}}}
	base, err := buildExecutionOptionsDigest(baseStages, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []struct {
		stages []planner.Stage
		hosts  []string
	}{
		{stages: resolveBuildNetwork(baseStages, "none")},
		{stages: baseStages, hosts: []string{"example.test:127.0.0.1"}},
	} {
		got, err := buildExecutionOptionsDigest(candidate.stages, candidate.hosts)
		if err != nil {
			t.Fatal(err)
		}
		if got == base {
			t.Fatalf("execution options did not change identity: %+v", candidate)
		}
	}
}

func TestHostGatewayDisablesBuildResultCaches(t *testing.T) {
	_, hosts, err := NormalizeBuildNetworkOptions("default", []string{"gateway.test:host-gateway"})
	if err != nil {
		t.Fatal(err)
	}
	if buildResultCacheEligible(hosts) {
		t.Fatal("runtime-resolved host-gateway was treated as a stable cache input")
	}
	input := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}}})
	input.AddHosts = hosts
	if key, cacheable, err := instructionCacheKey(input); err != nil || cacheable || key != "" {
		t.Fatalf("host-gateway instruction cache key = %q, %v, %v", key, cacheable, err)
	}
}

func TestNormalizeAddHostsRejectsWhitespaceInHostname(t *testing.T) {
	_, err := normalizeAddHosts([]string{"bad host:127.0.0.1"})
	if err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("invalid hostname error = %v", err)
	}
}

func TestBuilderOptionsPassNormalizedAddHosts(t *testing.T) {
	options, err := newBuilderOptionsWithHosts("scratch", define.IsolationChroot, nil, nil, t.TempDir(), "", []string{"example.test:127.0.0.1"}, RunControls{})
	if err != nil {
		t.Fatal(err)
	}
	if options.CommonBuildOpts == nil || !reflect.DeepEqual(options.CommonBuildOpts.AddHost, []string{"example.test:127.0.0.1"}) {
		t.Fatalf("builder add-host options = %#v", options.CommonBuildOpts)
	}
}

func TestImplicitNoneEnablesPortablePackageAndComponentCaches(t *testing.T) {
	operation := planner.Operation{Instruction: definition.Instruction{Name: "run", Properties: map[string]string{"network": "default"}}}
	original := []planner.Stage{{ID: "output", Kind: "extend", Operations: []planner.Operation{operation}}}
	effective := resolveBuildNetwork(original, "none")
	if _, eligible := packageCacheEligibility(effective, RunControls{}); !eligible {
		t.Fatal("implicit build-wide none did not enable package cache eligibility")
	}
	resolved := &ResolvedComponentPlan{Plan: &planner.Plan{Outputs: []string{"output"}, Stages: effective}}
	if !componentCacheEligible(resolved, RunControls{}) {
		t.Fatal("implicit build-wide none did not enable component cache eligibility")
	}
	plan := &planner.Plan{DefinitionType: "component", Platform: "linux/amd64"}
	defaultDigest, err := packagePlanDigest(plan, original, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	noneDigest, err := packagePlanDigest(plan, effective, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if defaultDigest == noneDigest {
		t.Fatal("effective build-wide network did not change package cache identity")
	}
}

func TestRequestFromPlanResolvesBuildNetworkAndHosts(t *testing.T) {
	plan := testPlan(t, "from \"scratch\"\nrun \"printf implicit\"\nrun \"printf explicit\" network=\"default\"\n")
	request, err := RequestFromPlan(plan, PlanOptions{Network: "none", AddHosts: []string{"EXAMPLE.TEST:127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Operations) != 2 {
		t.Fatalf("operations = %#v", request.Operations)
	}
	first, firstOK := request.Operations[0].(Run)
	second, secondOK := request.Operations[1].(Run)
	if !firstOK || !secondOK || first.Network != "none" || second.Network != "default" {
		t.Fatalf("resolved RUN networks = %#v", request.Operations)
	}
	if !reflect.DeepEqual(request.AddHosts, []string{"example.test:127.0.0.1"}) {
		t.Fatalf("request add-hosts = %#v", request.AddHosts)
	}
}
