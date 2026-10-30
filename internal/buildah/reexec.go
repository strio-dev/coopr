package buildah

import (
	upstream "go.podman.io/buildah"
	"go.podman.io/storage/pkg/unshare"
)

// InitReexec dispatches registered Buildah helpers and Coopr build workers.
// A command using this backend must call it at the start of main.
func InitReexec() bool {
	return upstream.InitReexec()
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
