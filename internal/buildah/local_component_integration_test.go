package buildah

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coopr/internal/oci"
	"coopr/internal/planner"
	"go.podman.io/image/v5/types"
)

type localComponentFixture struct {
	ctx        context.Context
	root       string
	contextDir string
	options    PlanOptions
}

func newLocalComponentFixture(t *testing.T) localComponentFixture {
	t.Helper()
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: filepath.Join(root, "components"), ImageStoreDir: filepath.Join(root, "images")})
	if err != nil {
		t.Fatal(err)
	}
	return localComponentFixture{ctx: ctx, root: root, contextDir: contextDir, options: PlanOptions{
		Store: cacheTestStore(root), ContextDir: contextDir, Isolation: "rootless", Runtime: "crun", Resolver: resolver,
		CacheLocalDir: filepath.Join(root, "cache"),
		SystemContext: &types.SystemContext{SignaturePolicyPath: writeComponentTestPolicy(t, root), BigFilesTemporaryDir: root},
	}}
}

func (f localComponentFixture) write(t *testing.T, name, contents string) {
	t.Helper()
	path := filepath.Join(f.contextDir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f localComponentFixture) build(t *testing.T, name, source string) (Result, string) {
	t.Helper()
	options := f.options
	options.Output = Output{Path: filepath.Join(f.root, name)}
	result, err := BuildPlan(f.ctx, testPlan(t, source), options)
	if err != nil {
		t.Fatal(err)
	}
	return result, options.Output.Path
}

func localComponentLastFile(t *testing.T, layout, name string) string {
	t.Helper()
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) == 0 {
		t.Fatal("expected a filesystem layer")
	}
	last := manifest.Layers[len(manifest.Layers)-1]
	return readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), name)
}

func TestLocalComponentSharedByContainersRunsAgainstCaller(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "components/shared.coopr", `package as="assets"
copy "payload" "/payload"
extend
copy "/payload" "/payload" from="assets"
copy "caller" "/call-context"
run "cat /caller /call-context /payload > /proof" network="none"
`)
	f.write(t, "payload", "shared\n")
	for _, caller := range []string{"first", "second"} {
		f.write(t, "caller", caller+"\n")
		_, layout := f.build(t, caller, fmt.Sprintf("from %q\ncopy \"caller\" \"/caller\"\ncomponent \"./components/shared.coopr\"\n", base.reference))
		if got := localComponentLastFile(t, layout, "proof"); got != caller+"\n"+caller+"\nshared\n" {
			t.Fatalf("caller proof = %q", got)
		}
	}
}

func TestLocalComponentInvalidatesDefinitionAndPackageInputs(t *testing.T) {
	f := newLocalComponentFixture(t)
	definition := `package as="assets"
copy "payload" "/payload"
extend
copy "/payload" "/payload" from="assets"
env revision="one"
`
	f.write(t, "shared.coopr", definition)
	f.write(t, "payload", "one")
	source := "from \"scratch\"\ncomponent \"./shared.coopr\"\n"
	cold, _ := f.build(t, "cold", source)
	warm, warmLayout := f.build(t, "warm", source)
	if cold.CacheStats.Stored == 0 || warm.CacheStats.Hits == 0 {
		t.Fatalf("cold=%+v warm=%+v", cold.CacheStats, warm.CacheStats)
	}
	if got := localComponentLastFile(t, warmLayout, "payload"); got != "one" {
		t.Fatalf("warm payload = %q", got)
	}
	f.write(t, "payload", "two")
	_, changedLayout := f.build(t, "changed-payload", source)
	if got := localComponentLastFile(t, changedLayout, "payload"); got != "two" {
		t.Fatalf("changed payload = %q", got)
	}
	f.write(t, "shared.coopr", strings.ReplaceAll(definition, `revision="one"`, `revision="two"`))
	_, changedDefinition := f.build(t, "changed-definition", source)
	_, image := readPlanImage(t, changedDefinition)
	if strings.Join(image.Config.Env, "\n") != "revision=two" {
		t.Fatalf("changed definition env = %v", image.Config.Env)
	}
}

