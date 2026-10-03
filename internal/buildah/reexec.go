package buildah

import (
	"os"

	upstream "go.podman.io/buildah"
	"go.podman.io/storage/pkg/unshare"
)

// InitReexec dispatches Buildah helpers and Coopr's private OCI runtime
// wrapper. A command using this backend must call it at the start of main.
func InitReexec() bool {
	// The OCI runtime wrapper executes this same binary by /proc/self/exe.
	// Dispatch by a private argument rather than registering that path as a
	// reexec name: Buildah and our workers also use /proc/self/exe normally.
	if len(os.Args) > 1 && os.Args[1] == insecureRuntimeMarker {
		runInsecureRuntimeWrapper()
		return true
	}
	if upstream.InitReexec() {
		return true
	}
	return false
}

// InitRootlessReexec initializes helper dispatch and, when necessary,
// re-executes a test or command in a rootless user namespace.
func InitRootlessReexec() bool {
	if InitReexec() {
		return true
	}
	unshare.MaybeReexecUsingUserNamespace(false)
	return false
}
