package buildah

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"coopr/internal/buildcontext"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func componentSourceLayout(t *testing.T, f localComponentFixture) {
	t.Helper()
	fixture := liveBusyBoxImage(t, f.ctx)
	layout, err := orasoci.NewWithContext(f.ctx, filepath.Join(f.contextDir, "input"))
	if err != nil {
		t.Fatal(err)
	}
	if err := oras.CopyGraph(f.ctx, fixture.store, layout, fixture.manifest, oras.DefaultCopyGraphOptions); err != nil {
		t.Fatal(err)
	}
	if err := layout.Tag(f.ctx, fixture.manifest, "latest"); err != nil {
		t.Fatal(err)
	}
}

func TestComponentInvocationTransportSources(t *testing.T) {
	for _, mode := range []string{"static", "static-no-layers", "generated", "named", "named-native"} {
		t.Run(mode, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			componentSourceLayout(t, f)
			base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
			f.options.Lifecycle.NoLayers = mode == "static-no-layers"
			source := `extend as="caller"
from "oci:input:latest" as="tool" after="caller"
run "printf transport >/proof"
from "caller" as="result"
copy "/proof" "/proof" from="tool"
`
			if mode == "generated" {
				source = `extend as="caller"
run "cp -a /ctx/input /ctx/out" { mount "bind" target="/ctx" rw="true" }
from "oci:out:latest" as="tool" after="caller"
run "printf transport >/proof"
from "caller" as="result"
copy "/proof" "/proof" from="tool"
`
			}
			if mode == "named" || mode == "named-native" {
				source = `extend as="caller"
from "tools" as="tool"
run "printf transport >/proof"
from "caller" as="result"
copy "/proof" "/proof" from="tool"
`
				f.options.BuildContexts = []buildcontext.Spec{{Name: "tools", Kind: buildcontext.DockerImage, Reference: "registry.invalid/component:latest"}}
				policy := filepath.Join(f.root, "source-policy.json")
				if err := os.WriteFile(policy, []byte(`{"rules":[{"action":"CONVERT","selector":{"identifier":"docker-image://registry.invalid/component:latest"},"updates":{"identifier":"oci:input:latest"}}]}`), 0600); err != nil {
					t.Fatal(err)
				}
				f.options.SourcePolicyFile = policy
				if mode == "named-native" {
					f.options.BuildContexts[0].Reference = base.reference
					f.options.SourcePolicyFile = ""
				}
			}
			f.write(t, "component.coopr", source)
			for _, run := range []string{"cold", "warm"} {
				_, layout := f.build(t, run, fmt.Sprintf("from %q\ncomponent \"./component.coopr\"\n", base.reference))
				if got := localComponentLastFile(t, layout, "proof"); got != "transport" {
					t.Fatalf("proof=%q", got)
				}
				manifest, _ := readPlanImage(t, layout)
				if name := manifest.Annotations[v1.AnnotationBaseImageName]; name != base.reference {
					t.Fatalf("component lost caller base name %q: %+v", name, manifest.Annotations)
				}
				if _, err := os.Stat(filepath.Join(f.contextDir, "out")); !os.IsNotExist(err) {
					t.Fatalf("host context mutated: %v", err)
				}
			}
		})
	}
}

func TestLocalComponentPackageTransportSources(t *testing.T) {
	for _, mode := range []string{"static", "generated", "named"} {
		t.Run(mode, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			componentSourceLayout(t, f)
			source := `from "oci:input:latest" as="tool"
run "printf packaged >/proof"
package as="assets"
copy "/proof" "/proof" from="tool"
extend
copy "/proof" "/proof" from="assets"
`
			if mode == "generated" {
				source = `from "oci:input:latest" as="producer"
run "cp -a /ctx/input /ctx/out" { mount "bind" target="/ctx" rw="true" }
from "oci:out:latest" as="tool" after="producer"
run "printf packaged >/proof"
package as="assets"
copy "/proof" "/proof" from="tool"
extend
copy "/proof" "/proof" from="assets"
`
			}
			if mode == "named" {
				source = `from "tools" as="tool"
run "printf packaged >/proof"
package as="assets"
copy "/proof" "/proof" from="tool"
extend
copy "/proof" "/proof" from="assets"
`
				f.options.BuildContexts = []buildcontext.Spec{{Name: "tools", Kind: buildcontext.DockerImage, Reference: "registry.invalid/component:latest"}}
				policy := filepath.Join(f.root, "source-policy.json")
				if err := os.WriteFile(policy, []byte(`{"rules":[{"action":"CONVERT","selector":{"identifier":"docker-image://registry.invalid/component:latest"},"updates":{"identifier":"oci:input:latest"}}]}`), 0600); err != nil {
					t.Fatal(err)
				}
				f.options.SourcePolicyFile = policy
			}
			f.write(t, "component.coopr", source)
			def := parseWorkerDefinition(t, source)
			for _, run := range []string{"cold", "warm"} {
				publication, err := PublishDefinitionSupervised(f.ctx, def, planner.Options{Mode: planner.Publish, BuildContexts: f.options.BuildContexts}, SupervisedPlanOptions{
					Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun",
					SourcePolicyFile: f.options.SourcePolicyFile, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath,
					Output: Output{Path: filepath.Join(f.root, "standalone-"+run)}, CacheLocalDir: f.options.CacheLocalDir,
				})
				if err != nil {
					t.Fatalf("standalone package: %v", err)
				}
				metadata := readComponentMetadata(t, f.ctx, publication.Layout, publication.Root)
				if len(metadata.Packages) != 1 {
					t.Fatalf("standalone packages: %+v", metadata.Packages)
				}
				if got := readLayerFile(t, filepath.Join(publication.Layout, "blobs", "sha256", metadata.Packages[0].Descriptor.Digest.Encoded()), "proof"); got != "packaged" {
					t.Fatalf("standalone proof=%q", got)
				}
				_, layout := f.build(t, run, "from \"scratch\"\ncomponent \"./component.coopr\"\n")
				if got := localComponentLastFile(t, layout, "proof"); got != "packaged" {
					t.Fatalf("proof=%q", got)
				}
				if _, err := os.Stat(filepath.Join(f.contextDir, "out")); !os.IsNotExist(err) {
					t.Fatalf("host context mutated: %v", err)
				}
			}
		})
	}
}
