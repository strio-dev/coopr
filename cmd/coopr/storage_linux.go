//go:build linux && cgo

package main

import (
	"fmt"

	"github.com/moby/sys/capability"
	"go.podman.io/storage/pkg/unshare"
)

// Enter the storage library's user/mount namespace before opening a native
// store. This is the capability check used by Skopeo's storage path.
// The library reexecutes Coopr itself and configures subordinate UID/GID maps.
func prepareStorageNamespace() error {
	caps, err := capability.NewPid2(0)
	if err != nil {
		return fmt.Errorf("read storage capabilities: %w", err)
	}
	if err := caps.Load(); err != nil {
		return fmt.Errorf("load storage capabilities: %w", err)
	}
	for _, cap := range []capability.Cap{
		capability.CAP_CHOWN, capability.CAP_DAC_OVERRIDE, capability.CAP_FOWNER,
		capability.CAP_FSETID, capability.CAP_MKNOD, capability.CAP_SETFCAP,
		capability.CAP_SYS_ADMIN,
	} {
		if !caps.Get(capability.EFFECTIVE, cap) {
			unshare.MaybeReexecUsingUserNamespace(true)
			break
		}
	}
	return nil
}
