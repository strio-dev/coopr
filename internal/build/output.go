package build

import (
	"fmt"

	"coopr/internal/transfer"
)

func archiveOutput(destination transfer.Destination, input string) (string, error) {
	if destination.Transport != "oci-archive" {
		return "", nil
	}
	if isHTTPDefinition(input) {
		return destination.Name, nil
	}
	same, err := sameDestination(destination.Name, input)
	if err != nil {
		return "", err
	}
	if same {
		return "", fmt.Errorf("output %q would overwrite an input", destination.Name)
	}
	return destination.Name, nil
}
