package buildah

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
)

func TestOCIOutputRetainsHealthcheckAndOnBuild(t *testing.T) {
	if testing.Short() || os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah OCI metadata test")
	}
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	layout := filepath.Join(root, "image")
	_, err := BuildPlan(context.Background(), testPlan(t, `
from "scratch"
healthcheck { exec "/bin/check" }
onbuild { env FROM_PARENT="yes" }
`), PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
	})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := oci.LayoutRoot(layout)
	if err != nil {
		t.Fatal(err)
	}
	if selected.MediaType != v1.MediaTypeImageManifest {
		t.Fatalf("manifest media type = %q, want OCI", selected.MediaType)
	}
	config, err := oci.ReadImageConfigLayout(context.Background(), layout)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Config struct {
			Healthcheck struct {
				Test []string `json:"Test"`
			} `json:"Healthcheck"`
			OnBuild []string `json:"OnBuild"`
		} `json:"config"`
	}
	if err := json.Unmarshal(config, &document); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(document.Config.Healthcheck.Test, []string{"CMD", "/bin/check"}) ||
		!slices.Equal(document.Config.OnBuild, []string{"ENV FROM_PARENT=yes"}) {
		t.Fatalf("OCI config lost image metadata: %s", config)
	}
}

func TestDockerBaseHealthcheckAndOnBuildSurviveNativeStorage(t *testing.T) {
	if testing.Short() || os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah Docker-base test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture"), []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal("live Buildah tests require the Nix dev shell's static busybox binary")
	}
	busyboxData, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "busybox"), busyboxData, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	parentLayout := filepath.Join(root, "parent")
	parentDefinition, err := definition.Parse(strings.NewReader(`
from "scratch"
copy "busybox" "/bin/sh" chmod="0755"
healthcheck interval="30s" retries=2 { exec "/bin/check" }
`))
	if err != nil {
		t.Fatal(err)
	}
	// These are imported Docker image metadata, including heredoc forms that
	// an external base image can contain independently of Coopr source syntax.
	for _, trigger := range []string{
		"COPY fixture /external",
		"COPY <<INLINE_COPY /inline/\ncopied inline\nINLINE_COPY",
		"ADD <<'INLINE_ADD' /inline-add/\n$LITERAL\nINLINE_ADD",
		"COPY fixture <<MIXED /mixed/\nmixed inline\nMIXED",
		`RUN <<EOF
read value < /inline/INLINE_COPY; printf '%s\n' "$value" > /run-result
EOF`,
		"USER 1000",
		`RUN <<SCRIPT
#!/bin/sh
read value < /inline/INLINE_COPY
[ "$value" = "copied inline" ]
SCRIPT`,
	} {
		parentDefinition.Instructions = append(parentDefinition.Instructions, definition.Instruction{
			Name: "onbuild", Arguments: []string{trigger},
		})
	}
	parentPlan, err := planner.Create(parentDefinition, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := BuildPlan(ctx, parentPlan, PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless",
		Output: Output{Path: parentLayout, Format: outputFormatDocker}, SystemContext: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	parentConfig, err := oci.ReadImageConfigLayout(ctx, parentLayout)
	if err != nil {
		t.Fatal(err)
	}
	assertDockerMetadata(t, parentConfig, true)
	selected, err := oci.LayoutRoot(parentLayout)
	if err != nil {
		t.Fatal(err)
	}
	const reference = "registry.example/coopr/onbuild:latest"
	if err := nameNativeFixture(ctx, store, reference, oci.StoredSelection{
		Root: selected, Manifest: selected, ImageID: parent.ImageID, ConfigData: parentConfig,
	}); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{})
	if err != nil {
		t.Fatal(err)
	}
	childLayout := filepath.Join(root, "child")
	_, err = BuildPlan(ctx, testPlan(t, "from \""+reference+"\"\nlabel CHILD=\"yes\"\n"), PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Resolver: resolver,
		Output: Output{Path: childLayout, Format: outputFormatDocker}, SystemContext: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, childLayout)
	if len(manifest.Layers) < 6 {
		t.Fatalf("external ONBUILD heredocs created only %d layers", len(manifest.Layers))
	}
	layer := func(index int) string {
		layerIndex := len(manifest.Layers) - 6 + index
		return filepath.Join(childLayout, "blobs", "sha256", manifest.Layers[layerIndex].Digest.Encoded())
	}
	if got := readLayerFile(t, layer(0), "external"); got != "external\n" {
		t.Fatalf("external ONBUILD COPY content = %q", got)
	}
	if got := readLayerFile(t, layer(1), "inline/INLINE_COPY"); got != "copied inline\n" {
		t.Fatalf("external ONBUILD COPY heredoc content = %q", got)
	}
	if got := readLayerFile(t, layer(2), "inline-add/INLINE_ADD"); got != "$LITERAL\n" {
		t.Fatalf("external ONBUILD ADD heredoc content = %q", got)
	}
	if got := readLayerFile(t, layer(3), "mixed/fixture"); got != "external\n" {
		t.Fatalf("external ONBUILD mixed local content = %q", got)
	}
	if got := readLayerFile(t, layer(3), "mixed/MIXED"); got != "mixed inline\n" {
		t.Fatalf("external ONBUILD mixed inline content = %q", got)
	}
	if got := readLayerFile(t, layer(4), "run-result"); got != "copied inline\n" {
		t.Fatalf("external ONBUILD RUN heredoc content = %q", got)
	}
	childConfig, err := oci.ReadImageConfigLayout(ctx, childLayout)
	if err != nil {
		t.Fatal(err)
	}
	assertDockerMetadata(t, childConfig, false)
}

