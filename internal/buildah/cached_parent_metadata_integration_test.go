package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"coopr/internal/oci"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
)

func TestInstructionCacheRetainsCurrentParentAnnotations(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, tail := range []struct{ name, source string }{
		{"final-run", ""},
		{"metadata-tail", "cmd { exec \"/bin/echo\" \"current-command\" }\n"},
	} {
		t.Run(tail.name, func(t *testing.T) {
			testCachedParentAnnotations(t, tail.source)
		})
	}
}

func TestInstructionCacheSeparatesInheritedPlatformMetadata(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	fixture := liveBusyBoxImage(t, ctx)
	policy := writeComponentTestPolicy(t, root)
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
	manifestData, err := os.ReadFile(filepath.Join(fixture.layout, "blobs", "sha256", fixture.manifest.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var baselineProof string
	for _, change := range []struct {
		name, version string
		features      []string
	}{
		{name: "baseline"},
		{name: "os-version", version: "test-version"},
		{name: "os-features", features: []string{"test-feature"}},
	} {
		var image v1.Image
		if err := json.Unmarshal(fixture.configData, &image); err != nil {
			t.Fatal(err)
		}
		image.OSVersion, image.OSFeatures = change.version, change.features
		configData, err := json.Marshal(image)
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(manifestData, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.Config = descriptor(v1.MediaTypeImageConfig, configData)
		changedManifestData, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		selected := descriptor(v1.MediaTypeImageManifest, changedManifestData)
		if manifest.Config.Digest != descriptor(v1.MediaTypeImageConfig, fixture.configData).Digest {
			if err := fixture.store.Push(ctx, manifest.Config, bytes.NewReader(configData)); err != nil {
				t.Fatal(err)
			}
		}
		if selected.Digest != fixture.manifest.Digest {
			if err := fixture.store.Push(ctx, selected, bytes.NewReader(changedManifestData)); err != nil {
				t.Fatal(err)
			}
		}
		reference := "fixture.local/coopr/platform:" + change.name
		importTestImageToNative(t, ctx, store, fixture.layout, reference, selected, system)
		plan := testPlan(t, fmt.Sprintf(`from %q
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"
`, reference))
		var coldProof string
		for attempt := range 2 {
			layout := filepath.Join(root, fmt.Sprintf("%s-%d", change.name, attempt))
			var logs strings.Builder
			_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
				Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
				Output:              Output{Path: layout},
				SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: &logs,
			})
			t.Logf("%s attempt %d worker progress:\n%s", change.name, attempt, logs.String())
			if err != nil {
				t.Fatal(err)
			}
			proof := localComponentLastFile(t, layout, "proof")
			hit := strings.Contains(logs.String(), "--> Using cache ")
			if attempt == 0 {
				coldProof = proof
				if hit || change.name != "baseline" && proof == baselineProof {
					t.Errorf("%s inherited platform reused a different platform's RUN", change.name)
				}
			} else if !hit || proof != coldProof {
				t.Errorf("%s warm RUN did not reuse its platform cache", change.name)
			}
			_, output := readPlanImage(t, layout)
			if output.OSVersion != change.version || !slices.Equal(output.OSFeatures, change.features) {
				t.Errorf("%s output inherited platform = %+v", change.name, output.Platform)
			}
		}
		if change.name == "baseline" {
			baselineProof = coldProof
		}
	}
}

func testCachedParentAnnotations(t *testing.T, tail string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	fixture := liveBusyBoxImage(t, ctx)
	policy := writeComponentTestPolicy(t, root)
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
	var coldProof string
	for index, origin := range []string{"one", "two"} {
		annotations := []string{"org.example.origin=" + origin}
		if index == 0 {
			annotations = append(annotations, "org.example.first-only=yes")
		}
		manifest, err := oci.ReplaceImageAnnotationsLayout(ctx, fixture.layout, false, annotations, nil)
		if err != nil {
			t.Fatal(err)
		}
		reference := "fixture.local/coopr/annotated:" + origin
		// Both manifests share exactly the original config and root filesystem.
		importTestImageToNative(t, ctx, store, fixture.layout, reference, manifest, system)
		plan := testPlan(t, fmt.Sprintf(`from %q
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"
%s`, reference, tail))
		layout := filepath.Join(root, "output-"+origin)
		var logs strings.Builder
		_, err = BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output:              Output{Path: layout},
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: &logs,
		})
		t.Logf("%s worker progress:\n%s", origin, logs.String())
		if err != nil {
			t.Fatal(err)
		}
		proof := localComponentLastFile(t, layout, "proof")
		if index == 0 {
			coldProof = proof
		} else {
			if !strings.Contains(logs.String(), "--> Using cache ") || proof != coldProof {
				t.Errorf("annotation-only parent change reexecuted RUN: cold=%q warm=%q\n%s", coldProof, proof, logs.String())
			}
		}
		outputManifest, outputImage := readPlanImage(t, layout)
		if got := outputManifest.Annotations["org.example.origin"]; got != origin {
			t.Errorf("output origin = %q, want current parent %q", got, origin)
		}
		if _, exists := outputManifest.Annotations["org.example.first-only"]; exists != (index == 0) {
			t.Errorf("output inherited stale first-only annotation: %#v", outputManifest.Annotations)
		}
		if tail != "" && strings.Join(outputImage.Config.Cmd, " ") != "/bin/echo current-command" {
			t.Errorf("metadata tail command = %#v", outputImage.Config.Cmd)
		}
	}
}
