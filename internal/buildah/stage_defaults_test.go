package buildah

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
	"github.com/moby/buildkit/util/system"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestStageEntryDefaultsPrepareScratchAndPreserveCaller(t *testing.T) {
	for _, tc := range []struct{ name, kind, raw, wantPath, wantWork string }{
		{"scratch", "from", "", system.DefaultPathEnvUnix, "/"},
		{"caller missing path", "extend", `{"config":{"WorkingDir":"/caller"}}`, "", "/caller"},
		{"caller empty path", "extend", `{"config":{"Env":["PATH="],"WorkingDir":"/caller"}}`, "", "/caller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bindings *graphBindings
			if tc.kind == "extend" {
				logical, err := imageconfig.Parse([]byte(tc.raw))
				if err != nil {
					t.Fatal(err)
				}
				bindings = &graphBindings{caller: stageState{storageImageID: "caller", config: logical}, cacheScope: CacheMountScope{Kind: "component", Source: digest.FromString("component"), Platform: v1.Platform{OS: "linux", Architecture: "amd64"}}}
			}
			prepared, err := (&graphExecutor{}).prepareGraphStage(context.Background(), &planner.Plan{Mode: planner.Build, Platform: "linux/amd64"}, planner.Stage{ID: "0", Kind: tc.kind, Source: "scratch", Platform: "linux/amd64"}, nil, nil, bindings)
			if err != nil {
				t.Fatal(err)
			}
			if tc.kind == "from" {
				if err := inheritScratchWorkDir(prepared.logical, "/"); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := prepared.logical.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Config struct {
					Env        []string
					WorkingDir string
				}
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			environment, err := imageEnvironment(prepared.logical)
			if err != nil {
				t.Fatal(err)
			}
			if environment["PATH"] != tc.wantPath || doc.Config.WorkingDir != tc.wantWork {
				t.Fatalf("PATH=%q WorkingDir=%q; want %q %q", environment["PATH"], doc.Config.WorkingDir, tc.wantPath, tc.wantWork)
			}
		})
	}
}

