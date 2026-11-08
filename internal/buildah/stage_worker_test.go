package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
	commonconfig "go.podman.io/common/pkg/config"
)

func TestStageWorkerResultPreservesCommittedImageOnCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping committed worker response in short mode")
	}
	root := t.TempDir()
	options := PlanOptions{Store: cacheTestStore(filepath.Join(root, "store")), ContextDir: root, Isolation: "rootless", Lifecycle: LifecycleControls{NoLayers: true}, Output: Output{Path: filepath.Join(root, "output"), DisableCompression: true}}
	seed, err := BuildPlan(context.Background(), testPlan(t, `from "scratch" {
    env proof="committed"
}
`), options)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(options.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	response := stageWorkerResponse{ImageID: seed.ImageID, CommandSet: true, Config: json.RawMessage(`{"config":{"Env":["proof=committed"],"Cmd":["hello"]}}`)}
	for _, workerErr := range []error{nil, context.Canceled, context.DeadlineExceeded, errors.New("worker exit failure")} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		finished, err := (&graphExecutor{store: lease.store}).completeStageWorkerResponse(ctx, "producer", response, nil, workerErr, false, "")
		if !errors.Is(err, context.Canceled) || (workerErr != nil && !errors.Is(err, workerErr)) || finished.state.storageImageID != seed.ImageID || finished.state.config == nil {
			t.Fatalf("lost committed ownership or worker error: %#v, %v", finished.state, err)
		}
		if err := finished.state.config.Apply(definition.Instruction{Name: "entrypoint", Arguments: []string{"echo"}}); err != nil {
			t.Fatal(err)
		}
		raw, err := finished.state.config.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var image v1.Image
		if err := json.Unmarshal(raw, &image); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(image.Config.Cmd, []string{"hello"}) {
			t.Fatalf("worker result lost CMD authorship: %v", image.Config.Cmd)
		}
	}
	response.ImageID = "invalid"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finished, err := (&graphExecutor{store: lease.store}).completeStageWorkerResponse(ctx, "producer", response, nil, nil, false, "")
	if !errors.Is(err, context.Canceled) || finished.state.storageImageID != "" {
		t.Fatalf("accepted invalid canceled worker response: %#v, %v", finished.state, err)
	}
}

func TestExecuteStageWorkerWritesFailureResponseBeforeReturning(t *testing.T) {
	root := t.TempDir()
	requestPath := filepath.Join(root, "request.json")
	resultPath := filepath.Join(root, "result.json")
	request := stageWorkerRequest{
		Mode:       planner.Build,
		Stage:      planner.Stage{ID: "bad", Kind: "from", Source: "scratch", Platform: "linux/amd64"},
		Logical:    json.RawMessage(`{"config":[]}`),
		JobID:      strings.Repeat("a", 32),
		ResultPath: resultPath,
	}
	if err := writeWorkerJSON(requestPath, request); err != nil {
		t.Fatal(err)
	}
	err := executeStageWorker(requestPath)
	if err == nil {
		t.Fatal("invalid stage config unexpectedly succeeded")
	}
	response, readErr := readStageWorkerResponse(resultPath)
	if readErr != nil {
		t.Fatalf("read failure response: %v", readErr)
	}
	if response.Failure == nil || !strings.Contains(response.Failure.Message, "decode stage bad image config") {
		t.Fatalf("failure response = %+v", response)
	}
}

func TestStageFailurePreservesSupervisionErrorAndRUNStatus(t *testing.T) {
	transportErr := exec.Command("sh", "-c", "exit 1").Run()
	cleanupErr := errors.New("fixture group cleanup failed")
	response := stageWorkerResponse{Failure: &workerError{Message: "RUN exited 42", Code: 42}}
	for _, processErr := range []error{transportErr, errors.Join(transportErr, cleanupErr)} {
		_, err := (&graphExecutor{}).completeStageWorkerResponse(context.Background(), "failed", response, nil, processErr, false, "")
		var status interface{ ExitCode() int }
		if !errors.As(err, &status) || status.ExitCode() != 42 {
			t.Fatalf("failed operation lost RUN status: %v", err)
		}
		if errors.Is(processErr, cleanupErr) && !errors.Is(err, cleanupErr) {
			t.Fatalf("failed operation lost supervision error: %v", err)
		}
		if processErr == transportErr && err.Error() != response.Failure.Message {
			t.Fatalf("ordinary worker transport changed RUN diagnostic: %v", err)
		}
	}
}

func TestExecuteStageWorkerRejectsMalformedJobIDWithoutOpeningStore(t *testing.T) {
	root := t.TempDir()
	requestPath := filepath.Join(root, "request.json")
	resultPath := filepath.Join(root, "result.json")
	if err := writeWorkerJSON(requestPath, stageWorkerRequest{
		JobID: "not-a-job", ResultPath: resultPath, Logical: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	err := executeStageWorker(requestPath)
	if err == nil || !strings.Contains(err.Error(), "invalid stage worker job ID") {
		t.Fatalf("malformed job ID error = %v", err)
	}
	if _, statErr := os.Stat(resultPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("malformed request wrote a response: %v", statErr)
	}
}

func TestStageWorkerRecognizesReapedSignalExit(t *testing.T) {
	command := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
	command.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "self-kill")
	err := command.Run()
	if err == nil || command.ProcessState == nil {
		t.Fatalf("signaled worker exit = %v, state = %v", err, command.ProcessState)
	}
	if command.ProcessState.Exited() || !stageWorkerExitConfirmed(command, err) {
		t.Fatalf("reaped signaled worker was treated as alive: state = %v", command.ProcessState)
	}
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("worker status = %v, want SIGKILL", command.ProcessState.Sys())
	}
}

