package main

import (
	"errors"

	"coopr/internal/transfer"

	"github.com/spf13/cobra"
)

// pushTransferFlags is shared by image and manifest publication.
type pushTransferFlags struct {
	options      transfer.PushOptions
	level        int
	manifestList bool
}

func (f *pushTransferFlags) addTo(cmd *cobra.Command, manifestList bool) {
	f.manifestList = manifestList
	flags := cmd.Flags()
	formatHelp := "manifest format: oci, v2s2, or v2s1"
	if manifestList {
		formatHelp = "manifest list format: oci or v2s2"
	}
	flags.StringVarP(&f.options.Format, "format", "f", "", formatHelp)
	flags.StringVar(&f.options.CompressionFormat, "compression-format", "", "layer compression format")
	flags.IntVar(&f.level, "compression-level", 0, "layer compression level")
	flags.BoolVar(&f.options.ForceCompression, "force-compression", false, "force the requested layer compression")
	if !manifestList {
		flags.StringArrayVar(&f.options.EncryptionKeys, "encryption-key", nil, "key for encrypting image layers (repeatable)")
		flags.IntSliceVar(&f.options.EncryptLayers, "encrypt-layer", nil, "layer indexes to encrypt (negative indexes count from the end)")
	}
	flags.BoolVar(&f.options.RemoveSignatures, "remove-signatures", false, "discard existing image signatures")
}
func (f *pushTransferFlags) apply(cmd *cobra.Command, options *transfer.Options) error {
	if f.manifestList && f.options.Format == "v2s1" {
		return errors.New("manifest lists support only oci or v2s2 format")
	}
	if cmd.Flags().Changed("compression-level") {
		f.options.CompressionLevel = &f.level
	}
	if cmd.Flags().Changed("compression-format") && !cmd.Flags().Changed("force-compression") {
		f.options.ForceCompression = true
	}
	if cmd.Flags().Changed("encrypt-layer") && len(f.options.EncryptionKeys) == 0 {
		return errors.New("--encrypt-layer requires --encryption-key")
	}
	if err := f.options.Validate(); err != nil {
		return err
	}
	options.Push = f.options
	return nil
}
