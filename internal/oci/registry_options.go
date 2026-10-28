package oci

import "go.podman.io/image/v5/types"

// ApplyTLSVerify preserves native registries.conf policy when verify is omitted.
func ApplyTLSVerify(system *types.SystemContext, verify *bool) {
	system.DockerInsecureSkipTLSVerify = types.OptionalBoolUndefined
	if verify != nil {
		system.DockerInsecureSkipTLSVerify = types.NewOptionalBool(!*verify)
	}
}

func tlsVerifyOption(value types.OptionalBool) *bool {
	if value == types.OptionalBoolUndefined {
		return nil
	}
	verify := value == types.OptionalBoolFalse
	return &verify
}
