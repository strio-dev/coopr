package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func controlPair(t *testing.T, handler componentReturnHandler, selection componentSelectionHandler, bases ...baseSelectionHandler) (*stageWorkerControlClient, context.Context) {
	t.Helper()
	control, err := newStageWorkerControl()
	if err != nil {
		t.Fatal(err)
	}
	client, err := stageWorkerControlFromFiles(control.childRead, control.childWrite)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- control.serve(ctx, "stage", handler, selection, bases...) }()
	t.Cleanup(func() {
		cancel()
		control.close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("control server did not stop")
		}
	})
	return client, ctx
}

func TestStageWorkerControlReturnRoundTripAndBoundaryValidation(t *testing.T) {
	calls := 0
	client, ctx := controlPair(t, func(_ context.Context, request componentReturnRequest) (componentReturnReply, error) {
		calls++
		if request.Environment["proof"] != "actual" {
			t.Errorf("lost returned environment: %v", request.Environment)
		}
		return componentReturnReply{Revision: request.Revision + 1, ComponentPins: map[string]string{"ref": "immutable"}}, nil
	}, nil)
	request := componentReturnRequest{Boundary: planner.ComponentBoundary{StageID: "stage", Instruction: 2}, Environment: map[string]string{"proof": "actual"}}
	result, err := client.componentReturn(ctx, request)
	if err != nil || result.ComponentPins["ref"] != "immutable" {
		t.Fatalf("return = %#v, %v", result, err)
	}
	for _, test := range []struct {
		name    string
		request componentReturnRequest
		want    string
	}{
		{"stale", request, "stale revision"},
		{"duplicate", componentReturnRequest{Boundary: request.Boundary, Revision: 1}, "duplicate"},
		{"wrong stage", componentReturnRequest{Boundary: planner.ComponentBoundary{StageID: "other"}, Revision: 1}, "invalid stage"},
		{"negative boundary", componentReturnRequest{Boundary: planner.ComponentBoundary{StageID: "stage", Instruction: -1}, Revision: 1}, "invalid stage"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.componentReturn(ctx, test.request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if calls != 1 {
		t.Fatalf("invalid return reached coordinator: %d calls", calls)
	}
}

func TestStageWorkerControlSelectionAndCoordinatorError(t *testing.T) {
	client, ctx := controlPair(t, func(context.Context, componentReturnRequest) (componentReturnReply, error) {
		return componentReturnReply{}, errors.New("dependency failed")
	}, func(_ context.Context, request componentSelectionRequest) (componentSelectionReply, error) {
		if request.Reference != "./shared.coopr" || request.CallerEnvironment["proof"] != "caller" || !request.ActiveLocalComponents["canonical-target"] || !request.ActiveComponents[digest.FromString("ancestor")] {
			t.Errorf("selection input lost: %#v", request)
		}
		return componentSelectionReply{ComponentPins: map[string]string{"selection": "sha256:proof"}, ComponentSources: map[string]string{"selection": "canonical-target"}}, nil
	})
	selected, err := client.selectComponent(ctx, componentSelectionRequest{Reference: "./shared.coopr", CallerEnvironment: map[string]string{"proof": "caller"}, ActiveLocalComponents: map[string]bool{"canonical-target": true}, ActiveComponents: map[digest.Digest]bool{digest.FromString("ancestor"): true}})
	if err != nil || selected.ComponentPins["selection"] != "sha256:proof" || selected.ComponentSources["selection"] != "canonical-target" {
		t.Fatalf("selection = %#v, %v", selected, err)
	}
	_, err = client.componentReturn(ctx, componentReturnRequest{Boundary: planner.ComponentBoundary{StageID: "stage"}})
	if err == nil || !strings.Contains(err.Error(), "dependency failed") {
		t.Fatalf("coordinator error = %v", err)
	}
}

func TestStageWorkerControlPendingEOF(t *testing.T) {
	control, err := newStageWorkerControl()
	if err != nil {
		t.Fatal(err)
	}
	defer control.close()
	client, err := stageWorkerControlFromFiles(control.childRead, control.childWrite)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var request stageControlRequest
		err := json.NewDecoder(control.parentRead).Decode(&request)
		_ = control.parentWrite.Close()
		done <- err
	}()
	_, err = client.componentReturn(context.Background(), componentReturnRequest{})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("pending return EOF = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStageWorkerControlCancellationClosesPendingRead(t *testing.T) {
	entered := make(chan struct{})
	client, ctx := controlPair(t, func(ctx context.Context, _ componentReturnRequest) (componentReturnReply, error) {
		close(entered)
		<-ctx.Done()
		return componentReturnReply{}, ctx.Err()
	}, nil)
	callCtx, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() {
		_, err := client.componentReturn(callCtx, componentReturnRequest{Boundary: planner.ComponentBoundary{StageID: "stage"}})
		result <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled control = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending control did not cancel")
	}
}

func TestStageWorkerControlRejectsInvalidReplyRevision(t *testing.T) {
	client, ctx := controlPair(t, func(context.Context, componentReturnRequest) (componentReturnReply, error) {
		return componentReturnReply{Revision: 9}, nil
	}, nil)
	_, err := client.componentReturn(ctx, componentReturnRequest{Boundary: planner.ComponentBoundary{StageID: "stage"}})
	if err == nil || !strings.Contains(err.Error(), "invalid revision") {
		t.Fatalf("revision error = %v", err)
	}
}

func TestStageWorkerControlDescriptorsCloseOnExec(t *testing.T) {
	control, err := newStageWorkerControl()
	if err != nil {
		t.Fatal(err)
	}
	defer control.close()
	client, err := stageWorkerControlFromFiles(control.childRead, control.childWrite)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []*os.File{client.read, client.write} {
		path := "/proc/self/fd/" + strconv.Itoa(int(file.Fd()))
		identity, err := os.Readlink(path)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(testWorkerBinary(t), "-test.run=^TestStageWorkerControlFDHelper$", "-test.short=true", "--", path, identity)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("control pipe inherited by subprocess: %s, %v", output, err)
		}
	}
}

func TestStageWorkerControlFDHelper(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "--" {
		return
	}
	identity, err := os.Readlink(os.Args[len(os.Args)-2])
	if err == nil && identity == os.Args[len(os.Args)-1] {
		t.Fatal("control pipe survived exec")
	}
}

func TestControlledStageWorkerSupervision(t *testing.T) {
	for _, mode := range []string{"return", "cancel", "malformed", "start-failure"} {
		t.Run(mode, func(t *testing.T) {
			control, err := newStageWorkerControl()
			if err != nil {
				t.Fatal(err)
			}
			defer control.close()
			command := exec.Command(testWorkerBinary(t), "-test.run=^TestStageWorkerControlProcessHelper$", "-test.short=true", "--", mode)
			if mode == "start-failure" {
				command.Path = "/coopr-test-missing-worker"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if mode == "cancel" {
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
			}
			called := false
			handler := func(ctx context.Context, request componentReturnRequest) (componentReturnReply, error) {
				called = true
				if mode == "cancel" {
					cancel()
					<-ctx.Done()
					return componentReturnReply{}, ctx.Err()
				}
				return componentReturnReply{Revision: request.Revision + 1}, nil
			}
			err = runControlledStageWorker(ctx, command, "stage", control, handler, nil)
			switch mode {
			case "return":
				if err != nil || !called {
					t.Fatalf("normal worker = %v, callback=%t", err, called)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || !called {
					t.Fatalf("canceled worker = %v, callback=%t", err, called)
				}
			case "malformed":
				if err == nil || !strings.Contains(err.Error(), "read stage control request") {
					t.Fatalf("malformed request = %v", err)
				}
			case "start-failure":
				if err == nil || !strings.Contains(err.Error(), "start build worker") {
					t.Fatalf("start failure = %v", err)
				}
			}
			if !stageWorkerExitConfirmed(command) {
				t.Fatal("worker was not reaped")
			}
		})
	}
}

func TestStageWorkerControlProcessHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	read, write := os.NewFile(stageWorkerControlReadFD, "control-read"), os.NewFile(stageWorkerControlWriteFD, "control-write")
	client, err := stageWorkerControlFromFiles(read, write)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if os.Args[len(os.Args)-1] == "malformed" {
		_, _ = write.WriteString("{]\n")
		time.Sleep(30 * time.Second)
		return
	}
	_, err = client.componentReturn(context.Background(), componentReturnRequest{Boundary: planner.ComponentBoundary{StageID: "stage", Instruction: 1}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStageWorkerControlRejectsUnavailableDescriptors(t *testing.T) {
	control, err := newStageWorkerControl()
	if err != nil {
		t.Fatal(err)
	}
	defer control.close()
	_ = control.childRead.Close()
	if _, err := stageWorkerControlFromFiles(control.childRead, control.childWrite); err == nil {
		t.Fatal("closed inherited control pipe accepted")
	}
	command := exec.Command("unused")
	command.ExtraFiles = []*os.File{control.parentRead}
	if err := control.attach(command); err == nil {
		t.Fatal("unrelated inherited descriptors overwritten")
	}
}

func TestStageWorkerControlBaseSelection(t *testing.T) {
	calls := 0
	client, ctx := controlPair(t, nil, nil, func(_ context.Context, request baseSelectionRequest) (ResolvedImageSource, error) {
		calls++
		if request.Reference != "example.com/base:mutable" || request.Platform.Architecture != "amd64" {
			t.Errorf("lost selection: %#v", request)
		}
		return ResolvedImageSource{Reference: "example.com/base@sha256:pinned", ConfigData: []byte(`{"config":{"Env":["proof=selected"]}}`)}, nil
	})
	got, err := client.selectBase(ctx, baseSelectionRequest{Reference: "example.com/base:mutable", Platform: v1.Platform{Architecture: "amd64"}})
	if err != nil || got.Reference != "example.com/base@sha256:pinned" || len(got.ConfigData) == 0 || calls != 1 {
		t.Fatalf("selected=%#v, calls=%d, error=%v", got, calls, err)
	}
	_, err = client.exchange(ctx, stageControlRequest{Base: &baseSelectionRequest{}, Selection: &componentSelectionRequest{}})
	if err == nil || !strings.Contains(err.Error(), "mixes operation types") || calls != 1 {
		t.Fatalf("mixed request=%v calls=%d", err, calls)
	}
}
