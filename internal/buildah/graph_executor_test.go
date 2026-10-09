package buildah

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestGraphExecutorBuilderNamesAreMonotonicAcrossGraphs(t *testing.T) {
	executor := &graphExecutor{options: PlanOptions{JobID: "job"}}

	first := executor.builderContainerName("0")
	second := executor.builderContainerName("0")
	third := executor.builderContainerName("1")
	prefix := "coopr-job-" + strconv.Itoa(os.Getpid()) + "-"
	if first != prefix+"1-0" || second != prefix+"2-0" || third != prefix+"3-1" {
		t.Fatalf("builder names = %q, %q, %q", first, second, third)
	}
	if unnamed := (&graphExecutor{}).builderContainerName("0"); unnamed != "" {
		t.Fatalf("unsupervised builder name = %q", unnamed)
	}
}

func TestGraphExecutorBuilderNamesAreUniqueAcrossParallelStages(t *testing.T) {
	executor := &graphExecutor{options: PlanOptions{JobID: "job"}}
	const count = 64
	names := make(chan string, count)
	var workers sync.WaitGroup
	for range count {
		workers.Add(1)
		go func() {
			defer workers.Done()
			names <- executor.builderContainerName("stage")
		}()
	}
	workers.Wait()
	close(names)
	seen := make(map[string]bool, count)
	for name := range names {
		if seen[name] {
			t.Fatalf("duplicate builder name %q", name)
		}
		seen[name] = true
	}
	if len(seen) != count {
		t.Fatalf("got %d builder names, want %d", len(seen), count)
	}
}