func assertDockerMetadata(t *testing.T, data []byte, wantTrigger bool) {
	t.Helper()
	var document struct {
		Config struct {
			Healthcheck struct {
				Test    []string `json:"Test"`
				Retries int      `json:"Retries"`
			} `json:"Healthcheck"`
			OnBuild []string `json:"OnBuild"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	healthcheck := document.Config.Healthcheck
	if len(healthcheck.Test) != 2 || healthcheck.Test[0] != "CMD" || healthcheck.Test[1] != "/bin/check" || healthcheck.Retries != 2 {
		t.Fatalf("Docker healthcheck lost: %s", data)
	}
	if wantTrigger && (len(document.Config.OnBuild) != 7 || document.Config.OnBuild[0] != "COPY fixture /external") {
		t.Fatalf("Docker ONBUILD trigger lost: %s", data)
	}
	if !wantTrigger && len(document.Config.OnBuild) != 0 {
		t.Fatalf("Docker ONBUILD trigger was not consumed: %s", data)
	}
}

func TestBuildPlanConsumesInheritedOnBuildBeforeChildInstructions(t *testing.T) {
	if testing.Short() || os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah ONBUILD test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture"), []byte("inherited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	layout := filepath.Join(root, "layout")
	_, err := BuildPlanSupervised(ctx, testPlan(t, `
from "scratch" as="parent"
env SOURCE="fixture"
onbuild { env ORDER="inherited" }
onbuild { copy "$SOURCE" "/inherited" }
from "parent"
env ORDER="child"
`), SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless",
		Output: Output{Path: layout},
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) == 0 || !slices.Contains(image.Config.Env, "ORDER=child") {
		t.Fatalf("ONBUILD order or COPY was lost: layers=%d env=%v", len(manifest.Layers), image.Config.Env)
	}
	last := manifest.Layers[len(manifest.Layers)-1]
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "inherited"); got != "inherited\n" {
		t.Fatalf("ONBUILD COPY content = %q", got)
	}
	config, err := oci.ReadImageConfigLayout(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(config, &document); err != nil {
		t.Fatal(err)
	}
	if _, exists := document.Config["OnBuild"]; exists {
		t.Fatalf("consumed ONBUILD triggers leaked into child config: %s", config)
	}
}

func TestInheritedInsecureRunRequiresEntitlementAndExecutes(t *testing.T) {
	if testing.Short() || os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live inherited insecure RUN test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "busybox"), content, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, `
from "scratch" as="parent"
copy "busybox" "/bin/sh"
onbuild { run "printf inherited >/proof" security="insecure" network="none" }
from "parent"
`)
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	options := SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Stdout: io.Discard, Stderr: io.Discard,
	}
	denied := options
	denied.Output = Output{Path: filepath.Join(root, "denied")}
	if _, err := BuildPlanSupervised(ctx, plan, denied); err == nil || !strings.Contains(err.Error(), "--allow security.insecure") {
		t.Fatalf("inherited insecure RUN without entitlement error = %v", err)
	}
	allowed := options
	allowed.Allow = []string{"security.insecure"}
	allowed.Output = Output{Path: filepath.Join(root, "allowed")}
	if _, err := BuildPlanSupervised(ctx, plan, allowed); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, allowed.Output.Path)
	if len(manifest.Layers) == 0 {
		t.Fatal("inherited insecure RUN produced no layers")
	}
	last := manifest.Layers[len(manifest.Layers)-1]
	if got := readLayerFile(t, filepath.Join(allowed.Output.Path, "blobs", "sha256", last.Digest.Encoded()), "proof"); got != "inherited" {
		t.Fatalf("inherited insecure RUN proof = %q", got)
	}
}
