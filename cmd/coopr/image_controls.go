package main

import (
	"os"
	"strings"

	"coopr/internal/buildah"
	"github.com/spf13/cobra"
	buildahcli "go.podman.io/buildah/pkg/cli"
)

type imageControlFlags struct {
	env, labels, unsetEnv, unsetLabels             []string
	annotations, unsetAnnotations                  []string
	layerLabels, osFeatures                        []string
	osVersion                                      string
	inheritLabels, inheritAnnotations, omitHistory bool
	identityLabel, createdAnnotation               bool
}

func (flags *imageControlFlags) addTo(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringArrayVarP(&flags.env, "env", "e", nil, "set an image environment variable; NAME, PREFIX*, or * inherits from the host (repeatable)")
	f.StringArrayVar(&flags.labels, "label", nil, "set an image label as NAME[=VALUE] (repeatable)")
	f.StringArrayVar(&flags.unsetEnv, "unsetenv", nil, "remove an environment variable from the final image (repeatable)")
	f.StringArrayVar(&flags.unsetLabels, "unsetlabel", nil, "remove an image label (repeatable)")
	f.StringArrayVar(&flags.annotations, "annotation", nil, "set an OCI image annotation as NAME[=VALUE] (repeatable)")
	f.StringArrayVar(&flags.unsetAnnotations, "unsetannotation", nil, "remove an OCI image annotation (repeatable)")
	f.BoolVar(&flags.inheritLabels, "inherit-labels", true, "inherit labels from base images")
	f.BoolVar(&flags.inheritAnnotations, "inherit-annotations", true, "inherit annotations from base images")
	f.BoolVar(&flags.omitHistory, "omit-history", false, "omit build history from the final image")
	f.BoolVar(&flags.identityLabel, "identity-label", true, "add the default Buildah identity label")
	f.StringArrayVar(&flags.layerLabels, "layer-label", nil, "set metadata on intermediate images as NAME[=VALUE] (repeatable)")
	f.BoolVar(&flags.createdAnnotation, "created-annotation", true, "set org.opencontainers.image.created on image descriptors")
	f.StringArrayVar(&flags.osFeatures, "os-feature", nil, "add a required OS feature; append - to remove one (repeatable)")
	f.StringVar(&flags.osVersion, "os-version", "", "set the required OS version")
}

func (flags imageControlFlags) controls(commands ...*cobra.Command) (buildah.ImageControls, error) {
	env := buildahcli.LookupEnvVarReferences(flags.env, os.Environ())
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
		Env: env, Labels: normalizeValues(flags.labels), UnsetEnv: flags.unsetEnv, UnsetLabels: flags.unsetLabels,
		Annotations: normalizeValues(flags.annotations), UnsetAnnotations: flags.unsetAnnotations,
		DropInheritedLabels: !flags.inheritLabels, DropInheritedAnnotations: !flags.inheritAnnotations,
		OmitHistory: flags.omitHistory,
		LayerLabels: normalizeValues(flags.layerLabels), OSFeatures: flags.osFeatures, OSVersion: flags.osVersion,
	}
	if len(commands) != 0 && commands[0] != nil {
		if commands[0].Flags().Changed("identity-label") {
			controls.IdentityLabel = &flags.identityLabel
		}
		if commands[0].Flags().Changed("created-annotation") {
			controls.CreatedAnnotation = &flags.createdAnnotation
		}
	}
	if err := controls.Validate(); err != nil {
		return buildah.ImageControls{}, err
	}
	return controls, nil
}
