package acceptance

import (
	"fmt"
	"os"
	"testing"

	"coopr/internal/buildah"

	"go.podman.io/storage/pkg/reexec"
)

func TestMain(m *testing.M) {
	// Build APIs launch same-binary helpers, but host Podman must retain the
	// caller's namespace and storage environment.
	if buildah.InitReexec() {
		return
	}
	os.Exit(m.Run())
}

func init() {
	reexec.Register("coopr-acceptance-dispatch-test", func() {
		fmt.Print("dispatched")
	})
}

func TestAcceptanceReexecDispatch(t *testing.T) {
	command := reexec.Command("coopr-acceptance-dispatch-test")
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "dispatched" {
		t.Fatalf("dispatch acceptance helper: %v: %s", err, output)
	}
}
