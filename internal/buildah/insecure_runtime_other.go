//go:build !linux

package buildah

import (
	"fmt"
	"os"
)

const (
	insecureRunRequestedMarker = "coopr.internal.run-security-insecure.v1"
	insecureRuntimeExecutable  = "/proc/self/exe"
	insecureRuntimeMarker      = "coopr-buildah-insecure-runtime-v1"
)

func runInsecureRuntimeWrapper() {
	_, _ = fmt.Fprintln(os.Stderr, "coopr: RUN security=insecure requires Linux")
	os.Exit(125)
}
