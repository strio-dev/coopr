package buildah

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/componentstore"
	"github.com/strio-dev/coopr/internal/planner"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestComponentTopLevelFromUsesInvocationContextAndArguments(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "component.coopr", `arg "source" "not-present-at-publication"
from "scratch" as="helper" {
    arg "source"
    copy "${source}" "/payload"
}
extend {
    copy "/payload" "/proof" from="helper"
}
`)
	for _, attempt := range []struct{ name, content string }{
		{"cold", "first caller"}, {"warm", "first caller"}, {"changed", "second caller"},
	} {
		f.write(t, "caller-input", attempt.content)
		_, layout := f.build(t, attempt.name, `from "scratch" {
    env caller="preserved"
    component "./component.coopr" { arg "source" "caller-input"; }
}
`)
		if got := localComponentLastFile(t, layout, "proof"); got != attempt.content {
			t.Fatalf("%s invocation payload = %q", attempt.name, got)
		}
		_, image := readPlanImage(t, layout)
		if !strings.Contains(strings.Join(image.Config.Env, "\n"), "caller=preserved") {
			t.Fatalf("%s lost caller configuration: %v", attempt.name, image.Config.Env)
		}
	}
}

func TestPublishedComponentSharedFromUsesEachPhaseContext(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "payload", "publisher\n")
	component := parseWorkerDefinition(t, `from "scratch" as="helper" {
    copy "payload" "/payload"
}
package as="assets" { copy "/payload" "/payload" from="helper"; }
extend {
    copy "/payload" "/fixed" from="assets"
    copy "/payload" "/current" from="helper"
    run "cat /fixed /current > /proof" network="none"
}
`)
	componentLayout := filepath.Join(f.root, "published")
	publication, err := PublishDefinitionSupervised(f.ctx, component, planner.Options{
		Mode: planner.Publish, Platform: "linux/" + runtime.GOARCH,
	}, SupervisedPlanOptions{
		Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun",
		Output: Output{Path: componentLayout}, CacheLocalDir: f.options.CacheLocalDir,
		SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.NewWithContext(f.ctx, componentLayout)
	if err != nil {
		t.Fatal(err)
	}
	if err := componentstore.Put(f.ctx, filepath.Join(f.root, "components"), source, publication.Root, "shared-phase"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.contextDir); err != nil {
		t.Fatal(err)
	}
	f.contextDir = filepath.Join(f.root, "consumer")
	if err := os.Mkdir(f.contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.options.ContextDir = f.contextDir
	for _, attempt := range []struct{ name, content string }{
		{"cold", "consumer-one\n"}, {"warm", "consumer-one\n"}, {"changed", "consumer-two\n"},
	} {
		f.write(t, "payload", attempt.content)
		_, layout := f.build(t, attempt.name, fmt.Sprintf(`from %q {
    component "shared-phase"
}
`, base.reference))
		if got := localComponentLastFile(t, layout, "proof"); got != "publisher\n"+attempt.content {
			t.Fatalf("%s publication/invocation payload = %q", attempt.name, got)
		}
	}
}

func TestComponentTopLevelFromOutputPreservesCachedImage(t *testing.T) {
	f := newLocalComponentFixture(t)
	epoch := int64(1234567890)
	f.options.Timestamp = &epoch
	f.write(t, "payload", "package image")
	f.write(t, "caller", "discarded caller file")
	f.write(t, "component.coopr", `package as="assets" { copy "payload" "/payload"; }
from "assets" as="helper" { env selected="helper"; }
extend { from "helper" {}; }
`)
	var coldLayers []string
	for _, attempt := range []string{"cold", "warm"} {
		result, layout := f.build(t, attempt, `from "scratch" {
    copy "caller" "/caller"
    env caller_only="discarded"
    component "./component.coopr"
}

`)
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 || !slices.Contains(image.Config.Env, "selected=helper") || slices.Contains(image.Config.Env, "caller_only=discarded") {
			t.Fatalf("%s selected image state = %+v, %+v", attempt, manifest, image.Config)
		}
		if got := localComponentLastFile(t, layout, "payload"); got != "package image" {
			t.Fatalf("%s selected payload = %q", attempt, got)
		}
		layers := make([]string, len(manifest.Layers))
		for i, layer := range manifest.Layers {
			layers[i] = layer.Digest.String()
		}
		if attempt == "cold" {
			coldLayers = layers
		} else if !slices.Equal(coldLayers, layers) || result.CacheStats.Hits == 0 {
			t.Fatalf("warm image chain/cache = %v/%+v, cold = %v", layers, result.CacheStats, coldLayers)
		}
	}
}

func TestComponentTopLevelFromInheritsLocalOnBuild(t *testing.T) {
	for _, reference := range []string{"assets", "1"} {
		t.Run(reference, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "payload", "packaged trigger input")
			f.write(t, "component.coopr", fmt.Sprintf(`from "scratch" as="unused" { copy "missing" "/missing"; }
package as="assets" { copy "payload" "/payload"; }
from "scratch" as="helper" {
    onbuild { copy "/payload" "/proof" from=%q; }
}
extend { from "helper" {}; }
`, reference))
			for _, attempt := range []string{"cold", "warm"} {
				_, layout := f.build(t, attempt, `from "scratch" { component "./component.coopr"; }
`)
				if got := localComponentLastFile(t, layout, "proof"); got != "packaged trigger input" {
					t.Fatalf("%s inherited trigger payload = %q", attempt, got)
				}
			}
		})
	}
}

func TestComponentTopLevelFromRetainsDynamicOnBuildDependencies(t *testing.T) {
	for _, reference := range []string{"assets", "1"} {
		t.Run(reference, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "payload", "runtime trigger input")
			f.write(t, "trigger.coopr", fmt.Sprintf(`extend {
    onbuild { copy "/payload" "/proof" from=%q; }
}
`, reference))
			f.write(t, "component.coopr", `from "scratch" as="unused" { copy "missing" "/missing"; }
package as="assets" { copy "payload" "/payload"; }
from "scratch" as="helper" { component "./trigger.coopr"; }
extend { from "helper" {}; }
from "scratch" as="trailing" { copy "missing-trailing-input" "/unused"; }
package as="trailing_assets" { copy "missing-trailing-input" "/unused"; }
`)
			for _, attempt := range []string{"cold", "warm"} {
				_, layout := f.build(t, attempt, `from "scratch" { component "./component.coopr"; }
`)
				if got := localComponentLastFile(t, layout, "proof"); got != "runtime trigger input" {
					t.Fatalf("%s dynamically inherited payload = %q", attempt, got)
				}
			}
		})
	}
}
