package buildah

import (
	"fmt"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	commonconfig "go.podman.io/common/pkg/config"
	"go.podman.io/image/v5/pkg/compression"
)

// Resolve containers.conf defaults and explicit-flag precedence.
// Compression affects output transport, never step keys.
func normalizeOutputCompression(output Output) (Output, error) {
	if output.compressionResolved {
		return output, nil
	}
	config, err := commonconfig.Default()
	if err != nil {
		return Output{}, fmt.Errorf("load compression configuration: %w", err)
	}
	return applyOutputCompressionDefaults(output, config)
}

func normalizeOutputCompressionForControls(output Output, controls RunControls) (Output, error) {
	if output.compressionResolved {
		return output, nil
	}
	config, err := loadRunContainerConfig(controls.ConfigModules)
	if err != nil {
		return Output{}, err
	}
	return applyOutputCompressionDefaults(output, config)
}

func applyOutputCompressionDefaults(output Output, config *commonconfig.Config) (Output, error) {
	forceCompression := output.ForceCompression != nil && *output.ForceCompression
	if output.CompressionFormat == "" && (config.Engine.CompressionFormat != "gzip" || forceCompression) {
		output.CompressionFormat = config.Engine.CompressionFormat
	}
	if output.CompressionFormat != "" {
		if _, err := compression.AlgorithmByName(output.CompressionFormat); err != nil {
			return Output{}, err
		}
		output.DisableCompression = false
		if output.ForceCompression == nil {
			value := true
			output.ForceCompression = &value
		}
	}
	if output.CompressionLevel == nil {
		output.CompressionLevel = config.Engine.CompressionLevel
	}
	output.compressionResolved = true
	return output, nil
}

func applyFinalCommitOptions(options *upstream.CommitOptions, output Output) error {
	options.Compression = define.Gzip
	if output.DisableCompression && output.CompressionFormat == "" {
		options.Compression = define.Uncompressed
	}
	options.CompressionFormat = nil
	if output.CompressionFormat != "" {
		algorithm, err := compression.AlgorithmByName(output.CompressionFormat)
		if err != nil {
			return err
		}
		options.CompressionFormat = &algorithm
	}
	options.BlobDirectory = output.BlobDirectory
	options.CompressionLevel = output.CompressionLevel
	options.ForceCompressionFormat = output.CompressionFormat != ""
	if output.ForceCompression != nil {
		options.ForceCompressionFormat = *output.ForceCompression
	}
	return nil
}
