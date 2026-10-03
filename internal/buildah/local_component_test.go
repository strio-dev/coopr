package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestReadLocalComponentCopyPaths(t *testing.T) {
	root := t.TempDir()
	const source = "extend\nenv shared=\"yes\"\n"
	if err := os.WriteFile(filepath.Join(root, "shared.coopr"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{"shared.coopr", "./shared.coopr", "../shared.coopr", "/shared.coopr", "./*.coopr"} {
		t.Run(reference, func(t *testing.T) {
			data, canonical, err := readLocalComponent(context.Background(), PlanOptions{ContextDir: root}, reference)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != source || canonical != filepath.Join(root, "shared.coopr") {
				t.Fatalf("data=%q canonical=%q", data, canonical)
			}
		})
	}
}

func TestReadLocalComponentDoesNotGuessSuffix(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "shared.coopr"), []byte("extend\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := PlanOptions{ContextDir: root}
	if _, _, err := readLocalComponent(context.Background(), opts, "./shared"); err == nil {
		t.Fatal("resolved shared.coopr from an extensionless reference")
	}
	const source = "extend\nenv exact=\"yes\"\n"
	if err := os.WriteFile(filepath.Join(root, "shared"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	data, path, err := readLocalComponent(context.Background(), opts, "./shared")
	if err != nil || string(data) != source || path != filepath.Join(root, "shared") {
		t.Fatalf("exact component = (%q, %q), %v", data, path, err)
	}
}

func TestReadLocalComponentConfinesSymlinkAndIgnores(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.coopr"), []byte("extend"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.coopr"), filepath.Join(root, "escape.coopr")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readLocalComponent(context.Background(), PlanOptions{ContextDir: root}, "./escape.coopr"); err == nil {
		t.Fatal("read outside context through symlink")
	}
	for _, name := range []string{"one.coopr", "two.coopr"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("extend"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := readLocalComponent(context.Background(), PlanOptions{ContextDir: root}, "./*.coopr"); err == nil {
		t.Fatal("accepted multiple definitions")
	}
	if err := os.WriteFile(filepath.Join(root, ".containerignore"), []byte("*.coopr\n!one.coopr\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readLocalComponent(context.Background(), PlanOptions{ContextDir: root}, "./two.coopr"); err == nil {
		t.Fatal("read excluded definition")
	}
	if _, _, err := readLocalComponent(context.Background(), PlanOptions{ContextDir: root}, "./one.coopr"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readLocalComponent(context.Background(), PlanOptions{ContextDir: root, ContextArtifacts: []string{filepath.Join(root, "one.coopr")}}, "./one.coopr"); err == nil {
		t.Fatal("read protected definition")
	}
	if _, _, err := readLocalComponent(context.Background(), PlanOptions{ContextDir: root}, "."); err == nil {
		t.Fatal("accepted directory")
	}
}

func TestLocalComponentSelectionIncludesParameters(t *testing.T) {
	target := v1.Platform{OS: "linux", Architecture: "amd64"}
	for _, reference := range []string{"./shared.coopr", "registry.example/shared:latest", "shared.coopr", "team/shared.coopr"} {
		first, err := componentInvocationSelectionKey(reference, map[string]string{"version": "one"}, target)
		if err != nil {
			t.Fatal(err)
		}
		second, err := componentInvocationSelectionKey(reference, map[string]string{"version": "two"}, target)
		if err != nil {
			t.Fatal(err)
		}
		if (first != second) != strings.HasPrefix(reference, "./") {
			t.Fatalf("selection keys for %s: %q %q", reference, first, second)
		}
	}
}

func TestUnprefixedComponentReferenceDoesNotReadContext(t *testing.T) {
	for _, reference := range []string{"shared.coopr", "components/shared.coopr"} {
		t.Run(reference, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, reference)
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("not a component definition"), 0o600); err != nil {
				t.Fatal(err)
			}
			identity := digest.FromString("registry component")
			called := false
			executor := &graphExecutor{
				options: PlanOptions{ContextDir: root},
				resolveComponent: func(_ context.Context, request ComponentPlanRequest) (*ResolvedComponentPlan, error) {
					called = true
					if request.Reference != reference || request.LocalParameters {
						t.Fatalf("registry request=%+v", request)
					}
					return &ResolvedComponentPlan{Identity: identity}, nil
				},
			}
			if _, err := executor.resolveComponentInvocation(context.Background(), reference, nil, v1.Platform{OS: "linux", Architecture: "amd64"}, ""); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("component reference did not reach the OCI resolver")
			}
		})
	}
}

func TestLocalComponentInvocationParameters(t *testing.T) {
	fixed := map[string]string{"build": "one"}
	supplied := map[string]string{"build": "one", "runtime": "two"}
	params, err := componentInvocationParameters(fixed, supplied, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(params) != 1 || params["runtime"] != "two" || supplied["build"] != "one" {
		t.Fatalf("parameters=%v original=%v", params, supplied)
	}
	if _, err := componentInvocationParameters(fixed, map[string]string{"build": "different"}, true); err == nil {
		t.Fatal("accepted changed fixed argument")
	}
	params, err = componentInvocationParameters(fixed, supplied, false)
	if err != nil || len(params) != 2 {
		t.Fatalf("registry parameters=%v err=%v", params, err)
	}
}

func TestPinnedLocalComponentDoesNotReadContext(t *testing.T) {
	target := v1.Platform{OS: "linux", Architecture: "amd64"}
	params := map[string]string{"build": "one"}
	key, err := componentInvocationSelectionKey("./missing.coopr", params, target)
	if err != nil {
		t.Fatal(err)
	}
	identity := digest.FromString("frozen")
	called := false
	executor := &graphExecutor{
		componentPins: map[string]string{key: identity.String()},
		resolveComponent: func(_ context.Context, request ComponentPlanRequest) (*ResolvedComponentPlan, error) {
			called = true
			if request.Reference != identity.String() || !request.LocalParameters || request.Parameters["build"] != "one" {
				t.Fatalf("request=%+v", request)
			}
			return &ResolvedComponentPlan{Identity: identity}, nil
		},
	}
	if _, err := executor.resolveComponentInvocation(context.Background(), "./missing.coopr", params, target, ""); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("frozen component was not resolved")
	}
	if _, err := executor.resolveComponentInvocation(context.Background(), "./other.coopr", params, target, ""); err == nil || !strings.Contains(err.Error(), "not frozen") {
		t.Fatalf("unfrozen error=%v", err)
	}
}

func TestMemoizedComponentRetainsBasesInPublicationExecutor(t *testing.T) {
	target := v1.Platform{OS: "linux", Architecture: "amd64"}
	reference := "registry.example/tool:latest"
	key, err := componentInvocationSelectionKey(reference, nil, target)
	if err != nil {
		t.Fatal(err)
	}
	baseKey := ResolvedBaseKey{Reference: "registry.example/base:latest", Platform: "linux/amd64"}
	resolved := &ResolvedComponentPlan{Identity: digest.FromString("component"), SelectedBases: map[ResolvedBaseKey]ResolvedImageSource{baseKey: {ImageID: "selected"}}}
	executor := &graphExecutor{resolvedComponents: map[string]*componentResolutionState{key: {identity: resolved.Identity, variants: map[string]*ResolvedComponentPlan{"{}": resolved}}}}
	selected, err := executor.resolveComponentInvocation(context.Background(), reference, nil, target, "")
	if err != nil {
		t.Fatal(err)
	}
	if selected != resolved || executor.resolvedBases[baseKey].ImageID != "selected" {
		t.Fatalf("selected=%p bases=%v", selected, executor.resolvedBases)
	}
	executor.resolvedBases[baseKey] = ResolvedImageSource{ImageID: "changed"}
	if _, err := executor.resolveComponentInvocation(context.Background(), reference, nil, target, ""); err == nil || !strings.Contains(err.Error(), "changed selection") {
		t.Fatalf("changed base error=%v", err)
	}
}

func TestNestedPublicationBuilderNamesShareCounter(t *testing.T) {
	parent := &graphExecutor{options: PlanOptions{JobID: "job"}}
	child := &graphExecutor{options: PlanOptions{JobID: "job"}, sharedBuilderCounter: &parent.nextBuilderID}
	if first, second, third := parent.builderContainerName("parent"), child.builderContainerName("child"), parent.builderContainerName("parent"); first == second || second == third || !strings.Contains(second, "-2-child") || !strings.Contains(third, "-3-parent") {
		t.Fatalf("builder names %q %q %q", first, second, third)
	}
}

func TestNestedPublicationUsesBuildLevelBasePin(t *testing.T) {
	target := v1.Platform{OS: "linux", Architecture: "amd64"}
	key := ResolvedBaseKey{Reference: "registry.example/package-only:latest", Platform: "linux/amd64"}
	parent := &graphExecutor{resolvedBases: make(map[ResolvedBaseKey]ResolvedImageSource)}
	child := &graphExecutor{options: PlanOptions{componentParent: parent}, resolvedBases: make(map[ResolvedBaseKey]ResolvedImageSource)}
	// This pin is discovered after the child clones the build's selections.
	// A child miss must reuse the build-level pin without another registry pull.
	parent.resolvedBases[key] = ResolvedImageSource{ImageID: "frozen-package-base"}
	selected, err := child.resolveBaseImage(context.Background(), key.Reference, target)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ImageID != "frozen-package-base" || child.resolvedBases[key].ImageID != selected.ImageID {
		t.Fatalf("selected=%+v child=%v", selected, child.resolvedBases)
	}
	sibling := &graphExecutor{options: PlanOptions{componentParent: parent}, resolvedBases: make(map[ResolvedBaseKey]ResolvedImageSource)}
	other, err := sibling.resolveBaseImage(context.Background(), key.Reference, target)
	if err != nil || other.ImageID != selected.ImageID {
		t.Fatalf("sibling=%+v err=%v", other, err)
	}
	build, err := parent.resolveBaseImage(context.Background(), key.Reference, target)
	if err != nil || build.ImageID != selected.ImageID {
		t.Fatalf("parent=%+v err=%v", build, err)
	}
}

func TestReadLocalComponentGlobKeepsLiteralMetacharacterFilename(t *testing.T) {
	root := t.TempDir()
	literal := "file[1].coopr"
	const source = "extend\nenv literal=\"yes\"\n"
	if err := os.WriteFile(filepath.Join(root, literal), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	data, canonical, err := readLocalComponent(context.Background(), PlanOptions{ContextDir: root}, "./*.coopr")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != source || canonical != filepath.Join(root, literal) {
		t.Fatalf("data=%q canonical=%q", data, canonical)
	}
	// The selected filename must not be reinterpreted as a second glob and
	// accidentally read file1.coopr instead of file[1].coopr.
	if err := os.WriteFile(filepath.Join(root, "file1.coopr"), []byte("extend\nenv wrong=\"yes\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data, canonical, err = readLocalComponent(context.Background(), PlanOptions{ContextDir: root}, "./file[[]1].coopr")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != source || canonical != filepath.Join(root, literal) {
		t.Fatalf("competing file changed selection: data=%q canonical=%q", data, canonical)
	}
	if _, _, err := readLocalComponent(context.Background(), PlanOptions{ContextDir: root}, "./*.coopr"); err == nil {
		t.Fatal("accepted glob matching two definitions")
	}
}

func TestLocalComponentPackageCopyExcludesCallerOutput(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "shared.coopr", `package as="assets"
copy "generated-output" "/proof"
extend
copy "/proof" "/proof" from="assets"
`)
	f.write(t, "generated-output/secret", "previous build output must stay excluded")
	options := f.options
	options.Output = Output{Path: filepath.Join(f.contextDir, "generated-output")}
	_, err := BuildPlan(f.ctx, testPlan(t, "from \"scratch\"\ncomponent \"./shared.coopr\"\n"), options)
	if err == nil {
		t.Fatal("package COPY consumed caller output directory")
	}
	if !strings.Contains(err.Error(), "filtered out") {
		t.Fatalf("package COPY error=%v", err)
	}
}

func TestLocalComponentInvocationCycleWithChangingPackageArguments(t *testing.T) {
	for _, jobs := range []int{1, 2} {
		t.Run(fmt.Sprintf("jobs-%d", jobs), func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "payload", "proof")
			f.write(t, "cycle.coopr", `arg "depth" "x"
package as="assets"
arg "depth"
copy "payload" "/payload"
env depth="${depth}"
extend
arg "depth"
copy "/payload" "/payload" from="assets"
component "./cycle.coopr" depth="${depth}x"
`)
			options := f.options
			options.Jobs = jobs
			options.Output = Output{Path: filepath.Join(f.root, "result")}
			source := "from \"scratch\"\ncomponent \"./cycle.coopr\" depth=\"x\"\n"
			if jobs == 2 {
				source = "from \"scratch\" as=\"recursive\"\ncomponent \"./cycle.coopr\" depth=\"x\"\nfrom \"scratch\" as=\"independent\"\ncopy \"payload\" \"/independent\"\nfrom \"scratch\"\ncopy \"/payload\" \"/payload\" from=\"recursive\"\ncopy \"/independent\" \"/independent\" from=\"independent\"\n"
			}
			ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
			defer cancel()
			_, err := BuildPlan(ctx, testPlan(t, source), options)
			if err == nil || !strings.Contains(err.Error(), "component invocation cycle at") || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("changing-parameter cycle error=%v", err)
			}
		})
	}
}
