package buildah

import (
	"fmt"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/pkg/cli"
	"go.podman.io/common/pkg/config"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/pkg/compression"
	"go.podman.io/image/v5/types"
)

// PushOptions are serializable native image-copy controls shared with workers.
type PushOptions struct {
	AddCompression    []string
	Format            string
	CompressionFormat string
	CompressionLevel  *int
	ForceCompression  bool
	EncryptionKeys    []string
	EncryptLayers     []int
	RemoveSignatures  bool
}

func (p PushOptions) ChangesManifest() bool {
	return p.Format != "" || p.CompressionFormat != "" || p.CompressionLevel != nil || len(p.EncryptionKeys) > 0 || len(p.AddCompression) > 0 || p.ForceCompression
}
func (p PushOptions) Validate() error {
	return p.Apply(&imagecopy.Options{DestinationCtx: &types.SystemContext{}})
}
func (p PushOptions) Apply(options *imagecopy.Options) error {
	switch p.Format {
	case "":
	case "oci":
		options.ForceManifestMIMEType = v1.MediaTypeImageManifest
	case "docker", "v2s2":
		options.ForceManifestMIMEType = manifest.DockerV2Schema2MediaType
	case "v2s1":
		options.ForceManifestMIMEType = manifest.DockerV2Schema1SignedMediaType
	default:
		return fmt.Errorf("unsupported manifest format %q", p.Format)
	}
	if p.CompressionFormat == "" && (p.ForceCompression || p.CompressionLevel != nil) {
		cfg, err := config.Default()
		if err != nil {
			return err
		}
		p.CompressionFormat = cfg.Engine.CompressionFormat
	}
	if options.DestinationCtx == nil {
		options.DestinationCtx = &types.SystemContext{}
	}
	if p.CompressionFormat != "" {
		algorithm, err := compression.AlgorithmByName(p.CompressionFormat)
		if err != nil {
			return err
		}
		options.DestinationCtx.CompressionFormat = &algorithm
	}
	options.DestinationCtx.CompressionLevel = p.CompressionLevel
	options.ForceCompressionFormat = p.ForceCompression
	for _, name := range p.AddCompression {
		algorithm, err := compression.AlgorithmByName(name)
		if err != nil {
			return err
		}
		options.EnsureCompressionVariantsExist = append(options.EnsureCompressionVariantsExist, imagecopy.OptionCompressionVariant{Algorithm: algorithm, Level: p.CompressionLevel})
	}
	encrypted, layers, err := cli.EncryptConfig(p.EncryptionKeys, p.EncryptLayers)
	if err != nil {
		return err
	}
	options.OciEncryptConfig = encrypted
	options.OciEncryptLayers = layers
	options.RemoveSignatures = p.RemoveSignatures
	if p.ChangesManifest() {
		options.PreserveDigests = false
	}
	return nil
}