func TestStageEntryDefaultsPlanPath(t *testing.T) {
	for _, tc := range []struct{ name, base, env, want string }{
		{"scratch", "scratch", "", system.DefaultPathEnvUnix},
		{"external missing", "base", "", system.DefaultPathEnvUnix},
		{"external custom", "base", `"Env":["PATH=/custom"]`, "/custom"},
		{"external empty", "base", `"Env":["PATH="]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := definition.Parse(strings.NewReader(`from "` + tc.base + `" { env observed="$PATH"; }`))
			if err != nil {
				t.Fatal(err)
			}
			plan, _, err := planDefinitionWithExternalBases(context.Background(), def, planner.Options{Mode: planner.Build, Platform: "linux/amd64"}, func(context.Context, string, v1.Platform) (ResolvedImageSource, error) {
				return ResolvedImageSource{ImageID: "base", ConfigData: []byte(`{"architecture":"amd64","os":"linux","config":{` + tc.env + `}}`)}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			got := plan.Stages[0].Operations[0].Properties["observed"]
			if got != tc.want {
				t.Fatalf("observed=%q; want %q", got, tc.want)
			}
		})
	}
}

func TestStageEntryDefaultsPreserveRawConfiguration(t *testing.T) {
	for _, workdir := range []string{`"WorkingDir":""`, `"WorkingDir":"/custom"`} {
		logical, err := imageconfig.Parse([]byte(`{"extension":{"kept":true},"config":{` + workdir + `,"Env":["PATH="],"OnBuild":["ENV later=$PATH"],"custom":{"kept":true}}}`))
		if err != nil {
			t.Fatal(err)
		}
		before, err := logical.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := seedStagePath(logical); err != nil {
			t.Fatal(err)
		}
		if err := inheritScratchWorkDir(logical, "/"); err != nil {
			t.Fatal(err)
		}
		after, err := logical.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatalf("explicit configuration changed: %s -> %s", before, after)
		}
	}
}

func TestStageEntryDefaultsKeepPublicationBaseUnknown(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`from "registry.invalid/base" as="source" { onbuild { label inherited="yes"; }; }
from "source" as="derived" { env observed="$PATH"; }
extend { copy "/payload" "/payload" from="derived"; }
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.CreateDemandDriven(def, planner.Options{Mode: planner.Publish, Platform: "linux/amd64"}, func(planner.FromSource) (planner.StageBind, error) {
		t.Fatal("invocation base resolved while publishing")
		return planner.StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), system.DefaultPathEnvUnix) || !strings.Contains(string(encoded), "$PATH") {
		t.Fatalf("publication lost symbolic base PATH: %s", encoded)
	}
	invocation, err := planner.InstantiateDemandDriven(plan.Component, planner.Options{}, nil, func(planner.FromSource) (planner.StageBind, error) {
		return planner.StageBind{BaseEnvironment: map[string]string{"PATH": "/actual-base"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, stage := range invocation.Stages {
		for _, operation := range stage.Operations {
			if operation.Name == "env" {
				found = true
				if operation.Properties["observed"] != "/actual-base" {
					t.Fatalf("bound invocation lost actual PATH: %+v", operation)
				}
			}
		}
	}
	if !found {
		t.Fatal("no materialized invocation ENV examined")
	}
}

func TestStageEntryDefaultsSeparateComponentCacheLowering(t *testing.T) {
	key, err := new(componentCache).key(&graphExecutor{store: componentCacheKeyStore{}}, validComponentTestKey().Input, &ResolvedComponentPlan{Identity: digest.FromString("component"), Plan: &planner.Plan{}}, nil, v1.Platform{OS: "linux", Architecture: "amd64"}, false)
	if err != nil {
		t.Fatal(err)
	}
	current, err := key.Digest()
	if err != nil {
		t.Fatal(err)
	}
	key.Lowering = "coopr-buildah-component-image-v4"
	prior, err := key.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if current == prior {
		t.Fatal("pre-default component lowering reused the current key")
	}
}

func TestStageEntryDefaultsFromExtendUsesKnownInvocationCaller(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`extend as="caller" {}
from "caller" as="child" { env observed="$PATH"; }
extend { from "child" {}; }
`))
	if err != nil {
		t.Fatal(err)
	}
	publication, err := planner.CreateDemandDriven(def, planner.Options{Mode: planner.Publish, Platform: "linux/amd64"}, func(planner.FromSource) (planner.StageBind, error) {
		t.Fatal("unexpected external base")
		return planner.StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(plan *planner.Plan, want string) {
		t.Helper()
		found := false
		for _, stage := range plan.Stages {
			for _, op := range stage.Operations {
				if op.Name == "env" {
					found = true
					if got := op.Properties["observed"]; got != want {
						t.Fatalf("observed=%q want %q", got, want)
					}
				}
			}
		}
		if !found {
			t.Fatal("missing child ENV")
		}
	}
	encoded, err := json.Marshal(publication)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), system.DefaultPathEnvUnix) {
		t.Fatal("publication invented a caller PATH")
	}

	invocation, err := planner.InstantiateDemandDriven(publication.Component, planner.Options{CallerEnvironment: map[string]string{"caller": "yes"}}, nil, func(planner.FromSource) (planner.StageBind, error) {
		t.Fatal("unexpected external base")
		return planner.StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	check(invocation, system.DefaultPathEnvUnix)
}

func TestStageEntryDefaultsScratchAliasChain(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`from "scratch" as="base" {}
from "base" as="middle" {}
from "middle" { env observed="$PATH"; }
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := planDefinitionWithExternalBases(context.Background(), def, planner.Options{Mode: planner.Build, Platform: "linux/amd64"}, func(context.Context, string, v1.Platform) (ResolvedImageSource, error) {
		t.Fatal("unexpected external base")
		return ResolvedImageSource{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	last := plan.Stages[len(plan.Stages)-1]
	if got := last.Operations[0].Properties["observed"]; got != system.DefaultPathEnvUnix {
		t.Fatalf("observed=%q", got)
	}
}
