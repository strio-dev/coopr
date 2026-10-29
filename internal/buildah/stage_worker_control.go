package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/strio-dev/coopr/internal/planner"
)

const stageWorkerControlReadFD = 3
const stageWorkerControlWriteFD = 4

type stageControlRequest struct {
	Return    *componentReturnRequest    `json:"return,omitempty"`
	Selection *componentSelectionRequest `json:"selection,omitempty"`
	Base      *baseSelectionRequest      `json:"base,omitempty"`
}

type stageControlReply struct {
	Return    *componentReturnReply    `json:"return,omitempty"`
	Selection *componentSelectionReply `json:"selection,omitempty"`
	Base      *ResolvedImageSource     `json:"base,omitempty"`
	Error     string                   `json:"error,omitempty"`
}

// The two pipes are private to the coordinator and stage process. They never
// share the progress streams or the request/result file protocol.
type stageWorkerControl struct {
	parentRead, parentWrite *os.File
	childRead, childWrite   *os.File
	once                    sync.Once
}

func newStageWorkerControl() (*stageWorkerControl, error) {
	childRead, parentWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	parentRead, childWrite, err := os.Pipe()
	if err != nil {
		_ = childRead.Close()
		_ = parentWrite.Close()
		return nil, err
	}
	return &stageWorkerControl{parentRead: parentRead, parentWrite: parentWrite, childRead: childRead, childWrite: childWrite}, nil
}

func (control *stageWorkerControl) close() {
	control.once.Do(func() {
		for _, file := range []*os.File{control.parentRead, control.parentWrite, control.childRead, control.childWrite} {
			_ = file.Close()
		}
	})
}

func (control *stageWorkerControl) attach(command *exec.Cmd) error {
	if len(command.ExtraFiles) != 0 {
		return errors.New("stage worker control requires exclusive inherited descriptors")
	}
	command.ExtraFiles = []*os.File{control.childRead, control.childWrite}
	return nil
}

// serve runs only transport work; the callback forwards requests into the
// scheduler's own event loop rather than touching graph maps from this goroutine.
func (control *stageWorkerControl) serve(ctx context.Context, stageID string, handler componentReturnHandler, selectComponent componentSelectionHandler, baseHandlers ...baseSelectionHandler) error {
	decoder, encoder := json.NewDecoder(control.parentRead), json.NewEncoder(control.parentWrite)
	seen := make(map[planner.ComponentBoundary]bool)
	var revision uint64
	for {
		var request stageControlRequest
		if err := decoder.Decode(&request); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read stage control request: %w", err)
		}
		reply := stageControlReply{}
		count := 0
		for _, present := range []bool{request.Return != nil, request.Selection != nil, request.Base != nil} {
			if present {
				count++
			}
		}
		if count > 1 {
			reply.Error = "stage control request mixes operation types"
			if err := encoder.Encode(reply); err != nil {
				return err
			}
			continue
		}
		if request.Base != nil {
			if len(baseHandlers) == 0 || baseHandlers[0] == nil {
				reply.Error = "stage has no base selection coordinator"
			} else {
				selected, err := baseHandlers[0](ctx, *request.Base)
				if err != nil {
					reply.Error = err.Error()
				} else {
					reply.Base = &selected
				}
			}
			if err := encoder.Encode(reply); err != nil {
				return errors.Join(ctx.Err(), err)
			}
			continue
		}
		call := request.Return
		if request.Selection != nil {
			if selectComponent == nil {
				reply.Error = "stage has no component selection coordinator"
			} else {
				selected, err := selectComponent(ctx, *request.Selection)
				if err != nil {
					reply.Error = err.Error()
				} else {
					reply.Selection = &selected
				}
			}
			if err := encoder.Encode(reply); err != nil {
				return errors.Join(ctx.Err(), err)
			}
			continue
		}
		switch {
		case call == nil:
			reply.Error = "stage control request lacks a component return"
		case call.Boundary.StageID != stageID || call.Boundary.Instruction < 0:
			reply.Error = "component return has an invalid stage boundary"
		case call.Revision != revision:
			reply.Error = "component return has a stale revision"
		case seen[call.Boundary]:
			reply.Error = "duplicate component return boundary"
		case handler == nil:
			reply.Error = "stage has no component return coordinator"
		default:
			result, err := handler(ctx, *call)
			if err != nil {
				reply.Error = err.Error()
			} else if result.Revision != revision+1 {
				reply.Error = "component return coordinator supplied an invalid revision"
			} else {
				reply.Return = &result
				seen[call.Boundary] = true
				revision = result.Revision
			}
		}
		if err := encoder.Encode(reply); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("write stage control reply: %w", err)
		}
	}
}

