package buildah

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLocalComponentNamedTargetsAndDefault(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "packaged settings")
	f.write(t, "shared.coopr", `package as="assets" {
    copy "payload" "/payload"
}
extend as="first" {
    arg "target" "default"
    env selected="first" argument="${target}"
    copy "/payload" "/first" from="assets"
}
extend as="last" {
    env selected="last"
    copy "/payload" "/last" from="assets"
}
`)
	var firstLayers []string
	for i, tc := range []struct {
		reference, selected, file string
	}{
		{"./shared.coopr#first", "first", "first"},
		{"./shared.coopr", "last", "last"},
		{"./shared.coopr#last", "last", "last"},
		{"./shared.coopr#FIRST", "first", "first"},
		{"./shared.coopr#first", "first", "first"},
	} {
		_, layout := f.build(t, fmt.Sprintf("selected-%d", i), fmt.Sprintf(`from "scratch" {
    component %q {
        arg "target" "ordinary-argument"
    }
}
`, tc.reference))
		manifest, image := readPlanImage(t, layout)
		if !strings.Contains(strings.Join(image.Config.Env, "\n"), "selected="+tc.selected) {
			t.Fatalf("%s environment = %v", tc.reference, image.Config.Env)
		}
		if tc.selected == "first" && !strings.Contains(strings.Join(image.Config.Env, "\n"), "argument=ordinary-argument") {
			t.Fatalf("target argument was consumed as a selector: %v", image.Config.Env)
		}
		if got := localComponentLastFile(t, layout, tc.file); got != "packaged settings" {
			t.Fatalf("%s payload = %q", tc.reference, got)
		}
		if tc.selected == "first" {
			layers := make([]string, len(manifest.Layers))
			for j, layer := range manifest.Layers {
				layers[j] = layer.Digest.String()
			}
			if firstLayers == nil {
				firstLayers = layers
			} else if !reflect.DeepEqual(firstLayers, layers) {
				t.Fatalf("cached selected image layers changed: %v != %v", firstLayers, layers)
			}
		}
	}
	_, layout := f.build(t, "expanded", `from "scratch" {
    arg "selected" "first"
    component "./shared.coopr#${selected}" {
        arg "target" "expanded-argument"
    }
}
`)
	_, image := readPlanImage(t, layout)
	if !strings.Contains(strings.Join(image.Config.Env, "\n"), "argument=expanded-argument") {
		t.Fatalf("expanded target environment = %v", image.Config.Env)
	}
}

func TestLocalComponentNamedTargetPrunesUnusedOutput(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "shared.coopr", `extend as="usable" {
    env selected="usable"
}
extend as="unused" {
    copy "missing-input" "/missing-input"
}
`)
	_, layout := f.build(t, "pruned", `from "scratch" {
    component "./shared.coopr#usable"
}
`)
	_, image := readPlanImage(t, layout)
	if !strings.Contains(strings.Join(image.Config.Env, "\n"), "selected=usable") {
		t.Fatalf("selected target config = %v", image.Config.Env)
	}
}

func TestLocalComponentTargetsRemainDistinctInParallelBranches(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.options.Jobs = 2
	f.write(t, "left", "left payload")
	f.write(t, "right", "right payload")
	f.write(t, "shared.coopr", `package as="assets" {
    copy "left" "/left"
    copy "right" "/right"
}
extend as="left" {
    copy "/left" "/proof" from="assets"
}
extend as="right" {
    copy "/right" "/proof" from="assets"
}
`)
	for _, name := range []string{"cold", "warm"} {
		_, layout := f.build(t, name, `from "scratch" as="left" {
    component "./shared.coopr#left"
}
from "scratch" as="right" {
    component "./shared.coopr#right"
}
from "scratch" {
    copy "/proof" "/left" from="left"
    copy "/proof" "/right" from="right"
}
`)
		manifest, _ := readPlanImage(t, layout)
		for index, target := range []string{"left", "right"} {
			layer := manifest.Layers[len(manifest.Layers)-2+index]
			if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", layer.Digest.Encoded()), target); got != target+" payload" {
				t.Fatalf("%s %s target payload = %q", name, target, got)
			}
		}
	}
}

