package buildah

import (
	"bytes"
	"encoding/json"
	"fmt"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"path/filepath"
	"strings"
	"testing"
)

func TestComponentEnvironmentEntryAndReturn(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "shared.coopr", `extend {
    arg "private" "callee"
    env captured="${path}" value="returned"
}
`)
	for _, attempt := range []string{"cold", "warm"} {
		_, layout := f.build(t, attempt, `from "scratch" {
    arg "value" "caller"
    env path="/expected"
    component "./shared.coopr"
    arg "default" "${value}"
    env after="${value}" default_after="${default}" private_after="${private}"
}
`)
		_, image := readPlanImage(t, layout)
		for _, expected := range []string{"captured=/expected", "after=returned", "default_after=returned", "private_after="} {
			if !strings.Contains("\n"+strings.Join(image.Config.Env, "\n")+"\n", "\n"+expected+"\n") {
				t.Fatalf("%s config missing %q: %v", attempt, expected, image.Config.Env)
			}
		}
	}
}

func TestComponentEnvironmentShadowsCallerRunArgument(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "shared.coopr", `extend { env value="returned"; }
`)
	for _, attempt := range []string{"cold", "warm"} {
		_, layout := f.build(t, attempt, fmt.Sprintf(`from %q {
    arg "value" "caller"
    component "./shared.coopr"
    run "printf '%%s' \"$value\" > /proof"
}
`, base.reference))
		if got := localComponentLastFile(t, layout, "proof"); got != "returned" {
			t.Fatalf("%s RUN used %q, want returned image ENV", attempt, got)
		}
	}
}

func TestComponentEnvironmentNestedParallelLayersAndCaches(t *testing.T) {
	for _, noLayers := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-layers=%t", noLayers), func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.options.Jobs = 2
			f.options.Lifecycle.NoLayers = noLayers
			base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
			f.write(t, "inner.coopr", `arg "selected"
extend {
    env nested="${selected}"
}
`)
			f.write(t, "shared.coopr", `extend {
    layer {
        env captured="${caller}" value="returned"
        component "./inner.coopr" { arg "selected" "${captured}-${value}"; }
    }
}
`)
			source := fmt.Sprintf(`from %q as="left" {
    env caller="left"
    layer { component "./shared.coopr"; }
    arg "value" "argument"
    run "printf '%%s/%%s' \"$captured\" \"$nested\" >/proof"
}
from %q as="right" {
    env caller="right"
    layer { component "./shared.coopr"; }
    run "printf '%%s/%%s' \"$captured\" \"$nested\" >/proof"
}
from "scratch" {
    copy "/proof" "/left" from="left"
    copy "/proof" "/right" from="right"
}
`, base.reference, base.reference)
			for _, attempt := range []string{"cold", "warm"} {
				_, layout := f.build(t, attempt, source)
				manifest, _ := readPlanImage(t, layout)
				wantLayers := 2
				if noLayers {
					wantLayers = 1
				}
				if len(manifest.Layers) != wantLayers {
					t.Fatalf("%s output layers=%d, want%d", attempt, len(manifest.Layers), wantLayers)
				}
				for index, name := range []string{"left", "right"} {
					layerIndex := index
					if noLayers {
						layerIndex = 0
					}
					got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[layerIndex].Digest.Encoded()), name)
					if want := name + "/" + name + "-returned"; got != want {
						t.Fatalf("%s %s proof=%q want%q", attempt, name, got, want)
					}
				}
			}
		})
	}
}

