package main

import (
	"fmt"

	"coopr/internal/oci"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"go.podman.io/common/libimage"
	"go.podman.io/common/libimage/manifests"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/storage"
)

type manifestArtifactFlags struct {
	enabled      bool
	artifactType string
	configFile   string
	annotations  []string
	options      libimage.ManifestListAddArtifactOptions
}

func (flags *manifestArtifactFlags) addTo(cmd *cobra.Command) {
	f := cmd.Flags()
	f.BoolVar(&flags.enabled, "artifact", false, "add files as a single OCI artifact manifest")
	f.StringVar(&flags.artifactType, "artifact-type", "", "set artifact type (explicit empty value suppresses it)")
	f.StringVar(&flags.configFile, "artifact-config", "", "read artifact config from file")
	f.StringVar(&flags.options.ConfigType, "artifact-config-type", v1.DescriptorEmptyJSON.MediaType, "set artifact config media type")
	f.StringVar(&flags.options.LayerType, "artifact-layer-type", "", "set artifact layer media type")
	f.StringVar(&flags.options.Subject, "artifact-subject", "", "set artifact subject reference")
	f.BoolVar(&flags.options.ExcludeTitles, "artifact-exclude-titles", false, "omit artifact layer title annotations")
	f.StringArrayVar(&flags.annotations, "artifact-annotation", nil, "set artifact manifest annotation KEY=VALUE (repeatable)")
}

func (flags *manifestArtifactFlags) prepare(cmd *cobra.Command) error {
	for _, name := range []string{"artifact-type", "artifact-config", "artifact-config-type", "artifact-layer-type", "artifact-subject", "artifact-exclude-titles", "artifact-annotation"} {
		if !flags.enabled && cmd.Flags().Changed(name) {
			return fmt.Errorf("--%s requires --artifact", name)
		}
	}
	if !flags.enabled {
		return nil
	}
	if cmd.Flags().Changed("artifact-type") {
		flags.options.Type = &flags.artifactType
	}
	var err error
	flags.options.Annotations, err = parseManifestAnnotations(flags.annotations)
	if err != nil {
		return err
	}

	return nil
}

// Use the public native ConfigFile path so even zero-byte config files retain
// their exact bytes; libimage's string Config field uses empty as unset.
func (flags *manifestArtifactFlags) add(cmd *cobra.Command, store storage.Store, imageID string, registry oci.Options, files ...string) (digest.Digest, error) {
	resolver, err := oci.NewResolver(registry)
	if err != nil {
		return "", err
	}
	options := manifests.AddArtifactOptions{
		ManifestArtifactType: flags.options.Type,
		ConfigFile:           flags.configFile,
		ExcludeTitles:        flags.options.ExcludeTitles,
		Annotations:          flags.options.Annotations,
	}
	if flags.options.ConfigType != "" {
		config := v1.DescriptorEmptyJSON
		config.MediaType = flags.options.ConfigType
		options.ConfigDescriptor = &config
	}
	if cmd.Flags().Changed("artifact-layer-type") {
		options.LayerMediaType = &flags.options.LayerType
	}
	if flags.options.Subject != "" {
		name := flags.options.Subject
		if !manifestMemberUsesTransport(name) {
			name = "docker://" + name
		}
		options.SubjectReference, err = alltransports.ParseImageName(name)
		if err != nil {
			return "", err
		}
	}
	lock, err := manifests.LockerForImage(store, imageID)
	if err != nil {
		return "", err
	}
	lock.Lock()
	defer lock.Unlock()
	_, list, err := manifests.LoadFromImage(store, imageID)
	if err != nil {
		return "", err
	}
	added, err := list.AddArtifact(cmd.Context(), resolver.SystemContext(), options, files...)
	if err != nil {
		return "", err
	}
	_, err = list.SaveToImage(store, imageID, nil, "")
	return added, err
}
