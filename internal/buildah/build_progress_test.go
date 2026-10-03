package buildah

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
)

func TestBuildProgressInstructionHeaders(t *testing.T) {
	tests := []struct {
		name        string
		instruction definition.Instruction
		want        string
	}{
		{"from", definition.Instruction{Name: "from", Arguments: []string{"docker.io/library/ubuntu:24.04"}}, "FROM docker.io/library/ubuntu:24.04"},
		{"shell", definition.Instruction{Name: "run", Arguments: []string{"printf 'hello world'"}}, "RUN printf 'hello world'"},
		{"exec", definition.Instruction{Name: "run", Form: "exec", Arguments: []string{"printf", "hello world"}}, `RUN ["printf","hello world"]`},
		{"copy flags", definition.Instruction{Name: "copy", Properties: map[string]string{"from": "tools", "chmod": "0640"}, Arguments: []string{"/config", "/etc/config"}}, "COPY --chmod=0640 --from=tools /config /etc/config"},
		{"metadata", definition.Instruction{Name: "env", Properties: map[string]string{"z": "last", "a": "first"}}, "ENV a=first z=last"},
		{"mount", definition.Instruction{Name: "run", Arguments: []string{"make"}, Children: []definition.Instruction{{Name: "mount", Arguments: []string{"cache"}, Properties: map[string]string{"target": "/cache", "id": "toolchain"}}}}, "RUN --mount=type=cache,id=toolchain,target=/cache make"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			progress := stageProgress{writer: &output, total: 7}
			progress.step(2, test.instruction)
			if got, want := output.String(), "STEP 2/7: "+test.want+"\n"; got != want {
				t.Fatalf("header = %q, want %q", got, want)
			}
		})
	}
}

func TestBuildProgressMultilineCannotInjectExtraHeaders(t *testing.T) {
	var output bytes.Buffer
	progress := stageProgress{writer: &output, total: 2}
	progress.step(2, definition.Instruction{Name: "run", Arguments: []string{"echo first\t\x1b[31m\n\rSTEP 99/99: false"}})
	got := output.String()
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("instruction emitted multiple lines: %q", got)
	}
	if strings.ContainsAny(strings.TrimSuffix(got, "\n"), "\r\x1b") {
		t.Fatalf("instruction retained terminal control characters: %q", got)
	}
	if !strings.Contains(got, "echo first") || !strings.Contains(got, "...") || strings.Contains(got, "STEP 99/99") {
		t.Fatalf("multiline command was not summarized safely: %q", got)
	}
}

func TestBuildProgressKeepsInterleavedStageAttribution(t *testing.T) {
	var output bytes.Buffer
	first := stageProgress{writer: &output, prefix: "[linux/amd64 app] ", total: 2}
	second := stageProgress{writer: &output, prefix: "[linux/amd64 app/component] ", total: 1}
	first.step(1, definition.Instruction{Name: "from", Arguments: []string{"scratch"}})
	second.step(1, definition.Instruction{Name: "extend"})
	first.step(2, definition.Instruction{Name: "label", Properties: map[string]string{"name": "app"}})
	want := "[linux/amd64 app] STEP 1/2: FROM scratch\n" +
		"[linux/amd64 app/component] STEP 1/1: EXTEND\n" +
		"[linux/amd64 app] STEP 2/2: LABEL name=app\n"
	if got := output.String(); got != want {
		t.Fatalf("interleaved headers = %q, want %q", got, want)
	}
}

func TestBuildProgressUsesActualCacheAndImageIDs(t *testing.T) {
	var output bytes.Buffer
	progress := stageProgress{writer: &output, prefix: "[app] "}
	imageID := strings.Repeat("0123456789abcdef", 4)
	progress.cache(imageID)
	progress.image(imageID)
	progress.commit("coopr-demo:base")
	want := "[app] --> Using cache " + imageID + "\n[app] --> 0123456789ab\n[app] COMMIT coopr-demo:base\n"
	if got := output.String(); got != want {
		t.Fatalf("cache/completion output = %q, want %q", got, want)
	}
	output.Reset()
	progress.commit("")
	if got, want := output.String(), "[app] COMMIT\n"; got != want {
		t.Fatalf("untagged completion = %q, want %q", got, want)
	}
	output.Reset()
	progress.cache("")
	progress.image("")
	if got := output.String(); got != "" {
		t.Fatalf("missing image ID fabricated output: %q", got)
	}
}

func TestBuildProgressAllowsDiscardedOutput(t *testing.T) {
	progress := stageProgress{total: 1}
	progress.step(1, definition.Instruction{Name: "from", Arguments: []string{"scratch"}})
	progress.cache("actual-id")
	progress.image("actual-id")
	progress.commit("")
}

