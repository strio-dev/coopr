//go:build !linux || !cgo

package main

import "errors"

func prepareStorageNamespace() error {
	return errors.New("local container storage requires a Linux build with CGO enabled; use --tag oci-archive:PATH for an OCI archive")
}
