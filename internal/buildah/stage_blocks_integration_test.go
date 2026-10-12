package buildah

import (
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStageBlocksComponentDefaultsToLastExtend(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "packaged stage-block proof")
	f.write(t, "shared.coopr", `arg "payload" "payload"
arg "global" "component-global"
package as="assets" {
    arg "payload"
    copy "${payload}" "/payload"
}
extend as="earlier" { env selected="earlier"; }
extend as="valid" {
    arg "global"
    arg "channel" "component-default"
    arg "private" "component-private"
    arg "target"
    arg "platform"
    env component_global="${global}" component_channel="${channel}" component_target="${target}" component_platform="${platform}"
    copy "/payload" "/proof" from="assets"
}
from "scratch" as="unused" { copy "missing" "/missing"; }
package as="unused_assets" { copy "missing" "/missing"; }
`)
	for _, reference := range []string{"./shared.coopr#assets", "./shared.coopr#unused"} {
		options := f.options
		options.Output = Output{Path: filepath.Join(f.root, fmt.Sprintf("rejected-%d", len(reference)))}
		_, err := BuildPlan(f.ctx, testPlan(t, fmt.Sprintf(`from "scratch" {
    component %q
}
`, reference)), options)
		if err == nil || !strings.Contains(err.Error(), "selected component output must be an extend target") {
			t.Fatalf("%s output selection error = %v", reference, err)
		}
	}
	var coldLayers []string
	for _, name := range []string{"cold", "warm", "explicit"} {
		var logs strings.Builder
		layout := filepath.Join(f.root, name)
		reference := "./shared.coopr"
		if name == "explicit" {
			reference += "#valid"
		}
		plan := testPlan(t, fmt.Sprintf(`arg "global" "caller-global"
from "scratch" {
    arg "global"
    arg "channel" "caller-channel"
    env requested="bound-channel"
    component %q {
        arg "channel" "${requested}"
        arg "target" "ordinary-target"
        arg "platform" "ordinary-platform"
    }
    env caller_global="${global}" caller_channel="${channel}" private_after="${private}"
}
`, reference))
		_, err := BuildPlanSupervised(f.ctx, plan, SupervisedPlanOptions{
			Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, ComponentStoreDir: filepath.Join(f.root, "components"),
			CacheLocalDir: f.options.CacheLocalDir, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: &logs,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		for _, expected := range []string{
			"component_global=component-global", "component_channel=bound-channel",
			"component_target=ordinary-target", "component_platform=ordinary-platform",
			"caller_global=caller-global", "caller_channel=caller-channel", "private_after=",
		} {
			if !strings.Contains("\n"+strings.Join(image.Config.Env, "\n")+"\n", "\n"+expected+"\n") {
				t.Fatalf("%s config missing %q: %v", name, expected, image.Config.Env)
			}
		}
		if got := localComponentLastFile(t, layout, "proof"); got != "packaged stage-block proof" {
			t.Fatalf("%s packaged payload = %q", name, got)
		}
		layers := make([]string, len(manifest.Layers))
		for i, layer := range manifest.Layers {
			layers[i] = layer.Digest.String()
		}
		if name == "cold" {
			coldLayers = layers
		} else if !reflect.DeepEqual(coldLayers, layers) || !strings.Contains(logs.String(), "Using component cache") {
			t.Fatalf("warm component cache did not preserve layers: cold=%v warm=%v logs=%s", coldLayers, layers, &logs)
		}
	}
	_, layout := f.build(t, "earlier", `from "scratch" {
    component "./shared.coopr#earlier"
}
`)
	_, image := readPlanImage(t, layout)
	if !strings.Contains(strings.Join(image.Config.Env, "\n"), "selected=earlier") {
		t.Fatalf("explicit earlier target: %v", image.Config.Env)
	}
}

func TestExtendHelperStageThenCallerInstructionSelectsCaller(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "helper", "helper payload")
	f.write(t, "caller", "caller payload")
	f.write(t, "shared.coopr", `package as="assets" {
    copy "helper" "/helper"
}
extend as="runtime" {
    from "scratch" as="helper" {
        copy "/helper" "/helper" from="assets"
        env helper_only="yes"
    }
    copy "/helper" "/proof" from="helper"
    arg "unused" "argument-after-output"
}
`)
	for _, attempt := range []string{"cold", "warm"} {
		_, layout := f.build(t, attempt, `from "scratch" {
    copy "caller" "/caller"
    env caller="preserved"
    component "./shared.coopr#runtime"
}
`)
		_, image := readPlanImage(t, layout)
		if !strings.Contains(strings.Join(image.Config.Env, "\n"), "caller=preserved") || strings.Contains(strings.Join(image.Config.Env, "\n"), "helper_only=yes") {
			t.Fatalf("%s selected helper config instead of caller: %v", attempt, image.Config.Env)
		}
		if got := localComponentLastFile(t, layout, "proof"); got != "helper payload" {
			t.Fatalf("%s helper payload = %q", attempt, got)
		}
	}
}
