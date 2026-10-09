package planner

import (
	"encoding/json"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/onbuildparse"
)

func TestStaticSchemaErrorsPrecedeBaseResolution(t *testing.T) {
	for _, body := range []string{
		`frobnicate "x"`,
		`run "true" netwrok="none"`,
		`copy "a" "/a" parnets=#true`,
		`run "true" { mount "cahce" target="/cache"; }`,
		`run "true" { mount "cache" targte="/cache"; }`,
		`shell`,
	} {
		t.Run(body, func(t *testing.T) {
			def, err := definition.Parse(strings.NewReader("from \"example.invalid/base:latest\" as=\"unused\"\n" + body + "\nfrom \"scratch\" as=\"final\"\n"))
			calls := 0
			if err == nil {
				_, err = CreateDemandDriven(def, Options{Mode: Build, Target: "final"}, func(FromSource) (StageBind, error) {
					calls++
					return StageBind{}, nil
				})
			}
			if err == nil {
				t.Fatal("invalid instruction in dormant stage was accepted")
			}
			if calls != 0 {
				t.Fatalf("invalid static schema triggered %d image resolutions", calls)
			}
		})
	}
}

func TestMountTypeAndDeviceRequiredUseStageArguments(t *testing.T) {
	for _, expression := range []string{"$kind", "${kind}", "${missing:-cache}"} {
		t.Run(expression, func(t *testing.T) {
			plan := makePlan(t, `from "scratch"
arg "kind" "cache"
arg "required" #false
run "true" {
    mount "`+expression+`" target="/cache"
    device "vendor.example/gpu=one" required="${required}"
}
`, Options{Mode: Build})
			run := plan.Stages[0].Operations[0]
			if got := run.Children[0].Arguments[0]; got != "cache" {
				t.Fatalf("mount type = %q, want cache", got)
			}
			if got := run.Children[1].Properties["required"]; got != "false" {
				t.Fatalf("device required = %q, want false", got)
			}
		})
	}
}

func TestResolvedOptionValuesAreValidated(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"device", `run "true" { device "vendor.example/gpu=one" required="${invalid}"; }`},
		{"healthcheck", `healthcheck "true" interval="${invalid}"`},
		{"mount type", `run "true" { mount "${invalid}" target="/cache"; }`},
	} {
		t.Run(test.name, func(t *testing.T) {
			def, err := definition.Parse(strings.NewReader("from \"scratch\"\narg \"invalid\" \"nonsense\"\n" + test.body + "\n"))
			if err != nil {
				t.Fatalf("unresolved expression rejected: %v", err)
			}
			if _, err := Create(def, Options{Mode: Build}); err == nil {
				t.Fatal("invalid effective option value was accepted")
			}
		})
	}
}

func TestQuotedPermissionArgumentPreservesMode(t *testing.T) {
	plan := makePlan(t, `from "scratch"
arg "permissions" "0644"
arg "linked" #true
copy "payload" "/payload" chmod="${permissions}" link="${linked}"
run "true" { mount "secret" id="token" mode="${permissions}"; }
`, Options{Mode: Build})
	if got := plan.Stages[0].Operations[0].Properties; got["chmod"] != "0644" || got["link"] != "true" {
		t.Fatalf("COPY options = %v, want chmod=0644 link=true", got)
	}
	if got := plan.Stages[0].Operations[1].Children[0].Properties["mode"]; got != "0644" {
		t.Fatalf("secret mode = %q, want 0644", got)
	}
}