func TestLocalComponentParametersRemainDistinctInParallelBranches(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.options.Jobs = 2
	f.write(t, "left", "left payload")
	f.write(t, "right", "right payload")
	f.write(t, "shared.coopr", `arg "value" "default"
package as="assets"
arg "value"
copy "$value" "/payload"
extend
arg "value"
copy "/payload" "/$value" from="assets"
`)
	_, layout := f.build(t, "parallel", `from "scratch" as="left"
component "./shared.coopr" value="left"
from "scratch" as="right"
component "./shared.coopr" value="right"
from "scratch"
copy "/left" "/left" from="left"
copy "/right" "/right" from="right"
`)
	// The final COPY layer contains only right; left is in the preceding layer.
	manifest, _ := readPlanImage(t, layout)
	for index, name := range []string{"left", "right"} {
		layer := manifest.Layers[len(manifest.Layers)-2+index]
		if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", layer.Digest.Encoded()), name); got != name+" payload" {
			t.Fatalf("%s parameter payload = %q", name, got)
		}
	}
}

func TestLocalComponentNestedReferencesUseContextRoot(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "nested/outer.coopr", "extend\ncomponent \"./nested/inner.coopr\"\n")
	f.write(t, "nested/inner.coopr", "extend\nenv nested=\"yes\"\n")
	_, layout := f.build(t, "nested", "from \"scratch\"\ncomponent \"./nested/outer.coopr\"\n")
	_, image := readPlanImage(t, layout)
	if strings.Join(image.Config.Env, "\n") != "nested=yes" {
		t.Fatalf("nested config = %v", image.Config.Env)
	}
}

func TestLocalComponentIdenticalDefinitionsCanFormFinitePackageGraph(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "proof", "finite graph")
	f.write(t, "leaf.coopr", "package as=\"files\"\ncopy \"proof\" \"/proof\"\nextend\ncopy \"/proof\" \"/proof\" from=\"files\"\n")
	const wrapper = `arg "next" "./leaf.coopr"
from "scratch" as="producer"
arg "next"
component "${next}" next="./leaf.coopr"
package as="payload"
copy "/proof" "/proof" from="producer"
extend
copy "/proof" "/proof" from="payload"
`
	f.write(t, "a.coopr", wrapper)
	f.write(t, "b.coopr", wrapper)
	_, layout := f.build(t, "finite", "from \"scratch\"\ncomponent \"./a.coopr\" next=\"./b.coopr\"\n")
	if got := localComponentLastFile(t, layout, "proof"); got != "finite graph" {
		t.Fatalf("finite graph proof = %q", got)
	}
}

func TestLocalComponentCyclesIncludeAliasedPaths(t *testing.T) {
	for _, reference := range []string{"./cycle.coopr", "./nested/../cycle.coopr"} {
		t.Run(reference, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "cycle.coopr", fmt.Sprintf("extend\ncomponent %q\n", reference))
			options := f.options
			options.Output = Output{Path: filepath.Join(f.root, "result")}
			_, err := BuildPlan(f.ctx, testPlan(t, "from \"scratch\"\ncomponent \"./cycle.coopr\"\n"), options)
			if err == nil || !strings.Contains(err.Error(), "component invocation cycle at") {
				t.Fatalf("cycle error = %v", err)
			}
		})
	}
}

func TestLocalComponentHonorsIgnoredDefinitionsAndPayloads(t *testing.T) {
	for _, ignored := range []string{"shared.coopr", "payload"} {
		t.Run(ignored, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "shared.coopr", "package as=\"assets\"\ncopy \"payload\" \"/payload\"\nextend\ncopy \"/payload\" \"/payload\" from=\"assets\"\n")
			f.write(t, "payload", "secret")
			f.write(t, ".containerignore", ignored+"\n")
			options := f.options
			options.Output = Output{Path: filepath.Join(f.root, "result")}
			_, err := BuildPlan(f.ctx, testPlan(t, "from \"scratch\"\ncomponent \"./shared.coopr\"\n"), options)
			if err == nil {
				t.Fatalf("ignored %s was consumed", ignored)
			}
		})
	}
}

func TestLocalComponentPathsFollowCopyNormalization(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "shared.coopr", "extend\nenv selected=\"context-root\"\n")
	for index, reference := range []string{"./shared.coopr", "/shared.coopr", "../shared.coopr", "./missing/../shared.coopr"} {
		t.Run(reference, func(t *testing.T) {
			_, layout := f.build(t, fmt.Sprintf("normalized-%d", index), fmt.Sprintf("from \"scratch\"\ncopy %q \"/definition\"\ncomponent %q\n", reference, reference))
			_, image := readPlanImage(t, layout)
			if strings.Join(image.Config.Env, "\n") != "selected=context-root" {
				t.Fatalf("%s config = %v", reference, image.Config.Env)
			}
		})
	}
}

