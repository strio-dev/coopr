package build

import (
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
	if os.Getenv("COOPR_TEST_PACKAGED_ACCEPTANCE") == "1" {
		// Assembled-image acceptance invokes host Podman, so it must retain the
		// caller's namespace and storage configuration rather than native-test isolation.
		os.Exit(m.Run())
	}
	if os.Getenv("COOPR_TEST_BUILDAH") != "" || os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") != "" || os.Getenv("COOPR_TEST_CONTAINER_STORAGE") != "" {
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
