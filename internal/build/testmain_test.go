package build

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"go.podman.io/storage/pkg/reexec"
	"go.podman.io/storage/pkg/unshare"
)

func TestMain(m *testing.M) {
	if reexec.Init() {
		return
	}
	flag.Parse()
	if !testing.Short() && flag.Lookup("test.list").Value.String() == "" {
		// Live native storage needs the same user/mount namespace as the CLI.
		unshare.MaybeReexecUsingUserNamespace(false)
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
