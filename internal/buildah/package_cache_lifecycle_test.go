package buildah

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestInstructionCacheRejectsNativeHostVolumes(t *testing.T) {
	for _, fromConfig := range []bool{false, true} {
		t.Run("from-config="+strconv.FormatBool(fromConfig), func(t *testing.T) {
			testInstructionCacheRejectsNativeHostVolumes(t, fromConfig)
		})
	}
}

func testInstructionCacheRejectsNativeHostVolumes(t *testing.T, fromConfig bool) {
	root := t.TempDir()
	volume := root + ":/live:ro"
	supplied := RunControlInput{Volumes: []string{volume}}
	if fromConfig {
		configPath := filepath.Join(root, "containers.conf")
		if err := os.WriteFile(configPath, []byte("[containers]\nvolumes = ["+strconv.Quote(volume)+"]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CONTAINERS_CONF", configPath)
		supplied.Volumes = nil
	}
	controls, err := ParseRunControls(supplied)
	if err != nil || len(controls.Volumes) != 1 {
		t.Fatalf("normalized live volume = %v, error %v", controls.Volumes, err)
	}
	input := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "run", Form: "shell", Arguments: []string{"true"}}})
	input.RunControls = controls
	if key, eligible, err := instructionCacheKey(input); err != nil || eligible || key != "" {
		t.Fatalf("instruction cache accepted live native volume: %s eligible=%v error=%v", key, eligible, err)
	}
	input.Operation = planner.Operation{Instruction: definition.Instruction{Name: "copy", Arguments: []string{"source", "/result"}}}
	input.InputDigest = digest.FromString("selected copy")
	if _, eligible, err := instructionCacheKey(input); err != nil || !eligible {
		t.Fatalf("unused RUN volume disabled COPY cache: eligible=%v error=%v", eligible, err)
	}
	plan := testPublicationPlan(t, `package as="bundle" { run "true"; }
extend { copy "/payload" "/payload" from="bundle"; }
`)
	if _, eligible := packageCacheEligibility(plan.Stages, controls); eligible {
		t.Fatal("package cache accepted normalized native host volume")
	}
	plan.Stages[0].Operations = nil
	if _, eligible := packageCacheEligibility(plan.Stages, controls); !eligible {
		t.Fatal("unused native host volume disabled a package without RUN")
	}
}

func TestPackageCacheRejectsLiveHostInputs(t *testing.T) {
	for _, readonly := range []bool{true, false} {
		t.Run("readonly="+strconv.FormatBool(readonly), func(t *testing.T) {
			plan := testPublicationPlan(t, `package as="bundle" {
    run "true" { mount "bind" target="/live"; }
}
extend { copy "/payload" "/payload" from="bundle"; }
`)
			operation := &plan.Stages[0].Operations[0]
			operation.Children[0].Properties["readonly"] = strconv.FormatBool(readonly)
			operation.HostMounts = map[int]string{0: t.TempDir()}
			delete(operation.MountContexts, 0)
			if _, eligible := packageCacheEligibility(plan.Stages, RunControls{}); eligible {
				t.Fatal("aggregate package cache accepted an uncaptured live host input")
			}
			if readonly {
				operation.HostMounts = nil
				if needsContext, eligible := packageCacheEligibility(plan.Stages, RunControls{}); !eligible || !needsContext {
					t.Fatal("normal read-only primary context lost aggregate cache eligibility")
				}
				operation.Children[0].Arguments[0] = "cache"
				if needsContext, eligible := packageCacheEligibility(plan.Stages, RunControls{}); !eligible || needsContext {
					t.Fatal("disposable cache volume lost aggregate cache eligibility")
				}
			}
		})
	}
}

func TestPackageCacheKeyIncludesOutputLifecycle(t *testing.T) {
	plan := testPublicationPlan(t, `package as="bundle" { copy "payload" "/payload" }
extend { copy "/payload" "/payload" from="bundle" }
`)
	root := t.TempDir()
	stages, outputs, err := validatePublicationGraph(plan, map[string]string{"bundle": filepath.Join(root, "bundle.tar")})
	if err != nil {
		t.Fatal(err)
	}
	options := PlanOptions{Store: StoreOptions{GraphDriverName: "vfs"}, ContextDir: root, Isolation: "rootless", Runtime: "crun"}
	// The context contents are a separate existing cache input.
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	options, input, err := captureBuildContext(t.Context(), options, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := input.cleanup(); err != nil {
			t.Error(err)
		}
	})
	for _, candidate := range []*capturedContext{nil, {directory: root, identity: input.identity}, {directory: input.directory}} {
		if keys, eligible, err := packageCacheKeys(t.Context(), options, plan, stages, outputs, candidate); err == nil || eligible || keys != nil {
			t.Fatalf("package cache accepted missing/mismatched capture: keys=%v eligible=%v error=%v", keys, eligible, err)
		}
	}
	key := func(controls LifecycleControls) digest.Digest {
		t.Helper()
		current := options
		current.Lifecycle = controls
		keys, eligible, err := packageCacheKeys(context.Background(), current, plan, stages, outputs, &input)
		if err != nil || !eligible {
			t.Fatalf("eligible=%v err=%v", eligible, err)
		}
		currentKey := keys[plan.Outputs[0]]
		result, err := keys[plan.Outputs[0]].Digest()
		if err != nil {
			t.Fatal(err)
		}
		changedKey := currentKey
		changedKey.Lowering += "-different-semantics"
		changedDigest, err := changedKey.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if result == changedDigest {
			t.Fatal("different package lowering semantics reused the current key")
		}
		return result
	}
	baseline := key(LifecycleControls{})
	for _, test := range []struct {
		name     string
		controls LifecycleControls
	}{
		{"compat-volumes", LifecycleControls{CompatVolumes: true}},
		{"no-layers", LifecycleControls{NoLayers: true}},
		{"stage-labels", LifecycleControls{StageLabels: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if changed := key(test.controls); changed == baseline {
				t.Fatalf("%s reused the opposite output policy's whole-package key %s", test.name, changed)
			}
		})
	}
	if key(LifecycleControls{KeepFailed: true, KeepIntermediate: true, SaveStages: true}) != baseline {
		t.Fatal("retention-only lifecycle settings invalidated package output identity")
	}
}
