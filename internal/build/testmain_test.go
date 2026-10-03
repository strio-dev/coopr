package build

import (
	"fmt"
	"os"
	"testing"

	"go.podman.io/storage/pkg/reexec"
)

func TestMain(m *testing.M) {
	if reexec.Init() {
		return
	}
	dataDir, err := os.MkdirTemp("", "coopr-build-test-data-*")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create test data directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("XDG_DATA_HOME", dataDir); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "set test data directory: %v\n", err)
		_ = os.RemoveAll(dataDir)
		os.Exit(1)
	}
	status := m.Run()
	_ = os.RemoveAll(dataDir)
	os.Exit(status)
}