func TestStrictValidationPreservesInheritedEnvironmentAndDormantExpansion(t *testing.T) {
	def := parse(t, `from "example.invalid/unused:latest" as="unused"
healthcheck "true" interval="${invalid}"
from "example.invalid/base:latest" as="final"
healthcheck "true" interval="${interval}"
`)
	var sources []string
	plan, err := CreateDemandDriven(def, Options{Mode: Build, Target: "final"}, func(source FromSource) (StageBind, error) {
		sources = append(sources, source.Source)
		return StageBind{BaseEnvironment: map[string]string{"interval": "30s"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0] != "example.invalid/base:latest" {
		t.Fatalf("resolved sources = %v, want only selected base", sources)
	}
	if got := plan.Stages[len(plan.Stages)-1].Operations[0].Properties["interval"]; got != "30s" {
		t.Fatalf("inherited interval = %q, want 30s", got)
	}
}

func TestInheritedMountTypedOptionsExpandInChildScope(t *testing.T) {
	for _, test := range []struct {
		name                                        string
		environment                                 map[string]string
		wantTarget, wantRequired, wantMode, wantUID string
	}{
		{name: "defaults", wantTarget: "/run/token", wantRequired: "true", wantMode: "400", wantUID: "1000"},
		{name: "inherited environment", environment: map[string]string{"target": "/run/child", "required": "false", "mode": "0440", "uid": "1001"}, wantTarget: "/run/child", wantRequired: "", wantMode: "440", wantUID: "1001"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inherited, err := onbuildparse.ParseDeferred(`RUN --mount=type=secret,id=token,target=${target:-/run/token},required=${required:-true},mode=${mode:-0400},uid=${uid:-1000} true`)
			if err != nil {
				t.Fatal(err)
			}
			plan := makePlan(t, `from "example.invalid/base:latest"`, Options{Mode: Build, StageBinds: map[string]StageBind{"0": {Inherited: inherited, BaseEnvironment: test.environment}}})
			properties := plan.Stages[0].Operations[0].Children[0].Properties
			for name, want := range map[string]string{"target": test.wantTarget, "required": test.wantRequired, "mode": test.wantMode, "uid": test.wantUID} {
				if properties[name] != want {
					t.Errorf("mount %s = %q, want %q", name, properties[name], want)
				}
			}
		})
	}
}

func TestInheritedMountEnumsFollowDockerExpansionRules(t *testing.T) {
	for _, test := range []struct {
		name, trigger string
		environment   map[string]string
	}{
		{name: "defaults", trigger: `RUN --mount=type=${kind:-cache},id=scope,target=/cache,sharing=${sharing:-locked} true`},
		{name: "literal case", trigger: `RUN --mount=type=CACHE,id=scope,target=/cache,sharing=LOCKED true`},
		{name: "expanded case", trigger: `RUN --mount=type=${kind},id=scope,target=/cache,sharing=${sharing} true`, environment: map[string]string{"kind": "CACHE", "sharing": "LOCKED"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			inherited, err := onbuildparse.ParseDeferred(test.trigger)
			if err != nil {
				t.Fatal(err)
			}
			plan := makePlan(t, `from "example.invalid/base:latest"`, Options{Mode: Build, StageBinds: map[string]StageBind{"0": {Inherited: inherited, BaseEnvironment: test.environment}}})
			mount := plan.Stages[0].Operations[0].Children[0]
			if mount.Arguments[0] != "cache" || mount.Properties["sharing"] != "locked" {
				t.Fatalf("inherited mount = %#v, want cache with locked sharing", mount)
			}
		})
	}
}

func TestInheritedMountRejectsInvalidOverwrittenTypedOption(t *testing.T) {
	inherited, err := onbuildparse.ParseDeferred(`RUN --mount=type=secret,id=token,mode=invalid,mode=0400 true`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Create(parse(t, `from "example.invalid/base:latest"`), Options{
		Mode:       Build,
		StageBinds: map[string]StageBind{"0": {Inherited: inherited}},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid overwritten mount mode must be rejected, got %v", err)
	}
}

func TestInheritedMountExpansionDoesNotExpandReplacementAgain(t *testing.T) {
	inherited, err := onbuildparse.ParseDeferred(`RUN --mount=type=cache,id=${identity},target=${target} true`)
	if err != nil {
		t.Fatal(err)
	}
	plan := makePlan(t, `from "example.invalid/base:latest"`, Options{
		Mode: Build,
		StageBinds: map[string]StageBind{"0": {
			Inherited:       inherited,
			BaseEnvironment: map[string]string{"identity": "$another", "target": "/cache/$another", "another": "unexpected"},
		}},
	})
	mount := plan.Stages[0].Operations[0].Children[0]
	if mount.Properties["id"] != "$another" || mount.Properties["target"] != "/cache/$another" {
		t.Fatalf("replacement was expanded again: mount = %#v", mount)
	}
}

func TestInheritedMountSurvivesStageBindTransferAndNormalizesWithoutDeferredMetadata(t *testing.T) {
	inherited, err := onbuildparse.ParseDeferred(`RUN --mount=type=${kind:-cache},id=transfer,target=/cache,sharing=${sharing:-locked},mode=${permissions:-0400} true`)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(StageBind{Inherited: inherited})
	if err != nil {
		t.Fatal(err)
	}
	var transferred StageBind
	if err := json.Unmarshal(encoded, &transferred); err != nil {
		t.Fatal(err)
	}
	plan := makePlan(t, `from "example.invalid/base:latest"`, Options{Mode: Build, StageBinds: map[string]StageBind{"0": transferred}})
	mount := plan.Stages[0].Operations[0].Children[0]
	if mount.Arguments[0] != "cache" || mount.Properties["sharing"] != "locked" || mount.Properties["mode"] != "400" {
		t.Fatalf("transfer lost deferred mount semantics: %#v", mount)
	}
	encoded, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "deferred_onbuild") {
		t.Fatalf("normalized plan retains deferred metadata: %s", encoded)
	}
	encoded, err = json.Marshal(plan.Stages[0].Operations[0].Instruction)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "deferred_onbuild") {
		t.Fatalf("normalized cache instruction retains deferred metadata: %s", encoded)
	}
}
