package main

import (
	"github.com/spf13/cobra"
	"github.com/strio-dev/coopr/internal/oci"
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
	return policy.String(), nil
}
