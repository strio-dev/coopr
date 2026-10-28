package oci

import (
	"io"
	"os"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

// spoolArtifact verifies an immutable upload input and makes it rewindable
// without retaining package layers in memory. The caller closes and removes it.
func spoolArtifact(reader io.Reader, descriptor v1.Descriptor) (*os.File, error) {
	if err := checkDescriptor(descriptor); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "coopr-registry-*")
	if err != nil {
		return nil, err
	}
	verified := content.NewVerifyReader(reader, descriptor)
	if _, err = io.Copy(file, verified); err == nil {
		err = verified.Verify()
	}
	if err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}
