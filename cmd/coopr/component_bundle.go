package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/strio-dev/coopr/internal/oci"
)

type componentBundleFlags struct {
	origin, output     string
	inputs, components []string
}

func (f *componentBundleFlags) addTo(cmd *cobra.Command, prefix string) {
	cmd.Flags().StringVar(&f.origin, prefix+"origin", "", "qualified portable origin coordinate; does not publish")
	if prefix == "" {
		cmd.Flags().StringVarP(&f.output, "output", "o", "", "write a new complete Bale OCI layout directory")
	} else {
		cmd.Flags().StringVar(&f.output, prefix+"output", "", "write a new complete Bale OCI layout directory")
	}
	cmd.Flags().StringArrayVar(&f.inputs, "input-bundle", nil, "complete local Bale dependency layout (repeatable)")
	cmd.Flags().StringArrayVar(&f.components, "input-component", nil, "select a local child as IMMUTABLE_REF=LOCAL_NAME; delivered children require their complete input bundle (repeatable)")
}

func (f *componentBundleFlags) requested() bool {
	return f.output != "" || f.origin != "" || len(f.inputs) != 0 || len(f.components) != 0
}

func (f *componentBundleFlags) options() (oci.ComponentBundleOptions, error) {
	options := oci.ComponentBundleOptions{Origin: f.origin, Output: f.output, Inputs: f.inputs, Components: make(map[string]string, len(f.components))}
	for _, value := range f.components {
		reference, local, ok := strings.Cut(value, "=")
		if !ok || reference == "" || local == "" {
			return options, fmt.Errorf("input-component requires IMMUTABLE_REF=LOCAL_NAME")
		}
		if _, exists := options.Components[reference]; exists {
			return options, fmt.Errorf("input-component %q is repeated", reference)
		}
		options.Components[reference] = local
	}
	return options, nil
}

func newComponentBundleCommand() *cobra.Command {
	var flags componentBundleFlags
	cmd := &cobra.Command{
		Use:   "bundle <name-or-digest>",
		Short: "Capture a local component and dependencies as a complete Bale layout",
		Long:  "Capture a newly produced local component without a staging registry. --origin declares its qualified registry coordinate for later Bale delivery; it does not publish. Select local children with --input-component and supply original complete captures for delivered children with --input-bundle. Existing delivery-bound parents retain their original complete bundle.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options, err := flags.options()
			if err != nil {
				return err
			}
			options.Reference = args[0]
			resolver, err := oci.NewResolver(oci.Options{})
			if err != nil {
				return err
			}
			result, err := resolver.BundleComponent(cmd.Context(), options)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), result.Descriptor.Digest)
			return err
		},
	}
	flags.addTo(cmd, "")
	_ = cmd.MarkFlagRequired("origin")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}
