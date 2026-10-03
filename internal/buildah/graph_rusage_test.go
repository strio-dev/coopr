package buildah

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingRusageWriter struct{}

func (failingRusageWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestInstructionRusageLoggerWritesNativeFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rusage.log")
	logger, err := newInstructionRusageLogger(PlanOptions{LogRusage: true, RusageLogFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if logger == nil {
		t.Skip("resource usage unsupported")
	}
	if err := logger.log(); err != nil {
		t.Fatal(err)
	}
	if err := logger.close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"(system)", "(user)", "(elapsed)", " input ", " output"} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("log %q lacks %q", data, field)
		}
	}
}

func TestInstructionRusageLoggerReportsWriteFailure(t *testing.T) {
	logger, err := newInstructionRusageLogger(PlanOptions{LogRusage: true})
	if err != nil {
		t.Fatal(err)
	}
	if logger == nil {
		t.Skip("resource usage unsupported")
	}
	logger.writer = failingRusageWriter{}
	if err := logger.log(); err == nil || !strings.Contains(err.Error(), "write resource usage information") {
		t.Fatalf("resource usage write error = %v", err)
	}
}
