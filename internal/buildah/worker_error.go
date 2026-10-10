package buildah

import "errors"

// workerError relays the execution status independently of its diagnostic.
// Worker processes themselves exit unsuccessfully to trigger parent cleanup.
type workerError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

func (err *workerError) Error() string { return err.Message }
func (err *workerError) ExitCode() int { return err.Code }

func workerFailure(err error) *workerError {
	if err == nil {
		return nil
	}
	code := 125
	var status interface{ ExitCode() int }
	if errors.As(err, &status) && status.ExitCode() > 0 && status.ExitCode() <= 255 {
		code = status.ExitCode()
	}
	return &workerError{Message: err.Error(), Code: code}
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
