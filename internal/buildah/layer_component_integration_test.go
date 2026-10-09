package buildah

import (
	"archive/tar"
	"coopr/internal/buildcontext"
	"errors"
	"fmt"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	storagearchive "go.podman.io/storage/pkg/archive"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestComponentPreservesInstructionLayers(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "one", "one")
	f.write(t, "two", "two")
	f.write(t, "component.coopr", `package as="assets"
copy "one" "/one"
copy "two" "/two"
extend
copy "/one" "/one" from="assets"
copy "/two" "/two" from="assets"
`)
	_, layout := f.build(t, "cold", "from \"scratch\"\ncomponent \"./component.coopr\"\n")
	cold, coldConfig := readPlanImage(t, layout)
	if len(cold.Layers) != 2 {
		t.Fatalf("component layers=%d, want each of 2 COPY layers preserved", len(cold.Layers))
	}
	if len(coldConfig.RootFS.DiffIDs) != 2 {
		t.Fatalf("diffids=%v", coldConfig.RootFS.DiffIDs)
	}
	result, warmLayout := f.build(t, "warm", "from \"scratch\"\ncomponent \"./component.coopr\"\n")
	warm, _ := readPlanImage(t, warmLayout)
	if result.CacheStats.Hits == 0 {
		t.Fatal("expected warm component/image cache hit")
	}
	if len(warm.Layers) != len(cold.Layers) {
		t.Fatalf("warm layers=%d cold=%d", len(warm.Layers), len(cold.Layers))
	}
	for i := range cold.Layers {
		if warm.Layers[i].Digest != cold.Layers[i].Digest {
			t.Fatalf("warm layer %d differs", i)
		}
	}
}

func TestLayerGroupNetChangeNestedAndConfigurationOnly(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "one", "one")
	f.write(t, "two", "two")
	for _, test := range []struct {
		name, body string
		extra      int
	}{
		{"net", `layer {
 copy "one" "/temporary"
 layer { copy "two" "/keep" }
 run "/bin/busybox rm /temporary" network="none"
 env grouped="yes"
}`, 1},
		{"config", `layer { env grouped="yes" }`, 0},
		{"empty", `layer {}`, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, initial := f.build(t, test.name+"-base", fmt.Sprintf("from %q\ncopy \"one\" \"/before\"\n", base.reference))
			start, _ := readPlanImage(t, initial)
			_, layout := f.build(t, test.name, fmt.Sprintf("from %q\ncopy \"one\" \"/before\"\n%s\n", base.reference, test.body))
			manifest, image := readPlanImage(t, layout)
			if len(manifest.Layers) != len(start.Layers)+test.extra {
				t.Fatalf("layers=%d start=%d extra=%d", len(manifest.Layers), len(start.Layers), test.extra)
			}
			for i := range start.Layers {
				if start.Layers[i].Digest != manifest.Layers[i].Digest {
					t.Fatal("preceding layer changed")
				}
			}
			if test.name != "empty" && !strings.Contains(strings.Join(image.Config.Env, "\n"), "grouped=yes") {
				t.Fatal("lost grouped env")
			}
			if test.name == "config" {
				found := false
				for _, entry := range image.History {
					if strings.Contains(entry.CreatedBy, "grouped=yes") {
						found = true
					}
				}
				if !found {
					t.Fatal("configuration-only group lost authored history")
				}
			}
			if test.extra != 0 {
				last := manifest.Layers[len(manifest.Layers)-1]
				if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "keep"); got != "two" {
					t.Fatal(got)
				}
			}
			if test.name == "net" {
				last := manifest.Layers[len(manifest.Layers)-1]
				assertLayerNamesAbsent(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "temporary", ".wh.temporary")
			}
			nonempty := 0
			for _, h := range image.History {
				if !h.EmptyLayer {
					nonempty++
				}
			}
			if nonempty != len(image.RootFS.DiffIDs) {
				t.Fatalf("history filesystem entries=%d diffids=%d", nonempty, len(image.RootFS.DiffIDs))
			}
		})
	}
}

func TestLayerAroundComponentAndInsideComponent(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "one", "one")
	f.write(t, "two", "two")
	f.write(t, "three", "three")
	f.write(t, "component.coopr", `package as="assets"
copy "one" "/one"
copy "two" "/two"
extend
layer {
 copy "/one" "/one" from="assets"
 copy "/two" "/two" from="assets"
}
`)
	_, inside := f.build(t, "inside", "from \"scratch\"\ncomponent \"./component.coopr\"\n")
	m, _ := readPlanImage(t, inside)
	if len(m.Layers) != 1 {
		t.Fatalf("inner group layers=%d", len(m.Layers))
	}
	_, outer := f.build(t, "outer", `from "scratch"
layer {
 component "./component.coopr"
 copy "three" "/three"
}
`)
	m, _ = readPlanImage(t, outer)
	if len(m.Layers) != 1 {
		t.Fatalf("outer group layers=%d", len(m.Layers))
	}
}

