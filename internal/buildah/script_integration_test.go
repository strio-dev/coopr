package buildah

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"coopr/internal/oci"
)

func TestRunScriptChildrenShareMountsAndCacheOneInstruction(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	writeLinkedRunContext(t, contextDir)
	plan := testPlan(t, `from "scratch" {
 copy "busybox" "/busybox"
 shell "/busybox" "sh" "-ec"
 arg "greeting" "hello"
 run network="none" {
  mount "tmpfs" target="/temporary"
  mount "cache" target="/cache" id="script-children" sharing="locked"
  script """
   cd /temporary
   export only_this_shell=yes
   printf '%s' "$greeting" >value
   printf '%s' "$only_this_shell" >/cache/value
   test "$PWD" = /temporary
   """
  exec "/busybox" "sh" "-ec" "test \"$1\" = '$greeting'; test \"$(/busybox cat /temporary/value)\" = hello" "--" "$greeting"
  script "test -z \"${only_this_shell+x}\"; test \"$PWD\" = /; test \"$(/busybox cat /cache/value)\" = yes; /busybox od -An -N16 -tx1 /dev/urandom >/proof"
 }
}`)
	store := cacheTestStore(root)
	var coldProof string
	for attempt := range 2 {
		layout := filepath.Join(root, fmt.Sprintf("layout-%d", attempt))
		var progress strings.Builder
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, Stdout: io.Discard, Stderr: &progress,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != 2 || len(image.RootFS.DiffIDs) != 2 {
			t.Fatalf("build %d layers/diffids = %d/%d, want COPY plus one RUN layer", attempt, len(manifest.Layers), len(image.RootFS.DiffIDs))
		}
		runEntries := 0
		for _, entry := range image.History {
			if strings.HasPrefix(entry.CreatedBy, "RUN ") {
				runEntries++
				if entry.EmptyLayer {
					t.Fatal("mixed RUN was recorded as an empty layer")
				}
			}
		}
		if runEntries != 1 {
			t.Fatalf("RUN history entries = %d, want 1", runEntries)
		}
		blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
		proof := readLayerFile(t, blob, "proof")
		if strings.TrimSpace(proof) == "" {
			t.Fatal("last script did not produce proof")
		}
		assertLayerNamesAbsent(t, blob, "temporary/value", "cache/value")
		assertRunSequenceHelperAbsent(t, blob)
		if attempt == 0 {
			coldProof = proof
		} else if proof != coldProof || !strings.Contains(progress.String(), "--> Using cache ") {
			t.Fatalf("whole mixed RUN was not reused: cold=%q warm=%q progress=%s", coldProof, proof, progress.String())
		}
	}
}

func TestRunScriptChildrenStopAtFirstFailure(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	writeLinkedRunContext(t, contextDir)
	options := SupervisedPlanOptions{
		Store: cacheTestStore(root), ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
		Output: Output{Path: filepath.Join(root, "failed")}, Stdout: io.Discard, Stderr: io.Discard,
	}
	failed := testPlan(t, `from "scratch" {
 copy "busybox" "/busybox"
 shell "/busybox" "sh" "-ec"
 run network="none" {
  mount "cache" target="/cache" id="script-failfast" sharing="locked"
  script "printf reached >/cache/before; exit 23"
  exec "/busybox" "touch" "/cache/after"
  script "printf later >/cache/later"
 }
}`)
	if _, err := BuildPlanSupervised(ctx, failed, options); err == nil || !strings.Contains(err.Error(), "exit status 23") {
		t.Fatalf("failed script error = %v, want exit status 23", err)
	}
	probe := testPlan(t, `from "scratch" {
 copy "busybox" "/busybox"
 shell "/busybox" "sh" "-ec"
 run network="none" {
  mount "cache" target="/cache" id="script-failfast" sharing="locked"
  script "test \"$(/busybox cat /cache/before)\" = reached; test ! -e /cache/after; test ! -e /cache/later; printf stopped >/proof"
 }
}`)
	options.Output.Path = filepath.Join(root, "probe")
	if _, err := BuildPlanSupervised(ctx, probe, options); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, options.Output.Path)
	blob := filepath.Join(options.Output.Path, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded())
	if proof := readLayerFile(t, blob, "proof"); proof != "stopped" {
		t.Fatalf("fail-fast proof = %q", proof)
	}
}

func TestRunScriptRuntimeConfigurationMatchesShellForm(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	// These executables do not exist. A successful build proves that runtime
	// commands are recorded in the configuration rather than executed now.
	definitions := []string{
		`from "scratch" {
 shell "/custom-shell" "-ec"
 entrypoint "printf entrypoint"
 cmd "printf command"
 healthcheck "printf health"
}`,
		`from "scratch" {
 shell "/custom-shell" "-ec"
 entrypoint { script "printf entrypoint" }
 cmd { script "printf command" }
 healthcheck { script "printf health" }
}`,
	}
	var previous map[string]any
	for index, source := range definitions {
		layout := filepath.Join(root, fmt.Sprintf("config-%d", index))
		_, err := BuildPlanSupervised(ctx, testPlan(t, source), SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		data, err := oci.ReadImageConfigLayout(ctx, layout)
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			previous = document.Config
		} else if !reflect.DeepEqual(previous, document.Config) {
			t.Fatalf("script children changed runtime configuration:\nshell form: %#v\nscript children: %#v", previous, document.Config)
		}
	}
}

func TestRunScriptOnBuildRetainsConfiguredShell(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	writeLinkedRunContext(t, contextDir)
	plan := testPlan(t, `from "scratch" as="base" {
 copy "busybox" "/busybox"
 shell "/busybox" "sh" "-ec"
 onbuild {
  run {
   script """
    #!/unavailable/interpreter
    printf inherited >/inherited-proof
    """
  }
 }
}
from "base" {
 run "/busybox test \"$(/busybox cat /inherited-proof)\" = inherited; printf verified >/proof"
}`)
	layout := filepath.Join(root, "image")
	var progress strings.Builder
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: cacheTestStore(root), ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
		Output: Output{Path: layout}, Stdout: io.Discard, Stderr: &progress,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, progress.String())
	}
	manifest, _ := readPlanImage(t, layout)
	blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded())
	if proof := readLayerFile(t, blob, "proof"); proof != "verified" {
		t.Fatalf("inherited configured-shell proof = %q", proof)
	}
}