func TestGraphExecutorMemoizesComponentSelectionAndRejectsDigestDrift(t *testing.T) {
	calls := 0
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	executor := &graphExecutor{
		resolveComponent: func(_ context.Context, request ComponentPlanRequest) (*ResolvedComponentPlan, error) {
			calls++
			identity := digest.FromString("first")
			if calls > 1 {
				identity = digest.FromString("changed")
			}
			return &ResolvedComponentPlan{Identity: identity}, nil
		},
	}
	first, err := executor.resolveComponentInvocation(context.Background(), "registry.example/tool:latest", map[string]string{"a": "1", "b": "2"}, platform, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := executor.resolveComponentInvocation(context.Background(), "registry.example/tool:latest", map[string]string{"b": "2", "a": "1"}, platform, "")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first != second {
		t.Fatalf("identical component resolution calls=%d first=%p second=%p", calls, first, second)
	}

	_, err = executor.resolveComponentInvocation(context.Background(), "registry.example/tool:latest", map[string]string{"a": "different"}, platform, "")
	if err == nil || !strings.Contains(err.Error(), "changed digest during build") {
		t.Fatalf("digest drift error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("different parameter resolution calls=%d, want 2", calls)
	}
}

func TestInvocationGraphRequiresPlannedExternalBaseSelection(t *testing.T) {
	executor := &graphExecutor{resolvedBases: make(map[ResolvedBaseKey]ResolvedImageSource)}
	_, err := executor.prepareGraphStage(context.Background(), &planner.Plan{Mode: planner.Invoke}, planner.Stage{
		ID: "0", Kind: "from", Source: "registry.example/base:latest", Platform: "linux/amd64",
	}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "was not selected during planning") {
		t.Fatalf("unplanned invocation base error = %v", err)
	}
}

func TestLowerGraphOperationsPreservesComponentSourceOrder(t *testing.T) {
	planned := []planner.Operation{
		{Instruction: definition.Instruction{Name: "env", Arguments: []string{"BEFORE", "yes"}}},
		{Instruction: definition.Instruction{Name: "component", Arguments: []string{"registry.example/component@sha256:deadbeef"}}},
		{Instruction: definition.Instruction{Name: "label", Arguments: []string{"after", "yes"}}},
	}

	operations, err := lowerGraphOperations(planned, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 3 {
		t.Fatalf("lowered operations = %#v", operations)
	}
	if _, ok := operations[0].(Env); !ok {
		t.Fatalf("operation 1 type = %T", operations[0])
	}
	component, ok := operations[1].(componentGraphOperation)
	if !ok || component.planned.Arguments[0] != planned[1].Arguments[0] {
		t.Fatalf("operation 2 = %#v", operations[1])
	}
	if _, ok := operations[2].(Label); !ok {
		t.Fatalf("operation 3 type = %T", operations[2])
	}
}

func TestLowerGraphOperationsPreservesParentsForStageCopyAndAdd(t *testing.T) {
	images := map[string]stageState{"source": {storageImageID: "image-id", config: imageconfig.New()}}
	aliases := map[string]string{"source": "source"}
	for _, name := range []string{"copy", "add"} {
		t.Run(name, func(t *testing.T) {
			operation := planner.Operation{Instruction: definition.Instruction{
				Name: name, Arguments: []string{"/nested/file", "/output"},
				Properties: map[string]string{"from": "source", "parents": "true"},
			}}
			lowered, err := lowerGraphOperations([]planner.Operation{operation}, aliases, images, nil, []string{"/bin/sh", "-c"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			copied, ok := lowered[0].(copyFromImageOperation)
			if !ok || !copied.options.Parents {
				t.Fatalf("lowered %s = %#v, want Parents", name, lowered[0])
			}
		})
	}
}

func TestLowerGraphOperationsPreservesExplicitUnpackForStageAdd(t *testing.T) {
	unpack := false
	operation := planner.Operation{Instruction: definition.Instruction{
		Name: "add", Arguments: []string{"/archive.tar", "/output"},
		Properties: map[string]string{"from": "source", "unpack": "false"},
	}}
	lowered, err := lowerGraphOperations(
		[]planner.Operation{operation},
		map[string]string{"source": "source-stage"},
		map[string]stageState{"source-stage": {storageImageID: "image-id", config: imageconfig.New()}},
		nil, []string{"/bin/sh", "-c"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	copied, ok := lowered[0].(copyFromImageOperation)
	if !ok || copied.extract != unpack {
		t.Fatalf("lowered stage ADD = %#v, want extract=%t", lowered[0], unpack)
	}
}

func TestLowerGraphOperationsUsesSelectedExternalImage(t *testing.T) {
	const reference = "registry.example/tool:latest"
	const imageID = "immutable-image-id"
	operation := planner.Operation{Instruction: definition.Instruction{
		Name: "copy", Arguments: []string{"/bin/tool", "/tool"}, Properties: map[string]string{"from": reference},
	}}
	lowered, err := lowerGraphOperationsWithSelectedImages([]planner.Operation{operation}, nil, nil, nil, []string{"/bin/sh", "-c"}, nil,
		map[ResolvedBaseKey]ResolvedImageSource{{Reference: reference, Platform: "linux/amd64"}: {ImageID: imageID}})
	if err != nil {
		t.Fatal(err)
	}
	copy, ok := lowered[0].(copyFromImageOperation)
	if !ok || copy.imageID != imageID {
		t.Fatalf("lowered external COPY = %#v", lowered[0])
	}
}

func TestLowerGraphOperationsBindsSelectedExternalRunMountImages(t *testing.T) {
	const reference = "registry.example/tool:latest"
	const imageID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	operation := planner.Operation{Instruction: definition.Instruction{
		Name: "run", Form: "shell", Arguments: []string{"true"}, Children: []definition.Instruction{
			{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"from": reference, "source": "/bin", "target": "/tools", "readonly": "true"}},
			{Name: "mount", Arguments: []string{"cache"}, Properties: map[string]string{"from": reference, "source": "/var/cache", "target": "/cache", "id": "tools"}},
		},
	}}
	lowered, err := lowerGraphOperationsWithSelectedImages([]planner.Operation{operation}, nil, nil, nil, []string{"/bin/sh", "-c"}, nil,
		map[ResolvedBaseKey]ResolvedImageSource{{Reference: reference, Platform: "linux/amd64"}: {ImageID: imageID}})
	if err != nil {
		t.Fatal(err)
	}
	run, ok := lowered[0].(Run)
	if !ok || len(run.Mounts) != 2 {
		t.Fatalf("lowered external RUN mounts = %#v", lowered)
	}
	for index, mount := range run.Mounts {
		if !mount.BoundFrom || mount.Properties["from"] != imageID {
			t.Fatalf("RUN mount %d = %#v, want immutable selected image", index+1, mount)
		}
	}
	if serialized, err := serializeRunMounts(run.Mounts, ""); err != nil {
		t.Fatal(err)
	} else if len(serialized) != 2 || !strings.Contains(serialized[0], "from="+imageID) || !strings.Contains(serialized[1], "from="+imageID) {
		t.Fatalf("serialized external RUN mounts = %#v", serialized)
	}
}

func TestStandaloneGraphDefersComponentFailureToExecutor(t *testing.T) {
	plan := &planner.Plan{
		Mode: planner.Build, DefinitionType: "container",
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Inputs:   []planner.Input{{Kind: "component", Reference: "example/component@sha256:deadbeef"}},
		Stages: []planner.Stage{{
			ID: "0", Kind: "from", Source: "scratch",
			Platform: runtime.GOOS + "/" + runtime.GOARCH,
			Operations: []planner.Operation{{Instruction: definition.Instruction{
				Name: "component", Arguments: []string{"example/component@sha256:deadbeef"},
			}}},
		}},
		Outputs: []string{"0"},
	}
	stages, _, err := validateStandaloneGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	operations, err := lowerGraphOperations(stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := &graphExecutor{}
	_, err = executor.applyComponentOperation(context.Background(), nil, nil, imageconfig.New(), v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}, operations[0].(componentGraphOperation).planned, stageProgress{})
	if err == nil || !strings.Contains(err.Error(), "component resolver") {
		t.Fatalf("component interception error = %v", err)
	}
}

func TestLazyGraphLoweringReadsUpdatedLogicalShell(t *testing.T) {
	logical := imageconfig.New()
	if err := logical.Apply(definition.Instruction{Name: "shell", Arguments: []string{"/bin/component-shell", "-ceu"}}); err != nil {
		t.Fatal(err)
	}
	operations, err := lowerPlannedGraphOperation(planner.Operation{Instruction: definition.Instruction{
		Name: "run", Form: "shell", Arguments: []string{"echo after-component"},
	}}, nil, nil, nil, "linux/amd64", nil, logical, nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := operations[0].(Run)
	if !ok || len(run.Command) != 3 || run.Command[0] != "/bin/component-shell" || run.Command[2] != "echo after-component" {
		t.Fatalf("lowered RUN after component config = %#v", operations)
	}
}

func TestInvocationContextsUseCallerBuildContext(t *testing.T) {
	operations := []planner.Operation{
		{Instruction: definition.Instruction{Name: "copy", Arguments: []string{"payload", "/payload"}}, InputContext: "caller"},
		{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Children: []definition.Instruction{{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"target": "/src"}}}}, MountContexts: map[int]string{0: "caller"}},
	}
	normalized := normalizeGraphOperationContexts(planner.Invoke, operations)
	if normalized[0].InputContext != "build" || normalized[1].MountContexts[0] != "build" {
		t.Fatalf("normalized invocation contexts = %+v", normalized)
	}
	if operations[0].InputContext != "caller" || operations[1].MountContexts[0] != "caller" {
		t.Fatalf("source operations mutated = %+v", operations)
	}
	if _, err := lowerGraphOperations(normalized, nil, nil, nil, []string{"/bin/sh", "-c"}, nil); err != nil {
		t.Fatalf("lower caller contexts: %v", err)
	}
}
