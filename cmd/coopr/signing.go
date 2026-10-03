package main

import (
	"coopr/internal/transfer"
	"github.com/spf13/cobra"
)

type signingFlags struct {
	signBy         string
	privateKeyFile string
	passphraseFile string
}

func (flags *signingFlags) addTo(command *cobra.Command) {
	command.Flags().StringVar(&flags.signBy, "sign-by", "", "sign a local or registry image with the GPG key fingerprint")
	command.Flags().StringVar(&flags.privateKeyFile, "sign-by-sigstore-private-key", "", "sign a registry image with the Sigstore private key at path")
	command.Flags().StringVar(&flags.passphraseFile, "sign-passphrase-file", "", "read the signing-key passphrase from path (Sigstore defaults to COSIGN_PASSWORD)")
}

func (flags signingFlags) options() transfer.SigningOptions {
	return transfer.SigningOptions{SignBy: flags.signBy, SigstorePrivateKeyFile: flags.privateKeyFile, PassphraseFile: flags.passphraseFile}
}
