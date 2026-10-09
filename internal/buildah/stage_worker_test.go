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

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestStageWorkerResultPreservesCommittedImageOnCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping committed worker response in short mode")
	}
	root := t.TempDir()
	options := PlanOptions{Store: cacheTestStore(filepath.Join(root, "store")), ContextDir: root, Isolation: "rootless", Lifecycle: LifecycleControls{NoLayers: true}, Output: Output{Path: filepath.Join(root, "output"), DisableCompression: true}}
	seed, err := BuildPlan(context.Background(), testPlan(t, "from \"scratch\"\nenv proof=\"committed\"\n"), options)
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
	response := stageWorkerResponse{ImageID: seed.ImageID, Config: json.RawMessage(`{"config":{"Env":["proof=committed"]}}`)}
	for _, workerErr := range []error{nil, context.Canceled, context.DeadlineExceeded, errors.New("worker exit failure")} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		finished, err := (&graphExecutor{store: lease.store}).completeStageWorkerResponse(ctx, "producer", response, nil, workerErr, false, "")
		if !errors.Is(err, context.Canceled) || (workerErr != nil && !errors.Is(err, workerErr)) || finished.state.storageImageID != seed.ImageID || finished.state.config == nil {
			t.Fatalf("lost committed ownership or worker error: %#v, %v", finished.state, err)
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
	if response.Error == "" || !strings.Contains(response.Error, "decode stage bad image config") {
		t.Fatalf("failure response = %+v", response)
	}
}

func TestStageWorkerMessagePreservesExecutionInputs(t *testing.T) {
	root := t.TempDir()
	logical := imageconfig.New()
	if err := logical.Apply(definition.Instruction{Name: "env", Properties: map[string]string{"NAME": "value"}}); err != nil {
		t.Fatal(err)
	}
	config, err := logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	request := stageWorkerRequest{
		ProgressPrefix:    "[linux/amd64] [2/3] [component shared] ",
		ProgressReference: "app:latest",
		BaseRoot:          &PackageRootMetadata{Mode: 0711, UID: 1, GID: 2, PAXRecords: map[string]string{"SCHILY.xattr.user.coopr": "root"}},
		Mode:              planner.Build,
		Stage:             planner.Stage{ID: "1", Name: "build", Kind: "from", Source: "base", Platform: "linux/amd64"},
		Base:              "storage-base", BaseManifest: digest.FromString("selected base manifest"), Logical: config,
		Operations:    []planner.Operation{{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"network": "none"}}}},
		Aliases:       map[string]string{"base": "0"},
		Images:        map[string]string{"0": "storage-parent"},
		ComponentPins: map[string]string{"selection": "sha256:" + strings.Repeat("1", 64)},
		CacheScope:    &CacheMountScope{Kind: "component", Source: digest.FromString("component"), Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Parameters: map[string]string{"channel": "stable"}, Stage: "1"},
		Jobs:          1,
		Store:         StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir:    root, ContextArtifacts: []string{filepath.Join(root, "worker")}, Isolation: "rootless", Runtime: "crun",
		NoCache:       true,
		CacheLocalDir: filepath.Join(root, "cache"), CacheRepository: "registry.example/cache",
		CacheFrom:      []CacheSpec{{Transport: "registry", Reference: "registry.example/read"}},
		CacheTo:        []CacheSpec{{Transport: "registry", Reference: "registry.example/write"}},
		ComponentRelay: filepath.Join(root, "component-relay"), InstructionRelay: filepath.Join(root, "instruction-relay"),
		Network:     "none",
		AddHosts:    []string{"example.test:127.0.0.1"},
		ProxyArgs:   map[string]string{"http_proxy": "http://proxy.example"},
		RunControls: RunControls{HTTPProxy: true, ShmSize: 16 << 20, DNSOptions: []string{"ndots:2"}},
		AuthFile:    "/tmp/auth.json", CertDir: "/tmp/certs", TLSVerify: new(false),
		ResolverEnabled: true, ComponentStore: filepath.Join(root, "components"),
		Pull: true, PullPolicy: "always",
		ImageOutput: Output{Path: filepath.Join(root, "layout"), Reference: "example", Format: "docker"},
		Output:      true, CaptureRoot: true, JobID: strings.Repeat("b", 32),
		SignaturePolicy: filepath.Join(root, "policy.json"), BigFilesTempDir: root,
		ResultPath: filepath.Join(root, "result.json"),
	}
	path := filepath.Join(root, "request.json")
	if err := writeWorkerJSON(path, request); err != nil {
		t.Fatal(err)
	}
	var decoded stageWorkerRequest
	if err := readWorkerJSON(path, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, request) {
		t.Fatalf("decoded request differs:\n got: %#v\nwant: %#v", decoded, request)
	}
	parsed, err := imageconfig.Parse(decoded.Logical)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := parsed.MarshalJSON()
	if err != nil || len(raw) == 0 || decoded.Images["0"] != "storage-parent" {
		t.Fatalf("round-tripped config = %s, source=%q, error=%v", raw, decoded.Images["0"], err)
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
	if command.ProcessState.Exited() || !stageWorkerExitConfirmed(command) {
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
