package planner

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/onbuildparse"
	"github.com/openshift/imagebuilder"
)

// Exercise the native parser/evaluator and the complete planner/config boundary,
// including ordering and acceptance, rather than reproducing expansion rules.
func TestNativeInstructionNormalization(t *testing.T) {
	cases := []string{
		`ARG A=one B=$A` + "\n" + `ENV RESULT=$B`,
		`ARG A=one B=${A:-fallback}` + "\n" + `ENV RESULT=$B`,
		`ENV $KEY=value SECOND=two`, `ENV Z=one A=two Z=three`,
		`ENV A=one B=$A`, `ENV RESULT=$TARGETARCH`, `ARG A=$TARGETARCH` + "\n" + `ENV RESULT=$A`, `EXPOSE $MISSING 80`, `EXPOSE $MISSING`,
		`USER ""`, `WORKDIR ""`,
	}
	for _, key := range []string{"TARGETOS", "TARGETARCH", "TARGETVARIANT", "TARGETPLATFORM", "BUILDOS", "BUILDARCH", "BUILDVARIANT", "BUILDPLATFORM"} {
		cases = append(cases, "ARG "+key+"=custom\nENV RESULT=$"+key)
	}
	for _, instruction := range []string{"ENV", "LABEL", "USER", "WORKDIR"} {
		for _, word := range []string{`${MISSING-default}`, `${X?bad}`, `${MISSING+set}`} {
			if instruction == "ENV" || instruction == "LABEL" {
				word = "A=" + word
			}
			cases = append(cases, instruction+" "+word)
		}
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			native := imagebuilder.NewBuilder(nil)
			native.Env = []string{"KEY=EXPANDED", "X=set"}
			native.RunConfig.WorkingDir = "/current"
			nodes, nativeErr := imagebuilder.ParseDockerfile(strings.NewReader(raw))
			if nativeErr == nil {
				for _, node := range nodes.Children {
					step := native.Step()
					nativeErr = step.Resolve(node)
					if nativeErr == nil {
						nativeErr = native.Run(step, imagebuilder.NoopExecutor, true)
					}
					if nativeErr != nil {
						break
					}
				}
			}
			var inherited []definition.Instruction
			var cooprErr error
			for _, line := range strings.Split(raw, "\n") {
				var items []definition.Instruction
				items, cooprErr = onbuildparse.ParseDeferred(line)
				if cooprErr != nil {
					break
				}
				inherited = append(inherited, items...)
			}
			var plan *Plan
			if cooprErr == nil {
				plan, cooprErr = Create(parse(t, `from "example.com/base:latest" {}`), Options{Mode: Build, StageBinds: map[string]StageBind{"0": {Inherited: inherited, BaseEnvironment: map[string]string{"KEY": "EXPANDED", "X": "set"}}}})
			}
			if (cooprErr == nil) != (nativeErr == nil) {
				t.Fatalf("native error=%v; Coopr error=%v", nativeErr, cooprErr)
			}
			if nativeErr != nil {
				return
			}
			config, err := imageconfig.Parse([]byte(`{"config":{"WorkingDir":"/current"}}`))
			if err != nil {
				t.Fatal(err)
			}
			for _, operation := range plan.Stages[0].Operations {
				if err := config.Apply(operation.Instruction); err != nil {
					t.Fatal(err)
				}
			}
			got, err := config.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			// Compare only changed metadata: native.Env is expansion input, not RunConfig.Env.
			var observed struct {
				Config struct {
					Env              []string
					User, WorkingDir string
					Labels           map[string]string
					ExposedPorts     map[string]struct{}
				}
			}
			if err := json.Unmarshal(got, &observed); err != nil {
				t.Fatal(err)
			}
			for port := range native.RunConfig.ExposedPorts {
				if _, ok := observed.Config.ExposedPorts[string(port)]; !ok {
					t.Fatalf("missing native port %s in %s", port, got)
				}
			}
			if !slices.Equal(observed.Config.Env, []string(native.RunConfig.Env)) || observed.Config.User != native.RunConfig.User || observed.Config.WorkingDir != native.RunConfig.WorkingDir || !maps.Equal(observed.Config.Labels, native.RunConfig.Labels) || len(observed.Config.ExposedPorts) != len(native.RunConfig.ExposedPorts) {
				t.Fatalf("Coopr config=%s; native=%+v", got, native.RunConfig)
			}
		})
	}
}

func TestAuthoredEmptyMetadataAndEnvironmentOrder(t *testing.T) {
	for _, body := range []string{`user ""`, `workdir ""`, `env Z="one" A="two"`} {
		t.Run(body, func(t *testing.T) {
			plan, err := Create(parse(t, "from \"scratch\" {\n"+body+"\n}"), Options{Mode: Build})
			if err != nil {
				t.Fatal(err)
			}
			config, err := imageconfig.Parse([]byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range plan.Stages[0].Operations {
				if err := config.Apply(op.Instruction); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(body, "env") {
				data, _ := config.MarshalJSON()
				var got struct{ Config struct{ Env []string } }
				_ = json.Unmarshal(data, &got)
				if !reflect.DeepEqual(got.Config.Env, []string{"Z=one", "A=two"}) {
					t.Fatalf("ENV order=%v", got.Config.Env)
				}
			}
		})
	}
}
