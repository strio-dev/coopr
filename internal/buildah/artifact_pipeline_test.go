package buildah

import (
	"context"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestArtifactFileSelectionIsNotAnImageBinding(t *testing.T) {
	const ref = "registry.invalid/assets:latest"
	asset := ResolvedImageSource{Artifact: true, Reference: ref, Root: oci.Descriptor(v1.MediaTypeImageManifest, []byte("artifact")), Selected: oci.Descriptor(v1.MediaTypeImageManifest, []byte("artifact"))}
	for _, tc := range []struct {
		name, body string
		fail       bool
	}{
		{"copy", `from "scratch" { copy "hello.txt" "/hello" from="` + ref + `" }`, false},
		{"add", `from "scratch" { add "hello.tar" "/hello" from="` + ref + `" }`, false},
		{"from after copy", `from "scratch" as="helper" { copy "hello.txt" "/hello" from="` + ref + `" }
from "` + ref + `" { copy "/hello" "/proof" from="helper" }`, true},
		{"mount after copy", `from "scratch" { copy "hello.txt" "/hello" from="` + ref + `"
run "true" { mount "bind" from="` + ref + `" target="/assets" } }`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := definition.Parse(strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			imageCalls, fileCalls := 0, 0
			images := func(context.Context, string, v1.Platform) (ResolvedImageSource, error) {
				imageCalls++
				return asset, nil
			}
			files := func(context.Context, string, v1.Platform) (ResolvedImageSource, error) {
				fileCalls++
				return asset, nil
			}
			_, selected, err := planDefinitionWithSourceResolvers(context.Background(), def, planner.Options{Mode: planner.Build, Platform: "linux/amd64"}, false, images, files)
			if tc.fail {
				if err == nil || !strings.Contains(err.Error(), "artifact") {
					t.Fatalf("wanted artifact image-binding error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if imageCalls != 0 || fileCalls != 1 || !selected.all[ResolvedBaseKey{Reference: ref, Platform: "linux/amd64"}].Artifact {
				t.Fatalf("image calls=%d file calls=%d selections=%+v", imageCalls, fileCalls, selected.all)
			}
		})
	}
}

func TestArtifactInputUnionValidation(t *testing.T) {
	root := oci.Descriptor(v1.MediaTypeImageManifest, []byte("artifact"))
	source := ResolvedImageSource{Artifact: true, Reference: "registry.invalid/assets:latest", Root: root, Selected: root}
	if err := validateSelectedInput(source, true); err != nil {
		t.Fatal(err)
	}
	if err := validateSelectedInput(source, false); err == nil {
		t.Fatal("artifact accepted as image")
	}
	source.ImageID = "image"
	if err := validateSelectedInput(source, true); err == nil {
		t.Fatal("mixed image/artifact accepted")
	}
	source.ImageID = ""
	source.SourceManifest = &root
	if err := validateSelectedInput(source, true); err == nil {
		t.Fatal("artifact accepted with an image origin association")
	}
	source.SourceManifest = nil
	source.Root = oci.Descriptor(v1.MediaTypeImageManifest, []byte("different root"))
	if err := validateSelectedInput(source, true); err == nil {
		t.Fatal("artifact accepted an index or different root")
	}
	source.Root = root
	source.Selected.Digest = ""
	if err := validateSelectedInput(source, true); err == nil {
		t.Fatal("unidentified artifact accepted")
	}
}

func TestArtifactSelectionsRemainCoordinatorOwned(t *testing.T) {
	root := oci.Descriptor(v1.MediaTypeImageManifest, []byte("artifact"))
	selected := ResolvedImageSource{Artifact: true, Reference: "registry.invalid/assets:latest", Root: root, Selected: root}
	calls := 0
	executor := &graphExecutor{baseSelectionHandler: func(_ context.Context, request baseSelectionRequest) (ResolvedImageSource, error) {
		calls++
		if !request.Files {
			t.Fatal("artifact selection lost file-input role")
		}
		return selected, nil
	}}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	got, err := executor.resolveFileInput(context.Background(), selected.Reference, platform)
	if err != nil || !got.Artifact {
		t.Fatalf("selection=%+v err=%v", got, err)
	}
	if _, err := executor.resolveBaseImage(context.Background(), selected.Reference, platform); err == nil || !strings.Contains(err.Error(), "artifact") {
		t.Fatalf("cached artifact accepted as FROM: %v", err)
	}
	if _, err := executor.resolveFileInput(context.Background(), selected.Reference, platform); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("coordinator selected mutable reference %d times", calls)
	}
	changed := selected
	changed.Selected = oci.Descriptor(v1.MediaTypeImageManifest, []byte("changed artifact"))
	if err := executor.retainComponentBases("component", &ResolvedComponentPlan{SelectedBases: map[ResolvedBaseKey]ResolvedImageSource{{Reference: selected.Reference, Platform: "linux/amd64"}: changed}}); err == nil {
		t.Fatal("empty image IDs masked a changed artifact selection")
	}
}

func TestArtifactCopyLoweringPreservesOptionsAndRejectsMounts(t *testing.T) {
	const ref = "registry.invalid/assets:latest"
	root := oci.Descriptor(v1.MediaTypeImageManifest, []byte("artifact"))
	selected := ResolvedImageSource{Artifact: true, Reference: ref, Root: root, Selected: root}
	inputs := map[ResolvedBaseKey]ResolvedImageSource{{Reference: ref, Platform: "linux/amd64"}: selected}
	plan := testPlan(t, `from "scratch" {
copy "hello.txt" "/result/" from="`+ref+`" chmod=0o640 parents=#true {
    exclude "*.tmp"
}
}`)
	lowered, err := lowerPlannedGraphOperation(plan.Stages[0].Operations[0], nil, nil, nil, "linux/amd64", inputs, imageconfig.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	copied, ok := lowered[0].(copyFromImageOperation)
	if !ok || copied.artifact == nil || copied.artifact.Selected.Digest != root.Digest || copied.imageID != "" || copied.options.Chmod != "0640" || !copied.options.Parents || len(copied.options.Excludes) != 1 {
		t.Fatalf("COPY lost artifact identity or native options: %+v", lowered)
	}
	if !copyAddProbeEligible(copied) {
		t.Fatal("artifact COPY cannot use existing native cache probing")
	}
	plan = testPlan(t, `from "scratch" {
run "true" { mount "bind" from="`+ref+`" target="/assets" }
}`)
	_, err = lowerPlannedGraphOperation(plan.Stages[0].Operations[0], nil, nil, nil, "linux/amd64", inputs, imageconfig.New(), nil)
	if err == nil || !strings.Contains(err.Error(), "artifact") {
		t.Fatalf("artifact accepted as RUN mount: %v", err)
	}
}

func TestPreparedStageRejectsArtifactPresetAsFrom(t *testing.T) {
	const reference = "registry.invalid/assets:latest"
	root := oci.Descriptor(v1.MediaTypeImageManifest, []byte("artifact"))
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		t.Run(platform, func(t *testing.T) {
			plan := testPlan(t, `from "`+reference+`" platform="`+platform+`" {}`)
			executor := &graphExecutor{resolvedBases: map[ResolvedBaseKey]ResolvedImageSource{
				{Reference: reference, Platform: plan.Stages[0].Platform}: {Artifact: true, Reference: reference, Root: root, Selected: root},
			}}
			_, err := executor.prepareGraphStage(context.Background(), plan, plan.Stages[0], nil, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "artifact") {
				t.Fatalf("preset artifact rejection = %v, want an artifact error", err)
			}
		})
	}
}
