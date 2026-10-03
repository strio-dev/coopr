package buildah

import (
	"fmt"

	"github.com/containerd/platforms"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
)

func executionPlatform(value string) (v1.Platform, error) {
	platform, err := platforms.Parse(value)
	if err != nil {
		return v1.Platform{}, err
	}
	platform = platforms.Normalize(platform)
	if platform.OS != "linux" || platform.Architecture == "" {
		return v1.Platform{}, fmt.Errorf("unsupported platform %q: Coopr requires Linux", value)
	}
	return platform, nil
}

func platformSystemContext(base *types.SystemContext, platform v1.Platform) *types.SystemContext {
	result := types.SystemContext{}
	if base != nil {
		result = *base
	}
	result.OSChoice = platform.OS
	result.ArchitectureChoice = platform.Architecture
	result.VariantChoice = platform.Variant
	return &result
}