type stageWorkerControlClient struct {
	read, write *os.File
	decoder     *json.Decoder
	encoder     *json.Encoder
	mu          sync.Mutex
	pending     bool
}

func stageWorkerControlFromFiles(read, write *os.File) (*stageWorkerControlClient, error) {
	// exec.Cmd clears CLOEXEC for ExtraFiles. Restore it before any RUN or helper
	// process can start, so grandchildren cannot keep these control pipes alive.
	for _, file := range []*os.File{read, write} {
		raw, err := file.SyscallConn()
		if err != nil {
			return nil, err
		}
		var flagErr syscall.Errno
		err = raw.Control(func(fd uintptr) {
			_, _, flagErr = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETFD, syscall.FD_CLOEXEC)
		})
		if err != nil {
			return nil, err
		}
		if flagErr != 0 {
			return nil, flagErr
		}
	}
	return &stageWorkerControlClient{read: read, write: write, decoder: json.NewDecoder(read), encoder: json.NewEncoder(write)}, nil
}

func (client *stageWorkerControlClient) close() {
	_ = client.read.Close()
	_ = client.write.Close()
}

func (client *stageWorkerControlClient) exchange(ctx context.Context, request stageControlRequest) (stageControlReply, error) {
	client.mu.Lock()
	if client.pending {
		client.mu.Unlock()
		return stageControlReply{}, errors.New("concurrent stage control request")
	}
	client.pending = true
	client.mu.Unlock()
	defer func() {
		client.mu.Lock()
		client.pending = false
		client.mu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return stageControlReply{}, err
	}
	stop := context.AfterFunc(ctx, client.close)
	defer stop()
	if err := client.encoder.Encode(request); err != nil {
		return stageControlReply{}, errors.Join(ctx.Err(), fmt.Errorf("send stage control request: %w", err))
	}
	var reply stageControlReply
	if err := client.decoder.Decode(&reply); err != nil {
		return stageControlReply{}, errors.Join(ctx.Err(), fmt.Errorf("receive stage control reply: %w", err))
	}
	if reply.Error != "" {
		return stageControlReply{}, errors.New(reply.Error)
	}
	return reply, nil
}

func (client *stageWorkerControlClient) componentReturn(ctx context.Context, request componentReturnRequest) (componentReturnReply, error) {
	reply, err := client.exchange(ctx, stageControlRequest{Return: &request})
	if err != nil {
		return componentReturnReply{}, err
	}
	if reply.Return == nil || reply.Selection != nil || reply.Base != nil || reply.Return.Revision != request.Revision+1 {
		return componentReturnReply{}, errors.New("invalid component return reply revision")
	}
	return *reply.Return, nil
}

func (client *stageWorkerControlClient) selectComponent(ctx context.Context, request componentSelectionRequest) (componentSelectionReply, error) {
	reply, err := client.exchange(ctx, stageControlRequest{Selection: &request})
	if err != nil {
		return componentSelectionReply{}, err
	}
	if reply.Selection == nil || reply.Return != nil || reply.Base != nil {
		return componentSelectionReply{}, errors.New("invalid component selection reply")
	}
	return *reply.Selection, nil
}

func (client *stageWorkerControlClient) selectBase(ctx context.Context, request baseSelectionRequest) (ResolvedImageSource, error) {
	reply, err := client.exchange(ctx, stageControlRequest{Base: &request})
	if err != nil {
		return ResolvedImageSource{}, err
	}
	if reply.Base == nil || reply.Return != nil || reply.Selection != nil {
		return ResolvedImageSource{}, errors.New("invalid base selection reply")
	}
	return *reply.Base, nil
}

func runControlledStageWorker(ctx context.Context, command *exec.Cmd, stageID string, control *stageWorkerControl, returns componentReturnHandler, selections componentSelectionHandler, bases ...baseSelectionHandler) error {
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer control.close()
	if err := control.attach(command); err != nil {
		return err
	}
	served := make(chan error, 1)
	go func() {
		err := control.serve(ctx, stageID, returns, selections, bases...)
		served <- err
		if err != nil {
			cancel()
		}
	}()
	processErr := runWorkerProcess(ctx, command, stageWorkerGrace)
	cancel()
	control.close()
	serveErr := <-served
	if errors.Is(serveErr, context.Canceled) {
		serveErr = nil
	}
	return errors.Join(processErr, serveErr, parentCtx.Err())
}