func TestResolvedBaseSnapshotSeparatesSchedulerAndWorker(t *testing.T) {
	key := ResolvedBaseKey{Reference: "registry.example/base:latest", Platform: "linux/amd64"}
	resolved := map[ResolvedBaseKey]ResolvedImageSource{key: {ImageID: "first"}}
	snapshot := maps.Clone(resolved)
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		for range 100_000 {
			for range snapshot {
			}
		}
		close(done)
	}()
	<-started
	for range 100_000 {
		resolved[key] = ResolvedImageSource{ImageID: "second"}
	}
	<-done
	if got := snapshot[key].ImageID; got != "first" {
		t.Fatalf("worker snapshot image ID = %q, want first", got)
	}
}

func TestStageWorkerProtocolPreservesOpaqueSelectedConfig(t *testing.T) {
	config := []byte("{\n  \"architecture\" : \"amd64\",\n  \"os\" : \"linux\",\n  \"x-extension\" : { \"value\" : 7 }\n}\n")
	request := stageWorkerRequest{ResolvedBases: []stageWorkerBase{{Key: ResolvedBaseKey{Reference: "example:base", Platform: "linux/amd64"}, Source: ResolvedImageSource{ImageID: "selected", ConfigData: config}}}}
	path := filepath.Join(t.TempDir(), "request.json")
	if err := writeWorkerJSON(path, request); err != nil {
		t.Fatal(err)
	}
	var decoded stageWorkerRequest
	if err := readWorkerJSON(path, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.ResolvedBases) != 1 || string(decoded.ResolvedBases[0].Source.ConfigData) != string(config) {
		t.Fatalf("worker transport changed verified config bytes: %+v", decoded.ResolvedBases)
	}
}

func TestStageWorkerInstallsModulesBeforeResolver(t *testing.T) {
	t.Setenv("REGISTRY_AUTH_FILE", "")
	t.Setenv("DOCKER_CONFIG", "")
	for _, test := range []struct{ name, baselineDelay, moduleDelay, wantError string }{
		{"module repairs baseline retry", "invalid-baseline", "13ms", "missing-source-policy.json"},
		{"module retry validated by resolver", "11ms", "invalid-module", "create stage resolver: parse containers.conf retry_delay"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			baseline := filepath.Join(root, "baseline.conf")
			module := filepath.Join(root, "module.conf")
			if err := os.WriteFile(baseline, []byte("[engine]\nretry=1\nretry_delay='"+test.baselineDelay+"'\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(module, []byte("[engine]\nretry=7\nretry_delay='"+test.moduleDelay+"'\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = commonconfig.New(&commonconfig.Options{SetDefault: true}) })
			if _, err := commonconfig.New(&commonconfig.Options{Modules: []string{baseline}, SetDefault: true}); err != nil {
				t.Fatal(err)
			}
			request := stageWorkerRequest{
				workerExecutionOptions: workerExecutionOptions{
					RunControls:      RunControls{ConfigModules: []string{module}},
					SourcePolicyFile: filepath.Join(root, "missing-source-policy.json"),
				},
				Stage:   planner.Stage{ID: "module-order", Platform: "linux/amd64"},
				Logical: json.RawMessage(`{}`), BaseLogical: json.RawMessage(`{}`),
				ResolverEnabled: true, ComponentStore: filepath.Join(root, "components"),
			}
			path := filepath.Join(root, "request.json")
			if err := writeWorkerJSON(path, request); err != nil {
				t.Fatal(err)
			}
			var decoded stageWorkerRequest
			if err := readWorkerJSON(path, &decoded); err != nil {
				t.Fatal(err)
			}
			_, err := executeStageWorkerRequestControlled(context.Background(), decoded, nil)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("worker boundary error=%v, want %q", err, test.wantError)
			}
			cfg, err := commonconfig.Default()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Engine.Retry != 7 || cfg.Engine.RetryDelay != test.moduleDelay {
				t.Fatalf("worker selected wrong config: retry=%d delay=%q", cfg.Engine.Retry, cfg.Engine.RetryDelay)
			}
		})
	}
}

func TestExecuteStageWorkerDoesNotSuppressFinalWriteFailure(t *testing.T) {
	directory := t.TempDir()
	requestPath := filepath.Join(directory, "request.json")
	request := stageWorkerRequest{Mode: planner.Build, Stage: planner.Stage{ID: "bad", Kind: "from", Source: "scratch", Platform: "linux/amd64"}, Logical: json.RawMessage(`{"config":[]}`), JobID: strings.Repeat("a", 32), ResultPath: directory}
	if err := writeWorkerJSON(requestPath, request); err != nil {
		t.Fatal(err)
	}
	err := executeStageWorker(requestPath)
	if err == nil || !strings.Contains(err.Error(), "write build worker message") {
		t.Fatalf("missing final write diagnostic: %v", err)
	}
	if workerFailureReported(err) {
		t.Fatalf("unwritten failure would suppress stderr: %v", err)
	}
}

func TestStageWorkerMissingFailureRetainsFailedHelper(t *testing.T) {
	processErr := exec.Command("sh", "-c", "exit 1").Run()
	if processErr == nil {
		t.Fatal("fixture helper succeeded")
	}
	response := stageWorkerResponse{CleanupComplete: true}
	_, err := (&graphExecutor{}).completeStageWorkerResponse(context.Background(), "missing-failure", response, nil, processErr, false, "")
	if !errors.Is(err, processErr) {
		t.Fatalf("failed helper without a structured failure was accepted: %v", err)
	}
	if errors.Is(err, ErrWorkerWaitIncomplete) {
		t.Fatalf("confirmed cleanup was lost: %v", err)
	}
}

func executeStageWorker(requestPath string) error {
	return executeStageWorkerScoped(requestPath, nil)
}