func TestPublishedComponentEnvironmentReturnAndFreshStoreCache(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.options.Resolver = localConfigComponentResolver(t, f.ctx, f.root, `extend {
    env captured="${caller}" value="returned"
}
`, false)
	source := `from "scratch" {
    arg "value" "argument"
    env caller="current"
    component "config"
    arg "default" "${value}"
    env result="${captured}-${value}-${default}"
}
`
	for _, attempt := range []string{"cold", "warm", "fresh-store"} {
		if attempt == "fresh-store" {
			f.options.Store = cacheTestStore(filepath.Join(f.root, "fresh-store"))
		}
		result, layout := f.build(t, attempt, source)
		_, image := readPlanImage(t, layout)
		if !strings.Contains("\n"+strings.Join(image.Config.Env, "\n")+"\n", "\nresult=current-returned-returned\n") {
			t.Fatalf("%s ENV =%v", attempt, image.Config.Env)
		}
		if attempt != "cold" && result.CacheStats.Hits == 0 {
			t.Fatalf("%s did not restore published component cache: %+v", attempt, result.CacheStats)
		}
	}
}

// This output's ENV comes from an OCI config rewritten by RUN, rather than
// component ENV instructions or a registry config available while planning.
func TestComponentEnvironmentGeneratedOCIOutput(t *testing.T) {
	for _, jobs := range []int{1, 2} {
		t.Run(fmt.Sprintf("jobs=%d", jobs), func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.options.Jobs = jobs
			componentSourceLayout(t, f)
			input, err := orasoci.NewWithContext(f.ctx, filepath.Join(f.contextDir, "input"))
			if err != nil {
				t.Fatal(err)
			}
			manifestDescriptor, err := input.Resolve(f.ctx, "latest")
			if err != nil {
				t.Fatal(err)
			}
			manifestBytes, err := content.FetchAll(f.ctx, input, manifestDescriptor)
			if err != nil {
				t.Fatal(err)
			}
			var manifest v1.Manifest
			if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
				t.Fatal(err)
			}
			configBytes, err := content.FetchAll(f.ctx, input, manifest.Config)
			if err != nil {
				t.Fatal(err)
			}
			var image v1.Image
			if err := json.Unmarshal(configBytes, &image); err != nil {
				t.Fatal(err)
			}
			image.Config.Env = []string{"PATH=/bin:/usr/bin", "value=initial"}
			configBytes, err = json.Marshal(image)
			if err != nil {
				t.Fatal(err)
			}
			manifest.Config = descriptor(v1.MediaTypeImageConfig, configBytes)
			if err := input.Push(f.ctx, manifest.Config, bytes.NewReader(configBytes)); err != nil {
				t.Fatal(err)
			}
			manifestBytes, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			manifestDescriptor = descriptor(v1.MediaTypeImageManifest, manifestBytes)
			if err := input.Push(f.ctx, manifestDescriptor, bytes.NewReader(manifestBytes)); err != nil {
				t.Fatal(err)
			}
			if err := input.Tag(f.ctx, manifestDescriptor, "latest"); err != nil {
				t.Fatal(err)
			}
			// Rewrite all content-addressed edges in the mounted output layout.
			script := fmt.Sprintf(`set -eu
cp -a /ctx/input /ctx/out
cd /ctx/out
/bin/busybox sed 's/value=initial/value=runtime-returned/' blobs/sha256/%s > /tmp/config
config_hash=$(/bin/busybox sha256sum /tmp/config); config_hash=${config_hash%%%% *}
config_size=$(/bin/busybox wc -c </tmp/config)
cp /tmp/config "blobs/sha256/$config_hash"
/bin/busybox sed "s/sha256:%s/sha256:$config_hash/; s/\"size\":%d/\"size\":$config_size/" blobs/sha256/%s >/tmp/manifest
manifest_hash=$(/bin/busybox sha256sum /tmp/manifest); manifest_hash=${manifest_hash%%%% *}
manifest_size=$(/bin/busybox wc -c </tmp/manifest)
cp /tmp/manifest "blobs/sha256/$manifest_hash"
/bin/busybox sed "s/sha256:%s/sha256:$manifest_hash/; s/\"size\":%d/\"size\":$manifest_size/" index.json >/tmp/index
cp /tmp/index index.json
`, manifest.Config.Digest.Encoded(), manifest.Config.Digest.Encoded(), manifest.Config.Size, manifestDescriptor.Digest.Encoded(), manifestDescriptor.Digest.Encoded(), manifestDescriptor.Size)
			f.write(t, "rewrite.sh", script)
			f.write(t, "generated.coopr", `extend as="producer" {
    run "sh /ctx/rewrite.sh" { mount "bind" target="/ctx" rw="true"; }
}
extend {
    from "oci:out:latest" after="producer" {}
}
`)
			base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
			for _, attempt := range []string{"cold", "warm"} {
				source := fmt.Sprintf(`from %q as="caller" {
    env value="caller"
    component "./generated.coopr"
    arg "default" "${value}"
    env after="${value}" default_after="${default}"
    run "printf '%%s' \"$value\" >/proof"
}
`, base.reference)
				if jobs == 2 {
					// Two initially ready stages force the isolated-worker path.
					// Only caller writes the generated OCI layout.
					source = `from "scratch" as="independent" { env ready="yes"; }
` + source + `from "caller" after="independent" {}`
				}
				_, layout := f.build(t, attempt, source)
				_, output := readPlanImage(t, layout)
				for _, expected := range []string{"after=runtime-returned", "default_after=runtime-returned"} {
					if !strings.Contains("\n"+strings.Join(output.Config.Env, "\n")+"\n", "\n"+expected+"\n") {
						t.Fatalf("%s missing %s: %v", attempt, expected, output.Config.Env)
					}
				}
				if got := localComponentLastFile(t, layout, "proof"); got != "runtime-returned" {
					t.Fatalf("%s RUN got %q", attempt, got)
				}
			}
		})
	}
}

