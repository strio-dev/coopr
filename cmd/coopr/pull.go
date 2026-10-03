package main

import (
	"coopr/internal/oci"
	"github.com/spf13/cobra"
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
	normalized, err := oci.NormalizePullPolicy(policy, false)
	if err != nil {
		return err
	}
	*value.policy, *value.pull = string(normalized), false
	return nil
}

func addPullFlags(command *cobra.Command, policy *string, pull *bool) {
	flags := command.Flags()
	flags.StringVar(policy, "pull-policy", string(oci.PullMissing), "base image pull policy: always, missing, never, or newer")
	flags.Var(pullValue{policy, pull}, "pull", "base image pull policy (without a value: always)")
	flags.Lookup("pull").NoOptDefVal = "always"
}