func TestLocalComponentSameFileTargetDependencies(t *testing.T) {
	for _, packaging := range []bool{false, true} {
		t.Run(fmt.Sprint(packaging), func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "payload", "prepared by leaf")
			if packaging {
				f.write(t, "shared.coopr", `from "scratch" as="producer" {
    component "./shared.coopr#leaf"
    copy "payload" "/proof"
}
package as="assets" {
    copy "/proof" "/proof" from="producer"
}
extend as="wrapper" {
    copy "/proof" "/proof" from="assets"
}
extend as="leaf" {
    env leaf="yes"
}
`)
			} else {
				f.write(t, "shared.coopr", `extend as="leaf" {
    env leaf="yes"
}
extend as="wrapper" {
    component "./shared.coopr#leaf"
    env wrapper="yes"
}
`)
			}
			_, layout := f.build(t, "finite", `from "scratch" {
    component "./shared.coopr#wrapper"
}
`)
			if packaging {
				if got := localComponentLastFile(t, layout, "proof"); got != "prepared by leaf" {
					t.Fatalf("package producer payload = %q", got)
				}
			} else {
				_, image := readPlanImage(t, layout)
				if got := strings.Join(image.Config.Env, "\n"); !strings.Contains(got, "leaf=yes") || !strings.Contains(got, "wrapper=yes") {
					t.Fatalf("finite target dependency config = %v", image.Config.Env)
				}
			}
		})
	}
}

func TestLocalComponentNamedTargetCycles(t *testing.T) {
	for _, tc := range []struct {
		name, source, reference string
	}{
		{"cross-target", `extend as="a" {
    component "./cycle.coopr#b"
}
extend as="b" {
    component "./cycle.coopr#a"
}
`, "./cycle.coopr#a"},
		{"default-alias", `extend as="final" {
    component "./cycle.coopr#FINAL"
}
`, "./cycle.coopr"},
		{"symlink-alias", `extend as="final" {
    component "./alias.coopr#FINAL"
}
`, "./cycle.coopr#final"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "cycle.coopr", tc.source)
			if err := os.Symlink("cycle.coopr", filepath.Join(f.contextDir, "alias.coopr")); err != nil {
				t.Fatal(err)
			}
			options := f.options
			options.Output = Output{Path: filepath.Join(f.root, "cycle")}
			_, err := BuildPlan(f.ctx, testPlan(t, fmt.Sprintf(`from "scratch" {
    component %q
}
`, tc.reference)), options)
			if err == nil || !strings.Contains(err.Error(), "invocation cycle") {
				t.Fatalf("cycle error = %v", err)
			}
		})
	}
}

func TestLocalComponentInvalidNamedTargets(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "shared.coopr", `from "scratch" as="producer" {}
package as="assets" {}
extend as="valid" {}
`)
	for _, tc := range []struct{ target, message string }{
		{"missing", "unknown target"},
		{"assets", "selected component output must be an extend target"},
		{"producer", "selected component output must be an extend target"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			options := f.options
			options.Output = Output{Path: filepath.Join(f.root, tc.target)}
			_, err := BuildPlan(f.ctx, testPlan(t, fmt.Sprintf(`from "scratch" {
    component "./shared.coopr#%s"
}
`, tc.target)), options)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("target %s error = %v", tc.target, err)
			}
		})
	}
}

func TestLocalComponentNamedTargetUsesCopyPathRules(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "shared.coopr", `extend as="selected" {
    env selected="yes"
}
`)
	for i, reference := range []string{"./shared.coopr#selected", "/shared.coopr#selected", "../shared.coopr#selected", "./missing/../shared.coopr#selected", "./*.coopr#selected"} {
		_, layout := f.build(t, fmt.Sprintf("path-%d", i), fmt.Sprintf(`from "scratch" {
    component %q
}
`, reference))
		_, image := readPlanImage(t, layout)
		if !strings.Contains(strings.Join(image.Config.Env, "\n"), "selected=yes") {
			t.Fatalf("%s selected config = %v", reference, image.Config.Env)
		}
	}
	f.write(t, ".cooprignore", "shared.coopr\n")
	options := f.options
	options.Output = Output{Path: filepath.Join(f.root, "ignored")}
	if _, err := BuildPlan(f.ctx, testPlan(t, `from "scratch" {
    component "./shared.coopr#selected"
}
`), options); err == nil {
		t.Fatal("target selector bypassed ignored definition")
	}
}
