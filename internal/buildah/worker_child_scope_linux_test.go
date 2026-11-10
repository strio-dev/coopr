//go:build linux

package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
	"golang.org/x/sys/unix"
)

const childScopeHelper = "COOPR_TEST_CHILD_SCOPE"

// All subreaper changes take place in a disposable worker process. In particular,
// the Go test runner must not acquire ownership of unrelated tests' children.
func TestWorkerChildScopeDrainsDetachedDescendants(t *testing.T) {
	requireChildScopeProc(t)
	before, err := workerSubreaperFlag()
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"success", "failure", "cancel", "relayed-incomplete", "many", "observation-failed"} {
		t.Run(outcome, func(t *testing.T) {
			command := childScopeCommand(t, "owner", t.TempDir())
			command.Env = append(command.Env, "COOPR_TEST_SCOPE_OUTCOME="+outcome)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("scope helper: %v\n%s", err, output)
			}
		})
	}
	after, err := workerSubreaperFlag()
	if err != nil || after != before {
		t.Fatalf("caller subreaper changed: %d -> %d (%v)", before, after, err)
	}
}

func TestWorkerChildScopeDoesNotReapCallerChildren(t *testing.T) {
	requireChildScopeProc(t)
	sibling := childScopeCommand(t, "sleeper", t.TempDir())
	if err := sibling.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sibling.Process.Kill(); _ = sibling.Wait() }()
	commands := []*exec.Cmd{childScopeCommand(t, "owner", t.TempDir()), childScopeCommand(t, "owner", t.TempDir())}
	for _, command := range commands {
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if err := sibling.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated caller child affected: %v", err)
	}
}