func TestComponentReplacementAdoptsSelectedBaseAndDropsCaller(t *testing.T) {
	for _, noLayers := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-layers=%v", noLayers), func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.options.Lifecycle.NoLayers = noLayers
			base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
			f.write(t, "caller", "old")
			f.write(t, "one", "replacement")
			f.write(t, "after", "after")
			f.write(t, "replace.coopr", fmt.Sprintf(`package as="assets"
copy "one" "/one"
extend as="caller"
from %q as="output"
copy "/one" "/one" from="assets"
env selected="yes"
label selected="yes"
workdir "/selected"
user "0"
cmd { exec "/bin/busybox" "echo" "selected" }
entrypoint { exec "/bin/busybox" }
volume "/selected-volume"
`, base.reference))
			source := fmt.Sprintf(`from %q
copy "caller" "/caller-only"
env abandoned="yes"
component "./replace.coopr"
copy "after" "/after"
run "/bin/busybox test ! -e /caller-only; /bin/busybox test -e /one; /bin/busybox test -e /after; /bin/busybox echo checked >/proof" network="none"
`, base.reference)
			_, layout := f.build(t, "replacement", source)
			m, image := readPlanImage(t, layout)
			if strings.Contains(strings.Join(image.Config.Env, "\n"), "abandoned=yes") || !strings.Contains(strings.Join(image.Config.Env, "\n"), "selected=yes") {
				t.Fatalf("replacement env=%v", image.Config.Env)
			}
			if image.Config.Labels["selected"] != "yes" || image.Config.WorkingDir != "/selected" || image.Config.User != "0" || strings.Join(image.Config.Entrypoint, " ") != "/bin/busybox" || strings.Join(image.Config.Cmd, " ") != "/bin/busybox echo selected" {
				t.Fatalf("replacement config %+v", image.Config)
			}
			if _, ok := image.Config.Volumes["/selected-volume"]; !ok {
				t.Fatal("lost selected volume")
			}
			if noLayers && len(m.Layers) != 2 {
				t.Fatalf("no-layers replacement layers=%d want selected base+one final layer", len(m.Layers))
			}
		})
	}
}

func TestLayerRejectsDirectAndNestedReplacementEvenWithoutLayers(t *testing.T) {
	for _, noLayers := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-layers=%v", noLayers), func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.options.Lifecycle.NoLayers = noLayers
			f.write(t, "replacement.coopr", "extend\nfrom \"scratch\"\nenv replacement=\"yes\"\n")
			f.write(t, "nested.coopr", "extend\ncomponent \"./replacement.coopr\"\n")
			for _, reference := range []string{"./replacement.coopr", "./nested.coopr"} {
				options := f.options
				options.Output = Output{Path: filepath.Join(f.root, strings.TrimPrefix(reference, "./"))}
				_, err := BuildPlan(f.ctx, testPlan(t, fmt.Sprintf("from \"scratch\"\nlayer { component %q }\n", reference)), options)
				if err == nil || !strings.Contains(err.Error(), "lineage") {
					t.Fatalf("replacement %s error=%v", reference, err)
				}
			}
		})
	}
}

func assertLayerNamesAbsent(t *testing.T, blob string, names ...string) {
	t.Helper()
	file, err := os.Open(blob)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck
	reader, err := storagearchive.DecompressStream(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close() //nolint:errcheck
	archive := tar.NewReader(reader)
	for {
		entry, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.TrimPrefix(entry.Name, "./") == name {
				t.Fatalf("temporary group artifact leaked as %s", entry.Name)
			}
		}
	}
}

