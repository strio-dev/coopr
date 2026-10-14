//go:build !containers_image_sequoia

package buildah

// NativeSequoia reports whether this executable includes the upstream Sequoia backend.
const NativeSequoia = false
