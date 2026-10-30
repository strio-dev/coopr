package buildah

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"go.podman.io/storage/pkg/reexec"
)

const retiredRuntimeTestHelper = "coopr-test-retired-runtime-argument"

func init() {
	reexec.Register(retiredRuntimeTestHelper, func() {
		_, _ = fmt.Fprint(os.Stdout, "native-helper-dispatched")
		os.Exit(0)
	})
}

func TestReexecDoesNotDispatchRetiredRuntimeArgument(t *testing.T) {
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"create", "start", "state", "kill", "delete"} {
		t.Run(verb, func(t *testing.T) {
			command := exec.Command(testWorkerBinary(t), "coopr-buildah-insecure-runtime-v1", echo, verb, "retired-executable-ran")
			command.Args[0] = retiredRuntimeTestHelper
			output, err := command.CombinedOutput()
			if err != nil || string(output) != "native-helper-dispatched" {
				t.Fatalf("retired argv marker intercepted native helper dispatch: %v: %s", err, output)
			}
		})
	}
}
