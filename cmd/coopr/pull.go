package main

import (
	"coopr/internal/oci"
	"fmt"
	"github.com/spf13/cobra"
	"go.podman.io/buildah/define"
	buildahparse "go.podman.io/buildah/pkg/parse"
)

func addPullFlags(command *cobra.Command, policy *string) {
	flags := command.Flags()
	*policy = string(oci.PullMissing)
	flags.StringVar(policy, "pull", string(oci.PullMissing), "base image pull policy: always, missing, never, or newer (without a value: always)")
	flags.Lookup("pull").NoOptDefVal = "always"
	flags.Bool("pull-always", false, "always pull base images")
	flags.Bool("pull-never", false, "use only base images already in storage")
	_ = flags.MarkHidden("pull-always")
	_ = flags.MarkHidden("pull-never")
}

func resolveBuildPullPolicy(command *cobra.Command) (string, error) {
	policy, err := buildahparse.PullPolicyFromOptions(command)
	if err != nil {
		return "", err
	}
	return nativePullPolicyString(policy)
}

func nativePullPolicyString(policy define.PullPolicy) (string, error) {
	switch policy {
	case define.PullAlways:
		return string(oci.PullAlways), nil
	case define.PullNever:
		return string(oci.PullNever), nil
	case define.PullIfMissing:
		return string(oci.PullMissing), nil
	case define.PullIfNewer:
		return string(oci.PullNewer), nil
	default:
		return "", fmt.Errorf("unsupported native pull policy %d", policy)
	}
}
