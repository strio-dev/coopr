package definition_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/openshift/imagebuilder"
	"github.com/strio-dev/coopr/internal/onbuildparse"
)

func nativeOnBuildConfig(t *testing.T, raw string) *imagebuilder.Builder {
	t.Helper()
	b := imagebuilder.NewBuilder(nil)
	b.RunConfig.Image = "scratch"
	ast, err := imagebuilder.ParseDockerfile(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	step := b.Step()
	if err = step.Resolve(ast.Children[0]); err == nil {
		err = b.Run(step, imagebuilder.NoopExecutor, true)
	}
	if err != nil {
		t.Fatalf("native metadata %q: %v", raw, err)
	}
	return b
}

func TestOnBuildPreservesEnvironmentAndLabelKeys(t *testing.T) {
	for _, kind := range []string{"env", "label"} {
		for _, key := range []string{"normal", "two words", "a\tb", `quote"key`, `quote'key`, `back\slash`, `trailing\`} {
			for _, properties := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%q/properties=%t", kind, key, properties), func(t *testing.T) {
					child := fmt.Sprintf("%s %q %q", kind, key, "value")
					if properties {
						child = fmt.Sprintf("%s %q=%q", kind, key, "value")
					}
					raw, _ := parseOnBuildRoundTrip(t, child)
					got, err := onbuildparse.ParseExpandedEnvironment(raw, nil)
					if err != nil {
						t.Fatal(err)
					}
					if len(got) != 1 || !reflect.DeepEqual(got[0].Arguments, []string{key, "value"}) {
						t.Fatalf("child metadata %q: %+v", raw, got)
					}
					native := nativeOnBuildConfig(t, raw)
					if kind == "env" {
						if !reflect.DeepEqual([]string(native.RunConfig.Env), []string{key + "=value"}) {
							t.Fatalf("native environment: %v", native.RunConfig.Env)
						}
					} else if native.RunConfig.Labels[key] != "value" {
						t.Fatalf("native labels: %v", native.RunConfig.Labels)
					}
				})
			}
		}
	}
}

func TestOnBuildJSONLookingShellCommandExecutesUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "[proof]"), []byte("#!/bin/sh\nprintf original-ok"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{`["proof"]`, ` ["proof"]`, "\t [\"proof\"]  "} {
		for _, kind := range []string{"run", "cmd", "entrypoint", "healthcheck"} {
			t.Run(fmt.Sprintf("%s/%q", kind, command), func(t *testing.T) {
				control := exec.Command("sh", "-c", command)
				control.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
				want, err := control.CombinedOutput()
				if err != nil {
					t.Fatalf("original command: %v", err)
				}
				raw, parsed := parseOnBuildRoundTrip(t, fmt.Sprintf("%s %q", kind, command))
				native := nativeOnBuildConfig(t, raw)
				var argv []string
				switch kind {
				case "run":
					if len(parsed) != 1 || parsed[0].Form != "shell" {
						t.Fatalf("RUN metadata %v", parsed)
					}
					argv = []string{"sh", "-c", parsed[0].Arguments[0]}
				case "cmd":
					argv = []string(native.RunConfig.Cmd)
				case "entrypoint":
					argv = []string(native.RunConfig.Entrypoint)
				case "healthcheck":
					argv = []string{"sh", "-c", native.RunConfig.Healthcheck.Test[1]}
				}
				actual := exec.Command(argv[0], argv[1:]...)
				actual.Env = control.Env
				output, err := actual.CombinedOutput()
				if err != nil || string(output) != string(want) {
					t.Fatalf("metadata %q argv=%q output=%q: %v, want %q", raw, argv, output, err, want)
				}
			})
		}
	}
}