func TestBuildProgressStagePrefixesRetainParentAndNumbering(t *testing.T) {
	stages := []planner.Stage{{ID: "0"}, {ID: "1", Name: "app"}}
	if got, want := graphProgressPrefix(stages[1], "[linux/amd64] [component shared] ", 2), "[linux/amd64] [component shared] [2/2] "; got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
	if got, want := graphProgressPrefix(stages[0], "[component shared] ", 1), "[component shared] "; got != want {
		t.Fatalf("single prefix = %q, want %q", got, want)
	}
}

func TestBuildProgressPreparedStageCountsInheritedAndScopedInstructions(t *testing.T) {
	plan := testPlan(t, `
arg "global" "excluded"
from "scratch" as="base"
onbuild { env inherited="yes" }
from "base" as="app"
arg "channel" "stable"
env output="yes"
`)
	base, stage := plan.Stages[0], plan.Stages[1]
	config := imageconfig.New()
	if err := config.Apply(definition.Instruction{Name: "onbuild", Arguments: []string{"ENV inherited=yes"}}); err != nil {
		t.Fatal(err)
	}
	executor := &graphExecutor{}
	prepared, err := executor.prepareGraphStage(context.Background(), plan, stage, map[string]string{"base": base.ID}, map[string]stageState{base.ID: {storageImageID: "existing-base", config: config}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	progress := stageProgress{writer: &output, total: len(prepared.operations) + 1}
	progress.step(1, definition.Instruction{Name: "from", Arguments: []string{"base", "AS", "app"}})
	for index, operation := range prepared.operations {
		progress.step(index+2, operation.Instruction)
	}
	want := "STEP 1/4: FROM base AS app\nSTEP 2/4: ENV inherited=yes\nSTEP 3/4: ARG channel=stable\nSTEP 4/4: ENV output=yes\n"
	if got := output.String(); got != want {
		t.Fatalf("prepared stage output = %q, want %q", got, want)
	}
}

func TestBuildProgressParsedCommandForms(t *testing.T) {
	tests := []struct{ name, source, want string }{
		{"shell", `shell "/bin/bash" "-o" "pipefail" "-c"`, `SHELL ["/bin/bash","-o","pipefail","-c"]`},
		{"healthcheck shell", `healthcheck "curl --fail http://localhost/" interval="30s"`, `HEALTHCHECK --interval=30s CMD curl --fail http://localhost/`},
		{"healthcheck exec", `healthcheck interval="30s" { exec "curl" "--fail" "http://localhost/" }`, `HEALTHCHECK --interval=30s CMD ["curl","--fail","http://localhost/"]`},
		{"healthcheck none", `healthcheck NONE`, `HEALTHCHECK NONE`},
		{"onbuild", `onbuild { env inherited="yes" }`, `ONBUILD ENV inherited=yes`},
		{"shebang script", "run \"\"\"\n    #!/bin/bash\n    printf 'hello world'\n    \"\"\"", `RUN #!/bin/bash ...`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			def, err := definition.Parse(strings.NewReader(test.source + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			progress := stageProgress{writer: &output, total: 1}
			progress.step(1, def.Instructions[0])
			if got, want := output.String(), "STEP 1/1: "+test.want+"\n"; got != want {
				t.Fatalf("parsed progress = %q, want %q", got, want)
			}
		})
	}
}

func TestBuildProgressUnknownAuthoredTotalUsesStableIdentity(t *testing.T) {
	tests := []struct {
		stage planner.Stage
		want  string
	}{
		{planner.Stage{ID: "0", Name: "left"}, "[stage left] "},
		{planner.Stage{ID: "2"}, "[stage 2] "},
		{planner.Stage{ID: "app"}, "[stage app] "},
	}
	for _, test := range tests {
		if got := graphProgressPrefix(test.stage, "", 0); got != test.want {
			t.Fatalf("unknown-total prefix = %q, want %q", got, test.want)
		}
	}
}

func TestBuildProgressAuthoredCountIncludesForwardStages(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
arg "global" "excluded"
from "scratch" as="base"
from "base" as="target"
from "scratch" as="forward"
`))
	if err != nil {
		t.Fatal(err)
	}
	initial := []planner.Stage{{ID: "0", Kind: "from", Source: "scratch", Platform: "linux/amd64"}, {ID: "1", Kind: "from", Source: "scratch", Platform: "linux/amd64"}}
	replanned := []planner.Stage{initial[0], {ID: "2", Kind: "from", Source: "scratch", Platform: "linux/amd64"}, initial[1]}
	executor := &graphExecutor{options: PlanOptions{progressStageTotal: definitionStageCount(def)}}
	for _, stages := range [][]planner.Stage{initial, replanned} {
		plan := &planner.Plan{Mode: planner.Build, Platform: "linux/amd64", Stages: stages}
		for _, stage := range initial {
			prepared, err := executor.prepareGraphStage(context.Background(), plan, stage, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := "[1/3] "
			if stage.ID == "1" {
				want = "[2/3] "
			}
			if got := prepared.progressPrefix; got != want {
				t.Fatalf("prepared prefix = %q, want %q", got, want)
			}
		}
	}
}