func TestLocalPackageProducerComponentReturnDependency(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "proof", "packaged-return")
	f.write(t, "return.coopr", `extend {
    env source="helper" destination="/proof"
}
`)
	f.write(t, "packaged.coopr", `from "scratch" as="helper" {
    copy "proof" "/proof"
}
from "scratch" as="producer" {
    component "./return.coopr"
    copy "/proof" "${destination}" from="${source}"
}
package as="assets" {
    copy "/proof" "/proof" from="producer"
}
extend {
    copy "/proof" "/proof" from="assets"
}
`)
	for _, attempt := range []string{"cold", "warm"} {
		_, layout := f.build(t, attempt, `from "scratch" {
    component "./packaged.coopr"
}
`)
		if got := localComponentLastFile(t, layout, "proof"); got != "packaged-return" {
			t.Fatalf("%s packaged proof=%q", attempt, got)
		}
	}
}

func TestSuppliedComponentPlanFreshRecipePreservesExplicitBind(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "shared.coopr", `extend { env value="actual-return"; }`)
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`from %q {
    env before="${token}"
    component "./shared.coopr"
    env after="${value}"
}
`, base.reference)))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Create(def, planner.Options{Mode: planner.Build, AwaitComponentReturns: true, StageBinds: map[string]planner.StageBind{"0": {BaseEnvironment: map[string]string{"token": "supplied"}}}})
	if err != nil {
		t.Fatal(err)
	}
	replanned, err := plan.ReplanComponentReturn(*plan.Stages[0].AwaitingComponent, map[string]string{"value": "prior-execution"})
	if err != nil {
		t.Fatal(err)
	}
	for index, supplied := range []*planner.Plan{plan, replanned} {
		layout := filepath.Join(f.root, fmt.Sprintf("supplied-%d", index))
		_, err := BuildPlanSupervised(f.ctx, supplied, SupervisedPlanOptions{
			Store: f.options.Store, ContextDir: f.contextDir, ComponentStoreDir: f.options.Resolver.ComponentStoreDir(), CacheLocalDir: f.options.CacheLocalDir,
			Isolation: f.options.Isolation, Runtime: f.options.Runtime, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Output: Output{Path: layout},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, image := readPlanImage(t, layout)
		for _, expected := range []string{"before=supplied", "after=actual-return"} {
			if !strings.Contains("\n"+strings.Join(image.Config.Env, "\n")+"\n", "\n"+expected+"\n") {
				t.Fatalf("plan %d missing %q: %v", index, expected, image.Config.Env)
			}
		}
	}
}
