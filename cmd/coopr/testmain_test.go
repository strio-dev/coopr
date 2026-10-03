package main

import (
	"fmt"
	"os"
	"testing"

	"coopr/internal/buildah"
)

func TestMain(m *testing.M) {
	if os.Getenv("COOPR_TEST_BUILDAH") != "" {
		if buildah.InitRootlessReexec() {
			return
		}
	} else if buildah.InitReexec() {
		return
	}
	dataDir, err := os.MkdirTemp("", "coopr-cli-test-data-*")
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
