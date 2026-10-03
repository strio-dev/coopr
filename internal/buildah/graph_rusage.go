package buildah

import (
	"fmt"
	"io"
	"os"

	buildahrusage "go.podman.io/buildah/pkg/rusage"
)

type instructionRusageLogger struct {
	previous buildahrusage.Rusage
	writer   io.Writer
	close    func() error
}

func newInstructionRusageLogger(options PlanOptions) (*instructionRusageLogger, error) {
	if !options.LogRusage || !buildahrusage.Supported() {
		return nil, nil
	}
	previous, err := buildahrusage.Get()
	if err != nil {
		return nil, err
	}
	writer := io.Writer(os.Stdout)
	closeFn := func() error { return nil }
	if options.RusageLogFile != "" {
		file, err := os.OpenFile(options.RusageLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open resource usage log: %w", err)
		}
		writer, closeFn = file, file.Close
	}
	return &instructionRusageLogger{previous: previous, writer: writer, close: closeFn}, nil
}

func (logger *instructionRusageLogger) log() error {
	if logger == nil {
		return nil
	}
	usage, err := buildahrusage.Get()
	if err != nil {
		return fmt.Errorf("gather resource usage information: %w", err)
	}
	if _, err := fmt.Fprintln(logger.writer, buildahrusage.FormatDiff(usage.Subtract(logger.previous))); err != nil {
		return fmt.Errorf("write resource usage information: %w", err)
	}
	logger.previous = usage
	return nil
}
