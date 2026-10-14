package main

import (
	"fmt"

	"github.com/containerd/platforms"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"go.podman.io/buildah/pkg/parse"
)

func pullPlatform(cmd *cobra.Command, value string) (v1.Platform, error) {
	os, arch, variant, err := parse.Platform(value)
	if err != nil {
		return v1.Platform{}, err
	}
	platform := v1.Platform{OS: os, Architecture: arch, Variant: variant}
	for _, flag := range []string{"os", "arch", "variant"} {
		if !cmd.Flags().Changed(flag) {
			continue
		}
		if cmd.Flags().Changed("platform") {
			return v1.Platform{}, fmt.Errorf("--platform may not be used with --%s", flag)
		}
		value, err := cmd.Flags().GetString(flag)
		if err != nil {
			return v1.Platform{}, err
		}
		switch flag {
		case "os":
			platform.OS = value
		case "arch":
			platform.Architecture = value
			platform.Variant = ""
		case "variant":
			platform.Variant = value
		}
	}
	return platforms.Normalize(platform), nil
}