func TestComponentImageCachePreservesMultipleLayersAcrossFreshStores(t *testing.T) {
	f := newLocalComponentFixture(t)
	epoch := int64(1234567890)
	f.options.Timestamp = &epoch
	f.write(t, "one", "one")
	f.write(t, "two", "two")
	f.write(t, "component.coopr", `package as="assets"
copy "one" "/one"
copy "two" "/two"
extend
copy "/one" "/one" from="assets"
copy "/two" "/two" from="assets"
env image_cache="yes"
shell "/bin/custom-shell" "-c"
onbuild { env inherited="yes" }
`)
	var expectedManifest v1.Manifest
	var expectedConfig v1.Image
	for attempt := range 3 {
		var progress strings.Builder

		if attempt > 0 {
			f.options.Store = cacheTestStore(filepath.Join(f.root, fmt.Sprintf("fresh-store-%d", attempt)))
		}
		layout := filepath.Join(f.root, fmt.Sprintf("cache-%d", attempt))
		result, err := BuildPlanSupervised(f.ctx, testPlan(t, `from "scratch"
component "./component.coopr"
`), SupervisedPlanOptions{Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", ComponentStoreDir: f.options.Resolver.ComponentStoreDir(), CacheLocalDir: f.options.CacheLocalDir, Timestamp: &epoch, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: &progress, Output: Output{Path: layout}})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != 2 {
			t.Fatalf("attempt %d layers=%d", attempt, len(manifest.Layers))
		}
		if attempt == 0 {
			expectedManifest, expectedConfig = manifest, image
		} else {
			if !strings.Contains(progress.String(), "Using component cache") {
				t.Fatalf("fresh store did not restore whole-component cache:\n%s", progress.String())
			}
			if result.CacheStats.Hits == 0 {
				t.Fatalf("fresh store missed cache: %+v", result.CacheStats)
			}
			if !reflect.DeepEqual(manifest.Layers, expectedManifest.Layers) || !reflect.DeepEqual(image.RootFS, expectedConfig.RootFS) || !reflect.DeepEqual(image.History, expectedConfig.History) || !reflect.DeepEqual(image.Config, expectedConfig.Config) {
				t.Fatalf("attempt %d image graph/config differs\nmanifest=%+v\nexpected=%+v\nconfig=%+v\nexpected=%+v", attempt, manifest, expectedManifest, image, expectedConfig)
			}
			raw, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", manifest.Config.Digest.Encoded()))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "custom-shell") || !strings.Contains(string(raw), "OnBuild") {
				t.Fatal("cache lost logical Shell extension")
			}
		}
	}
}

func TestComponentCallerRootSurvivesStagesWorkersAndNextComponent(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.options.Jobs = 2
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "root.coopr", `extend as="first"
run "/bin/busybox test $(/bin/busybox stat -c %a /) = 711; /bin/busybox chmod 0701 /; /bin/busybox chown 3:4 /" network="none"
from "first" as="out"
run "/bin/busybox test $(/bin/busybox stat -c %a /) = 701; /bin/busybox test $(/bin/busybox stat -c %u /) = 3; /bin/busybox chmod 0710 /" network="none"
`)
	f.write(t, "next.coopr", `extend
run "/bin/busybox test $(/bin/busybox stat -c %a /) = 710; /bin/busybox test $(/bin/busybox stat -c %u /) = 3; /bin/busybox echo preserved >/proof" network="none"
`)
	source := fmt.Sprintf(`from %q
run "/bin/busybox chmod 0711 /; /bin/busybox chown 1:2 /" network="none"
component "./root.coopr"
component "./next.coopr"
`, base.reference)
	for attempt := range 2 {
		_, layout := f.build(t, fmt.Sprintf("root-%d", attempt), source)
		if got := localComponentLastFile(t, layout, "proof"); got != "preserved\n" {
			t.Fatal(got)
		}
	}
}

func TestLayerGroupNoLayersTakesPrecedence(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.options.Lifecycle.NoLayers = true
	f.write(t, "one", "one")
	f.write(t, "two", "two")
	f.write(t, "three", "three")
	_, layout := f.build(t, "no-layers", `from "scratch"
copy "one" "/one"
layer {
 copy "two" "/two"
 layer { copy "three" "/three" }
}
`)
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("global no-layers emitted %d layers", len(manifest.Layers))
	}
}

