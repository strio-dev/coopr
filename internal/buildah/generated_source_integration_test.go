package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage/pkg/reexec"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestGeneratedOCIBaseAfterProducer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping generated OCI base execution in short mode")
	}
	for _, explicitAfter := range []bool{true, false} {
		t.Run(fmt.Sprintf("explicit-after=%v", explicitAfter), func(t *testing.T) { testGeneratedOCIBase(t, explicitAfter, false, false) })
	}
}

func TestGeneratedOCIBasePolicyConversion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native integration in short mode")
	}
	testGeneratedOCIBase(t, false, true, false)
}

func TestGeneratedOCIBaseNamedPolicyConversion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native integration in short mode")
	}
	testGeneratedOCIBase(t, false, true, true)
}

func testGeneratedOCIBase(t *testing.T, explicitAfter bool, converted bool, namedContext bool) {
	ctx := context.Background()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	layout, err := orasoci.NewWithContext(ctx, filepath.Join(contextDir, "input"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := liveBusyBoxImage(t, ctx)
	var original v1.Image
	if err := json.Unmarshal(fixture.configData, &original); err != nil {
		t.Fatal(err)
	}
	original.Config.Labels = map[string]string{"preserved": "yes"}
	original.Config.Entrypoint = []string{"/bin/sh"}
	configBytes, _ := json.Marshal(original)
	configDescriptor := descriptor(v1.MediaTypeImageConfig, configBytes)
	if err := fixture.store.Push(ctx, configDescriptor, bytes.NewReader(configBytes)); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := content.FetchAll(ctx, fixture.store, fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	var originalManifest v1.Manifest
	if err := json.Unmarshal(manifestBytes, &originalManifest); err != nil {
		t.Fatal(err)
	}
	originalManifest.Config = configDescriptor
	manifestBytes, _ = json.Marshal(originalManifest)
	source := descriptor(v1.MediaTypeImageManifest, manifestBytes)
	if err := fixture.store.Push(ctx, source, bytes.NewReader(manifestBytes)); err != nil {
		t.Fatal(err)
	}
	if err := oras.CopyGraph(ctx, fixture.store, layout, source, oras.DefaultCopyGraphOptions); err != nil {
		t.Fatal(err)
	}
	if err := layout.Tag(ctx, source, "latest"); err != nil {
		t.Fatal(err)
	}
	store := cacheTestStore(root)
	text := `from "oci:input:latest" as="producer"
run "cp -a /ctx/input /ctx/out" {
 mount "bind" target="/ctx" rw="true"
}
from "oci:out:latest" after="producer"
env generated="yes"
`
	if !explicitAfter {
		text = strings.ReplaceAll(text, ` after="producer"`, "")
	}

	sourcePolicyFile := ""
	if converted {
		sourcePolicyFile = filepath.Join(root, "source-policy.json")
		if err := os.WriteFile(sourcePolicyFile, []byte(`{"rules":[{"action":"CONVERT","selector":{"identifier":"docker-image://registry.invalid/generated:latest"},"updates":{"identifier":"oci:out:latest"}}]}`), 0600); err != nil {
			t.Fatal(err)
		}
		text = strings.ReplaceAll(text, `from "oci:out:latest"`, `from "registry.invalid/generated:latest"`)
	}

	var contexts []buildcontext.Spec
	if namedContext {
		text = strings.ReplaceAll(text, `from "registry.invalid/generated:latest"`, `from "generated"`)
		contexts = []buildcontext.Spec{{Name: "generated", Kind: buildcontext.DockerImage, Reference: "registry.invalid/generated:latest"}}
	}
	def, err := definition.Parse(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}

	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	jobs := 2
	if !explicitAfter {
		jobs = 1
	}

	if converted {
		nativeRoot := filepath.Join(root, "native-policy")
		if err := os.Mkdir(nativeRoot, 0700); err != nil {
			t.Fatal(err)
		}
		cmd := reexec.CommandContext(ctx, upstreamGeneratedWorker, nativeRoot, contextDir, filepath.Join(root, "native-policy-result"), fmt.Sprint(explicitAfter), sourcePolicyFile, fmt.Sprint(namedContext))
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("native policy workflow: %v\n%s", err, data)
		}
		if err := os.RemoveAll(filepath.Join(contextDir, "out")); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		output := filepath.Join(root, fmt.Sprintf("result-%d", attempt))
		_, err = BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, BuildContexts: contexts, Platform: "linux/" + runtime.GOARCH, BuildUnusedStages: !explicitAfter}, SupervisedPlanOptions{SourcePolicyFile: sourcePolicyFile, Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: output, DisableCompression: true}, SignaturePolicyPath: policy, Jobs: jobs, Lifecycle: LifecycleControls{NoLayers: !explicitAfter}, Stdout: io.Discard, Stderr: os.Stderr})
		if err != nil {
			t.Fatal(err)
		}
		manifest, config := readPlanImage(t, output)
		if !slices.Contains(config.Config.Env, "generated=yes") || !slices.Equal(config.Config.Cmd, original.Config.Cmd) || !slices.Equal(config.Config.Entrypoint, original.Config.Entrypoint) || config.Config.Labels["preserved"] != "yes" {
			t.Fatalf("generated base config changed: %+v", config)
		}
		if len(manifest.Layers) != len(originalManifest.Layers) {
			t.Fatalf("generated source layers flattened: %+v", manifest.Layers)
		}
		for i, layer := range manifest.Layers {
			if layer.Digest != originalManifest.Layers[i].Digest {
				t.Fatalf("base layer %d changed: %+v", i, layer)
			}
		}
	}
	if err := VerifyStoredImageSupervised(ctx, store, configDescriptor.Digest.Encoded()); err != nil {
		t.Fatalf("borrowed transport image removed: %v", err)
	}
	upstreamRoot := filepath.Join(root, "upstream")
	if err := os.Mkdir(upstreamRoot, 0700); err != nil {
		t.Fatal(err)
	}
	upstreamOutput := filepath.Join(root, "upstream-result")
	command := reexec.CommandContext(ctx, upstreamGeneratedWorker, upstreamRoot, contextDir, upstreamOutput, fmt.Sprint(explicitAfter), sourcePolicyFile, fmt.Sprint(namedContext))
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pinned upstream workflow: %v\n%s", err, data)
	}
	upstreamManifest, upstreamConfig := readPlanImage(t, upstreamOutput)
	cooprManifest, cooprConfig := readPlanImage(t, filepath.Join(root, "result-0"))
	if !slices.Equal(upstreamConfig.RootFS.DiffIDs, cooprConfig.RootFS.DiffIDs) || len(upstreamManifest.Layers) != len(cooprManifest.Layers) || !slices.Equal(upstreamConfig.Config.Cmd, cooprConfig.Config.Cmd) || !slices.Equal(upstreamConfig.Config.Entrypoint, cooprConfig.Config.Entrypoint) || upstreamConfig.Config.Labels["preserved"] != cooprConfig.Config.Labels["preserved"] {
		t.Fatalf("upstream/Coopr generated base mismatch: %+v vs %+v", upstreamConfig, cooprConfig)
	}
	if _, err := os.Stat(filepath.Join(contextDir, "out")); !os.IsNotExist(err) {
		t.Fatalf("generated context escaped onto host: %v", err)
	}
}
