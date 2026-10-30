package buildah

import (
	"errors"
	"os/exec"
)

// workerError relays the execution status independently of its diagnostic.
// Worker processes themselves exit unsuccessfully to trigger parent cleanup.
type workerError struct {
	Message        string `json:"message"`
	Code           int    `json:"code"`
	WaitIncomplete bool   `json:"wait_incomplete,omitempty"`
}

func (err *workerError) Error() string { return err.Message }
func (err *workerError) ExitCode() int { return err.Code }
func (err *workerError) Is(target error) bool {
	return err != nil && target == ErrWorkerWaitIncomplete && err.WaitIncomplete
}

// WorkerFailure relays an operation's diagnostic, status and completion state.
func WorkerFailure(err error) *workerError {
	if err == nil {
		return nil
	}
	code := 125
	var status interface{ ExitCode() int }
	if errors.As(err, &status) && status.ExitCode() > 0 && status.ExitCode() <= 255 {
		code = status.ExitCode()
	}
	return &workerError{Message: err.Error(), Code: code, WaitIncomplete: errors.Is(err, ErrWorkerWaitIncomplete)}
}

// WorkerSupervisionError keeps supervision failures alongside a worker response.
// A bare exit status 1 only reports the helper's unsuccessful transport exit; the
// response already supplies the actual operation failure and its exit status.
func WorkerSupervisionError(err error) error {
	if exit, transport := err.(*exec.ExitError); transport && exit.ExitCode() == 1 {
		return nil
	}
	return err
}

type reportedWorkerError struct{ error }

func (err reportedWorkerError) Unwrap() error { return err.error }

func reportedWorkerFailure(err error) error {
	if err == nil {
		return nil
	}
	return reportedWorkerError{err}
}

func workerFailureReported(err error) bool {
	var reported reportedWorkerError
	return errors.As(err, &reported)
}