func TestComponentReplacementInheritsSelectedBaseConfiguration(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	selected, _ := f.build(t, "selected-base", fmt.Sprintf(`from %q
env inherited="yes"
label inherited="yes"
user "0"
workdir "/inherited"
cmd { exec "/bin/busybox" "echo" "inherited" }
entrypoint { exec "/bin/busybox" }
volume "/inherited-volume"
`, base.reference))
	f.write(t, "replacement.coopr", fmt.Sprintf("extend\nfrom %q\nenv override=\"yes\"\n", "containers-storage:"+selected.ImageID))
	for _, attempt := range []string{"cold", "warm"} {
		_, layout := f.build(t, "inherited-"+attempt, fmt.Sprintf("from %q\nenv abandoned=\"yes\"\ncomponent \"./replacement.coopr\"\n", base.reference))
		manifest, image := readPlanImage(t, layout)
		if manifest.Annotations[v1.AnnotationBaseImageDigest] != selected.ManifestDigest || manifest.Annotations[v1.AnnotationBaseImageName] == base.reference {
			t.Fatalf("replacement inherited caller provenance: selected=%s annotations=%v", selected.ManifestDigest, manifest.Annotations)
		}
		if strings.Contains(strings.Join(image.Config.Env, "\n"), "abandoned=") || !strings.Contains(strings.Join(image.Config.Env, "\n"), "inherited=yes") || !strings.Contains(strings.Join(image.Config.Env, "\n"), "override=yes") {
			t.Fatalf("env=%v", image.Config.Env)
		}
		if image.Config.Labels["inherited"] != "yes" || image.Config.User != "0" || image.Config.WorkingDir != "/inherited" || strings.Join(image.Config.Cmd, " ") != "/bin/busybox echo inherited" || strings.Join(image.Config.Entrypoint, " ") != "/bin/busybox" {
			t.Fatalf("config=%+v", image.Config)
		}
		if _, ok := image.Config.Volumes["/inherited-volume"]; !ok {
			t.Fatal("inherited volume lost")
		}
	}
}

func TestComponentReplacementCacheAndSquashRetainEffectiveBase(t *testing.T) {
	for _, wrapper := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%v", wrapper), func(t *testing.T) {
			f := newLocalComponentFixture(t)
			epoch := int64(1234567890)
			f.options.Timestamp = &epoch
			base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
			f.write(t, "one", "one")
			f.write(t, "caller", "abandoned")
			f.write(t, "replacement.coopr", fmt.Sprintf(`package as="assets"
copy "one" "/one"
extend
from %q
copy "/one" "/one" from="assets"
`, base.reference))
			reference := "./replacement.coopr"
			if wrapper {
				f.write(t, "wrapper.coopr", "extend\ncomponent \"./replacement.coopr\"\n")
				reference = "./wrapper.coopr"
			}
			source := fmt.Sprintf("from %q\ncopy \"caller\" \"/caller-only\"\ncomponent %q\n", base.reference, reference)
			f.options.Output.Squash = true
			// build helper supplies the output path; request-level squash lives in Output.
			for attempt := range 2 {
				options := f.options
				options.Output = Output{Path: filepath.Join(f.root, fmt.Sprintf("squash-%d", attempt)), Squash: true}
				_, err := BuildPlan(f.ctx, testPlan(t, source), options)
				if err != nil {
					t.Fatal(err)
				}
				manifest, _ := readPlanImage(t, options.Output.Path)
				if len(manifest.Layers) != 2 {
					t.Fatalf("squashed replacement layers=%d", len(manifest.Layers))
				}
				assertLayerNamesAbsent(t, filepath.Join(options.Output.Path, "blobs", "sha256", manifest.Layers[1].Digest.Encoded()), "caller-only")
			}
		})
	}
}

func TestLayerRejectsNamedContextReplacementOfCallerAlias(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.options.BuildContexts = []buildcontext.Spec{{Name: "caller", Kind: buildcontext.DockerImage, Reference: base.reference}}
	f.write(t, "named.coopr", "extend as=\"caller\"\nfrom \"caller\"\nenv named=\"replacement\"\n")
	options := f.options
	options.Output = Output{Path: filepath.Join(f.root, "named")}
	_, err := BuildPlan(f.ctx, testPlan(t, "from \"scratch\"\nlayer { component \"./named.coopr\" }\n"), options)
	if err == nil || !strings.Contains(err.Error(), "lineage") {
		t.Fatalf("named context output replacement err=%v", err)
	}
}

func TestLayerRejectsReplacementIntroducedByDynamicStageReplan(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "setting.coopr", "extend\nenv child=\"./replacement.coopr\"\n")
	f.write(t, "replacement.coopr", "extend\nfrom \"scratch\"\nenv replaced=\"yes\"\n")
	f.write(t, "dynamic.coopr", `extend as="first"
env child="./setting.coopr"
component "./setting.coopr"
from "first"
component "$child"
`)
	for _, noLayers := range []bool{false, true} {
		options := f.options
		options.Lifecycle.NoLayers = noLayers
		options.Output = Output{Path: filepath.Join(f.root, fmt.Sprintf("dynamic-%v", noLayers))}
		_, err := BuildPlan(f.ctx, testPlan(t, "from \"scratch\"\nlayer { component \"./dynamic.coopr\" }\n"), options)
		if err == nil || !strings.Contains(err.Error(), "lineage") {
			t.Fatalf("noLayers=%v dynamic nested replacement err=%v", noLayers, err)
		}
	}
}

