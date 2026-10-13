package main

import (
	"os"
	"strings"

	"coopr/internal/buildah"
	"github.com/spf13/cobra"
	buildahcli "go.podman.io/buildah/pkg/cli"
)

type imageControlFlags buildahcli.BudResults

func (flags *imageControlFlags) addTo(cmd *cobra.Command) {
	native := buildahcli.GetBudFlags((*buildahcli.BudResults)(flags))
	native.Lookup("env").Shorthand = "e"
	addNativeFlags(cmd, &native, buildahcli.GetBudFlagsCompletions(), "env", "label", "unsetenv", "unsetlabel", "annotation", "unsetannotation", "inherit-labels", "inherit-annotations", "omit-history", "identity-label", "layer-label", "created-annotation", "os-feature", "os-version")
	cmd.Flags().Lookup("env").Usage = "set an image environment variable; NAME, PREFIX*, or * inherits from the host (repeatable)"
}

func (flags imageControlFlags) controls(commands ...*cobra.Command) (buildah.ImageControls, error) {
	env := buildahcli.LookupEnvVarReferences(flags.Envs, os.Environ())
	normalizeValues := func(values []string) []string {
		result := make([]string, len(values))
		for index, item := range values {
			if !strings.Contains(item, "=") {
				item += "="
			}
			result[index] = item
		}
		return result
	}
	controls := buildah.ImageControls{
		Env: env, Labels: normalizeValues(flags.Label), UnsetEnv: flags.UnsetEnvs, UnsetLabels: flags.UnsetLabels,
		Annotations: normalizeValues(flags.Annotation), UnsetAnnotations: flags.UnsetAnnotations,
		DropInheritedLabels: !flags.InheritLabels, DropInheritedAnnotations: !flags.InheritAnnotations,
		OmitHistory: flags.OmitHistory,
		LayerLabels: normalizeValues(flags.LayerLabel), OSFeatures: flags.OSFeatures, OSVersion: flags.OSVersion,
	}
	if len(commands) != 0 && commands[0] != nil {
		if commands[0].Flags().Changed("identity-label") {
			controls.IdentityLabel = &flags.IdentityLabel
		}
		if commands[0].Flags().Changed("created-annotation") {
			controls.CreatedAnnotation = &flags.CreatedAnnotation
		}
	}
	if err := controls.Validate(); err != nil {
		return buildah.ImageControls{}, err
	}
	return controls, nil
}
