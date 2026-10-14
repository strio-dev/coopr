package planner

import (
	"coopr/internal/imageconfig"
	"coopr/internal/onbuildparse"
	"github.com/openshift/imagebuilder"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestInheritedExposeNativeWordExpansion(t *testing.T) {
	for _, word := range []string{`$PORTS`, `"$PORTS"`, `'$PORTS'`, `80 $PORTS`, `${PORTS:-8080}`} {
		t.Run(word, func(t *testing.T) {
			parsed, err := onbuildparse.ParseDeferred("EXPOSE " + word)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, inst := range parsed {
				normalized, err := normalize(inst, scope{"PORTS": {value: "80 443", present: true}})
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, normalized.Arguments...)
			}
			want, err := imagebuilder.ProcessWords(word, []string{"PORTS=80 443"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("expanded arguments = %#v, native %#v", got, want)
			}
		})
	}
}

func TestInheritedCopyFlagsUseChildScope(t *testing.T) {
	for _, raw := range []string{
		`COPY --chown=1 --chown="$OWNER" --chmod="$MODE" --link=$LINK --parents=$PARENTS --exclude='$EXCLUDED' --exclude="$EXCLUDED" src /dst`,
		`COPY --link=$LINK --link=false --parents=$PARENTS --parents=false --chmod=$MODE --chmod=0600 src /dst`,
		`ADD --chown=1 --chown="$OWNER" --chmod=$MODE --keep-git-dir=$LINK --unpack=$PARENTS src /dst`,
	} {
		t.Run(raw, func(t *testing.T) {
			parsed, err := onbuildparse.ParseDeferred(raw)
			if err != nil {
				t.Fatal(err)
			}
			got, err := normalize(parsed[0], scope{"OWNER": {value: "2:3", present: true}, "MODE": {value: "0640", present: true}, "LINK": {value: "true", present: true}, "PARENTS": {value: "true", present: true}, "EXCLUDED": {value: "*.tmp", present: true}})
			if err != nil {
				t.Fatal(err)
			}
			if got.Properties["chown"] != "" && got.Properties["chown"] != "2:3" {
				t.Fatalf("chown: %#v", got)
			}
			wantMode := "0640"
			if strings.Contains(raw, "--chmod=0600") {
				wantMode = "0600"
			}
			if got.Properties["chmod"] != wantMode {
				t.Fatalf("chmod: %#v", got)
			}
			if strings.Contains(raw, "--link=false") {
				if got.Properties["link"] != "" && got.Properties["link"] != "false" {
					t.Fatalf("last link value: %#v", got)
				}
			} else if got.Name == "copy" && got.Properties["link"] != "true" {
				t.Fatalf("link: %#v", got)
			}
			if len(got.Children) > 0 && !reflect.DeepEqual(got.Children[0].Arguments, []string{"*.tmp", "*.tmp"}) {
				t.Fatalf("exclude quoting: %#v", got.Children)
			}
		})
	}
}

func TestInheritedExposePlanPreservesNativePorts(t *testing.T) {
	for _, ports := range []string{`$PORTS`, `"$PORTS"`, `80 443`, `${MISSING:-8080}`} {
		t.Run(ports, func(t *testing.T) {
			definition := parse(t, `from "example.com/base:latest" {}`)
			plan, err := CreateDemandDriven(definition, Options{Mode: Build}, func(FromSource) (StageBind, error) {
				inherited, err := onbuildparse.ParseDeferred("EXPOSE " + ports)
				return StageBind{BaseEnvironment: map[string]string{"PORTS": "80 443"}, Inherited: inherited}, err
			})
			if err != nil {
				t.Fatal(err)
			}
			native, err := imagebuilder.ParseDockerfile(strings.NewReader("EXPOSE " + ports))
			if err != nil {
				t.Fatal(err)
			}
			builder := imagebuilder.NewBuilder(nil)
			builder.Env = []string{"PORTS=80 443"}
			step := builder.Step()
			if err := step.Resolve(native.Children[0]); err != nil {
				t.Fatal(err)
			}
			if err := builder.Run(step, imagebuilder.NoopExecutor, true); err != nil {
				t.Fatal(err)
			}
			parsed, _ := onbuildparse.ParseDeferred("EXPOSE " + ports)
			if len(plan.Stages[0].Operations) != len(parsed) {
				t.Fatal("word-list expansion changed inherited instruction ordinals")
			}
			var gotPorts []string
			config := imageconfig.New()
			for _, operation := range plan.Stages[0].Operations {
				inst := operation.Instruction
				if inst.ProcessQuotes {
					t.Fatalf("unconsumed native quotes: %#v", inst)
				}
				if err := config.Apply(inst); err != nil {
					t.Fatal(err)
				}
				values, err := onbuildparse.ExposedPorts(inst.Arguments)
				if err != nil {
					t.Fatal(err)
				}
				gotPorts = append(gotPorts, values...)
			}
			want := make([]string, 0, len(builder.RunConfig.ExposedPorts))
			for port := range builder.RunConfig.ExposedPorts {
				want = append(want, string(port))
			}
			slices.Sort(gotPorts)
			slices.Sort(want)
			if !reflect.DeepEqual(gotPorts, want) {
				t.Fatalf("Coopr ports %#v, native %#v", gotPorts, want)
			}
		})
	}
}

func TestInheritedExposeEmptyNativeExpansionFails(t *testing.T) {
	parsed, err := onbuildparse.ParseDeferred(`EXPOSE $MISSING`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := normalize(parsed[0], scope{}); err == nil {
		t.Fatal("native EXPOSE without expanded ports accepted")
	}
}

func TestInheritedRepeatedDynamicFlagsValidateEveryValue(t *testing.T) {
	for _, raw := range []string{`COPY --link=$BAD --link=false src /dst`, `COPY --chmod=$BAD --chmod=0644 src /dst`, `ADD --keep-git-dir=$BAD --keep-git-dir=false src /dst`} {
		parsed, err := onbuildparse.ParseDeferred(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := normalize(parsed[0], scope{"BAD": {value: "invalid", present: true}}); err == nil {
			t.Fatalf("invalid first dynamic native flag accepted: %s", raw)
		}
	}
}

func TestInheritedCopyDynamicFlagsRetainStructuralGlobals(t *testing.T) {
	parsed, err := onbuildparse.ParseDeferred(`COPY --from=$SOURCE --chown=$OWNER src /dst`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := normalizeWithStructuralScope(parsed[0], scope{"OWNER": {value: "2:3", present: true}}, scope{"OWNER": {value: "2:3", present: true}, "SOURCE": {value: "producer", present: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Properties["from"] != "producer" || got.Properties["chown"] != "2:3" || got.DeferredOnBuild != "" {
		t.Fatalf("normalized instruction %#v", got)
	}
}

func TestInheritedCopySourceCannotBorrowStructuralGlobals(t *testing.T) {
	parsed, err := onbuildparse.ParseDeferred(`COPY --chown=$OWNER ["--from=$SOURCE", "/dst"]`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := normalizeWithStructuralScope(parsed[0], scope{"OWNER": {value: "2:3", present: true}}, scope{"OWNER": {value: "2:3", present: true}, "SOURCE": {value: "producer", present: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Arguments[0] != "--from=" {
		t.Fatalf("source borrowed undeclared global: %#v", got)
	}
}
