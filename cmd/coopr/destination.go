package main

import "coopr/internal/transfer"

// prepareDestination enters the containers-storage namespace before any
// build or copy operation opens that store.
func prepareDestination(destination transfer.Destination) error {
	if destination.Transport == "podman" {
		return prepareStorageNamespace()
	}
	return nil
}
