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
	// Compress requests directory transport compression, as in Podman push.
	Compress                    bool
	AddCompression              []string
	Format                      string
	CompressionFormat           string
	CompressionLevel            *int
	ForceCompression            bool
	ForceCompressionSet         bool
	CompressionDefaultsResolved bool
	EncryptionKeys              []string
	EncryptLayers               []int
	RemoveSignatures            bool
}

func (p PushOptions) ChangesManifest() bool {
	return p.Compress || p.Format != "" || p.CompressionFormat != "" || p.CompressionLevel != nil || len(p.EncryptionKeys) > 0 || len(p.AddCompression) > 0 || p.ForceCompression
}
func (p PushOptions) Validate() error {
	return p.Apply(&imagecopy.Options{DestinationCtx: &types.SystemContext{}})
}

// WithDefaults resolves the native containers.conf publication defaults before
// transport selection and before options are sent to a worker.
func (p PushOptions) WithDefaults() (PushOptions, error) {
	if p.CompressionDefaultsResolved {
		return p, nil
	}
	cfg, err := config.Default()
	if err != nil {
		return p, err
	}
	if p.CompressionFormat == "" && (cfg.Engine.CompressionFormat != "gzip" || p.ForceCompression) {
		p.CompressionFormat = cfg.Engine.CompressionFormat
	}
	if p.CompressionFormat != "" && !p.ForceCompressionSet {
		p.ForceCompression = true
	}
	if p.CompressionLevel == nil {
		p.CompressionLevel = cfg.Engine.CompressionLevel
	}
	p.CompressionDefaultsResolved = true
	return p, nil
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
	var err error
	p, err = p.WithDefaults()
	if err != nil {
		return err
	}

	if options.DestinationCtx == nil {
		options.DestinationCtx = &types.SystemContext{}
	}
	options.DestinationCtx.DirForceCompress = options.DestinationCtx.DirForceCompress || p.Compress
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