func TestWorkerChildScopeHelper(t *testing.T) {
	role := os.Getenv(childScopeHelper)
	if role == "" {
		return
	}
	directory := os.Getenv("COOPR_TEST_SCOPE_DIRECTORY")
	switch role {
	case "sleeper":
		if os.Getenv("COOPR_TEST_SCOPE_OUTCOME") == "many" {
			signal.Ignore(syscall.SIGTERM)
			if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("child-%d", os.Getpid())), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(time.Minute)
	case "daemon":
		if owner := os.Getenv("COOPR_TEST_SCOPE_SIGNAL_OWNER"); owner != "" {
			pid, err := strconv.Atoi(owner)
			if err != nil {
				t.Fatal(err)
			}
			termination := make(chan os.Signal, 1)
			signal.Notify(termination, syscall.SIGTERM)
			go func() {
				<-termination
				if _, err := os.Stat(filepath.Join(directory, "result.json")); err == nil {
					_ = os.WriteFile(filepath.Join(directory, "early-response"), nil, 0o600)
				}
				_ = os.WriteFile(filepath.Join(directory, "draining"), nil, 0o600)
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}()
		}
		count := 1
		if os.Getenv("COOPR_TEST_SCOPE_OUTCOME") == "many" {
			count = 24
			signal.Ignore(syscall.SIGTERM)
		}
		pids := []string{strconv.Itoa(os.Getpid())}
		for range count {
			child := childScopeCommand(t, "sleeper", directory)
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			pids = append(pids, strconv.Itoa(child.Process.Pid))
		}
		if count > 1 {
			deadline := time.Now().Add(5 * time.Second)
			for _, pid := range pids[1:] {
				for {
					if _, err := os.Stat(filepath.Join(directory, "child-"+pid)); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("TERM-ignoring child did not start")
					}
					time.Sleep(time.Millisecond)
				}
			}
		}
		if err := os.WriteFile(filepath.Join(directory, "ready"), []byte(strings.Join(pids, " ")), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
	case "launcher":
		child := childScopeCommand(t, "daemon", directory)
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(directory, "ready")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("daemon did not become ready")
			}
			time.Sleep(time.Millisecond)
		}
	case "pending":
		scope, err := newWorkerChildScope()
		if err != nil {
			t.Fatal(err)
		}
		command := childScopeCommand(t, "sleeper", directory)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
		if err := scope.finish(errors.Join(ErrWorkerWaitIncomplete, errManagedWorkerWaitPending)); !errors.Is(err, ErrWorkerWaitIncomplete) {
			t.Fatalf("accepted active waiter: %v", err)
		}
		if err := command.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("managed child was touched: %v", err)
		}
	case "owner", "owner-stage", "owner-plan":
		scope, err := newWorkerChildScope()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = scope.finish(nil) }()
		if role != "owner" {
			t.Setenv("COOPR_TEST_SCOPE_SIGNAL_OWNER", strconv.Itoa(os.Getpid()))
		}
		launcher := childScopeCommand(t, "launcher", directory)
		var operationErr error
		if os.Getenv("COOPR_TEST_SCOPE_OUTCOME") == "observation-failed" {
			operationErr = runWorkerProcessControlled(context.Background(), launcher, time.Second, workerProcessControl{
				signalGroup: syscall.Kill, killProcess: func(process *os.Process) error { return process.Kill() }, killWait: time.Second,
				waitExit: func(pid int) error {
					if err := waitWorkerExit(pid); err != nil {
						return err
					}
					return syscall.EIO
				},
			})
			if !errors.Is(operationErr, ErrWorkerWaitIncomplete) || !errors.Is(operationErr, syscall.EIO) || errors.Is(operationErr, errManagedWorkerWaitPending) || launcher.ProcessState == nil {
				t.Fatalf("completed wait retained active ownership: %v", operationErr)
			}
		} else if output, err := launcher.CombinedOutput(); err != nil {
			t.Fatalf("launcher: %v %s", err, output)
		}
		started := time.Now()
		switch os.Getenv("COOPR_TEST_SCOPE_OUTCOME") {
		case "failure":
			operationErr = errors.New("operation failed")
		case "cancel":
			operationErr = context.Canceled
		case "relayed-incomplete":
			operationErr = WorkerFailure(ErrWorkerWaitIncomplete)
		}
		if role == "owner" {
			if err := scope.finish(operationErr); err != nil {
				t.Fatal(err)
			}
		} else {
			requestPath := filepath.Join(directory, "request.json")
			resultPath := filepath.Join(directory, "result.json")
			if role == "owner-stage" {
				request := stageWorkerRequest{Mode: planner.Build, Stage: planner.Stage{ID: "bad", Kind: "from", Source: "scratch", Platform: "linux/amd64"}, Logical: json.RawMessage(`{"config":[]}`), JobID: strings.Repeat("a", 32), ResultPath: resultPath}
				if err := writeWorkerJSON(requestPath, request); err != nil {
					t.Fatal(err)
				}
				if err := executeStageWorkerScoped(requestPath, scope); err == nil || !workerFailureReported(err) {
					t.Fatalf("stage finalizer error: %v", err)
				}
				response, err := readStageWorkerResponse(resultPath)
				if err != nil || !response.CleanupComplete || response.Failure == nil {
					t.Fatalf("stage completion proof: %+v (%v)", response, err)
				}
			} else {
				request := planWorkerRequest{
					workerExecutionOptions: workerExecutionOptions{
						SourcePolicyFile: filepath.Join(directory, "absent-policy.json"),
					},
					Mode: "build", JobID: strings.Repeat("a", 32), ResultPath: resultPath, Definition: &definition.Definition{Instructions: []definition.Instruction{{Name: "from", Arguments: []string{"scratch"}}}}, PlannerOptions: &planner.Options{Mode: planner.Build},
				}
				if err := writeWorkerJSON(requestPath, request); err != nil {
					t.Fatal(err)
				}
				if err := executePlanWorkerScoped(requestPath, scope); err == nil || !workerFailureReported(err) {
					t.Fatalf("plan finalizer error: %v", err)
				}
				response, err := readWorkerResponse(resultPath)
				if err != nil || !response.CleanupComplete || response.Failure == nil {
					t.Fatalf("plan completion proof: %+v (%v)", response, err)
				}
			}
			if _, err := os.Stat(filepath.Join(directory, "draining")); err != nil {
				t.Fatalf("signal was not delivered during finalization: %v", err)
			}
			if _, err := os.Stat(filepath.Join(directory, "early-response")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("response was published before descendant drain: %v", err)
			}
		}
		if time.Since(started) > workerChildDrainTimeout+time.Second {
			t.Fatal("unbounded drain")
		}
		data, err := os.ReadFile(filepath.Join(directory, "ready"))
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("descendant %d still present: %v", pid, err)
			}
		}
		if children, err := linuxProcessChildren(os.Getpid()); err != nil || len(children) != 0 {
			t.Fatalf("children remain: %v (%v)", children, err)
		}
	default:
		t.Fatalf("unknown child scope helper role %q", role)
	}
}

func childScopeCommand(t *testing.T, role, directory string) *exec.Cmd {
	t.Helper()
	command := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerChildScopeHelper$", "-test.timeout=15s", "-test.short="+strconv.FormatBool(testing.Short()))
	command.Env = append(os.Environ(), childScopeHelper+"="+role, "COOPR_TEST_SCOPE_DIRECTORY="+directory)
	return command
}

func TestWorkerChildScopePreservesManagedWaitOwnership(t *testing.T) {
	requireChildScopeProc(t)
	command := childScopeCommand(t, "pending", t.TempDir())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("scope pending waiter: %v\n%s", err, output)
	}
}

func requireChildScopeProc(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/task"); err != nil {
		t.Skipf("worker ownership proof requires procfs: %v", err)
	}
}

func workerSubreaperFlag() (int32, error) {
	var flag int32
	err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&flag)), 0, 0, 0)
	return flag, err
}

func TestWorkerChildScopeFinalizesBeforeResponseWithTERM(t *testing.T) {
	requireChildScopeProc(t)
	for _, role := range []string{"owner-stage", "owner-plan"} {
		t.Run(role, func(t *testing.T) {
			command := childScopeCommand(t, role, t.TempDir())
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("finalizer: %v\n%s", err, output)
			}
		})
	}
}