func TestLayerGroupPreservesDeletionTypesLinksAndMetadata(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	source := fmt.Sprintf(`from %q
run "/bin/busybox touch /doomed /node; /bin/busybox mkdir /type; /bin/busybox touch /type/old" network="none"
layer {
 run "/bin/busybox rm /doomed /node; /bin/busybox mkdir /node; echo kept >/node/value; /bin/busybox rm -r /type; echo regular >/type; /bin/busybox ln -s /node/value /link; /bin/busybox ln /node/value /hard; /bin/busybox chmod 0640 /node/value; /bin/busybox chown 3:4 /node/value; /bin/busybox chmod 0701 /; /bin/busybox chown 1:2 /" network="none"
}
run "/bin/busybox test ! -e /doomed; /bin/busybox test -d /node; /bin/busybox test -f /type; /bin/busybox test $(/bin/busybox readlink /link) = /node/value; /bin/busybox test $(/bin/busybox stat -c %%u /node/value) = 3; /bin/busybox test $(/bin/busybox stat -c %%a /node/value) = 640; /bin/busybox test $(/bin/busybox stat -c %%a /) = 701; /bin/busybox test $(/bin/busybox stat -c %%u /) = 1; /bin/busybox test $(/bin/busybox stat -c %%i /hard) = $(/bin/busybox stat -c %%i /node/value)" network="none"
`, base.reference)
	_, layout := f.build(t, "metadata-group", source)
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) != 3 {
		t.Fatalf("group+base+setup filesystem layers=%d want 3", len(manifest.Layers))
	}
	nonempty := 0
	for _, entry := range image.History {
		if !entry.EmptyLayer {
			nonempty++
		}
	}
	if nonempty != len(image.RootFS.DiffIDs) {
		t.Fatalf("history entries=%d diffids=%d", nonempty, len(image.RootFS.DiffIDs))
	}
}

func TestComponentReplacementStillEnforcesDormantExtendCompatibility(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprintf("published=%v", published), func(t *testing.T) {
			f := newLocalComponentFixture(t)
			component := `extend distro="not-a-real-compatible-distro"
from "scratch"
env output="independent"
`
			reference := "./restricted.coopr"
			if published {
				f.options.Resolver = localConfigComponentResolver(t, f.ctx, f.root, component, false)
				reference = "local:config"
			} else {
				f.write(t, "restricted.coopr", component)
			}
			options := f.options
			options.Output = Output{Path: filepath.Join(f.root, "restricted")}
			_, err := BuildPlan(f.ctx, testPlan(t, fmt.Sprintf("from \"scratch\"\ncomponent %q\n", reference)), options)
			if err == nil || !strings.Contains(err.Error(), "compatibility") {
				t.Fatalf("dormant mandatory EXTEND compatibility err=%v", err)
			}
		})
	}
}

func TestLayerEmptyConfigurationGroupsBothFormats(t *testing.T) {
	f := newLocalComponentFixture(t)
	for _, format := range []string{"oci", "docker"} {
		for _, body := range []string{"layer {}", "layer { env grouped=\"yes\" }"} {
			options := f.options
			options.Output = Output{Path: filepath.Join(f.root, fmt.Sprintf("empty-%s-%d", format, len(body))), Format: format}
			if _, err := BuildPlan(f.ctx, testPlan(t, "from \"scratch\"\n"+body+"\n"), options); err != nil {
				t.Fatal(err)
			}
			manifest, image := readPlanImage(t, options.Output.Path)
			if len(manifest.Layers) != 0 || len(image.RootFS.DiffIDs) != 0 {
				t.Fatalf("%s config-only/empty group produced filesystem layers", format)
			}
		}
	}
}

func TestNoLayersReplacementThenComponentUsesCurrentBaseManifest(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.options.Lifecycle.NoLayers = true
	f.write(t, "replacement.coopr", fmt.Sprintf("extend\nfrom %q\nenv selected=\"yes\"\n", base.reference))
	f.write(t, "next.coopr", "extend\nrun \"echo current >/proof\" network=\"none\"\n")
	_, layout := f.build(t, "no-layers-next", "from \"scratch\"\nenv abandoned=\"yes\"\ncomponent \"./replacement.coopr\"\ncomponent \"./next.coopr\"\n")
	if got := localComponentLastFile(t, layout, "proof"); got != "current\n" {
		t.Fatalf("next component proof=%q", got)
	}
	_, image := readPlanImage(t, layout)
	if !strings.Contains(strings.Join(image.Config.Env, "\n"), "selected=yes") || strings.Contains(strings.Join(image.Config.Env, "\n"), "abandoned=yes") {
		t.Fatalf("env=%v", image.Config.Env)
	}
}