func TestLocalComponentSymlinkCannotConsumeOutsideContext(t *testing.T) {
	f := newLocalComponentFixture(t)
	outside := filepath.Join(f.root, "outside.coopr")
	if err := os.WriteFile(outside, []byte("extend\nenv leaked=\"yes\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.contextDir, "linked.coopr")); err != nil {
		t.Fatal(err)
	}
	options := f.options
	options.Output = Output{Path: filepath.Join(f.root, "result")}
	_, err := BuildPlan(f.ctx, testPlan(t, "from \"scratch\"\ncomponent \"./linked.coopr\"\n"), options)
	if err == nil {
		t.Fatal("component consumed a symlink target outside its COPY context")
	}
}

func TestLocalComponentSupervisedBuildPackagesWithoutPrebuild(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "components/shared.coopr", "package as=\"assets\"\ncopy \"payload\" \"/payload\"\nextend\ncopy \"/payload\" \"/payload\" from=\"assets\"\n")
	f.write(t, "payload", "supervised payload")
	// A conflicting file beside the component must not replace the caller context.
	f.write(t, "components/payload", "wrong definition-directory payload")
	layout := filepath.Join(f.root, "supervised")
	_, err := BuildDefinitionSupervised(f.ctx, parseWorkerDefinition(t, "from \"scratch\"\ncomponent \"./components/shared.coopr\"\n"), planner.Options{Mode: planner.Build}, SupervisedPlanOptions{
		Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		ImageStoreDir: filepath.Join(f.root, "images"), ComponentStoreDir: filepath.Join(f.root, "components"),
		CacheLocalDir: f.options.CacheLocalDir, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := localComponentLastFile(t, layout, "payload"); got != "supervised payload" {
		t.Fatalf("supervised component payload = %q", got)
	}
}

func TestLocalComponentInternalSymlinkResolvesLikeCopy(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "shared.coopr", "extend\nenv symlink=\"resolved\"\n")
	if err := os.Symlink("shared.coopr", filepath.Join(f.contextDir, "linked.coopr")); err != nil {
		t.Fatal(err)
	}
	_, layout := f.build(t, "internal-symlink", "from \"scratch\"\ncopy \"linked.coopr\" \"/definition\"\ncomponent \"./linked.coopr\"\n")
	_, image := readPlanImage(t, layout)
	if strings.Join(image.Config.Env, "\n") != "symlink=resolved" {
		t.Fatalf("internal symlink component config = %v", image.Config.Env)
	}
}

func TestLocalComponentPackageProductionCycleFails(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "cycle.coopr", "from \"scratch\" as=\"producer\"\ncomponent \"./cycle.coopr\"\npackage as=\"assets\"\ncopy \"/payload\" \"/payload\" from=\"producer\"\nextend\ncopy \"/payload\" \"/payload\" from=\"assets\"\n")
	options := f.options
	options.Output = Output{Path: filepath.Join(f.root, "result")}
	_, err := BuildPlan(f.ctx, testPlan(t, "from \"scratch\"\ncomponent \"./cycle.coopr\"\n"), options)
	if err == nil || !strings.Contains(err.Error(), "component invocation cycle at") {
		t.Fatalf("package production cycle error = %v", err)
	}
}

func TestLocalComponentPackageProducerConsumesNestedComponent(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "nested package producer")
	f.write(t, "inner.coopr", "extend\ncopy \"payload\" \"/payload\"\n")
	f.write(t, "outer.coopr", "from \"scratch\" as=\"producer\"\ncomponent \"./inner.coopr\"\npackage as=\"assets\"\ncopy \"/payload\" \"/payload\" from=\"producer\"\nextend\ncopy \"/payload\" \"/payload\" from=\"assets\"\n")
	_, layout := f.build(t, "nested-producer", "from \"scratch\"\ncomponent \"./outer.coopr\"\n")
	if got := localComponentLastFile(t, layout, "payload"); got != "nested package producer" {
		t.Fatalf("nested package producer output = %q", got)
	}
}
