package main

import (
	"coopr/internal/oci"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.podman.io/buildah/define"
	buildahparse "go.podman.io/buildah/pkg/parse"
)

type pullValue struct {
	policy *string
	pull   *bool
}

func (value pullValue) String() string {
	if *value.policy == "" {
		return string(oci.PullMissing)
	}
	return *value.policy
}

func (value pullValue) Type() string { return "string" }

func (value pullValue) Set(policy string) error {
	flags := pflag.NewFlagSet("pull", pflag.ContinueOnError)
	flags.String("pull", policy, "")
	flags.Bool("pull-always", false, "")
	flags.Bool("pull-never", false, "")
	nativePolicy, err := buildahparse.PullPolicyFromFlagSet(flags, flags.Lookup)
	if err != nil {
		return err
	}
	normalized, err := nativePullPolicyString(nativePolicy)
	if err != nil {
		return err
	}
	*value.policy, *value.pull = normalized, false
	return nil
}

func addPullFlags(command *cobra.Command, policy *string, pull *bool) {
	flags := command.Flags()
	*policy = string(oci.PullMissing)
	flags.Var(pullValue{policy, pull}, "pull", "base image pull policy: always, missing, never, or newer (without a value: always)")
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
